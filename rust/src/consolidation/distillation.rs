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
    pub heuristic: HeuristicConfig,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct DistillationResult {
    pub considered: usize,
    pub attempted: usize,
    pub distilled: usize,
    pub rejected: usize,
    pub fallback_promotions: usize,
    pub skipped: usize,
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
    for group in groups.values() {
        for chunk in group.chunks(profile.max_cluster_size()) {
            if cancellation.is_cancelled() {
                bail!("context canceled");
            }
            if chunk.len() < 2 {
                result.skipped += 1;
                continue;
            }
            result.attempted += 1;
            let cluster = match build_cluster(&cx, chunk) {
                Ok(cluster) => cluster,
                Err(error) => {
                    result.skipped += 1;
                    eprintln!("consolidation build cluster failed: {error:#}");
                    continue;
                }
            };
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
                if let Some(error) = last_error {
                    eprintln!(
                        "consolidation cluster failed after retries; using heuristic fallback: {error:#}"
                    );
                }
                result.fallback_promotions += heuristic_fallback(&cx, chunk, &config.heuristic);
                continue;
            };
            if !distilled.cohesive {
                result.rejected += 1;
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
                Ok(_) => result.distilled += 1,
                Err(error) => {
                    result.skipped += 1;
                    eprintln!("consolidation distilled trace write failed: {error:#}");
                }
            }
        }
    }
    Ok(result)
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
            let cohesion = format!(
                "Below are {} short-term memories from a Noema cortex. Would a single consolidated summary be more useful because they share a specific topic, project, recurring activity, or investigation? Answer with a single word: yes or no.\n\n{}",
                cluster.len(),
                sources
            );
            let answer = llm
                .complete(model, &cohesion, 0.0, 16, cancellation)
                .await?;
            if !parse_cohesion(&answer) {
                return Ok(Distillation::default());
            }
            let template = format!(
                "Write one consolidated memory grounded only in these sources. Preserve specific names, identifiers, paths, and errors. Respond exactly as Title: <one line>, Tags: <comma-separated lowercase-kebab-case>, Body: <1-3 paragraphs>.\n\n{}",
                sources
            );
            let raw = llm
                .complete(model, &template, 0.2, 800, cancellation)
                .await?;
            let mut distilled = parse_template(&raw)?;
            distilled.cohesive = true;
            if matches!(profile, Profile::Large) {
                let confidence = format!(
                    "Rate how well this consolidation preserves the source information from 1-10. Give one sentence then the integer.\n\nTitle: {}\nTags: {}\nBody: {}\n\nSources:\n{}",
                    distilled.title,
                    distilled.tags.join(", "),
                    distilled.body,
                    sources
                );
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
    let prompt = format!(
        "Below are {} short-term memories from a Noema cortex. Decide whether they belong together and, if so, distill them into one mid-term memory. Respond with a JSON object and nothing else using cohesive (boolean), title, tags (array), body, and confidence (0.0-1.0). If cohesive is false, other fields may be null or omitted.\n\n{}",
        cluster.len(),
        format_traces(cluster, 800)
    );
    let raw = llm
        .complete(model, &prompt, 0.2, 1200, cancellation)
        .await?;
    parse_frontier(&raw)
}

fn format_traces(cluster: &[TraceInput], body_limit: usize) -> String {
    let mut output = String::new();
    for (index, trace) in cluster.iter().enumerate() {
        let mut characters = trace.body.chars();
        let body: String = characters.by_ref().take(body_limit).collect();
        let truncated = characters.next().is_some();
        output.push_str(&format!(
            "--- Trace {} (id={}) ---\nTitle: {}\n",
            index + 1,
            trace.id,
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
    if distilled.cohesive && (distilled.title.is_empty() || distilled.body.is_empty()) {
        bail!("frontier response claims cohesive but is missing title or body");
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
    if distilled.title.is_empty() || distilled.body.is_empty() {
        bail!("template response is missing required fields");
    }
    Ok(distilled)
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
}
