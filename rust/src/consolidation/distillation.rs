use std::{collections::BTreeMap, env, time::Duration};

use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;

use super::{HeuristicConfig, score_candidate};
use crate::cortex::{Cortex, DistilledTraceSpec, PromotionCandidate};

#[derive(Debug, Clone)]
pub struct DistillationConfig {
    pub window: Duration,
    pub model_tier: String,
    pub model_name: String,
    pub endpoint: String,
    pub api_key_env: String,
    pub max_retries: usize,
    pub dry_run: bool,
    pub heuristic: HeuristicConfig,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize)]
pub struct DistillationResult {
    #[serde(rename = "CandidatesConsidered")]
    pub considered: usize,
    #[serde(rename = "ClustersAttempted")]
    pub attempted: usize,
    #[serde(rename = "DistillationsCreated")]
    pub distilled: usize,
    #[serde(rename = "Rejected")]
    pub rejected: usize,
    #[serde(rename = "FallbackPromotions")]
    pub fallback_promotions: usize,
    #[serde(rename = "Skipped")]
    pub skipped: usize,
    #[serde(rename = "cluster_results", skip_serializing_if = "Vec::is_empty")]
    pub cluster_results: Vec<ClusterResult>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize)]
pub struct ClusterResult {
    pub ids: Vec<String>,
    pub bucket: String,
    pub profile: String,
    pub outcome: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub title: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub tags: Vec<String>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub body: String,
    #[serde(skip_serializing_if = "is_zero")]
    pub confidence: f64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub reason: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub sources: Vec<SourceTrace>,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct SourceTrace {
    pub id: String,
    pub title: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub tags: Vec<String>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub body: String,
}

fn is_zero(value: &f64) -> bool {
    *value == 0.0
}

#[derive(Debug, Clone)]
struct TraceInput {
    id: String,
    title: String,
    tags: Vec<String>,
    body: String,
}

#[derive(Debug, Clone, Default)]
struct Distillation {
    cohesive: bool,
    title: String,
    tags: Vec<String>,
    body: String,
    confidence: f64,
}

#[derive(Debug, Clone, Copy)]
enum Profile {
    Small,
    Large,
    Frontier,
}

impl Profile {
    fn from_name(name: &str) -> Self {
        match name {
            "small" => Self::Small,
            "frontier" => Self::Frontier,
            _ => Self::Large,
        }
    }

    fn name(self) -> &'static str {
        match self {
            Self::Small => "small",
            Self::Large => "large",
            Self::Frontier => "frontier",
        }
    }

    fn max_cluster_size(self) -> usize {
        match self {
            Self::Small => 5,
            Self::Large => 10,
            Self::Frontier => 20,
        }
    }
}

#[derive(Debug, Serialize)]
struct Message<'a> {
    role: &'static str,
    content: &'a str,
}

#[derive(Debug, Serialize)]
struct CompletionRequest<'a> {
    model: &'a str,
    messages: [Message<'a>; 1],
    temperature: f64,
    max_tokens: usize,
    stream: bool,
    chat_template_kwargs: Thinking,
}

#[derive(Debug, Serialize)]
struct Thinking {
    enable_thinking: bool,
}

#[derive(Debug, Deserialize)]
struct CompletionResponse {
    #[serde(default)]
    choices: Vec<Choice>,
    error: Option<ProviderError>,
}

#[derive(Debug, Deserialize)]
struct Choice {
    message: ResponseMessage,
    #[serde(default)]
    finish_reason: String,
}

#[derive(Debug, Deserialize)]
struct ResponseMessage {
    #[serde(default)]
    content: String,
    #[serde(default)]
    reasoning_content: String,
}

#[derive(Debug, Deserialize)]
struct ProviderError {
    message: String,
}

struct HttpLlm {
    endpoint: String,
    api_key: String,
    client: reqwest::Client,
}

impl HttpLlm {
    fn new(endpoint: &str, api_key_env: &str) -> Result<Self> {
        if endpoint.is_empty() {
            bail!("LLM endpoint is empty");
        }
        let endpoint = endpoint.trim_end_matches('/').to_owned();
        reqwest::Url::parse(&endpoint).context("invalid LLM endpoint")?;
        Ok(Self {
            endpoint,
            api_key: env::var(api_key_env).unwrap_or_default(),
            client: reqwest::Client::builder()
                .timeout(Duration::from_secs(5 * 60))
                .build()?,
        })
    }

    async fn complete(
        &self,
        model: &str,
        prompt: &str,
        temperature: f64,
        max_tokens: usize,
        cancellation: &CancellationToken,
    ) -> Result<String> {
        let request = CompletionRequest {
            model,
            messages: [Message {
                role: "user",
                content: prompt,
            }],
            temperature,
            max_tokens,
            stream: false,
            chat_template_kwargs: Thinking {
                enable_thinking: false,
            },
        };
        let mut builder = self
            .client
            .post(format!("{}/chat/completions", self.endpoint))
            .json(&request);
        if !self.api_key.is_empty() {
            builder = builder.bearer_auth(&self.api_key);
        }
        let response = tokio::select! {
            _ = cancellation.cancelled() => bail!("context canceled"),
            response = builder.send() => response.context("posting LLM request")?,
        };
        let status = response.status();
        let bytes = tokio::select! {
            _ = cancellation.cancelled() => bail!("context canceled"),
            bytes = response.bytes() => bytes.context("reading LLM response")?,
        };
        if !status.is_success() {
            if let Ok(parsed) = serde_json::from_slice::<CompletionResponse>(&bytes)
                && let Some(error) = parsed.error
            {
                bail!("LLM endpoint {status}: {}", error.message);
            }
            bail!("LLM endpoint returned {status}");
        }
        let parsed: CompletionResponse = serde_json::from_slice(&bytes)
            .with_context(|| format!("parsing LLM response (status {status})"))?;
        if let Some(error) = parsed.error {
            bail!("LLM error: {}", error.message);
        }
        let choice = parsed
            .choices
            .into_iter()
            .next()
            .context("LLM response has no choices")?;
        if choice.message.content.is_empty() && !choice.message.reasoning_content.is_empty() {
            bail!(
                "LLM produced reasoning only ({} chars, finish={:?})",
                choice.message.reasoning_content.len(),
                choice.finish_reason
            );
        }
        Ok(choice.message.content)
    }
}

pub async fn run_distillation_pass(
    cx: Cortex,
    config: &DistillationConfig,
    cancellation: &CancellationToken,
) -> Result<DistillationResult> {
    let candidates = cx.llm_candidates(config.window)?;
    let mut result = DistillationResult {
        considered: candidates.len(),
        ..DistillationResult::default()
    };
    if candidates.len() < 2 {
        return Ok(result);
    }
    let llm = HttpLlm::new(&config.endpoint, &config.api_key_env)?;
    let profile = Profile::from_name(&config.model_tier);
    let groups = group_candidates(candidates);
    for (bucket, group) in &groups {
        for chunk in group.chunks(profile.max_cluster_size()) {
            if cancellation.is_cancelled() {
                bail!("context canceled");
            }
            if chunk.len() < 2 {
                result.skipped += 1;
                result.cluster_results.push(ClusterResult {
                    ids: ids(chunk),
                    bucket: bucket.clone(),
                    profile: profile.name().into(),
                    outcome: "skipped".into(),
                    reason: "singleton chunk after bucketing".into(),
                    ..ClusterResult::default()
                });
                continue;
            }
            result.attempted += 1;
            let cluster = match build_cluster(&cx, chunk) {
                Ok(cluster) => cluster,
                Err(error) => {
                    result.skipped += 1;
                    result.cluster_results.push(ClusterResult {
                        ids: ids(chunk),
                        bucket: bucket.clone(),
                        profile: profile.name().into(),
                        outcome: "error".into(),
                        reason: format!("build cluster: {error}"),
                        ..ClusterResult::default()
                    });
                    eprintln!("consolidation build cluster failed: {error:#}");
                    continue;
                }
            };
            let sources = source_snapshot(&cluster);
            let mut distilled = None;
            let mut last_error = None;
            for _ in 0..=config.max_retries {
                match run_profile(&llm, profile, &config.model_name, &cluster, cancellation).await {
                    Ok(value) => {
                        distilled = Some(value);
                        break;
                    }
                    Err(error) => last_error = Some(error),
                }
            }
            let Some(distilled) = distilled else {
                if cancellation.is_cancelled() {
                    bail!("context canceled");
                }
                let reason = last_error
                    .map_or_else(|| "LLM request failed".into(), |error| error.to_string());
                if config.dry_run {
                    eprintln!(
                        "consolidation cluster failed after retries; dry-run suppressed fallback: {reason}"
                    );
                    result.skipped += 1;
                    result.cluster_results.push(ClusterResult {
                        ids: ids(chunk),
                        bucket: bucket.clone(),
                        profile: profile.name().into(),
                        outcome: "skipped".into(),
                        reason: format!("llm error (dry-run fallback suppressed): {reason}"),
                        sources,
                        ..ClusterResult::default()
                    });
                    continue;
                }
                eprintln!(
                    "consolidation cluster failed after retries; using heuristic fallback: {reason}"
                );
                let promoted = heuristic_fallback(&cx, chunk, &config.heuristic);
                result.fallback_promotions += promoted;
                result.cluster_results.push(ClusterResult {
                    ids: ids(chunk),
                    bucket: bucket.clone(),
                    profile: profile.name().into(),
                    outcome: "fallback".into(),
                    reason: format!("llm error, heuristic-promoted {promoted}: {reason}"),
                    sources,
                    ..ClusterResult::default()
                });
                continue;
            };
            if !distilled.cohesive {
                result.rejected += 1;
                result.cluster_results.push(ClusterResult {
                    ids: ids(chunk),
                    bucket: bucket.clone(),
                    profile: profile.name().into(),
                    outcome: "rejected".into(),
                    reason: "cohesion gate returned no".into(),
                    sources,
                    ..ClusterResult::default()
                });
                continue;
            }
            let cluster_result = ClusterResult {
                ids: ids(chunk),
                bucket: bucket.clone(),
                profile: profile.name().into(),
                outcome: "distilled".into(),
                title: distilled.title.clone(),
                tags: distilled.tags.clone(),
                body: distilled.body.clone(),
                confidence: distilled.confidence,
                sources,
                ..ClusterResult::default()
            };
            if config.dry_run {
                result.distilled += 1;
                result.cluster_results.push(cluster_result);
                continue;
            }
            let spec = DistilledTraceSpec {
                title: distilled.title,
                body: distilled.body,
                tags: normalize_tags(distilled.tags),
                source_ids: chunk.iter().map(|candidate| candidate.id.clone()).collect(),
                model_name: config.model_name.clone(),
                model_tier_profile: profile.name().into(),
                cohesion_confidence: distilled.confidence,
                ..DistilledTraceSpec::default()
            };
            match cx.create_distilled_trace(spec) {
                Ok(_) => {
                    result.distilled += 1;
                    result.cluster_results.push(cluster_result);
                }
                Err(error) => {
                    result.skipped += 1;
                    result.cluster_results.push(ClusterResult {
                        outcome: "error".into(),
                        reason: format!("submit: {error}"),
                        ..cluster_result
                    });
                    eprintln!("consolidation distilled trace write failed: {error:#}");
                }
            }
        }
    }
    Ok(result)
}

fn ids(candidates: &[PromotionCandidate]) -> Vec<String> {
    candidates
        .iter()
        .map(|candidate| candidate.id.clone())
        .collect()
}

fn source_snapshot(cluster: &[TraceInput]) -> Vec<SourceTrace> {
    cluster
        .iter()
        .map(|trace| SourceTrace {
            id: trace.id.clone(),
            title: trace.title.clone(),
            tags: trace.tags.clone(),
            body: trace.body.clone(),
        })
        .collect()
}

fn group_candidates(
    candidates: Vec<PromotionCandidate>,
) -> BTreeMap<String, Vec<PromotionCandidate>> {
    let mut groups = BTreeMap::new();
    for candidate in candidates {
        if candidate.id.is_empty() {
            continue;
        }
        let day = candidate
            .created_at
            .get(..10)
            .unwrap_or(&candidate.created_at);
        let trace_type = if candidate.trace_type.is_empty() {
            "none"
        } else {
            &candidate.trace_type
        };
        groups
            .entry(format!("{trace_type}|{day}"))
            .or_insert_with(Vec::new)
            .push(candidate);
    }
    groups
}

fn build_cluster(cx: &Cortex, candidates: &[PromotionCandidate]) -> Result<Vec<TraceInput>> {
    candidates
        .iter()
        .map(|candidate| {
            let (row, trace) = cx.get_trace(&candidate.id)?;
            Ok(TraceInput {
                id: row.id,
                title: row.title,
                tags: row.tags,
                body: trace.body,
            })
        })
        .collect()
}

async fn run_profile(
    llm: &HttpLlm,
    profile: Profile,
    model: &str,
    cluster: &[TraceInput],
    cancellation: &CancellationToken,
) -> Result<Distillation> {
    match profile {
        Profile::Frontier => run_frontier(llm, model, cluster, cancellation).await,
        Profile::Small | Profile::Large => {
            let limit = if matches!(profile, Profile::Small) {
                300
            } else {
                400
            };
            let sources = format_traces(cluster, limit);
            let cohesion = cohesion_prompt(cluster.len(), &sources);
            let answer = llm
                .complete(model, &cohesion, 0.0, 16, cancellation)
                .await?;
            if !parse_cohesion(&answer) {
                return Ok(Distillation::default());
            }
            let template = template_prompt(cluster.len(), &sources);
            let raw = llm
                .complete(model, &template, 0.2, 800, cancellation)
                .await?;
            let mut distilled = parse_template(&raw)?;
            distilled.cohesive = true;
            if matches!(profile, Profile::Large) {
                let confidence = confidence_prompt(&distilled, &sources);
                if let Ok(raw) = llm
                    .complete(model, &confidence, 0.0, 120, cancellation)
                    .await
                {
                    distilled.confidence = parse_confidence(&raw);
                }
            }
            Ok(distilled)
        }
    }
}

async fn run_frontier(
    llm: &HttpLlm,
    model: &str,
    cluster: &[TraceInput],
    cancellation: &CancellationToken,
) -> Result<Distillation> {
    let sources = format_traces(cluster, 800);
    let prompt = frontier_prompt(cluster.len(), &sources);
    let raw = llm
        .complete(model, &prompt, 0.2, 1200, cancellation)
        .await?;
    parse_frontier(&raw)
}

fn cohesion_prompt(trace_count: usize, sources: &str) -> String {
    format!(
        r#"Below are {trace_count} short-term memories from a Noema cortex.

Would a single consolidated summary of these be more useful than keeping them as separate short-term memories?

Answer yes if they share a specific common thread. Each of these counts:
- same topic, project, agent session, or line of investigation
- same recurring activity across time (multiple session summaries, multiple heartbeat checks, multiple cron logs, multiple daily status reports) — a time-series of the same activity is cohesive even when individual entries are thin
- same debugging or troubleshooting effort (even across multiple steps)

Answer no if the memories are about different subjects, even if they share superficial features like the same day, the same author, or the same tag. A cluster containing "movie notes", "architecture docs", and "shopping list" is not cohesive even if they were all created on the same Tuesday. If you'd have to write an umbrella like "various activities", "mixed updates", or "general work" to summarize them together, the answer is no.

A shared word or name is also superficial when it refers to different entities. For example, notes about Java the island, Java coffee, and the Java programming language are not one topic merely because they contain "Java".

{sources}

Answer with a single word on one line, with no other text: yes or no."#
    )
}

fn template_prompt(trace_count: usize, sources: &str) -> String {
    format!(
        r#"Below are {trace_count} short-term memories from a Noema cortex that are cohesive enough to consolidate. Write one consolidated memory for the mid-term tier.

{sources}

Grounding rules:
- Only reference entities, projects, tools, people, or topics that actually appear in the memories above. Do not invent, add, or infer topics that are not explicitly present.
- Preserve specific named entities verbatim: skill names, tool names, identifiers, file paths, error strings, bug references, proper nouns. If a source mentions "service-integration-token-expiration" or "*.example.net_ecc TLS glob bug" or "ssh-key-troubleshooting skill", the body should name those specifics rather than flattening them to generic prose like "various skills" or "network issues". The mid-term memory loses all its value if the concrete artifacts disappear.
- The title must describe what the memories are actually about, not what they might relate to. If the cluster is about one thing (e.g. all deployment sessions), the title should name that one thing — do not add unrelated subjects to make the title sound broader.
- Tags should come from the source memories' tags or from terms that appear literally in the bodies.
- The title and body must state the durable memory directly. Do not discuss source formatting, tags, trace IDs, memory tiers, consolidation, or evaluation mechanics unless those are explicitly the subject of the source bodies.

Fill in each field exactly. Do not add other fields, do not omit any:

Title: <one line, <=100 chars, no date prefix>
Tags: <comma-separated list, 1-8 tags, each tag lowercase-kebab-case>
Body: <1-3 paragraphs distilling the cluster>

Tag format rules: each tag must be a single token in lowercase-kebab-case. Good: "mcp-server", "career-goals", "multi-agent", "fastmail-api". Bad: "MCP Server", "AI SME", "Hugging Face", "Memory Consolidation". If a concept naturally has spaces, join the words with hyphens and lowercase them. Never use spaces inside a tag."#
    )
}

fn confidence_prompt(distilled: &Distillation, sources: &str) -> String {
    let title = &distilled.title;
    let tags = distilled.tags.join(", ");
    let body = &distilled.body;
    format!(
        r#"You just wrote this consolidation:

Title: {title}
Tags: {tags}
Body: {body}

From these source memories:

{sources}

Rate how well the consolidation preserves the source information on this calibrated scale:

  10 = every specific fact, name, and detail from every source is preserved
  7-9 = all key points preserved, minor details omitted
  4-6 = general theme preserved, specific facts lost
  1-3 = only a vague umbrella description

Be strict. Most summaries fall in the 4-8 range.

Answer in exactly this format, nothing else:
<one-sentence justification>
<integer 1-10>"#
    )
}

fn frontier_prompt(trace_count: usize, sources: &str) -> String {
    format!(
        r#"Below are {trace_count} short-term memories from a Noema cortex. Your job is to decide whether they belong together (same topic / same decision / same ongoing work) and, if so, to distill them into a single consolidated memory for the mid-term tier.

{sources}

Decision and grounding rules:
- Set cohesive to true only when the memories share a specific topic, project, recurring activity, or line of investigation. Shared dates, authors, tags, or words are not enough.
- A shared word or name is superficial when it refers to different entities. For example, notes about Java the island, Java coffee, and the Java programming language are not one topic merely because they contain "Java".
- Only reference entities, projects, tools, people, or topics that actually appear in the memories above. Do not invent, add, or infer topics that are not explicitly present.
- Preserve specific named entities verbatim: skill names, tool names, identifiers, file paths, error strings, bug references, proper nouns, and numeric values.
- The title must describe what the memories are actually about, not what they might relate to. Tags should come from source tags or terms that appear literally in the source bodies.
- The title and body must state the durable memory directly. Do not discuss source formatting, tags, trace IDs, memory tiers, consolidation, or evaluation mechanics unless those are explicitly the subject of the source bodies.

Respond with a JSON object and nothing else:

{{
  "cohesive": <true|false>,
  "title": "<=100 chars, one line, no date prefix",
  "tags": ["tag1", "tag2", ...],          // 1-8 tags
  "body": "1-3 paragraphs distilling the cluster",
  "confidence": <0.0-1.0>                  // how confident you are the distillation preserves the essential information
}}

If "cohesive" is false, the other fields may be null or omitted."#
    )
}

fn format_traces(cluster: &[TraceInput], body_limit: usize) -> String {
    let mut output = String::new();
    for (index, trace) in cluster.iter().enumerate() {
        let mut characters = trace.body.chars();
        let body: String = characters.by_ref().take(body_limit).collect();
        let truncated = characters.next().is_some();
        output.push_str(&format!(
            "--- Trace {} ---\nTitle: {}\n",
            index + 1,
            trace.title
        ));
        if !trace.tags.is_empty() {
            output.push_str(&format!("Tags: {}\n", trace.tags.join(", ")));
        }
        output.push_str(&format!(
            "Body:\n{}{}\n\n",
            body,
            if truncated { "\n[…truncated]" } else { "" }
        ));
    }
    output
}

fn parse_frontier(raw: &str) -> Result<Distillation> {
    #[derive(Deserialize)]
    struct Body {
        cohesive: bool,
        #[serde(default)]
        title: Option<String>,
        #[serde(default)]
        tags: Option<Vec<String>>,
        #[serde(default)]
        body: Option<String>,
        #[serde(default)]
        confidence: f64,
    }
    let mut source = raw.trim();
    source = source
        .strip_prefix("```json")
        .or_else(|| source.strip_prefix("```"))
        .unwrap_or(source);
    source = source.strip_suffix("```").unwrap_or(source).trim();
    let body: Body = serde_json::from_str(source).context("frontier JSON parse failed")?;
    let distilled = Distillation {
        cohesive: body.cohesive,
        title: body.title.unwrap_or_default().trim().into(),
        tags: body.tags.unwrap_or_default(),
        body: body.body.unwrap_or_default().trim().into(),
        confidence: body.confidence,
    };
    if distilled.cohesive {
        validate_distillation_shape(&distilled)
            .context("frontier response claims cohesive but has invalid shape")?;
    }
    Ok(distilled)
}

fn parse_template(raw: &str) -> Result<Distillation> {
    let mut distilled = Distillation::default();
    let mut body = Vec::new();
    let mut in_body = false;
    for line in raw.lines() {
        let trimmed = line.trim();
        if !in_body {
            if let Some(value) = trimmed.strip_prefix("Title:") {
                distilled.title = value.trim().into();
            } else if let Some(value) = trimmed.strip_prefix("Tags:") {
                distilled.tags = value
                    .split(',')
                    .map(str::trim)
                    .filter(|tag| !tag.is_empty())
                    .map(str::to_owned)
                    .collect();
            } else if let Some(value) = trimmed.strip_prefix("Body:") {
                in_body = true;
                if !value.trim().is_empty() {
                    body.push(value.trim());
                }
            }
        } else {
            body.push(line);
        }
    }
    distilled.body = body.join("\n").trim().into();
    validate_distillation_shape(&distilled).context("template response has invalid shape")?;
    Ok(distilled)
}

fn validate_distillation_shape(distilled: &Distillation) -> Result<()> {
    if distilled.title.is_empty() || distilled.body.is_empty() {
        bail!("missing required title or body");
    }
    if distilled.title.chars().count() > 100 {
        bail!("title exceeds 100 characters");
    }
    let lower_title = distilled.title.to_ascii_lowercase();
    if lower_title.contains("tags:") || lower_title.contains("body:") {
        bail!("title contains an inline field label");
    }
    if distilled.tags.is_empty() || distilled.tags.len() > 8 {
        bail!("tag count {} is outside 1-8", distilled.tags.len());
    }
    Ok(())
}

fn parse_confidence(raw: &str) -> f64 {
    raw.split(|character: char| !character.is_ascii_digit())
        .filter_map(|part| part.parse::<u8>().ok())
        .rfind(|score| *score <= 10)
        .map_or(0.0, |score| f64::from(score) / 10.0)
}

fn parse_cohesion(raw: &str) -> bool {
    let value = raw.trim_start().to_ascii_lowercase();
    value
        .strip_prefix("yes")
        .is_some_and(|rest| rest.chars().next().is_none_or(|ch| !ch.is_alphanumeric()))
}

fn normalize_tags(tags: Vec<String>) -> Vec<String> {
    let mut normalized = Vec::new();
    for tag in tags {
        let mut output = String::new();
        let mut hyphen = false;
        for character in tag.trim().to_ascii_lowercase().chars() {
            if character.is_ascii_alphanumeric() || character == '_' {
                output.push(character);
                hyphen = false;
            } else if matches!(character, '-' | ' ' | '\t' | '/' | '.' | ',')
                && !hyphen
                && !output.is_empty()
            {
                output.push('-');
                hyphen = true;
            }
        }
        let output = output.trim_end_matches('-').to_owned();
        if output.len() >= 2 && !normalized.contains(&output) {
            normalized.push(output);
        }
    }
    normalized
}

fn heuristic_fallback(
    cx: &Cortex,
    candidates: &[PromotionCandidate],
    config: &HeuristicConfig,
) -> usize {
    candidates
        .iter()
        .filter(|candidate| score_candidate(candidate, config) >= config.promotion_threshold)
        .filter(|candidate| match cx.promote(&candidate.id, "mid") {
            Ok(()) => true,
            Err(error) => {
                eprintln!(
                    "consolidation fallback promote failed id={}: {error:#}",
                    candidate.id
                );
                false
            }
        })
        .count()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn frontier_parser_accepts_fences_and_rejects_incomplete_cohesive_output() {
        let parsed = parse_frontier(
            "```json\n{\"cohesive\":true,\"title\":\"Result\",\"tags\":[\"MCP Server\"],\"body\":\"Body\",\"confidence\":0.8}\n```",
        )
        .unwrap();
        assert!(parsed.cohesive);
        assert_eq!(parsed.confidence, 0.8);
        assert!(parse_frontier(r#"{"cohesive":true}"#).is_err());
        assert!(!parse_frontier(r#"{"cohesive":false}"#).unwrap().cohesive);
    }

    #[test]
    fn tag_and_template_normalization_matches_go_shape() {
        assert_eq!(
            normalize_tags(vec!["MCP Server".into(), "mcp-server".into(), "x".into()]),
            vec!["mcp-server"]
        );
        let parsed = parse_template("Title: T\nTags: A, b\nBody: first\nsecond").unwrap();
        assert_eq!(parsed.title, "T");
        assert_eq!(parsed.body, "first\nsecond");
        assert_eq!(parse_confidence("Looks good.\n8"), 0.8);
        assert!(parse_cohesion(" yes."));
        assert!(!parse_cohesion("yesterday"));
    }

    #[test]
    fn malformed_template_shapes_fail_closed() {
        for raw in [
            "Title: T\nTags:\nBody: B".to_owned(),
            "Title: T, Tags: x\nBody: B".to_owned(),
            format!("Title: {}\nTags: x\nBody: B", "x".repeat(101)),
            "Title: T\nTags: a, b, c, d, e, f, g, h, i\nBody: B".to_owned(),
        ] {
            assert!(
                parse_template(&raw).is_err(),
                "unexpectedly accepted {raw:?}"
            );
        }
    }

    #[test]
    fn model_input_omits_internal_trace_ids() {
        let cluster = vec![TraceInput {
            id: "20260420-private-source".into(),
            title: "Source title".into(),
            tags: vec!["source".into()],
            body: "Source body".into(),
        }];
        let formatted = format_traces(&cluster, 300);
        assert!(!formatted.contains("20260420-private-source"));
        assert!(!formatted.contains("id="));
        assert!(formatted.contains("--- Trace 1 ---"));
    }

    #[test]
    fn zero_temperature_request_is_explicit() {
        let request = CompletionRequest {
            model: "fixture-model",
            messages: [Message {
                role: "user",
                content: "prompt",
            }],
            temperature: 0.0,
            max_tokens: 16,
            stream: false,
            chat_template_kwargs: Thinking {
                enable_thinking: false,
            },
        };
        let value = serde_json::to_value(request).unwrap();
        assert_eq!(value["temperature"], 0.0);
        assert_eq!(value["max_tokens"], 16);
        assert_eq!(value["chat_template_kwargs"]["enable_thinking"], false);
    }
}
