use std::{
    collections::{BTreeMap, HashMap, HashSet},
    fmt::Write as _,
    path::PathBuf,
    sync::Arc,
};

use anyhow::{Context, Result, bail};
use axum::{
    body::Body,
    extract::State,
    http::{HeaderValue, Method, Request, StatusCode, header},
    middleware::Next,
    response::{IntoResponse, Response},
};
use rmcp::{
    ErrorData, Json, ServerHandler, ServiceExt,
    handler::server::{router::tool::ToolRouter, wrapper::Parameters},
    tool, tool_handler, tool_router,
    transport::streamable_http_server::{
        StreamableHttpServerConfig, StreamableHttpService, session::local::LocalSessionManager,
    },
};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::{Digest, Sha256};
use tokio::sync::{Mutex, OwnedMutexGuard};
use tokio_util::sync::CancellationToken;

use crate::{
    VERSION,
    cortex::{
        AccessKey, Cortex, DistilledTraceSpec, ListOptions, SemanticOptions, load_access_key,
        parse_since,
    },
    embedding::HttpEmbedder,
    lock::CortexLock,
    trace::Trace,
};

#[derive(Clone)]
pub struct NoemaServer {
    cortex: Arc<Mutex<Cortex>>,
    federation_mode: String,
    tool_router: ToolRouter<Self>,
}

impl NoemaServer {
    pub fn new(
        name: impl Into<String>,
        path: impl Into<PathBuf>,
        remote_transport: bool,
    ) -> Result<Self> {
        let name = name.into();
        let cortex = Cortex::open(&name, path.into())?;
        let federation_mode = if remote_transport {
            cortex
                .manifest
                .federation
                .as_ref()
                .map(|config| config.mode.as_str())
                .filter(|mode| !mode.is_empty())
                .unwrap_or("sync")
                .to_owned()
        } else {
            String::new()
        };
        Ok(Self {
            cortex: Arc::new(Mutex::new(cortex)),
            federation_mode,
            tool_router: Self::tool_router(),
        })
    }

    async fn open(&self) -> Result<OwnedMutexGuard<Cortex>, ErrorData> {
        Ok(self.cortex.clone().lock_owned().await)
    }

    fn ensure_writable(&self) -> Result<(), ErrorData> {
        if self.federation_mode == "publish" {
            return Err(ErrorData::invalid_params(
                "this cortex is in publish mode (read-only for remote peers); use a local stdio transport for writes",
                None,
            ));
        }
        Ok(())
    }
}

#[tool_handler(router = self.tool_router, name = "noema", version = "0.21.0")]
impl ServerHandler for NoemaServer {}

#[derive(Debug, Deserialize, JsonSchema)]
struct Empty {}

#[derive(Debug, Deserialize, JsonSchema)]
struct IdParam {
    /// Trace ID
    id: String,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct GetParams {
    /// Trace ID
    id: String,
    /// Record this as an agent read for memory-tier promotion signals (default false). Pass true for task-driven retrieval where the read should count toward durability.
    #[serde(default)]
    record_usage: bool,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct ListParams {
    /// Filter by trace type
    #[serde(default, rename = "type")]
    trace_type: String,
    /// Filter by author
    #[serde(default)]
    author: String,
    /// Filter by tag
    #[serde(default)]
    tag: String,
    /// Filter by origin cortex name
    #[serde(default)]
    origin: String,
    /// Show only archived traces
    #[serde(default)]
    archived: bool,
    /// Show active and archived traces
    #[serde(default)]
    all: bool,
}

#[derive(Debug, JsonSchema)]
#[schemars(rename_all = "lowercase")]
#[allow(dead_code)]
enum TraceTypeSchema {
    Fact,
    Decision,
    Preference,
    Context,
    Skill,
    Intent,
    Observation,
    Note,
    Divergence,
}

#[derive(Debug, JsonSchema)]
#[schemars(rename_all = "lowercase")]
#[allow(dead_code)]
enum VoteDirectionSchema {
    Up,
    Down,
}

#[derive(Debug, JsonSchema)]
#[schemars(rename_all = "lowercase")]
#[allow(dead_code)]
enum ModelTierSchema {
    Small,
    Large,
    Frontier,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct CreateParams {
    /// Trace title
    title: String,
    /// Trace type
    #[serde(rename = "type")]
    #[schemars(with = "TraceTypeSchema")]
    trace_type: String,
    /// Trace body content
    body: String,
    /// Author name or agent identifier
    #[serde(default)]
    author: String,
    /// Comma-separated tags
    #[serde(default)]
    tags: String,
    /// Comma-separated trace IDs this trace was derived from
    #[serde(default)]
    derived_from: String,
    /// Origin cortex name (defaults to current cortex)
    #[serde(default)]
    origin: String,
    /// Content hash from the source/publisher cortex
    #[serde(default)]
    source_hash: String,
    /// Mark trace as source-locked (immutable on consumer side)
    #[serde(default)]
    source_locked: bool,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct UpdateParams {
    /// Trace ID
    id: String,
    /// New title
    title: Option<String>,
    /// New type
    #[serde(rename = "type")]
    #[schemars(with = "Option<TraceTypeSchema>")]
    trace_type: Option<String>,
    /// New author
    author: Option<String>,
    /// New tags, comma-separated (replaces existing tags)
    tags: Option<String>,
    /// New derived_from, comma-separated trace IDs (replaces existing lineage)
    derived_from: Option<String>,
    /// New body content
    body: Option<String>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct SearchParams {
    /// Search query
    query: String,
    /// Include archived traces
    #[serde(default)]
    all: bool,
    /// Search mode: 'lexical' (FTS5, default), 'semantic' (embedding similarity), or 'hybrid'. Semantic/hybrid need a configured search block; if unavailable, falls back to lexical.
    #[serde(default)]
    mode: String,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct SimilarParams {
    /// ID of the source trace
    trace_id: String,
    /// Maximum matches to return (default 10)
    limit: Option<usize>,
    /// Include archived traces (default false)
    include_archived: Option<bool>,
    /// 'lexical' (FTS5, default) or 'semantic'/'hybrid' (embedding similarity). Falls back to lexical if semantic isn't configured or the source isn't embedded.
    mode: Option<String>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct TagsParam {
    /// Trace ID
    id: String,
    /// Comma-separated tags
    tags: String,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct TagStatsParams {
    /// Maximum tag rows to return (default 50, maximum 1000; 0 returns all)
    limit: Option<usize>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct RenameTagParams {
    /// Exact source tag spelling
    old_tag: String,
    /// Destination tag spelling
    new_tag: String,
    /// Match ASCII case variants of old_tag
    #[serde(default)]
    ignore_case: bool,
    /// Apply the plan. Defaults to false and returns a read-only preview.
    #[serde(default)]
    apply: bool,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct DeleteTagParams {
    /// Exact tag spelling to remove
    tag: String,
    /// Match ASCII case variants of tag
    #[serde(default)]
    ignore_case: bool,
    /// Apply the plan. Defaults to false and returns a read-only preview.
    #[serde(default)]
    apply: bool,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct TagDoctorParams {
    /// Build a deterministic repair plan. Ambiguous findings remain report-only.
    #[serde(default)]
    fix: bool,
    /// Apply the deterministic repair plan. Requires fix=true.
    #[serde(default)]
    apply: bool,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct VoteParam {
    /// Trace ID to vote on
    id: String,
    /// 'up' for promotion preference, 'down' for demotion preference
    #[schemars(with = "VoteDirectionSchema")]
    direction: String,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct AppendParam {
    /// Trace ID
    id: String,
    /// Content to append to the trace body
    content: String,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct SinceParam {
    /// Cursor; interpretation depends on the sync tool
    since: Option<String>,
    /// Maximum rows to return (default 100, max 1000)
    limit: Option<usize>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct TopParam {
    /// How many top traces and tags to return. Default 10. Capped at 100.
    top: Option<f64>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct CandidateParam {}

#[derive(Debug, Deserialize, JsonSchema)]
struct HealthParam {
    /// Lookback window for activity buckets, for example '24h' or '7d'
    since: Option<String>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct ConsolidateParam {
    /// Title for the distilled trace
    title: String,
    /// Body of the distilled trace
    body: String,
    /// Comma-separated source trace IDs (at least 2)
    source_ids: String,
    /// Comma-separated tags (optional)
    tags: Option<String>,
    /// Author identifier for the distilled trace (optional)
    author: Option<String>,
    /// Model that produced the distillation (optional)
    model_name: Option<String>,
    /// Prompt profile used (optional)
    #[schemars(with = "Option<ModelTierSchema>")]
    model_tier_profile: Option<String>,
    /// Confidence 0.0-1.0 that the cluster was cohesive (optional)
    cohesion_confidence: Option<f64>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct ResolveParam {
    /// Divergence trace ID
    id: String,
    /// Origin name whose version to accept
    accept: Option<String>,
    /// Custom merged body content, mutually exclusive with accept
    body: Option<String>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct AnnounceParam {
    /// Name of the announcing cortex
    name: String,
    /// Streamable HTTP base URL of the announcing cortex
    endpoint: String,
}

#[derive(Debug, Serialize, JsonSchema)]
struct TagMutationOutput {
    id: String,
    action: TagMutationAction,
    tags: Vec<String>,
}

#[derive(Debug, Serialize, JsonSchema)]
#[serde(rename_all = "lowercase")]
#[schemars(rename_all = "lowercase")]
enum TagMutationAction {
    Set,
    Append,
}

fn schema_version_schema(_: &mut schemars::SchemaGenerator) -> schemars::Schema {
    schemars::json_schema!({
        "type": "integer",
        "minimum": 0
    })
}

#[derive(Debug, Serialize, JsonSchema)]
struct CortexUsageOutput {
    #[schemars(schema_with = "schema_version_schema")]
    schema_version: u32,
    cortex: BTreeMap<String, serde_json::Value>,
    contract: CortexContractOutput,
    startup: BTreeMap<String, serde_json::Value>,
    trace_model: BTreeMap<String, serde_json::Value>,
    search: BTreeMap<String, serde_json::Value>,
    workflows: BTreeMap<String, serde_json::Value>,
    runtime: BTreeMap<String, serde_json::Value>,
    authoring_tips: Vec<String>,
}

#[derive(Debug, Serialize, JsonSchema)]
struct CortexContractOutput {
    tool_discovery_authoritative: bool,
    markdown_instructions_tool: String,
    structured_usage_tool: String,
    callable_tools_policy: String,
}

#[tool_router(router = tool_router)]
impl NoemaServer {
    #[tool(
        description = "Returns concise Markdown guidance for agent use of this Cortex. Call this first if you are unfamiliar with Noema; use cortex_usage for structured MCP/client context."
    )]
    async fn get_instructions(&self, _: Parameters<Empty>) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        Ok(render_instructions(&cx.manifest))
    }

    #[tool(
        description = "Returns structured JSON context for MCP clients: active Cortex identity, trace semantics, startup preference pattern, runtime posture, and operational constraints. Tool discovery remains authoritative for callable tools."
    )]
    async fn cortex_usage(
        &self,
        _: Parameters<Empty>,
    ) -> Result<Json<CortexUsageOutput>, ErrorData> {
        let cx = self.open().await?;
        build_cortex_usage(&cx).map(Json).map_err(mcp_error)
    }

    #[tool(description = "List traces in the cortex")]
    async fn list_traces(
        &self,
        Parameters(p): Parameters<ListParams>,
    ) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        let rows = cx
            .list(&ListOptions {
                trace_type: p.trace_type,
                author: p.author,
                tag: p.tag,
                origin: p.origin,
                archived: p.archived,
                all: p.all,
                ..Default::default()
            })
            .map_err(mcp_error)?;
        Ok(format_rows(&rows))
    }

    #[tool(description = "Get a trace by ID, including its full body")]
    async fn get_trace(&self, Parameters(p): Parameters<GetParams>) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        let (row, trace) = cx.get_trace(&p.id).map_err(mcp_error)?;
        if p.record_usage {
            cx.bump_read(&p.id).map_err(mcp_error)?;
        }
        Ok(json_text(
            json!({"id":row.id,"title":row.title,"type":row.trace_type,"tier":row.tier,"author":row.author,"tags":row.tags,"derived_from":row.derived_from,"origin":row.origin,"created":row.created_at,"updated":row.updated_at,"body":trace.body,"content_hash":row.content_hash,"source_hash":row.source_hash,"source_locked":row.source_locked}),
        ))
    }

    #[tool(description = "Create a new trace")]
    async fn create_trace(
        &self,
        Parameters(p): Parameters<CreateParams>,
    ) -> Result<String, ErrorData> {
        self.ensure_writable()?;
        let cx = self.open().await?;
        let mut trace = Trace::new(p.title, p.trace_type, p.author, csv(&p.tags), p.body);
        trace.frontmatter.derived_from = csv(&p.derived_from);
        trace.frontmatter.origin = p.origin;
        trace.frontmatter.source_hash = p.source_hash;
        trace.frontmatter.source_locked = p.source_locked;
        cx.add(&mut trace).map_err(mcp_error)?;
        Ok(format!("Trace created: {}", trace.frontmatter.id))
    }

    #[tool(description = "Full-text search across traces")]
    async fn search_traces(
        &self,
        Parameters(p): Parameters<SearchParams>,
    ) -> Result<String, ErrorData> {
        let mut cx = self.open().await?;
        let (mode, embedder, model, weight) =
            resolve_search_mode(&cx, &p.mode).map_err(mcp_error)?;
        let mut note = String::new();
        let rows = if matches!(mode.as_str(), "semantic" | "hybrid") {
            if let Some(embedder) = embedder {
                let options = SemanticOptions {
                    model,
                    include_archived: p.all,
                    ..Default::default()
                };
                let scored = if mode == "hybrid" {
                    cx.hybrid_search(&embedder, &p.query, &options, weight)
                        .await
                } else {
                    cx.semantic_search(&embedder, &p.query, &options).await
                };
                match scored {
                    Ok(scored) => scored.into_iter().map(|item| item.row).collect(),
                    Err(_) => {
                        note = format!(
                            "[{mode} search temporarily unavailable; showing lexical results]\n"
                        );
                        cx.search(
                            &p.query,
                            &ListOptions {
                                all: p.all,
                                ..Default::default()
                            },
                        )
                        .map_err(mcp_error)?
                    }
                }
            } else {
                note = "[semantic search not configured; showing lexical results]\n".into();
                cx.search(
                    &p.query,
                    &ListOptions {
                        all: p.all,
                        ..Default::default()
                    },
                )
                .map_err(mcp_error)?
            }
        } else {
            cx.search(
                &p.query,
                &ListOptions {
                    all: p.all,
                    ..Default::default()
                },
            )
            .map_err(mcp_error)?
        };
        cx.bump_search_hits(&rows);
        Ok(note + &format_rows(&rows))
    }

    #[tool(description = "Find traces related to a given trace")]
    async fn find_similar_traces(
        &self,
        Parameters(p): Parameters<SimilarParams>,
    ) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        let requested_mode = p.mode.as_deref().unwrap_or("");
        let (mode, _, model, weight) =
            resolve_search_mode(&cx, requested_mode).map_err(mcp_error)?;
        let include_archived = p.include_archived.unwrap_or(false);
        let limit = p.limit.unwrap_or(10);
        let mut note = None;
        let rows = if matches!(mode.as_str(), "semantic" | "hybrid") {
            if model.is_empty() {
                note = Some("semantic search not configured; showing lexical results".into());
                cx.find_similar(&p.trace_id, limit, include_archived)
                    .map_err(mcp_error)?
            } else {
                let options = SemanticOptions {
                    model,
                    limit,
                    include_archived,
                };
                let scored = if mode == "hybrid" {
                    cx.hybrid_similar(&p.trace_id, &options, weight)
                } else {
                    cx.semantic_similar(&p.trace_id, &options)
                };
                match scored {
                    Ok(scored) => scored.into_iter().map(|item| item.row).collect(),
                    Err(_) => {
                        note = Some(format!(
                            "{mode} similar temporarily unavailable; showing lexical results"
                        ));
                        cx.find_similar(&p.trace_id, limit, include_archived)
                            .map_err(mcp_error)?
                    }
                }
            }
        } else {
            cx.find_similar(&p.trace_id, limit, include_archived)
                .map_err(mcp_error)?
        };
        cx.bump_search_hits(&rows);
        Ok(json_text(json!({"mode":mode,"note":note,"results":rows})))
    }

    #[tool(
        description = "Move a trace to trash (soft-delete, recoverable for 30 days). Use recover_trace to restore it."
    )]
    async fn delete_trace(&self, Parameters(p): Parameters<IdParam>) -> Result<String, ErrorData> {
        self.ensure_writable()?;
        self.open().await?.trash(&p.id).map_err(mcp_error)?;
        Ok("Trace moved to trash".into())
    }
    #[tool(description = "Restore a trace from trash back to active")]
    async fn recover_trace(&self, Parameters(p): Parameters<IdParam>) -> Result<String, ErrorData> {
        self.ensure_writable()?;
        self.open().await?.recover(&p.id).map_err(mcp_error)?;
        Ok("Trace recovered".into())
    }
    #[tool(description = "Archive a trace")]
    async fn archive_trace(&self, Parameters(p): Parameters<IdParam>) -> Result<String, ErrorData> {
        self.ensure_writable()?;
        self.open().await?.archive(&p.id).map_err(mcp_error)?;
        Ok("Trace archived".into())
    }
    #[tool(description = "Restore an archived trace")]
    async fn unarchive_trace(
        &self,
        Parameters(p): Parameters<IdParam>,
    ) -> Result<String, ErrorData> {
        self.ensure_writable()?;
        self.open().await?.unarchive(&p.id).map_err(mcp_error)?;
        Ok("Trace unarchived".into())
    }

    #[tool(description = "Update fields of an existing trace. Only provided fields are changed.")]
    async fn update_trace(
        &self,
        Parameters(p): Parameters<UpdateParams>,
    ) -> Result<String, ErrorData> {
        self.ensure_writable()?;
        let cx = self.open().await?;
        let (_, mut trace) = cx.get_trace(&p.id).map_err(mcp_error)?;
        if let Some(v) = p.title {
            trace.frontmatter.title = v;
        }
        if let Some(v) = p.trace_type {
            trace.frontmatter.trace_type = v;
        }
        if let Some(v) = p.author {
            trace.frontmatter.author = v;
        }
        if let Some(v) = p.tags {
            trace.frontmatter.tags = csv(&v);
        }
        if let Some(v) = p.derived_from {
            trace.frontmatter.derived_from = csv(&v);
        }
        if let Some(v) = p.body {
            trace.body = v;
        }
        cx.update_trace(&p.id, &mut trace, true)
            .map_err(mcp_error)?;
        Ok("Trace updated".into())
    }

    #[tool(
        description = "Replace a trace's tags with the provided comma-separated list. Use for metadata hygiene; do not use vote_trace as a substitute for tag cleanup."
    )]
    async fn set_trace_tags(
        &self,
        Parameters(p): Parameters<TagsParam>,
    ) -> Result<Json<TagMutationOutput>, ErrorData> {
        self.ensure_writable()?;
        let tags = csv(&p.tags);
        self.open()
            .await?
            .set_tags(&p.id, tags.clone(), true)
            .map_err(mcp_error)?;
        Ok(Json(TagMutationOutput {
            id: p.id,
            action: TagMutationAction::Set,
            tags,
        }))
    }
    #[tool(
        description = "Add tags to a trace idempotently. Use for retrieval metadata; do not use vote_trace as a substitute for tag cleanup."
    )]
    async fn append_trace_tags(
        &self,
        Parameters(p): Parameters<TagsParam>,
    ) -> Result<Json<TagMutationOutput>, ErrorData> {
        self.ensure_writable()?;
        let tags = self
            .open()
            .await?
            .append_tags(&p.id, csv(&p.tags), true)
            .map_err(mcp_error)?;
        Ok(Json(TagMutationOutput {
            id: p.id,
            action: TagMutationAction::Append,
            tags,
        }))
    }
    #[tool(
        description = "Tag taxonomy statistics across active and archived traces, including assignment, tier, visibility, and engagement counts."
    )]
    async fn tag_stats(
        &self,
        Parameters(p): Parameters<TagStatsParams>,
    ) -> Result<String, ErrorData> {
        let requested = p.limit.unwrap_or(50);
        let limit = if requested == 0 {
            0
        } else {
            requested.min(1000)
        };
        let report = self.open().await?.tag_stats(limit).map_err(mcp_error)?;
        Ok(json_text(json!({"schema_version":1,"report":report})))
    }
    #[tool(
        description = "Diagnose noncanonical or Obsidian-problematic tags. Read-only by default. Set fix=true to include a deterministic repair plan, then apply=true to execute it; numeric-only and other ambiguous findings are never auto-fixed."
    )]
    async fn tag_doctor(
        &self,
        Parameters(p): Parameters<TagDoctorParams>,
    ) -> Result<String, ErrorData> {
        if p.apply && !p.fix {
            return Err(ErrorData::invalid_params(
                "apply=true requires fix=true",
                None,
            ));
        }
        if p.apply {
            self.ensure_writable()?;
        }
        let cx = self.open().await?;
        let rows = cx.tag_rows().map_err(mcp_error)?;
        let report = crate::tag::doctor(&rows);
        let plan = p
            .fix
            .then(|| crate::tag::doctor_fix_plan(&rows, &cx.name, &report));
        let applied = if p.apply {
            apply_mcp_tag_plan(&cx, plan.as_ref().unwrap()).map_err(mcp_error)?
        } else {
            0
        };
        Ok(json_text(json!({
            "schema_version":1,
            "report":report,
            "plan":plan.as_ref().map(tag_plan_value),
            "applied":p.apply,
            "changed_traces":applied,
        })))
    }
    #[tool(
        description = "Preview or apply a cortex-wide exact tag rename across active and archived traces. ASCII case-insensitive matching is opt-in. apply defaults to false."
    )]
    async fn rename_tag(
        &self,
        Parameters(p): Parameters<RenameTagParams>,
    ) -> Result<String, ErrorData> {
        if p.old_tag.is_empty() || p.new_tag.is_empty() {
            return Err(ErrorData::invalid_params(
                "tag names must not be empty",
                None,
            ));
        }
        if p.old_tag == p.new_tag {
            return Err(ErrorData::invalid_params(
                "source and destination tags are identical",
                None,
            ));
        }
        if p.apply {
            self.ensure_writable()?;
        }
        let cx = self.open().await?;
        let rows = cx.tag_rows().map_err(mcp_error)?;
        let plan = crate::tag::rename_plan(&rows, &cx.name, &p.old_tag, &p.new_tag, p.ignore_case);
        let applied = if p.apply {
            apply_mcp_tag_plan(&cx, &plan).map_err(mcp_error)?
        } else {
            0
        };
        Ok(json_text(json!({
            "schema_version":1,
            "action":"rename",
            "plan":tag_plan_value(&plan),
            "applied":p.apply,
            "changed_traces":applied,
        })))
    }
    #[tool(
        description = "Preview or apply cortex-wide removal of an exact tag from active and archived traces. ASCII case-insensitive matching is opt-in. apply defaults to false."
    )]
    async fn delete_tag(
        &self,
        Parameters(p): Parameters<DeleteTagParams>,
    ) -> Result<String, ErrorData> {
        if p.tag.is_empty() {
            return Err(ErrorData::invalid_params(
                "tag name must not be empty",
                None,
            ));
        }
        if p.apply {
            self.ensure_writable()?;
        }
        let cx = self.open().await?;
        let rows = cx.tag_rows().map_err(mcp_error)?;
        let plan = crate::tag::delete_plan(&rows, &cx.name, &p.tag, p.ignore_case);
        let applied = if p.apply {
            apply_mcp_tag_plan(&cx, &plan).map_err(mcp_error)?
        } else {
            0
        };
        Ok(json_text(json!({
            "schema_version":1,
            "action":"delete",
            "plan":tag_plan_value(&plan),
            "applied":p.apply,
            "changed_traces":applied,
        })))
    }
    #[tool(
        description = "Cast a tier-preference vote on a trace. Use sparingly: only when the user has clearly indicated preference. Votes accumulate across calls and are preferences, not overrides."
    )]
    async fn vote_trace(&self, Parameters(p): Parameters<VoteParam>) -> Result<String, ErrorData> {
        self.ensure_writable()?;
        let delta = match p.direction.as_str() {
            "up" => 1,
            "down" => -1,
            _ => {
                return Err(ErrorData::invalid_params(
                    "direction must be up or down",
                    None,
                ));
            }
        };
        self.open()
            .await?
            .vote(&p.id, delta, "agent")
            .map_err(mcp_error)?;
        Ok(format!("Vote recorded: {} {}.", p.direction, p.id))
    }

    #[tool(
        description = "Internal tool. Returns short-term traces within the rolling consolidation window along with their usage signals (read_count, modify_count, tier_votes, derived_from_count). Consumer scores these and submits distilled mid-tier traces via record_consolidation_result."
    )]
    async fn list_consolidation_candidates(
        &self,
        _: Parameters<CandidateParam>,
    ) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        let window = cx
            .manifest
            .consolidation_config()
            .map_err(mcp_error)?
            .unwrap_or_default()
            .effective_window();
        let candidates = cx
            .promotion_candidates("short", window)
            .map_err(mcp_error)?;
        let candidates = candidates
            .into_iter()
            .map(|candidate| {
                json!({
                    "ID": candidate.id,
                    "Tier": candidate.tier,
                    "Type": candidate.trace_type,
                    "ReadCount": candidate.read_count,
                    "ModifyCount": candidate.modify_count,
                    "SearchHitCount": candidate.search_hit_count,
                    "TierVotes": candidate.tier_votes,
                    "DerivedFromCount": candidate.derived_from_count,
                    "SourceCount": candidate.source_count,
                    "CreatedAt": candidate.created_at,
                })
            })
            .collect::<Vec<_>>();
        Ok(json_text(json!({
            "window_hours": window.as_secs() / 3600,
            "candidates": candidates,
        })))
    }
    #[tool(
        description = "Recent consolidation pipeline health: daily success/fail/promote/distill counts within the lookback window, short→mid and mid→long promotion-latency percentiles, and the 1-source mid leak detector. Lets an agent or operator answer 'is consolidation actually happening, and is anything leaking?' without raw SQL against the events table."
    )]
    async fn consolidation_health(
        &self,
        Parameters(p): Parameters<HealthParam>,
    ) -> Result<String, ErrorData> {
        let since = parse_since(p.since.as_deref().unwrap_or("24h")).map_err(mcp_error)?;
        let cx = self.open().await?;
        Ok(json_text(json!({
            "schema_version": 1,
            "activity": cx.consolidation_activity(since).map_err(mcp_error)?,
            "latency": cx.promotion_latency().map_err(mcp_error)?,
            "one_source_mid": cx.one_source_mid_count().map_err(mcp_error)?,
        })))
    }
    #[tool(
        description = "Top-N traces by federation-wide search popularity (search_hit_count then read_count) plus top-N tags by aggregate engagement. Lets an agent answer 'what's worth reading?' or 'which topics are hot?' without scanning every trace. Active traces only; archived/trashed are excluded."
    )]
    async fn search_activity(
        &self,
        Parameters(p): Parameters<TopParam>,
    ) -> Result<String, ErrorData> {
        let requested = p.top.unwrap_or(10.0) as i64;
        let top = if requested <= 0 {
            10
        } else {
            requested.min(100) as usize
        };
        let cx = self.open().await?;
        Ok(json_text(json!({
            "schema_version": 1,
            "top": top,
            "traces": cx.top_searched_traces(top).map_err(mcp_error)?,
            "tags": cx.tag_activity(top).map_err(mcp_error)?,
        })))
    }
    #[tool(
        description = "Internal tool. Materialises a distilled mid-tier trace from a set of short-term sources. Validates the source IDs exist (>=2 required), creates the new trace with derived_from lineage pointing at the sources, and emits an ActionConsolidate event carrying model/profile/confidence telemetry for the quality dashboard."
    )]
    async fn record_consolidation_result(
        &self,
        Parameters(p): Parameters<ConsolidateParam>,
    ) -> Result<String, ErrorData> {
        self.ensure_writable()?;
        let cx = self.open().await?;
        let id = cx
            .create_distilled_trace(DistilledTraceSpec {
                title: p.title,
                body: p.body,
                tags: p.tags.map(|s| csv(&s)).unwrap_or_default(),
                author: p.author.unwrap_or_default(),
                source_ids: csv(&p.source_ids),
                model_name: p.model_name.clone().unwrap_or_default(),
                model_tier_profile: p.model_tier_profile.clone().unwrap_or_default(),
                cohesion_confidence: p.cohesion_confidence.unwrap_or_default(),
            })
            .map_err(mcp_error)?;
        Ok(format!(
            "Distilled trace created: {} (from {} sources)",
            id,
            csv(&p.source_ids).len()
        ))
    }
    #[tool(
        description = "Append content to an existing trace's body without reading the full trace first. Ideal for fire-and-forget logging, running journals, or any case where an agent needs to add to a trace without consuming its current content."
    )]
    async fn append_trace(
        &self,
        Parameters(p): Parameters<AppendParam>,
    ) -> Result<String, ErrorData> {
        self.ensure_writable()?;
        self.open()
            .await?
            .append(&p.id, &p.content, false)
            .map_err(mcp_error)?;
        Ok(format!("Content appended to trace {}.", p.id))
    }
    #[tool(
        description = "Show the event log (audit trail) for a trace: all mutations in chronological order."
    )]
    async fn trace_history(&self, Parameters(p): Parameters<IdParam>) -> Result<String, ErrorData> {
        let events = self.open().await?.history(&p.id).map_err(mcp_error)?;
        if events.is_empty() {
            return Ok(format!("No events found for trace {}", p.id));
        }
        let mut output = String::new();
        for event in events {
            output.push_str(&format!(
                "{}  {:<10}  {}  origin={}\n",
                event.id, event.action, event.timestamp, event.origin
            ));
        }
        Ok(output)
    }
    #[tool(
        description = "Show the derivation graph for a trace: what it was derived from and what was derived from it."
    )]
    async fn trace_lineage(&self, Parameters(p): Parameters<IdParam>) -> Result<String, ErrorData> {
        let (from, by) = self.open().await?.lineage(&p.id).map_err(mcp_error)?;
        Ok(format!(
            "Trace: {}\nDerived from: {}\nDerived by:   {}\n",
            p.id,
            if from.is_empty() {
                "(none)".into()
            } else {
                from.join(", ")
            },
            if by.is_empty() {
                "(none)".into()
            } else {
                by.join(", ")
            }
        ))
    }
    #[tool(
        description = "Resolve a divergence (concurrent edit conflict). Either accept one of the versions by origin name, or supply a custom merged body."
    )]
    async fn resolve_divergence(
        &self,
        Parameters(p): Parameters<ResolveParam>,
    ) -> Result<String, ErrorData> {
        self.ensure_writable()?;
        let accept = p.accept.unwrap_or_default();
        let body = p.body.unwrap_or_default();
        self.open()
            .await?
            .resolve_divergence(&p.id, &accept, &body)
            .map_err(mcp_error)?;
        if !accept.is_empty() {
            Ok(format!(
                "Divergence {} resolved (accepted {}).",
                p.id, accept
            ))
        } else {
            Ok(format!("Divergence {} resolved (custom merge).", p.id))
        }
    }
    #[tool(
        description = "Returns this cortex's stable identity (ULID, name, manifest version). Federation peers call this on every sync to verify the remote endpoint still belongs to the cortex they originally paired with."
    )]
    async fn cortex_identity(&self, _: Parameters<Empty>) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        let mode = cx
            .manifest
            .federation
            .as_ref()
            .map(|federation| federation.mode.as_str())
            .filter(|mode| !mode.is_empty())
            .unwrap_or("sync");
        let mut payload =
            json!({"id":cx.id,"name":cx.name,"version":cx.manifest.version,"mode":mode});
        if let Some(public_key) = cx
            .manifest
            .signing
            .as_ref()
            .map(|signing| signing.public_key.as_str())
            .filter(|public_key| !public_key.is_empty())
        {
            payload["pubkey"] = json!(public_key);
        }
        let rank = crate::consolidation::get_local_rank(&cx).map_err(mcp_error)?;
        if !rank.cortex_id.is_empty() {
            payload["rank"] = serde_json::to_value(rank).map_err(mcp_error)?;
        }
        Ok(json_text(payload))
    }
    #[tool(
        description = "Returns events from this cortex for federation sync. Remote peers call this to pull new events. Returns a JSON array of event objects."
    )]
    async fn sync_events(
        &self,
        Parameters(p): Parameters<SinceParam>,
    ) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        if cx
            .manifest
            .federation
            .as_ref()
            .is_some_and(|federation| federation.mode == "subscribe")
        {
            return Err(ErrorData::invalid_params(
                "this cortex is in subscribe mode and does not serve events",
                None,
            ));
        }
        let limit = p.limit.unwrap_or(100);
        let limit = if (1..=1000).contains(&limit) {
            limit
        } else {
            100
        };
        let since = p.since.as_deref().unwrap_or("");
        if !since.is_empty() && ulid::Ulid::from_string(since).is_err() {
            return Err(ErrorData::invalid_params(
                "since must be a valid ULID cursor (26-char Crockford base32)",
                None,
            ));
        }
        serde_json::to_string(&cx.events_since(since, limit).map_err(mcp_error)?).map_err(mcp_error)
    }
    #[tool(
        description = "Returns per-peer tier-usage deltas (read_count, modify_count, search_hit_count, last_read_at) for federation sync. Each peer publishes only its own rows — the ring aggregates by SUMing over every peer's contribution, so consolidation decisions operate on a federation-wide signal rather than the local slice. Returns a JSON array of trace_usage rows owned by this cortex with updated_at > since. search_hit_count is omitted when zero for wire compatibility with pre-migration-015 peers."
    )]
    async fn sync_read_signal(
        &self,
        Parameters(p): Parameters<SinceParam>,
    ) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        if cx
            .manifest
            .federation
            .as_ref()
            .is_some_and(|federation| federation.mode == "subscribe")
        {
            return Err(ErrorData::invalid_params(
                "this cortex is in subscribe mode and does not serve read signal",
                None,
            ));
        }
        let limit = p.limit.unwrap_or(100);
        let limit = if (1..=1000).contains(&limit) {
            limit
        } else {
            100
        };
        serde_json::to_string(
            &cx.local_usage_since(p.since.as_deref().unwrap_or(""), limit)
                .map_err(mcp_error)?,
        )
        .map_err(mcp_error)
    }
    #[tool(description = "Show federation configuration, peer sync state, and local vector clock.")]
    async fn federation_status(&self, _: Parameters<Empty>) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        render_federation_status(&cx).map_err(mcp_error)
    }
    #[tool(
        description = "Accept a peer announcement from a remote cortex. Returns this cortex's identity for mutual discovery."
    )]
    async fn announce_peer(
        &self,
        Parameters(p): Parameters<AnnounceParam>,
    ) -> Result<String, ErrorData> {
        let endpoint = reqwest::Url::parse(&p.endpoint).map_err(|_| {
            ErrorData::invalid_params("endpoint must be a valid http:// or https:// URL", None)
        })?;
        if !matches!(endpoint.scheme(), "http" | "https") || endpoint.host().is_none() {
            return Err(ErrorData::invalid_params(
                "endpoint must be a valid http:// or https:// URL",
                None,
            ));
        }
        let cx = self.open().await?;
        if p.name == cx.manifest.name {
            return Err(ErrorData::invalid_params(
                format!(
                    "refusing announcement: name {:?} matches this cortex's own name. The announcing peer must rename its cortex (in its cortex.md) to a unique value before federation can proceed.",
                    p.name
                ),
                None,
            ));
        }
        let known = cx
            .manifest
            .federation
            .as_ref()
            .is_some_and(|federation| federation.peers.iter().any(|peer| peer.name == p.name));
        let detail = if known {
            format!("Peer {:?} is already configured.", p.name)
        } else {
            format!(
                "Peer {:?} ({}) is not yet configured. Add it to cortex.md to enable sync.",
                p.name, p.endpoint
            )
        };
        Ok(format!(
            "Acknowledged. This cortex is {:?}.\n{}\n",
            cx.manifest.name, detail
        ))
    }
}

pub async fn serve_stdio(
    name: String,
    path: PathBuf,
    watcher_settings: Option<crate::watch::Settings>,
) -> Result<()> {
    let server = NoemaServer::new(&name, &path, false)?;
    let (cortex_id, federation, manifest) = {
        let cortex = server.cortex.lock().await;
        (
            cortex.id.clone(),
            cortex.manifest.federation.clone().unwrap_or_default(),
            cortex.manifest.clone(),
        )
    };
    let background_lock = CortexLock::try_acquire_background(&cortex_id)?;
    let cancellation = CancellationToken::new();
    let registry = Arc::new(crate::consolidation::InFlightRegistry::default());
    let (scheduler, eligibility, cadence, watchdog, watcher, embedder) =
        if background_lock.is_some() {
            (
                Some(crate::federation::FederationScheduler::start(
                    name.clone(),
                    path.clone(),
                    federation,
                    cancellation.clone(),
                )?),
                crate::consolidation::EligibilityScheduler::start(
                    name.clone(),
                    path.clone(),
                    cancellation.clone(),
                )?,
                crate::consolidation::CadenceScheduler::start(
                    name.clone(),
                    path.clone(),
                    cancellation.clone(),
                    Arc::clone(&registry),
                )?,
                crate::consolidation::WatchdogScheduler::start(
                    name.clone(),
                    path.clone(),
                    cancellation.clone(),
                    registry,
                )?,
                watcher_settings
                    .map(|settings| {
                        crate::watch::WatchScheduler::start(
                            name.clone(),
                            path.clone(),
                            settings,
                            cancellation.clone(),
                        )
                    })
                    .transpose()?,
                crate::embedding::Maintainer::start(
                    name.clone(),
                    path.clone(),
                    &manifest,
                    cancellation.clone(),
                ),
            )
        } else {
            eprintln!("another process owns cortex background work; serving MCP only");
            (None, None, None, None, None, None)
        };
    let result = server
        .serve(rmcp::transport::stdio())
        .await?
        .waiting()
        .await;
    cancellation.cancel();
    if let Some(scheduler) = scheduler {
        scheduler.stop().await;
    }
    if let Some(eligibility) = eligibility {
        eligibility.stop().await;
    }
    if let Some(cadence) = cadence {
        cadence.stop().await;
    }
    if let Some(watchdog) = watchdog {
        watchdog.stop().await;
    }
    if let Some(watcher) = watcher {
        watcher.stop();
    }
    if let Some(embedder) = embedder {
        embedder.stop().await;
    }
    result?;
    Ok(())
}

pub struct HttpListenConfig {
    pub hosts: Vec<String>,
    pub dynamic_hosts: Vec<String>,
    pub allowed_hosts: Vec<String>,
    pub port: u16,
}

fn allowed_http_hosts(
    hosts: &[String],
    dynamic_hosts: &[String],
    allowed_hosts: &[String],
) -> Vec<String> {
    hosts
        .iter()
        .chain(dynamic_hosts)
        .chain(allowed_hosts)
        .cloned()
        .chain([
            "localhost".to_owned(),
            "127.0.0.1".to_owned(),
            "::1".to_owned(),
        ])
        .collect()
}

struct DynamicListener {
    address: std::net::SocketAddr,
    handle: axum_server::Handle<std::net::SocketAddr>,
    task: tokio::task::JoinHandle<Result<()>>,
}

#[cfg(unix)]
fn local_interface_addresses() -> Result<HashSet<std::net::IpAddr>> {
    let mut interfaces = std::ptr::null_mut();
    if unsafe { libc::getifaddrs(&mut interfaces) } != 0 {
        return Err(std::io::Error::last_os_error()).context("enumerating local interfaces");
    }
    let mut output = HashSet::new();
    let mut current = interfaces;
    while !current.is_null() {
        let address = unsafe { (*current).ifa_addr };
        if !address.is_null() {
            let family = unsafe { (*address).sa_family as i32 };
            if family == libc::AF_INET {
                let address = unsafe { &*(address.cast::<libc::sockaddr_in>()) };
                output.insert(std::net::IpAddr::V4(std::net::Ipv4Addr::from(
                    address.sin_addr.s_addr.to_ne_bytes(),
                )));
            } else if family == libc::AF_INET6 {
                let address = unsafe { &*(address.cast::<libc::sockaddr_in6>()) };
                output.insert(std::net::IpAddr::V6(std::net::Ipv6Addr::from(
                    address.sin6_addr.s6_addr,
                )));
            }
        }
        current = unsafe { (*current).ifa_next };
    }
    unsafe { libc::freeifaddrs(interfaces) };
    Ok(output)
}

#[cfg(not(unix))]
fn local_interface_addresses() -> Result<HashSet<std::net::IpAddr>> {
    bail!("dynamic interface discovery is not implemented on this platform")
}

pub async fn serve_http(
    name: String,
    path: PathBuf,
    listen: HttpListenConfig,
    access_key: AccessKey,
    tls: Option<(PathBuf, PathBuf)>,
    watcher_settings: Option<crate::watch::Settings>,
) -> Result<()> {
    let HttpListenConfig {
        hosts,
        dynamic_hosts,
        allowed_hosts,
        port,
    } = listen;
    let server = NoemaServer::new(&name, &path, true)?;
    let (cortex_id, federation, manifest) = {
        let cortex = server.cortex.lock().await;
        (
            cortex.id.clone(),
            cortex.manifest.federation.clone().unwrap_or_default(),
            cortex.manifest.clone(),
        )
    };
    let background_lock = CortexLock::try_acquire_background(&cortex_id)?;
    let service: StreamableHttpService<NoemaServer, LocalSessionManager> =
        StreamableHttpService::new(
            move || Ok(server.clone()),
            Default::default(),
            StreamableHttpServerConfig::default().with_allowed_hosts(allowed_http_hosts(
                &hosts,
                &dynamic_hosts,
                &allowed_hosts,
            )),
        );
    let certificate_path = tls.as_ref().map(|(certificate, _)| certificate.clone());
    let tls_config = match tls {
        Some((certificate, private_key)) => Some(
            axum_server::tls_rustls::RustlsConfig::from_pem_file(certificate, private_key).await?,
        ),
        None => None,
    };
    let mut listeners = Vec::with_capacity(hosts.len());
    for host in &hosts {
        let listener = std::net::TcpListener::bind((host.as_str(), port))?;
        listener.set_nonblocking(true)?;
        listeners.push(listener);
    }
    let cancellation = CancellationToken::new();
    let registry = Arc::new(crate::consolidation::InFlightRegistry::default());
    let (scheduler, eligibility, cadence, watchdog, watcher, embedder) =
        if background_lock.is_some() {
            (
                Some(crate::federation::FederationScheduler::start(
                    name.clone(),
                    path.clone(),
                    federation,
                    cancellation.clone(),
                )?),
                crate::consolidation::EligibilityScheduler::start(
                    name.clone(),
                    path.clone(),
                    cancellation.clone(),
                )?,
                crate::consolidation::CadenceScheduler::start(
                    name.clone(),
                    path.clone(),
                    cancellation.clone(),
                    Arc::clone(&registry),
                )?,
                crate::consolidation::WatchdogScheduler::start(
                    name.clone(),
                    path.clone(),
                    cancellation.clone(),
                    registry,
                )?,
                watcher_settings
                    .map(|settings| {
                        crate::watch::WatchScheduler::start(
                            name.clone(),
                            path.clone(),
                            settings,
                            cancellation.clone(),
                        )
                    })
                    .transpose()?,
                crate::embedding::Maintainer::start(
                    name.clone(),
                    path.clone(),
                    &manifest,
                    cancellation.clone(),
                ),
            )
        } else {
            eprintln!("another process owns cortex background work; serving MCP only");
            (None, None, None, None, None, None)
        };
    let cert_monitor = if background_lock.is_some() {
        certificate_path
            .as_deref()
            .map(crate::tlsutil::CertMonitor::start)
    } else {
        None
    };
    let signal_cancellation = cancellation.clone();
    let signal_task = tokio::spawn(async move {
        shutdown_signal().await;
        signal_cancellation.cancel();
    });
    let scheme = if tls_config.is_some() {
        "https"
    } else {
        "http"
    };
    for listener in &listeners {
        eprintln!(
            "Noema MCP listening on {scheme}://{}/mcp",
            listener.local_addr()?
        );
    }
    let router = apply_http_middleware(
        axum::Router::new().nest_service("/mcp", service),
        &access_key,
    );
    let server_handles = listeners
        .iter()
        .map(|_| axum_server::Handle::new())
        .collect::<Vec<_>>();
    let shutdown_handles = server_handles.clone();
    let server_cancellation = cancellation.clone();
    let shutdown_task = tokio::spawn(async move {
        server_cancellation.cancelled().await;
        for handle in shutdown_handles {
            handle.graceful_shutdown(Some(std::time::Duration::from_secs(5)));
        }
    });
    let mut servers = tokio::task::JoinSet::new();
    for (listener, handle) in listeners.into_iter().zip(server_handles) {
        let router = router.clone();
        let tls_config = tls_config.clone();
        servers.spawn(async move {
            if let Some(tls_config) = tls_config {
                axum_server::from_tcp_rustls(listener, tls_config)?
                    .handle(handle)
                    .serve(router.into_make_service())
                    .await?;
            } else {
                axum_server::from_tcp(listener)?
                    .handle(handle)
                    .serve(router.into_make_service())
                    .await?;
            }
            anyhow::Ok(())
        });
    }
    let dynamic_cancellation = cancellation.clone();
    let dynamic_router = router.clone();
    let dynamic_tls = tls_config.clone();
    let mut dynamic_server = (!dynamic_hosts.is_empty()).then(|| {
        tokio::spawn(async move {
            let mut active: HashMap<String, DynamicListener> = HashMap::new();
            let mut interval = tokio::time::interval(std::time::Duration::from_secs(5));
            loop {
                tokio::select! {
                    _ = dynamic_cancellation.cancelled() => break,
                    _ = interval.tick() => {}
                }
                let local_addresses = local_interface_addresses()?;
                for host in &dynamic_hosts {
                    let resolved = match tokio::net::lookup_host((host.as_str(), port)).await {
                        Ok(addresses) => addresses
                            .filter(|address| local_addresses.contains(&address.ip()))
                            .collect::<Vec<_>>(),
                        Err(error) => {
                            eprintln!(
                                "dynamic address {host} is unavailable ({error}); will retry"
                            );
                            Vec::new()
                        }
                    };
                    let keep = active.get(host).is_some_and(|listener| {
                        resolved.contains(&listener.address) && !listener.task.is_finished()
                    });
                    if keep {
                        continue;
                    }
                    if let Some(listener) = active.remove(host) {
                        listener
                            .handle
                            .graceful_shutdown(Some(std::time::Duration::from_secs(5)));
                        listener.task.abort();
                        eprintln!(
                            "dynamic address {host} disappeared or changed; stopping listener"
                        );
                    }
                    let mut listener = None;
                    for address in resolved {
                        match std::net::TcpListener::bind(address) {
                            Ok(candidate) => {
                                candidate.set_nonblocking(true)?;
                                listener = Some((address, candidate));
                                break;
                            }
                            Err(error)
                                if matches!(error.kind(), std::io::ErrorKind::AddrNotAvailable) => {
                            }
                            Err(error) => {
                                return Err(error).with_context(|| {
                                    format!("binding dynamic address {host} ({address})")
                                });
                            }
                        }
                    }
                    let Some((address, listener)) = listener else {
                        continue;
                    };
                    let handle = axum_server::Handle::new();
                    let task_handle = handle.clone();
                    let router = dynamic_router.clone();
                    let tls = dynamic_tls.clone();
                    let task = tokio::spawn(async move {
                        if let Some(tls) = tls {
                            axum_server::from_tcp_rustls(listener, tls)?
                                .handle(task_handle)
                                .serve(router.into_make_service())
                                .await?;
                        } else {
                            axum_server::from_tcp(listener)?
                                .handle(task_handle)
                                .serve(router.into_make_service())
                                .await?;
                        }
                        Ok(())
                    });
                    eprintln!("Noema MCP listening on {scheme}://{address}/mcp (dynamic {host})");
                    active.insert(
                        host.clone(),
                        DynamicListener {
                            address,
                            handle,
                            task,
                        },
                    );
                }
                for (host, listener) in &active {
                    if listener.task.is_finished() {
                        bail!("dynamic listener for {host} terminated unexpectedly");
                    }
                }
            }
            for (_, listener) in active {
                listener
                    .handle
                    .graceful_shutdown(Some(std::time::Duration::from_secs(5)));
                listener.task.abort();
            }
            Ok(())
        })
    });
    if let Some(dynamic_server) = dynamic_server.as_mut() {
        tokio::select! {
            result = servers.join_next() => {
                result.context("HTTP listener set terminated unexpectedly")???
            }
            result = dynamic_server => {
                result.context("dynamic-listener supervisor failed")??
            }
        }
    } else {
        servers
            .join_next()
            .await
            .context("HTTP listener set terminated unexpectedly")???;
    }
    cancellation.cancel();
    if let Some(scheduler) = scheduler {
        scheduler.stop().await;
    }
    if let Some(eligibility) = eligibility {
        eligibility.stop().await;
    }
    if let Some(cadence) = cadence {
        cadence.stop().await;
    }
    if let Some(watchdog) = watchdog {
        watchdog.stop().await;
    }
    if let Some(watcher) = watcher {
        watcher.stop();
    }
    if let Some(embedder) = embedder {
        embedder.stop().await;
    }
    if let Some(cert_monitor) = cert_monitor {
        cert_monitor.stop().await;
    }
    signal_task.abort();
    shutdown_task.abort();
    servers.abort_all();
    Ok(())
}

fn apply_http_middleware(mut router: axum::Router, access_key: &AccessKey) -> axum::Router {
    if access_key.keyed() {
        let expected = BearerDigest::new(&access_key.value);
        router = router.layer(axum::middleware::from_fn_with_state(expected, bearer_auth));
    }
    router.layer(axum::middleware::from_fn(cors))
}

async fn cors(request: Request<Body>, next: Next) -> Response {
    let origin = request.headers().get(header::ORIGIN).cloned();
    let allowed_origin = origin.as_ref().filter(|value| {
        value
            .to_str()
            .is_ok_and(|value| matches!(value, "app://obsidian.md" | "capacitor://localhost"))
    });
    if origin.is_some() && allowed_origin.is_none() {
        return StatusCode::FORBIDDEN.into_response();
    }
    let response = if request.method() == Method::OPTIONS {
        StatusCode::NO_CONTENT.into_response()
    } else {
        next.run(request).await
    };
    with_cors_headers(response, allowed_origin)
}

fn with_cors_headers(mut response: Response, origin: Option<&HeaderValue>) -> Response {
    let headers = response.headers_mut();
    if let Some(origin) = origin {
        headers.insert(header::ACCESS_CONTROL_ALLOW_ORIGIN, origin.clone());
        headers.insert(header::VARY, HeaderValue::from_static("Origin"));
    }
    headers.insert(
        header::ACCESS_CONTROL_ALLOW_METHODS,
        HeaderValue::from_static("POST, GET, DELETE, OPTIONS"),
    );
    headers.insert(
        header::ACCESS_CONTROL_ALLOW_HEADERS,
        HeaderValue::from_static("Authorization, Content-Type, Mcp-Session-Id"),
    );
    headers.insert(
        header::ACCESS_CONTROL_EXPOSE_HEADERS,
        HeaderValue::from_static("Mcp-Session-Id"),
    );
    response
}

#[derive(Clone)]
struct BearerDigest([u8; 32]);

impl BearerDigest {
    fn new(key: &str) -> Self {
        Self(Sha256::digest(format!("Bearer {key}")).into())
    }

    fn matches(&self, authorization: &[u8]) -> bool {
        let received: [u8; 32] = Sha256::digest(authorization).into();
        self.0
            .iter()
            .zip(received)
            .fold(0_u8, |difference, (expected, received)| {
                difference | (expected ^ received)
            })
            == 0
    }
}

async fn bearer_auth(
    State(expected): State<BearerDigest>,
    request: Request<Body>,
    next: Next,
) -> Response {
    let authorized = request
        .headers()
        .get(header::AUTHORIZATION)
        .is_some_and(|value| expected.matches(value.as_bytes()));
    if authorized {
        next.run(request).await
    } else {
        (
            StatusCode::UNAUTHORIZED,
            [(header::CONTENT_TYPE, "application/json")],
            "{\"error\":\"unauthorized: NOEMA_MCP_KEY / access.shared_key_file required\"}",
        )
            .into_response()
    }
}

#[cfg(unix)]
async fn shutdown_signal() {
    let mut terminate = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        .expect("install SIGTERM handler");
    tokio::select! {
        _ = tokio::signal::ctrl_c() => {},
        _ = terminate.recv() => {},
    }
}

#[cfg(not(unix))]
async fn shutdown_signal() {
    let _ = tokio::signal::ctrl_c().await;
}

fn render_instructions(manifest: &crate::cortex::Manifest) -> String {
    let purpose = if manifest.purpose.is_empty() {
        String::new()
    } else {
        format!("Purpose:  {}\n", manifest.purpose)
    };
    let owner = if manifest.owner.is_empty() {
        String::new()
    } else {
        format!("Owner:    {}\n", manifest.owner)
    };
    format!(
        r#"# Noema Agent Instructions

## Active Cortex
Name:     {}
Version:  noema {} (manifest v{})
{}{}
## Agent Startup
Before establishing user or project defaults, fetch durable preferences from
this Cortex:

1. list_traces with tag="user-preference".
2. get_trace for each relevant result with record_usage=false; the body is the binding content.

Do not broad-scan type="preference" during normal startup. Use type="preference"
only for task-scoped discovery when the current task needs preference search
beyond active startup rules.

If preference retrieval fails because of transport, auth, or schema issues,
surface that failure explicitly and proceed with ordinary defaults.

## MCP Usage
Use MCP tool discovery and each tool's input schema as the source of truth for
what this client can call right now. Some Noema deployments expose read-only,
federated, or client-filtered tool sets.

Call cortex_usage when you need structured JSON context for MCP clients:
runtime posture, trace semantics, startup sequence, search configuration, and
operational constraints.

## Memory Semantics
A Cortex is a named collection of Traces; this instance is {:?}. A Trace is one
memory unit with a markdown body, YAML frontmatter, and SQLite index row.

Choose the most specific trace type:

- fact: discrete thing that is true.
- decision: choice made and why.
- preference: behavioral or stylistic lean.
- context: situational background.
- skill: learned capability or procedure.
- intent: something that needs to happen.
- observation: witnessed but not yet verified.
- note: fallback for anything else.
- divergence: concurrent edit conflict, created by federation.

## Creating Traces
When create_trace is exposed, pass title, type, and body plus optional author,
tags, derived_from, origin, source_locked, and source_hash per the tool schema.

Aim for titles under 80 characters. Do NOT include a date in the title — the ID
generator prepends today's date automatically, and leading YYYYMMDD- or
YYYY-MM-DD- prefixes in the title are stripped to prevent doubled IDs like
20260402-20260402-foo. Avoid mid-title dates too, such as "session 20260416
142000"; only leading prefixes are stripped, so mid-title date fragments survive.
If a trace is about a specific date, put it in a tag such as event-2026-04-02 or
in the body.

Search before creating a durable trace when duplication would matter. Use
derived_from when synthesizing conclusions from existing traces.

append_trace is useful for running logs and fire-and-forget writes because it
appends content without reading the full trace first.

Use set_trace_tags or append_trace_tags for per-trace retrieval metadata
hygiene. Use tag_stats and tag_doctor to inspect the cortex-wide taxonomy.
rename_tag and delete_tag return a read-only plan unless apply=true. Do not use
vote_trace to compensate for missing or excessive tags; voting is only a
tier-preference signal.

## Guardrails
- Prefer specific types over note.
- Use tags for cross-cutting retrieval.
- Set author to the human or agent responsible for the memory.
- Keep public-facing content free of private hostnames, personal identifiers,
  cortex names, and secret-bearing output unless explicitly approved.
"#,
        manifest.name, VERSION, manifest.version, purpose, owner, manifest.name
    )
}

fn value_object(value: serde_json::Value) -> BTreeMap<String, serde_json::Value> {
    match value {
        serde_json::Value::Object(map) => map.into_iter().collect(),
        _ => BTreeMap::new(),
    }
}

fn build_cortex_usage(cortex: &Cortex) -> Result<CortexUsageOutput> {
    let manifest = &cortex.manifest;
    let federation = manifest.federation.as_ref();
    let federation_mode = federation
        .map(|config| config.mode.as_str())
        .filter(|mode| !mode.is_empty())
        .unwrap_or("sync");
    let federation_verify = federation
        .map(|config| config.verify.as_str())
        .filter(|verify| !verify.is_empty())
        .unwrap_or("off");
    let access_key = crate::cortex::load_access_key(&cortex.dir, manifest.access.as_ref());
    let access = match access_key {
        Ok(key) if key.keyed() => json!({
            "mode":"keyed",
            "source":key.source,
            "fingerprint":key.fingerprint,
            "tls_required_when_keyed":true
        }),
        Ok(_) => json!({"mode":"open","tls_required_when_keyed":true}),
        Err(error) => json!({
            "mode":"error",
            "error":error.to_string(),
            "tls_required_when_keyed":true
        }),
    };
    let watch_enabled = manifest
        .watch
        .as_ref()
        .and_then(|watch| watch.enabled)
        .unwrap_or(true);
    let search = manifest.search.as_ref();
    let default_mode = search
        .map(|config| config.effective_default_mode())
        .unwrap_or("lexical");
    let hybrid_weight = search
        .map(|config| config.effective_hybrid_weight())
        .unwrap_or(0.5);
    let max_chars = search
        .map(|config| config.effective_max_chars())
        .unwrap_or(32_000);
    let trace_types = crate::trace::VALID_TYPES
        .iter()
        .map(|name| {
            json!({
                "name":name,
                "description":trace_type_description(name)
            })
        })
        .collect::<Vec<_>>();

    Ok(CortexUsageOutput {
        schema_version: 1,
        cortex: value_object(json!({
            "id":manifest.id,
            "name":manifest.name,
            "purpose":manifest.purpose,
            "owner":manifest.owner,
            "created":manifest.created,
            "manifest_version":manifest.version,
            "noema_version":VERSION
        })),
        contract: CortexContractOutput {
            tool_discovery_authoritative: true,
            markdown_instructions_tool: "get_instructions".into(),
            structured_usage_tool: "cortex_usage".into(),
            callable_tools_policy: "Use MCP tool discovery and each tool schema as the source of truth for callable operations in this client session.".into(),
        },
        startup: value_object(json!({
            "preference_sequence":[
                {"tool":"list_traces","arguments":{"tag":"user-preference"}},
                {"tool":"get_trace","for_each_result":true,"arguments":{"record_usage":false},"body_policy":"binding durable preference content"}
            ],
            "preference_discovery":"Do not broad-scan type=preference during normal startup. Use type=preference only for task-scoped discovery when the current task needs preference search beyond active startup rules.",
            "failure_policy":"If preference retrieval fails because of transport, auth, or schema issues, surface the failure explicitly and proceed with ordinary defaults."
        })),
        trace_model: value_object(json!({
            "types":trace_types,
            "id":{"format":"YYYYMMDD-slugified-title","slug_max_len":crate::trace::MAX_SLUG_LEN,"generated_by":"create_trace"},
            "title_rules":[
                "Aim for titles under 80 characters.",
                "Do not include a date in the title; create_trace prepends today's date automatically.",
                "Leading YYYYMMDD- and YYYY-MM-DD- prefixes are stripped, but mid-title date fragments survive.",
                "If a trace is about a specific date, put it in a tag such as event-2026-04-02 or in the body."
            ],
            "required_fields":["id","title","type","created","updated"],
            "optional_fields":["author","tags","derived_from","origin","source_hash","source_locked"],
            "generated_fields":["content_hash"],
            "tier_glyphs":{"s":"short","m":"mid","L":"long"}
        })),
        search: value_object(json!({
            "modes":["lexical","semantic","hybrid"],
            "default_mode":default_mode,
            "semantic_enabled":search.is_some_and(|config| config.semantic_enabled),
            "embedding_endpoint_configured":!manifest.resolved_embedding_endpoint()?.is_empty(),
            "embedding_model_configured":search.is_some_and(|config| !config.embedding_model.is_empty()),
            "hybrid_weight":hybrid_weight,
            "max_chars":max_chars
        })),
        workflows: value_object(json!({
            "read":[
                "list_traces lists active traces by default; archived=true shows archived only; all=true shows active and archived.",
                "get_trace returns full body and metadata.",
                "search_traces searches by text and may support semantic or hybrid ranking when configured.",
                "find_similar_traces starts from an existing trace ID."
            ],
            "write":[
                "create_trace creates a new trace when exposed.",
                "update_trace changes selected fields when exposed.",
                "append_trace appends content without reading the full trace first.",
                "set_trace_tags replaces retrieval tags without touching title, body, type, or lineage.",
                "append_trace_tags adds retrieval tags idempotently without touching title, body, type, or lineage.",
                "rename_tag and delete_tag preview cortex-wide changes by default and require apply=true to mutate.",
                "archive_trace hides without deleting; delete_trace moves to trash; recover_trace restores from trash."
            ],
            "audit_federation":[
                "trace_history shows immutable mutation history.",
                "trace_lineage shows derived_from and derived_by relationships.",
                "resolve_divergence resolves federation conflicts by accepting a peer version or supplying a merge."
            ]
        })),
        runtime: value_object(json!({
            "federation_mode":federation_mode,
            "federation_verify":federation_verify,
            "access":access,
            "durability_profile":cortex.durability_profile(),
            "filesystem_watch_enabled":watch_enabled,
            "long_tier_content_mutable":false,
            "long_tier_tags_mutable":true,
            "trash_visible_through_mcp":false,
            "source_locking_description":"Source-locked foreign traces refuse update, delete, and remove outside their origin; archive and unarchive remain local visibility choices."
        })),
        authoring_tips: vec![
            "Prefer specific types over note.".into(),
            "Use tags for cross-cutting retrieval.".into(),
            "Use tag_stats and tag_doctor before cortex-wide tag cleanup; bulk tag tools preview unless apply=true.".into(),
            "Use set_trace_tags or append_trace_tags for per-trace tag cleanup; vote_trace is only a tier-preference signal.".into(),
            "Set author to the human or agent responsible for the memory.".into(),
            "Keep public-facing content free of private hostnames, personal identifiers, cortex names, and secret-bearing output unless explicitly approved.".into(),
        ],
    })
}

fn trace_type_description(trace_type: &str) -> &'static str {
    match trace_type {
        "fact" => "discrete thing that is true",
        "decision" => "choice made and why",
        "preference" => "behavioral or stylistic lean",
        "context" => "situational background",
        "skill" => "learned capability or procedure",
        "intent" => "something that needs to happen",
        "observation" => "witnessed but not yet verified",
        "divergence" => "concurrent edit conflict, created by federation",
        _ => "fallback for anything else",
    }
}

fn resolve_search_mode(
    cortex: &Cortex,
    requested: &str,
) -> Result<(String, Option<HttpEmbedder>, String, f64)> {
    let Some(search) = cortex.manifest.search.as_ref() else {
        return Ok((
            if requested.is_empty() {
                "lexical".into()
            } else {
                requested.into()
            },
            None,
            String::new(),
            0.5,
        ));
    };
    let mode = if requested.is_empty() {
        search.effective_default_mode().to_owned()
    } else {
        requested.to_owned()
    };
    let weight = search.effective_hybrid_weight();
    if mode == "lexical" || search.embedding_model.is_empty() {
        return Ok((mode, None, String::new(), weight));
    }
    let endpoint = cortex.manifest.resolved_embedding_endpoint()?;
    if endpoint.is_empty() {
        return Ok((mode, None, String::new(), weight));
    }
    let Ok(embedder) = HttpEmbedder::new(
        &endpoint,
        &cortex.manifest.resolved_embedding_api_key_env()?,
    ) else {
        return Ok((mode, None, String::new(), weight));
    };
    Ok((mode, Some(embedder), search.embedding_model.clone(), weight))
}

fn tag_plan_value(plan: &crate::tag::TagMutationPlan) -> serde_json::Value {
    json!({
        "matched_spellings":&plan.matched_spellings,
        "matched_assignments":plan.matched_assignments(),
        "affected_traces":plan.changes.len(),
        "changes":&plan.changes,
        "blocked_source_locked":&plan.blocked_source_locked,
    })
}

fn apply_mcp_tag_plan(cx: &Cortex, plan: &crate::tag::TagMutationPlan) -> Result<usize> {
    if !plan.blocked_source_locked.is_empty() {
        bail!(
            "{} source-locked trace(s) block this operation: {}",
            plan.blocked_source_locked.len(),
            plan.blocked_source_locked.join(", ")
        );
    }
    let mut changed = 0;
    for change in &plan.changes {
        cx.set_tags(&change.id, change.after.clone(), true)
            .with_context(|| {
                format!(
                    "bulk tag mutation stopped after {changed} successful trace mutation(s); rerun is safe"
                )
            })?;
        changed += 1;
    }
    Ok(changed)
}

fn csv(value: &str) -> Vec<String> {
    value
        .split([',', ';'])
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(str::to_owned)
        .collect()
}
fn json_text<T: Serialize>(value: T) -> String {
    serde_json::to_string_pretty(&value).unwrap_or_else(|_| "{}".into())
}

fn format_rows(rows: &[crate::cortex::Row]) -> String {
    if rows.is_empty() {
        return "No traces found.".into();
    }
    let mut output = String::new();
    for row in rows {
        let tier = match row.tier.as_str() {
            "short" => "s",
            "mid" => "m",
            "long" => "L",
            _ => "?",
        };
        let trace_type = if row.trace_type == "divergence" {
            "DIVERGENCE"
        } else {
            &row.trace_type
        };
        let created = row.created_at.get(..10).unwrap_or(&row.created_at);
        let _ = write!(output, "[{tier}] [{trace_type}] {} ({created})", row.id);
        if !row.author.is_empty() {
            let _ = write!(output, " — {}", row.author);
        }
        if !row.tags.is_empty() {
            let _ = write!(output, " [{}]", row.tags.join(", "));
        }
        let _ = writeln!(output, "\n  {}", row.title);
    }
    output
}

pub(crate) fn render_federation_status(cx: &Cortex) -> Result<String> {
    let manifest = &cx.manifest;
    let config = manifest.federation.as_ref();
    let mode = config
        .map(|federation| federation.mode.as_str())
        .filter(|mode| !mode.is_empty())
        .unwrap_or("sync");
    let mut output = format!("Cortex: {}\nMode: {}\n", manifest.name, mode);
    match load_access_key(&cx.dir, manifest.access.as_ref()) {
        Ok(key) if key.keyed() => output.push_str(&format!(
            "Access: keyed (source={}, fingerprint={})\n",
            key.source, key.fingerprint
        )),
        Ok(_) => output.push_str("Access: open\n"),
        Err(error) => output.push_str(&format!("Access: error loading key: {error}\n")),
    }
    output.push('\n');

    let peers = config
        .map(|federation| federation.peers.as_slice())
        .unwrap_or(&[]);
    if peers.is_empty() {
        output.push_str("Federation: not configured (no peers in cortex.md)\n");
    } else {
        output.push_str(&format!("Peers: {}\n", peers.len()));
        if let Some(interval) = config
            .map(|federation| federation.interval.as_str())
            .filter(|interval| !interval.is_empty())
        {
            output.push_str(&format!("Interval: {interval}\n"));
        }
        output.push_str(&format!(
            "Consolidation Rank: {}\n\n",
            format_rank(&crate::consolidation::get_local_rank(cx)?)
        ));
        for peer in peers {
            let cortex_id = display_state(
                &cx.federation_state(&format!("peer:{}:cortex_id", peer.name))?,
                "(unverified)",
            );
            let last_seen = display_state(
                &cx.federation_state(&format!("peer:{}:last_seen", peer.name))?,
                "(never)",
            );
            let last_event = display_state(
                &cx.federation_state(&format!("peer:{}:last_event", peer.name))?,
                "(none)",
            );
            let rank = crate::consolidation::get_peer_rank(cx, &peer.name)?;
            let peer_rank = if rank.observed_at.is_empty() {
                "(none)".into()
            } else {
                format_rank(&rank)
            };
            output.push_str(&format!(
                "  {}\n    endpoint:   {}\n    mode:       {}\n    cortex_id:  {}\n    rank:       {}\n    last_seen:  {}\n    last_event: {}\n",
                peer.name,
                peer.endpoint,
                if peer.mode.is_empty() {
                    "sync"
                } else {
                    peer.mode.as_str()
                },
                cortex_id,
                peer_rank,
                last_seen,
                last_event
            ));
        }
    }

    let clock = cx.get_clock()?;
    if !clock.is_empty() {
        output.push_str("\nVector Clock:\n");
        let mut peer_names = BTreeMap::new();
        peer_names.insert(cx.id.clone(), format!("{} (local)", manifest.name));
        for peer in peers {
            let id = cx.federation_state(&format!("peer:{}:cortex_id", peer.name))?;
            if !id.is_empty() {
                peer_names.insert(id, peer.name.clone());
            }
        }
        for (cortex_id, tick) in clock {
            let label = peer_names
                .get(&cortex_id)
                .map(String::as_str)
                .unwrap_or("(unknown peer)");
            output.push_str(&format!("  {cortex_id} [{label}]: {tick}\n"));
        }
    }

    let divergence_count = cx
        .list(&ListOptions {
            trace_type: "divergence".into(),
            ..Default::default()
        })?
        .len();
    if divergence_count > 0 {
        output.push_str(&format!(
            "\nUnresolved Divergences: {divergence_count}\n  Use resolve_divergence or `noema resolve` to resolve them.\n"
        ));
    }
    Ok(output)
}

fn display_state(value: &str, empty: &str) -> String {
    if value.is_empty() {
        empty.into()
    } else {
        value.into()
    }
}

fn format_rank(rank: &crate::consolidation::RankEntry) -> String {
    if rank.rank == 0 || rank.observed_at.is_empty() {
        "(ineligible)".into()
    } else {
        format!("{} (observed {})", rank.rank, rank.observed_at)
    }
}

fn mcp_error(error: impl std::fmt::Display) -> ErrorData {
    ErrorData::internal_error(error.to_string(), None)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cortex_usage_schema_uses_portable_schema_version() {
        let schema = serde_json::to_value(schemars::schema_for!(CortexUsageOutput)).unwrap();

        assert_eq!(
            schema["properties"]["schema_version"],
            json!({"type": "integer", "minimum": 0})
        );
    }

    #[tokio::test]
    async fn bulk_tag_tools_preview_before_applying() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("test", temp.path()).unwrap();
        let root = temp.path().join("test");
        let cx = Cortex::open("test", &root).unwrap();
        let mut upper = Trace::new("Upper", "note", "", vec!["AI".into()], "alpha");
        let upper_id = upper.frontmatter.id.clone();
        cx.add(&mut upper).unwrap();
        let mut lower = Trace::new("Lower", "note", "", vec!["ai".into()], "beta");
        let lower_id = lower.frontmatter.id.clone();
        cx.add(&mut lower).unwrap();
        drop(cx);

        let server = NoemaServer::new("test", &root, false).unwrap();
        let preview = server
            .rename_tag(Parameters(RenameTagParams {
                old_tag: "AI".into(),
                new_tag: "artificial-intelligence".into(),
                ignore_case: true,
                apply: false,
            }))
            .await
            .unwrap();
        let preview: serde_json::Value = serde_json::from_str(&preview).unwrap();
        assert_eq!(preview["applied"], false);
        assert_eq!(preview["plan"]["affected_traces"], 2);
        let changes = preview["plan"]["changes"].as_array().unwrap();
        assert!(
            changes
                .iter()
                .any(|change| change["before"] == json!(["AI"]))
        );
        assert!(
            changes
                .iter()
                .all(|change| { change["after"] == json!(["artificial-intelligence"]) })
        );
        {
            let cx = server.open().await.unwrap();
            assert_eq!(cx.get(&upper_id).unwrap().tags, vec!["AI"]);
            assert_eq!(cx.get(&lower_id).unwrap().tags, vec!["ai"]);
        }

        let applied = server
            .rename_tag(Parameters(RenameTagParams {
                old_tag: "AI".into(),
                new_tag: "artificial-intelligence".into(),
                ignore_case: true,
                apply: true,
            }))
            .await
            .unwrap();
        let applied: serde_json::Value = serde_json::from_str(&applied).unwrap();
        assert_eq!(applied["applied"], true);
        assert_eq!(applied["changed_traces"], 2);
        let cx = server.open().await.unwrap();
        assert_eq!(
            cx.get(&upper_id).unwrap().tags,
            vec!["artificial-intelligence"]
        );
        assert_eq!(
            cx.get(&lower_id).unwrap().tags,
            vec!["artificial-intelligence"]
        );
    }

    #[test]
    fn formats_compact_public_rows() {
        let rows = vec![crate::cortex::Row {
            id: "20260817-example".into(),
            title: "Example title".into(),
            trace_type: "divergence".into(),
            tier: "long".into(),
            author: "benchmark".into(),
            tags: vec!["alpha".into(), "beta".into()],
            created_at: "2026-08-17T12:00:00Z".into(),
            ..Default::default()
        }];
        assert_eq!(
            format_rows(&rows),
            "[L] [DIVERGENCE] 20260817-example (2026-08-17) — benchmark [alpha, beta]\n  Example title\n"
        );
        assert_eq!(format_rows(&[]), "No traces found.");
    }

    #[test]
    fn bearer_digest_requires_the_exact_scheme_and_key() {
        let digest = BearerDigest::new("test-secret");
        assert!(digest.matches(b"Bearer test-secret"));
        assert!(!digest.matches(b"bearer test-secret"));
        assert!(!digest.matches(b"Bearer wrong"));
        assert!(!digest.matches(b""));
    }

    #[tokio::test]
    async fn cors_preflight_bypasses_auth_and_post_remains_keyed() {
        let access_key = AccessKey {
            value: "test-secret".into(),
            ..Default::default()
        };
        let router = apply_http_middleware(
            axum::Router::new().route("/mcp", axum::routing::post(|| async { StatusCode::OK })),
            &access_key,
        );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let server = tokio::spawn(async move {
            axum::serve(listener, router).await.unwrap();
        });
        let client = reqwest::Client::new();
        let endpoint = format!("http://{address}/mcp");

        let preflight = client
            .request(reqwest::Method::OPTIONS, &endpoint)
            .header(header::ORIGIN, "app://obsidian.md")
            .header(header::ACCESS_CONTROL_REQUEST_METHOD, "POST")
            .header(
                header::ACCESS_CONTROL_REQUEST_HEADERS,
                "authorization, content-type, mcp-session-id",
            )
            .send()
            .await
            .unwrap();
        assert_eq!(preflight.status(), StatusCode::NO_CONTENT);
        assert_eq!(
            preflight.headers()[header::ACCESS_CONTROL_ALLOW_ORIGIN],
            "app://obsidian.md"
        );
        assert_eq!(
            preflight.headers()[header::ACCESS_CONTROL_ALLOW_HEADERS],
            "Authorization, Content-Type, Mcp-Session-Id"
        );

        let unauthorized = client.post(&endpoint).send().await.unwrap();
        assert_eq!(unauthorized.status(), StatusCode::UNAUTHORIZED);
        assert!(
            !unauthorized
                .headers()
                .contains_key(header::ACCESS_CONTROL_ALLOW_ORIGIN)
        );

        let rejected_origin = client
            .request(reqwest::Method::OPTIONS, &endpoint)
            .header(header::ORIGIN, "https://untrusted.example")
            .header(header::ACCESS_CONTROL_REQUEST_METHOD, "POST")
            .send()
            .await
            .unwrap();
        assert_eq!(rejected_origin.status(), StatusCode::FORBIDDEN);

        let authorized = client
            .post(&endpoint)
            .header(header::AUTHORIZATION, "Bearer test-secret")
            .header(header::ORIGIN, "capacitor://localhost")
            .send()
            .await
            .unwrap();
        assert_eq!(authorized.status(), StatusCode::OK);
        assert_eq!(
            authorized.headers()[header::ACCESS_CONTROL_ALLOW_ORIGIN],
            "capacitor://localhost"
        );
        assert_eq!(
            authorized.headers()[header::ACCESS_CONTROL_EXPOSE_HEADERS],
            "Mcp-Session-Id"
        );
        server.abort();
    }

    #[test]
    fn host_header_allowlist_includes_names_without_binding_them() {
        let hosts = vec!["127.0.0.1".to_owned()];
        let dynamic_hosts = vec!["192.0.2.10".to_owned()];
        let allowed_hosts = vec![
            "memory.example.com".to_owned(),
            "memory.example.com:3000".to_owned(),
        ];
        let actual = allowed_http_hosts(&hosts, &dynamic_hosts, &allowed_hosts);
        assert_eq!(
            actual,
            [
                "127.0.0.1",
                "192.0.2.10",
                "memory.example.com",
                "memory.example.com:3000",
                "localhost",
                "127.0.0.1",
                "::1",
            ]
        );
    }

    #[cfg(unix)]
    #[test]
    fn dynamic_listener_inventory_contains_loopback_interfaces() {
        let addresses = local_interface_addresses().unwrap();
        assert!(addresses.contains(&std::net::IpAddr::V4(std::net::Ipv4Addr::LOCALHOST)));
        assert!(addresses.contains(&std::net::IpAddr::V6(std::net::Ipv6Addr::LOCALHOST)));
    }
}
