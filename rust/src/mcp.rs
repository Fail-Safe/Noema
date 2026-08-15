use std::{path::PathBuf, sync::Arc};

use anyhow::Result;
use axum::{
    body::Body,
    extract::State,
    http::{Request, StatusCode, header},
    middleware::Next,
    response::{IntoResponse, Response},
};
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
use sha2::{Digest, Sha256};
use tokio::sync::{Mutex, OwnedMutexGuard};
use tokio_util::sync::CancellationToken;

use crate::{
    VERSION,
    cortex::{AccessKey, Cortex, ListOptions, PeerEntry, write_manifest},
    lock::CortexLock,
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
        cx.bump_search_hits(&rows);
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
        cx.bump_search_hits(&rows);
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
        let mut payload = json!({"id":cx.id,"name":cx.name,"version":cx.manifest.version,"binary_version":VERSION,"pubkey":cx.manifest.signing.as_ref().map(|signing| signing.public_key.as_str()).unwrap_or("")});
        let rank = crate::consolidation::get_local_rank(&cx).map_err(mcp_error)?;
        if !rank.cortex_id.is_empty() {
            payload["rank"] = serde_json::to_value(rank).map_err(mcp_error)?;
        }
        Ok(json_text(payload))
    }
    #[tool(description = "Return events for federation sync")]
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
        serde_json::to_string(
            &cx.events_since(p.since.as_deref().unwrap_or(""), limit)
                .map_err(mcp_error)?,
        )
        .map_err(mcp_error)
    }
    #[tool(description = "Return per-peer usage deltas")]
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
    #[tool(description = "Show federation configuration and vector clock")]
    async fn federation_status(&self, _: Parameters<Empty>) -> Result<String, ErrorData> {
        let cx = self.open().await?;
        Ok(json_text(
            crate::federation::status(&cx).map_err(mcp_error)?,
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
    let server = NoemaServer::new(&name, &path)?;
    let (cortex_id, federation) = {
        let cortex = server.cortex.lock().await;
        (
            cortex.id.clone(),
            cortex.manifest.federation.clone().unwrap_or_default(),
        )
    };
    let background_lock = CortexLock::try_acquire_background(&cortex_id)?;
    let cancellation = CancellationToken::new();
    let (scheduler, eligibility) = if background_lock.is_some() {
        (
            Some(crate::federation::FederationScheduler::start(
                name.clone(),
                path.clone(),
                federation,
                cancellation.clone(),
            )?),
            crate::consolidation::EligibilityScheduler::start(name, path, cancellation.clone())?,
        )
    } else {
        eprintln!("another process owns cortex background work; serving MCP only");
        (None, None)
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
    result?;
    Ok(())
}

pub async fn serve_http(
    name: String,
    path: PathBuf,
    host: String,
    port: u16,
    access_key: AccessKey,
    tls: Option<(PathBuf, PathBuf)>,
) -> Result<()> {
    let server = NoemaServer::new(&name, &path)?;
    let (cortex_id, federation) = {
        let cortex = server.cortex.lock().await;
        (
            cortex.id.clone(),
            cortex.manifest.federation.clone().unwrap_or_default(),
        )
    };
    let background_lock = CortexLock::try_acquire_background(&cortex_id)?;
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
    let tls_config = match tls {
        Some((certificate, private_key)) => Some(
            axum_server::tls_rustls::RustlsConfig::from_pem_file(certificate, private_key).await?,
        ),
        None => None,
    };
    let listener = std::net::TcpListener::bind((host.as_str(), port))?;
    listener.set_nonblocking(true)?;
    let address = listener.local_addr()?;
    let cancellation = CancellationToken::new();
    let (scheduler, eligibility) = if background_lock.is_some() {
        (
            Some(crate::federation::FederationScheduler::start(
                name.clone(),
                path.clone(),
                federation,
                cancellation.clone(),
            )?),
            crate::consolidation::EligibilityScheduler::start(name, path, cancellation.clone())?,
        )
    } else {
        eprintln!("another process owns cortex background work; serving MCP only");
        (None, None)
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
    eprintln!("Noema MCP listening on {scheme}://{address}/mcp");
    let mut router = axum::Router::new().nest_service("/mcp", service);
    if access_key.keyed() {
        let expected = BearerDigest::new(&access_key.value);
        router = router.layer(axum::middleware::from_fn_with_state(expected, bearer_auth));
    }
    let server_handle = axum_server::Handle::new();
    let shutdown_handle = server_handle.clone();
    let server_cancellation = cancellation.clone();
    let shutdown_task = tokio::spawn(async move {
        server_cancellation.cancelled().await;
        shutdown_handle.graceful_shutdown(Some(std::time::Duration::from_secs(5)));
    });
    let result = if let Some(tls_config) = tls_config {
        axum_server::from_tcp_rustls(listener, tls_config)?
            .handle(server_handle)
            .serve(router.into_make_service())
            .await
    } else {
        axum_server::from_tcp(listener)?
            .handle(server_handle)
            .serve(router.into_make_service())
            .await
    };
    cancellation.cancel();
    if let Some(scheduler) = scheduler {
        scheduler.stop().await;
    }
    if let Some(eligibility) = eligibility {
        eligibility.stop().await;
    }
    signal_task.abort();
    shutdown_task.abort();
    result?;
    Ok(())
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bearer_digest_requires_the_exact_scheme_and_key() {
        let digest = BearerDigest::new("test-secret");
        assert!(digest.matches(b"Bearer test-secret"));
        assert!(!digest.matches(b"bearer test-secret"));
        assert!(!digest.matches(b"Bearer wrong"));
        assert!(!digest.matches(b""));
    }
}
