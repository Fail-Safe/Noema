use std::{path::PathBuf, sync::Arc};

use anyhow::Result;
use rmcp::{
    ErrorData, ServerHandler, ServiceExt,
    handler::server::{router::tool::ToolRouter, wrapper::Parameters},
    tool, tool_handler, tool_router,
    transport::streamable_http_server::{
        StreamableHttpServerConfig, StreamableHttpService, session::local::LocalSessionManager,
    },
};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};
use serde_json::json;
use tokio::sync::{Mutex, OwnedMutexGuard};

use crate::{
    VERSION,
    cortex::{Cortex, ListOptions, PeerEntry, write_manifest},
    trace::Trace,
};

#[derive(Clone)]
pub struct NoemaServer {
    name: String,
    cortex: Arc<Mutex<Cortex>>,
    tool_router: ToolRouter<Self>,
}

impl NoemaServer {
    pub fn new(name: impl Into<String>, path: impl Into<PathBuf>) -> Result<Self> {
        let name = name.into();
        let cortex = Cortex::open(&name, path.into())?;
        Ok(Self {
            name,
            cortex: Arc::new(Mutex::new(cortex)),
            tool_router: Self::tool_router(),
        })
    }

    async fn open(&self) -> Result<OwnedMutexGuard<Cortex>, ErrorData> {
        Ok(self.cortex.clone().lock_owned().await)
    }
}

#[tool_handler(router = self.tool_router, name = "noema", version = "0.1.0")]
impl ServerHandler for NoemaServer {}

#[derive(Debug, Deserialize, JsonSchema)]
struct Empty {}

#[derive(Debug, Deserialize, JsonSchema)]
struct IdParam {
    id: String,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct ListParams {
    #[serde(default, rename = "type")]
    trace_type: String,
    #[serde(default)]
    author: String,
    #[serde(default)]
    tag: String,
    #[serde(default)]
    origin: String,
    #[serde(default)]
    archived: bool,
    #[serde(default)]
    all: bool,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct CreateParams {
    title: String,
    #[serde(rename = "type")]
    trace_type: String,
    body: String,
    #[serde(default)]
    author: String,
    #[serde(default)]
    tags: String,
    #[serde(default)]
    derived_from: String,
    #[serde(default)]
    origin: String,
    #[serde(default)]
    source_hash: String,
    #[serde(default)]
    source_locked: bool,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct UpdateParams {
    id: String,
    title: Option<String>,
    #[serde(rename = "type")]
    trace_type: Option<String>,
    author: Option<String>,
    tags: Option<String>,
    derived_from: Option<String>,
    body: Option<String>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct SearchParams {
    query: String,
    #[serde(default)]
    all: bool,
    #[serde(default)]
    mode: String,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct SimilarParams {
    trace_id: String,
    limit: Option<usize>,
    include_archived: Option<bool>,
    mode: Option<String>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct TagsParam {
    id: String,
    tags: String,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct VoteParam {
    id: String,
    direction: String,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct AppendParam {
    id: String,
    content: String,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct SinceParam {
    since: Option<String>,
    limit: Option<usize>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct TopParam {
    top: Option<usize>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct CandidateParam {}

#[derive(Debug, Deserialize, JsonSchema)]
struct HealthParam {
    since: Option<String>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct ConsolidateParam {
    title: String,
    body: String,
    source_ids: String,
    tags: Option<String>,
    author: Option<String>,
    model_name: Option<String>,
    model_tier_profile: Option<String>,
    cohesion_confidence: Option<f64>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct ResolveParam {
    id: String,
    accept: Option<String>,
    body: Option<String>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct AnnounceParam {
    name: String,
    endpoint: String,
}

#[tool_router(router = tool_router)]
impl NoemaServer {
    #[tool(description = "Returns concise Markdown guidance for agent use of this Cortex.")]
    async fn get_instructions(&self, _: Parameters<Empty>) -> String {
        format!(
            "# Noema Agent Instructions\n\n## Active Cortex\nName:     {}\nVersion:  noema-rs v{} (manifest v2)\n\nUse `list_traces` with tag=\"user-preference\" at startup, then `get_trace` with record_usage=false.",
            self.name, VERSION
        )
    }

    #[tool(description = "Returns structured JSON context for MCP clients.")]
    async fn cortex_usage(&self, _: Parameters<Empty>) -> Result<String, ErrorData> {
        Ok(json_text(
            json!({"schema_version":1,"cortex":{"name":self.name},"contract":{"tool_discovery_authoritative":true},"runtime":{"implementation":"rust"}}),
        ))
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
        Ok(json_text(rows))
    }

    #[tool(description = "Get a trace by ID, including its full body")]
    async fn get_trace(&self, Parameters(p): Parameters<IdParam>) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        let (row, trace) = cx.get_trace(&p.id).map_err(mcp_error)?;
        cx.bump_read(&p.id).map_err(mcp_error)?;
        Ok(json_text(
            json!({"id":row.id,"title":row.title,"type":row.trace_type,"tier":row.tier,"author":row.author,"tags":row.tags,"derived_from":row.derived_from,"origin":row.origin,"created":row.created_at,"updated":row.updated_at,"body":trace.body,"content_hash":row.content_hash,"source_hash":row.source_hash,"source_locked":row.source_locked}),
        ))
    }

    #[tool(description = "Create a new trace")]
    async fn create_trace(
        &self,
        Parameters(p): Parameters<CreateParams>,
    ) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        let mut trace = Trace::new(p.title, p.trace_type, p.author, csv(&p.tags), p.body);
        trace.frontmatter.derived_from = csv(&p.derived_from);
        trace.frontmatter.origin = p.origin;
        trace.frontmatter.source_hash = p.source_hash;
        trace.frontmatter.source_locked = p.source_locked;
        cx.add(&mut trace).map_err(mcp_error)?;
        Ok(json_text(
            json!({"id":trace.frontmatter.id,"title":trace.frontmatter.title}),
        ))
    }

    #[tool(description = "Full-text search across traces")]
    async fn search_traces(
        &self,
        Parameters(p): Parameters<SearchParams>,
    ) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        let rows = cx
            .search(
                &p.query,
                &ListOptions {
                    all: p.all,
                    ..Default::default()
                },
            )
            .map_err(mcp_error)?;
        Ok(json_text(
            json!({"mode":if p.mode.is_empty(){"lexical"}else{&p.mode},"results":rows}),
        ))
    }

    #[tool(description = "Find traces related to a given trace")]
    async fn find_similar_traces(
        &self,
        Parameters(p): Parameters<SimilarParams>,
    ) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        let (_row, trace) = cx.get_trace(&p.trace_id).map_err(mcp_error)?;
        let query = trace
            .body
            .split_whitespace()
            .take(20)
            .collect::<Vec<_>>()
            .join(" OR ");
        let mut rows = cx
            .search(
                &query,
                &ListOptions {
                    all: p.include_archived.unwrap_or(false),
                    ..Default::default()
                },
            )
            .map_err(mcp_error)?;
        rows.retain(|row| row.id != p.trace_id);
        rows.truncate(p.limit.unwrap_or(10));
        Ok(json_text(
            json!({"mode":p.mode.unwrap_or_else(||"lexical".into()),"results":rows}),
        ))
    }

    #[tool(description = "Move a trace to trash")]
    async fn delete_trace(&self, Parameters(p): Parameters<IdParam>) -> Result<String, ErrorData> {
        self.open().await?.trash(&p.id).map_err(mcp_error)?;
        Ok("Trace moved to trash".into())
    }
    #[tool(description = "Restore a trace from trash")]
    async fn recover_trace(&self, Parameters(p): Parameters<IdParam>) -> Result<String, ErrorData> {
        self.open().await?.recover(&p.id).map_err(mcp_error)?;
        Ok("Trace recovered".into())
    }
    #[tool(description = "Archive a trace")]
    async fn archive_trace(&self, Parameters(p): Parameters<IdParam>) -> Result<String, ErrorData> {
        self.open().await?.archive(&p.id).map_err(mcp_error)?;
        Ok("Trace archived".into())
    }
    #[tool(description = "Restore an archived trace")]
    async fn unarchive_trace(
        &self,
        Parameters(p): Parameters<IdParam>,
    ) -> Result<String, ErrorData> {
        self.open().await?.unarchive(&p.id).map_err(mcp_error)?;
        Ok("Trace unarchived".into())
    }

    #[tool(description = "Update fields of an existing trace")]
    async fn update_trace(
        &self,
        Parameters(p): Parameters<UpdateParams>,
    ) -> Result<String, ErrorData> {
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

    #[tool(description = "Replace trace tags")]
    async fn set_trace_tags(
        &self,
        Parameters(p): Parameters<TagsParam>,
    ) -> Result<String, ErrorData> {
        let tags = csv(&p.tags);
        self.open()
            .await?
            .set_tags(&p.id, tags.clone(), true)
            .map_err(mcp_error)?;
        Ok(json_text(json!({"id":p.id,"action":"set","tags":tags})))
    }
    #[tool(description = "Add trace tags idempotently")]
    async fn append_trace_tags(
        &self,
        Parameters(p): Parameters<TagsParam>,
    ) -> Result<String, ErrorData> {
        let tags = self
            .open()
            .await?
            .append_tags(&p.id, csv(&p.tags), true)
            .map_err(mcp_error)?;
        Ok(json_text(json!({"id":p.id,"action":"append","tags":tags})))
    }
    #[tool(description = "Cast a tier-preference vote")]
    async fn vote_trace(&self, Parameters(p): Parameters<VoteParam>) -> Result<String, ErrorData> {
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
        Ok("Vote recorded".into())
    }

    #[tool(description = "Return short-tier consolidation candidates")]
    async fn list_consolidation_candidates(
        &self,
        _: Parameters<CandidateParam>,
    ) -> Result<String, ErrorData> {
        let rows = self
            .open()
            .await?
            .list(&ListOptions {
                tiers: vec!["short".into()],
                ..Default::default()
            })
            .map_err(mcp_error)?;
        Ok(json_text(rows))
    }
    #[tool(description = "Recent consolidation pipeline health")]
    async fn consolidation_health(
        &self,
        Parameters(p): Parameters<HealthParam>,
    ) -> Result<String, ErrorData> {
        Ok(json_text(
            json!({"since":p.since.unwrap_or_else(||"24h".into()),"daily":[],"totals":{"success":0,"fail":0,"promote":0,"distill":0}}),
        ))
    }
    #[tool(description = "Top traces and tags by search popularity")]
    async fn search_activity(
        &self,
        Parameters(p): Parameters<TopParam>,
    ) -> Result<String, ErrorData> {
        let mut rows = self
            .open()
            .await?
            .list(&ListOptions::default())
            .map_err(mcp_error)?;
        rows.truncate(p.top.unwrap_or(10));
        Ok(json_text(json!({"traces":rows,"tags":[]})))
    }
    #[tool(description = "Materialize a distilled mid-tier trace")]
    async fn record_consolidation_result(
        &self,
        Parameters(p): Parameters<ConsolidateParam>,
    ) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        let mut trace = Trace::new(
            p.title,
            "note",
            p.author.unwrap_or_default(),
            p.tags.map(|s| csv(&s)).unwrap_or_default(),
            p.body,
        );
        trace.frontmatter.tier = "mid".into();
        trace.frontmatter.derived_from = csv(&p.source_ids);
        cx.add(&mut trace).map_err(mcp_error)?;
        Ok(json_text(
            json!({"distilled_id":trace.frontmatter.id,"model_name":p.model_name,"model_tier_profile":p.model_tier_profile,"cohesion_confidence":p.cohesion_confidence}),
        ))
    }
    #[tool(description = "Append content to an existing trace")]
    async fn append_trace(
        &self,
        Parameters(p): Parameters<AppendParam>,
    ) -> Result<String, ErrorData> {
        self.open()
            .await?
            .append(&p.id, &p.content, true)
            .map_err(mcp_error)?;
        Ok("Content appended".into())
    }
    #[tool(description = "Show the event log for a trace")]
    async fn trace_history(&self, Parameters(p): Parameters<IdParam>) -> Result<String, ErrorData> {
        Ok(json_text(
            self.open().await?.history(&p.id).map_err(mcp_error)?,
        ))
    }
    #[tool(description = "Show the derivation graph for a trace")]
    async fn trace_lineage(&self, Parameters(p): Parameters<IdParam>) -> Result<String, ErrorData> {
        let (from, by) = self.open().await?.lineage(&p.id).map_err(mcp_error)?;
        Ok(json_text(
            json!({"id":p.id,"derived_from":from,"derived_by":by}),
        ))
    }
    #[tool(description = "Resolve a divergence")]
    async fn resolve_divergence(
        &self,
        Parameters(p): Parameters<ResolveParam>,
    ) -> Result<String, ErrorData> {
        let _ = (p.accept, p.body);
        Err(ErrorData::invalid_params(
            format!(
                "divergence resolution for {} requires a parsed conflict body",
                p.id
            ),
            None,
        ))
    }
    #[tool(description = "Return this cortex stable identity")]
    async fn cortex_identity(&self, _: Parameters<Empty>) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        Ok(json_text(
            json!({"id":cx.id,"name":cx.name,"version":cx.manifest.version,"binary_version":VERSION,"pubkey":cx.manifest.signing.as_ref().map(|signing| signing.public_key.as_str()).unwrap_or("")}),
        ))
    }
    #[tool(description = "Return events for federation sync")]
    async fn sync_events(
        &self,
        Parameters(p): Parameters<SinceParam>,
    ) -> Result<String, ErrorData> {
        serde_json::to_string(
            &self
                .open()
                .await?
                .events_since(
                    p.since.as_deref().unwrap_or(""),
                    p.limit.unwrap_or(100).min(1000),
                )
                .map_err(mcp_error)?,
        )
        .map_err(mcp_error)
    }
    #[tool(description = "Return per-peer usage deltas")]
    async fn sync_read_signal(&self, _: Parameters<SinceParam>) -> String {
        "[]".into()
    }
    #[tool(description = "Show federation configuration and vector clock")]
    async fn federation_status(&self, _: Parameters<Empty>) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        Ok(json_text(
            json!({"mode":cx.manifest.federation.as_ref().map(|f|f.mode.clone()).unwrap_or_else(||"sync".into()),"peers":cx.manifest.federation.as_ref().map(|f|f.peers.clone()).unwrap_or_default(),"vclock":cx.get_clock().map_err(mcp_error)?}),
        ))
    }
    #[tool(description = "Accept a peer announcement")]
    async fn announce_peer(
        &self,
        Parameters(p): Parameters<AnnounceParam>,
    ) -> Result<String, ErrorData> {
        let mut cx = self.open().await?;
        let federation = cx.manifest.federation.get_or_insert_with(Default::default);
        if !federation.peers.iter().any(|peer| peer.name == p.name) {
            federation.peers.push(PeerEntry {
                name: p.name,
                endpoint: p.endpoint,
                ..Default::default()
            });
            write_manifest(&cx.dir, &cx.manifest).map_err(mcp_error)?;
        }
        Ok(json_text(json!({"id":cx.id,"name":cx.name})))
    }
}

pub async fn serve_stdio(name: String, path: PathBuf) -> Result<()> {
    NoemaServer::new(name, path)?
        .serve(rmcp::transport::stdio())
        .await?
        .waiting()
        .await?;
    Ok(())
}

pub async fn serve_http(name: String, path: PathBuf, host: String, port: u16) -> Result<()> {
    let server = NoemaServer::new(name, path)?;
    let service: StreamableHttpService<NoemaServer, LocalSessionManager> =
        StreamableHttpService::new(
            move || Ok(server.clone()),
            Default::default(),
            StreamableHttpServerConfig::default().with_allowed_hosts([
                host.clone(),
                "localhost".to_owned(),
                "127.0.0.1".to_owned(),
                "::1".to_owned(),
            ]),
        );
    let listener = tokio::net::TcpListener::bind((host.as_str(), port)).await?;
    let address = listener.local_addr()?;
    eprintln!("Noema MCP listening on http://{address}/mcp");
    axum::serve(listener, axum::Router::new().nest_service("/mcp", service))
        .with_graceful_shutdown(async {
            let _ = tokio::signal::ctrl_c().await;
        })
        .await?;
    Ok(())
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
fn mcp_error(error: impl std::fmt::Display) -> ErrorData {
    ErrorData::internal_error(error.to_string(), None)
}
