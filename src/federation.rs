use std::{
    collections::{BTreeMap, BTreeSet, HashMap},
    path::PathBuf,
    time::Duration,
};

use anyhow::{Context, Result, bail};
use chrono::Utc;
use rmcp::{
    ServiceExt,
    model::{CallToolRequestParams, ClientInfo},
    transport::{
        StreamableHttpClientTransport, streamable_http_client::StreamableHttpClientTransportConfig,
    },
};
use serde::{Deserialize, Serialize};
use serde_json::json;
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

use crate::{
    cortex::{
        Cortex, FederationConfig, MANIFEST_VERSION, PeerEntry, load_access_key, read_manifest,
    },
    event::Event,
    eventsig,
};

pub type VectorClock = BTreeMap<String, u64>;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum Relation {
    Before,
    Equal,
    After,
    Concurrent,
}

pub fn compare(left: &VectorClock, right: &VectorClock) -> Relation {
    let keys: BTreeSet<_> = left.keys().chain(right.keys()).collect();
    let mut less = false;
    let mut greater = false;
    for key in keys {
        let a = left.get(key).copied().unwrap_or_default();
        let b = right.get(key).copied().unwrap_or_default();
        less |= a < b;
        greater |= a > b;
    }
    match (less, greater) {
        (false, false) => Relation::Equal,
        (true, false) => Relation::Before,
        (false, true) => Relation::After,
        (true, true) => Relation::Concurrent,
    }
}

pub fn compare_for_replay(left: &VectorClock, right: &VectorClock) -> Relation {
    let relation = compare(left, right);
    if relation != Relation::Concurrent
        || !left
            .keys()
            .chain(right.keys())
            .any(|key| ulid::Ulid::from_string(key).is_err())
    {
        return relation;
    }

    let stable_left: VectorClock = left
        .iter()
        .filter(|(key, _)| ulid::Ulid::from_string(key).is_ok())
        .map(|(key, value)| (key.clone(), *value))
        .collect();
    let stable_right: VectorClock = right
        .iter()
        .filter(|(key, _)| ulid::Ulid::from_string(key).is_ok())
        .map(|(key, value)| (key.clone(), *value))
        .collect();
    if stable_left.is_empty() || stable_right.is_empty() {
        return relation;
    }

    match compare(&stable_left, &stable_right) {
        Relation::Before => Relation::Before,
        Relation::After => Relation::After,
        Relation::Equal | Relation::Concurrent => relation,
    }
}

pub fn merge(left: &mut VectorClock, right: &VectorClock) {
    for (key, value) in right {
        left.entry(key.clone())
            .and_modify(|current| *current = (*current).max(*value))
            .or_insert(*value);
    }
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct SyncReport {
    pub peer: String,
    pub batches: usize,
    pub events: usize,
    pub usage_rows: usize,
    pub cursor: String,
    pub peer_version: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct TraceUsage {
    pub trace_id: String,
    pub peer_cortex_id: String,
    pub read_count: i64,
    pub modify_count: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub search_hit_count: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_read_at: String,
    pub updated_at: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct PeerHealth {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub version: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub version_observed_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_success: String,
    #[serde(default, skip_serializing_if = "is_zero_usize")]
    pub consecutive_failures: usize,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_error: Option<PeerError>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PeerError {
    pub reason: String,
    pub observed_at: String,
}

pub struct FederationScheduler {
    cancellation: CancellationToken,
    manager: JoinHandle<()>,
}

struct PeerWorker {
    cancellation: CancellationToken,
    task: JoinHandle<()>,
}

pub fn status(cx: &Cortex) -> Result<serde_json::Value> {
    let manifest = read_manifest(&cx.dir)?;
    let access = match load_access_key(&cx.dir, manifest.access.as_ref()) {
        Ok(key) if key.keyed() => json!({
            "mode": "keyed",
            "source": key.source,
            "path": key.path,
            "fingerprint": key.fingerprint,
        }),
        Ok(_) => json!({"mode": "open"}),
        Err(error) => json!({"mode": "error", "error": error.to_string()}),
    };
    let config = manifest.federation.unwrap_or_default();
    let local_rank = crate::consolidation::get_local_rank(cx)?;
    let mut peers = Vec::with_capacity(config.peers.len());
    for peer in &config.peers {
        let health_text = cx.federation_state(&format!("peer:{}:health", peer.name))?;
        let health = if health_text.is_empty() {
            None
        } else {
            serde_json::from_str::<PeerHealth>(&health_text).ok()
        };
        let rank = crate::consolidation::get_peer_rank(cx, &peer.name)?;
        peers.push(json!({
            "name": peer.name,
            "endpoint": peer.endpoint,
            "mode": if peer.mode.is_empty() { "active" } else { peer.mode.as_str() },
            "cortex_id": cx.federation_state(&format!("peer:{}:cortex_id", peer.name))?,
            "last_event": cx.federation_state(&format!("peer:{}:last_event", peer.name))?,
            "last_seen": cx.federation_state(&format!("peer:{}:last_seen", peer.name))?,
            "last_usage": cx.federation_state(&format!("peer:{}:last_usage", peer.name))?,
            "consolidation_rank": rank,
            "health": health,
        }));
    }
    let quiet_period = parse_interval(&config.interval)
        .unwrap_or(Duration::from_secs(30))
        .saturating_mul(2);
    let election = crate::consolidation::decide(
        &cx.id,
        local_rank.clone(),
        crate::consolidation::configured_peer_ranks(cx, &config.peers),
        quiet_period,
        Utc::now(),
    );
    Ok(json!({
        "mode": if config.mode.is_empty() { "sync" } else { config.mode.as_str() },
        "interval": if config.interval.is_empty() { "30s" } else { config.interval.as_str() },
        "verify": if config.verify.is_empty() { "off" } else { config.verify.as_str() },
        "access": access,
        "consolidation": {
            "local_rank": local_rank,
            "quiet_period_ms": quiet_period.as_millis(),
            "winner": election.winner,
            "should_run_here": election.should_run,
            "reason": election.reason,
        },
        "peers": peers,
        "vclock": cx.get_clock()?,
    }))
}

impl FederationScheduler {
    pub fn start(
        name: String,
        path: PathBuf,
        config: FederationConfig,
        cancellation: CancellationToken,
    ) -> Result<Self> {
        let interval = parse_interval(&config.interval)?;
        Cortex::open(&name, &path)?;
        let manager_cancellation = cancellation.clone();
        let manager = tokio::spawn(async move {
            let mut workers = HashMap::new();
            let mut next_config = Some(config);
            loop {
                if manager_cancellation.is_cancelled() {
                    break;
                }
                let live_config = match next_config.take() {
                    Some(config) => Some(config),
                    None => match read_manifest(&path) {
                        Ok(manifest) => Some(manifest.federation.unwrap_or_default()),
                        Err(error) => {
                            eprintln!("federation configuration warning: {error:#}");
                            None
                        }
                    },
                };
                let wait = live_config
                    .as_ref()
                    .and_then(|config| parse_interval(&config.interval).ok())
                    .unwrap_or(interval);
                if let Some(config) = live_config {
                    reconcile_peer_workers(&name, &path, &config, interval, &mut workers).await;
                }
                tokio::select! {
                    _ = manager_cancellation.cancelled() => break,
                    _ = tokio::time::sleep(wait) => {}
                }
            }
            stop_peer_workers(&mut workers).await;
        });
        Ok(Self {
            cancellation,
            manager,
        })
    }

    pub async fn stop(self) {
        self.cancellation.cancel();
        if let Err(error) = self.manager.await {
            eprintln!("federation manager join warning: {error}");
        }
    }
}

async fn reconcile_peer_workers(
    name: &str,
    path: &std::path::Path,
    config: &FederationConfig,
    fallback_interval: Duration,
    workers: &mut HashMap<String, PeerWorker>,
) {
    let finished: Vec<_> = workers
        .iter()
        .filter(|(_, worker)| worker.task.is_finished())
        .map(|(peer_name, _)| peer_name.clone())
        .collect();
    for peer_name in finished {
        if let Some(worker) = workers.remove(&peer_name)
            && let Err(error) = worker.task.await
        {
            eprintln!("federation worker join warning for peer {peer_name}: {error}");
        }
    }
    let desired: BTreeSet<_> = config.peers.iter().map(|peer| peer.name.clone()).collect();
    let removed: Vec<_> = workers
        .keys()
        .filter(|peer_name| !desired.contains(*peer_name))
        .cloned()
        .collect();
    for peer_name in removed {
        if let Some(worker) = workers.remove(&peer_name) {
            stop_peer_worker(&peer_name, worker).await;
        }
    }
    for peer in &config.peers {
        if workers.contains_key(&peer.name) {
            continue;
        }
        match spawn_peer_worker(name, path, peer.name.clone(), fallback_interval) {
            Ok(worker) => {
                eprintln!("federation worker started for peer {}", peer.name);
                workers.insert(peer.name.clone(), worker);
            }
            Err(error) => {
                eprintln!(
                    "federation worker startup warning for peer {}: {error:#}",
                    peer.name
                );
            }
        }
    }
}

fn spawn_peer_worker(
    name: &str,
    path: &std::path::Path,
    peer_name: String,
    fallback_interval: Duration,
) -> Result<PeerWorker> {
    let mut cx = Cortex::open(name, path)?;
    let manifest_path = path.to_path_buf();
    let cancellation = CancellationToken::new();
    let worker_cancellation = cancellation.clone();
    let task = tokio::spawn(async move {
        let mut backoff = fallback_interval;
        let mut was_failing = false;
        let mut next_attempt = tokio::time::Instant::now();
        let mut last_configuration = None;
        loop {
            if worker_cancellation.is_cancelled() {
                break;
            }
            let manifest = match read_manifest(&manifest_path) {
                Ok(manifest) => manifest,
                Err(error) => {
                    eprintln!("federation configuration warning for peer {peer_name}: {error:#}");
                    tokio::select! {
                        _ = worker_cancellation.cancelled() => break,
                        _ = tokio::time::sleep(fallback_interval) => {}
                    }
                    continue;
                }
            };
            let live_access = manifest.access.clone();
            let live_config = manifest.federation.unwrap_or_default();
            let live_interval = parse_interval(&live_config.interval).unwrap_or(fallback_interval);
            let Some(live_peer) = live_config
                .peers
                .iter()
                .find(|candidate| candidate.name == peer_name)
                .cloned()
            else {
                tokio::select! {
                    _ = worker_cancellation.cancelled() => break,
                    _ = tokio::time::sleep(live_interval) => {}
                }
                continue;
            };
            let access_fingerprint = load_access_key(&manifest_path, live_access.as_ref())
                .map(|key| key.fingerprint)
                .unwrap_or_else(|_| "invalid".into());
            let configuration = (
                live_config.mode.clone(),
                live_peer.endpoint.clone(),
                live_peer.ca.clone(),
                live_peer.mode.clone(),
                live_peer.pubkey.clone(),
                access_fingerprint,
            );
            if last_configuration.as_ref() != Some(&configuration) {
                last_configuration = Some(configuration);
                backoff = live_interval;
                next_attempt = tokio::time::Instant::now();
            }
            if live_config.mode == "publish" || live_peer.mode == "paused" {
                backoff = live_interval;
                tokio::select! {
                    _ = worker_cancellation.cancelled() => break,
                    _ = tokio::time::sleep(live_interval) => {}
                }
                continue;
            }
            let now = tokio::time::Instant::now();
            if now < next_attempt {
                let wait = live_interval.min(next_attempt.duration_since(now));
                tokio::select! {
                    _ = worker_cancellation.cancelled() => break,
                    _ = tokio::time::sleep(wait) => {}
                }
                continue;
            }
            cx.manifest.access = live_access;
            cx.manifest.federation = Some(live_config);
            match sync_peer(&mut cx, &live_peer).await {
                Ok(report) => {
                    if let Err(error) =
                        record_health(&cx, &peer_name, Some(&report.peer_version), None)
                    {
                        eprintln!("federation health warning for peer {peer_name}: {error:#}");
                    }
                    if was_failing {
                        eprintln!("federation peer {peer_name} reachable again");
                    }
                    was_failing = false;
                    backoff = live_interval;
                    next_attempt = tokio::time::Instant::now() + live_interval;
                }
                Err(error) => {
                    if let Err(health_error) = record_health(&cx, &peer_name, None, Some(&error)) {
                        eprintln!(
                            "federation health warning for peer {peer_name}: {health_error:#}"
                        );
                    }
                    backoff = backoff.saturating_mul(2).min(Duration::from_secs(300));
                    next_attempt = tokio::time::Instant::now() + backoff;
                    eprintln!(
                        "federation peer {peer_name} poll failed; retrying in {backoff:?}: {error:#}"
                    );
                    was_failing = true;
                }
            }
            let wait = live_interval.min(
                next_attempt
                    .checked_duration_since(tokio::time::Instant::now())
                    .unwrap_or_default(),
            );
            tokio::select! {
                _ = worker_cancellation.cancelled() => break,
                _ = tokio::time::sleep(wait) => {}
            }
        }
    });
    Ok(PeerWorker { cancellation, task })
}

async fn stop_peer_worker(peer_name: &str, worker: PeerWorker) {
    worker.cancellation.cancel();
    if let Err(error) = worker.task.await {
        eprintln!("federation worker join warning for peer {peer_name}: {error}");
    } else {
        eprintln!("federation worker stopped for peer {peer_name}");
    }
}

async fn stop_peer_workers(workers: &mut HashMap<String, PeerWorker>) {
    for worker in workers.values() {
        worker.cancellation.cancel();
    }
    for (peer_name, worker) in workers.drain() {
        if let Err(error) = worker.task.await {
            eprintln!("federation worker join warning for peer {peer_name}: {error}");
        }
    }
}

#[derive(Debug, Deserialize)]
struct PeerIdentity {
    id: String,
    #[allow(dead_code)]
    name: String,
    #[serde(alias = "manifest_version")]
    version: u32,
    #[serde(default, alias = "public_key")]
    pubkey: String,
    #[serde(default)]
    rank: Option<crate::consolidation::RankEntry>,
}

pub async fn sync_peer(cx: &mut Cortex, peer: &PeerEntry) -> Result<SyncReport> {
    if peer.mode == "paused" {
        bail!("peer {:?} is paused", peer.name);
    }

    let endpoint = format!("{}/mcp", peer.endpoint.trim_end_matches('/'));
    let access_key = load_access_key(&cx.dir, cx.manifest.access.as_ref())?;
    let mut transport_config = StreamableHttpClientTransportConfig::with_uri(endpoint.clone());
    if access_key.keyed() {
        transport_config = transport_config.auth_header(access_key.value);
    }
    let mut client_builder = reqwest::Client::builder()
        .pool_max_idle_per_host(0)
        .redirect(reqwest::redirect::Policy::none())
        .tls_version_min(reqwest::tls::Version::TLS_1_2);
    if !peer.ca.is_empty() {
        let pem = std::fs::read(&peer.ca)
            .with_context(|| format!("reading CA file for peer {:?}", peer.name))?;
        let certificates = reqwest::Certificate::from_pem_bundle(&pem)
            .with_context(|| format!("parsing CA file for peer {:?}", peer.name))?;
        if certificates.is_empty() {
            bail!("CA file for peer {:?} contains no certificates", peer.name);
        }
        client_builder = client_builder.tls_certs_merge(certificates);
    }
    let http_client = client_builder
        .build()
        .context("building federation HTTP client")?;
    let transport = StreamableHttpClientTransport::with_client(http_client, transport_config);
    let mut client = ClientInfo::default()
        .serve(transport)
        .await
        .with_context(|| format!("connecting to peer {:?} at {endpoint}", peer.name))?;

    let identity_result = client
        .call_tool(
            CallToolRequestParams::new("cortex_identity").with_arguments(serde_json::Map::new()),
        )
        .await
        .with_context(|| format!("calling cortex_identity on peer {:?}", peer.name))?;
    if identity_result.is_error == Some(true) {
        bail!("peer {:?} refused cortex_identity", peer.name);
    }
    let identity: PeerIdentity = identity_result
        .into_typed()
        .with_context(|| format!("parsing cortex_identity from peer {:?}", peer.name))?;
    if let Some(mut rank) = identity.rank.clone() {
        rank.cortex_id.clone_from(&identity.id);
        crate::consolidation::set_peer_rank(cx, &peer.name, &rank)?;
    }
    verify_and_pin_identity(cx, peer, &identity)?;

    let cursor_key = format!("peer:{}:last_event", peer.name);
    let mut cursor = cx.federation_state(&cursor_key)?;
    let mut report = SyncReport {
        peer: peer.name.clone(),
        cursor: cursor.clone(),
        peer_version: client
            .peer_info()
            .and_then(|info| {
                info.server_info
                    .as_ref()
                    .map(|server| server.version.clone())
            })
            .unwrap_or_default(),
        ..SyncReport::default()
    };

    loop {
        let mut arguments = serde_json::Map::new();
        arguments.insert("limit".into(), json!(100));
        if !cursor.is_empty() {
            arguments.insert("since".into(), json!(cursor));
        }
        let result = client
            .call_tool(CallToolRequestParams::new("sync_events").with_arguments(arguments))
            .await
            .with_context(|| format!("calling sync_events on peer {:?}", peer.name))?;
        if result.is_error == Some(true) {
            bail!("peer {:?} refused sync_events", peer.name);
        }
        let events: Vec<Event> = result
            .into_typed::<Option<Vec<Event>>>()
            .with_context(|| format!("parsing sync_events from peer {:?}", peer.name))?
            .unwrap_or_default();
        if events.len() > 100 {
            bail!(
                "peer {:?} returned {} events for a 100-event request",
                peer.name,
                events.len()
            );
        }
        if serde_json::to_vec(&events)?.len() > 100 * 1024 * 1024 {
            bail!("peer {:?} returned an oversized event batch", peer.name);
        }
        if events.is_empty() {
            break;
        }

        report.batches += 1;
        let batch_len = events.len();
        for event in events {
            if !cursor.is_empty() && event.id <= cursor {
                bail!(
                    "peer {:?} returned non-advancing event {} after cursor {}",
                    peer.name,
                    event.id,
                    cursor
                );
            }
            cx.replay_event(&event).with_context(|| {
                format!(
                    "replaying event {} ({}) from peer {:?}; cursor remains at {}",
                    event.id, event.action, peer.name, cursor
                )
            })?;
            cursor = event.id;
            cx.set_federation_state(&cursor_key, &cursor)?;
            report.events += 1;
            report.cursor.clone_from(&cursor);
        }
        if batch_len < 100 {
            break;
        }
    }

    if let Err(error) = sync_usage(cx, &client, peer, &identity.id, &mut report).await {
        eprintln!(
            "federation usage sync warning for peer {}: {error:#}",
            peer.name
        );
    }

    cx.set_federation_state(
        &format!("peer:{}:last_seen", peer.name),
        &Utc::now().to_rfc3339(),
    )?;
    client.close().await.context("closing peer MCP client")?;
    Ok(report)
}

async fn sync_usage(
    cx: &mut Cortex,
    client: &rmcp::service::RunningService<rmcp::RoleClient, ClientInfo>,
    peer: &PeerEntry,
    peer_id: &str,
    report: &mut SyncReport,
) -> Result<()> {
    let cursor_key = format!("peer:{}:last_usage", peer.name);
    let cursor = cx.federation_state(&cursor_key)?;
    let mut arguments = serde_json::Map::new();
    arguments.insert("limit".into(), json!(500));
    if !cursor.is_empty() {
        arguments.insert("since".into(), json!(cursor));
    }
    let result = client
        .call_tool(CallToolRequestParams::new("sync_read_signal").with_arguments(arguments))
        .await
        .context("calling sync_read_signal")?;
    if result.is_error == Some(true) {
        bail!("peer refused sync_read_signal");
    }
    let rows: Vec<TraceUsage> = result
        .into_typed::<Option<Vec<TraceUsage>>>()
        .context("parsing sync_read_signal response")?
        .unwrap_or_default();
    if rows.len() > 500 {
        bail!("peer returned too many usage rows");
    }
    if rows
        .iter()
        .any(|row| row.peer_cortex_id != peer_id || row.updated_at.is_empty())
    {
        bail!("peer returned usage rows with invalid ownership or cursor");
    }
    cx.merge_remote_usage(&rows)?;
    if let Some(last) = rows.last() {
        cx.set_federation_state(&cursor_key, &last.updated_at)?;
    }
    report.usage_rows = rows.len();
    Ok(())
}

pub(crate) fn parse_interval(value: &str) -> Result<Duration> {
    if value.trim().is_empty() {
        return Ok(Duration::from_secs(30));
    }
    let value = value.trim();
    let (number, multiplier) = if let Some(number) = value.strip_suffix("ms") {
        (number, 1_u64)
    } else if let Some(number) = value.strip_suffix('s') {
        (number, 1_000)
    } else if let Some(number) = value.strip_suffix('m') {
        (number, 60_000)
    } else if let Some(number) = value.strip_suffix('h') {
        (number, 3_600_000)
    } else {
        bail!("invalid federation interval {value:?}");
    };
    let amount: u64 = number
        .parse()
        .with_context(|| format!("invalid federation interval {value:?}"))?;
    let milliseconds = amount
        .checked_mul(multiplier)
        .ok_or_else(|| anyhow::anyhow!("federation interval is too large"))?;
    if milliseconds == 0 {
        bail!("federation interval must be positive");
    }
    Ok(Duration::from_millis(milliseconds))
}

fn record_health(
    cx: &Cortex,
    peer: &str,
    version: Option<&str>,
    error: Option<&anyhow::Error>,
) -> Result<()> {
    let key = format!("peer:{peer}:health");
    let mut health: PeerHealth =
        serde_json::from_str(&cx.federation_state(&key)?).unwrap_or_default();
    let now = Utc::now().to_rfc3339();
    if let Some(version) = version.filter(|version| !version.is_empty()) {
        health.version = version.to_owned();
        health.version_observed_at.clone_from(&now);
    }
    if let Some(error) = error {
        health.consecutive_failures += 1;
        health.last_error = Some(PeerError {
            reason: classify_error(error),
            observed_at: now,
        });
    } else {
        health.last_success = now;
        health.consecutive_failures = 0;
        health.last_error = None;
    }
    cx.set_federation_state(&key, &serde_json::to_string(&health)?)
}

fn classify_error(error: &anyhow::Error) -> String {
    let message = format!("{error:#}").to_ascii_lowercase();
    if message.contains("identity mismatch") || message.contains("signing key") {
        "identity_mismatch"
    } else if message.contains("unauthorized")
        || message.contains("http status 401")
        || message.contains("status: 401")
        || message.contains("status 401")
    {
        "auth"
    } else if message.contains("tls") || message.contains("certificate") {
        "network_tls"
    } else if message.contains("connection refused")
        || message.contains("client error (connect)")
        || message.contains("tcp connect error")
        || message.contains("connect error")
        || message.contains("error sending request for url")
    {
        "network_refused"
    } else if message.contains("timeout") || message.contains("deadline") {
        "network_timeout"
    } else if message.contains("dns") || message.contains("no such host") {
        "network_dns"
    } else if message.contains("unknown action") || message.contains("does not replay action") {
        "unknown_action"
    } else if message.contains("invalid trace id") {
        "invalid_trace_id"
    } else {
        "other"
    }
    .into()
}

fn is_zero(value: &i64) -> bool {
    *value == 0
}

fn is_zero_usize(value: &usize) -> bool {
    *value == 0
}

fn verify_and_pin_identity(cx: &Cortex, peer: &PeerEntry, identity: &PeerIdentity) -> Result<()> {
    if identity.version < MANIFEST_VERSION {
        bail!(
            "peer {:?} manifest version {} is below required version {}",
            peer.name,
            identity.version,
            MANIFEST_VERSION
        );
    }
    if identity.id.is_empty() {
        bail!("peer {:?} reported no stable cortex ID", peer.name);
    }
    if identity.id == cx.id {
        bail!("peer {:?} resolves to this cortex", peer.name);
    }

    let identity_key = format!("peer:{}:cortex_id", peer.name);
    let pinned_id = cx.federation_state(&identity_key)?;
    if !pinned_id.is_empty() && pinned_id != identity.id {
        bail!(
            "peer {:?} identity mismatch: pinned {}, advertised {}; reset the peer only if this replacement is intentional",
            peer.name,
            pinned_id,
            identity.id
        );
    }

    let key_key = format!("cortexkey:{}", identity.id);
    let pinned_key = cx.federation_state(&key_key)?;
    if !peer.pubkey.is_empty() {
        if identity.pubkey.is_empty() || !public_keys_equal(&peer.pubkey, &identity.pubkey) {
            bail!(
                "peer {:?} does not match its configured public-key pin",
                peer.name
            );
        }
        cx.set_federation_state(&key_key, &peer.pubkey)?;
    } else if !identity.pubkey.is_empty() {
        if !pinned_key.is_empty() && !public_keys_equal(&pinned_key, &identity.pubkey) {
            bail!("peer {:?} changed its pinned signing key", peer.name);
        }
        if pinned_key.is_empty() {
            cx.set_federation_state(&key_key, &identity.pubkey)?;
        }
    }
    if pinned_id.is_empty() {
        cx.set_federation_state(&identity_key, &identity.id)?;
    }
    Ok(())
}

fn public_keys_equal(left: &str, right: &str) -> bool {
    match (eventsig::parse_public(left), eventsig::parse_public(right)) {
        (Ok(left), Ok(right)) => left == right,
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const CORTEX_A: &str = "01KNJX4991ASW6NNKS9BQHNCGB";
    const CORTEX_B: &str = "01KPAA8VQNG3TKMCY1XJ0JJG0Z";

    #[test]
    fn detects_concurrency() {
        let a = BTreeMap::from([("a".into(), 2), ("b".into(), 1)]);
        let b = BTreeMap::from([("a".into(), 1), ("b".into(), 2)]);
        assert_eq!(compare(&a, &b), Relation::Concurrent);
    }

    #[test]
    fn replay_ignores_retired_name_key_when_stable_clocks_are_ordered() {
        let historical = BTreeMap::from([
            (CORTEX_A.into(), 10),
            (CORTEX_B.into(), 20),
            ("legacy-name".into(), 3),
        ]);
        let current = BTreeMap::from([(CORTEX_A.into(), 11), (CORTEX_B.into(), 21)]);

        assert_eq!(compare(&historical, &current), Relation::Concurrent);
        assert_eq!(compare_for_replay(&historical, &current), Relation::Before);
    }

    #[test]
    fn replay_preserves_stable_id_concurrency() {
        let left = BTreeMap::from([
            (CORTEX_A.into(), 11),
            (CORTEX_B.into(), 20),
            ("legacy-name".into(), 3),
        ]);
        let right = BTreeMap::from([(CORTEX_A.into(), 10), (CORTEX_B.into(), 21)]);

        assert_eq!(compare_for_replay(&left, &right), Relation::Concurrent);
    }

    #[test]
    fn replay_preserves_ambiguous_legacy_only_concurrency() {
        let legacy = BTreeMap::from([("legacy-name".into(), 3)]);
        let current = BTreeMap::from([(CORTEX_A.into(), 11)]);

        assert_eq!(compare_for_replay(&legacy, &current), Relation::Concurrent);
    }

    #[test]
    fn parses_scheduler_intervals_and_rejects_invalid_values() {
        assert_eq!(parse_interval("").unwrap(), Duration::from_secs(30));
        assert_eq!(parse_interval("250ms").unwrap(), Duration::from_millis(250));
        assert_eq!(parse_interval("2m").unwrap(), Duration::from_secs(120));
        assert!(parse_interval("0s").is_err());
        assert!(parse_interval("fast").is_err());
    }

    #[test]
    fn classifies_identity_and_network_failures_without_storing_raw_errors() {
        assert_eq!(
            classify_error(&anyhow::anyhow!("peer changed its pinned signing key")),
            "identity_mismatch"
        );
        assert_eq!(
            classify_error(&anyhow::anyhow!("connection refused by host")),
            "network_refused"
        );
        assert_eq!(
            classify_error(&anyhow::anyhow!(
                "error sending request for url (http://127.0.0.1:54014/mcp)"
            )),
            "network_refused"
        );
    }

    #[tokio::test]
    async fn reconciliation_restarts_a_finished_worker() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("local", temp.path()).unwrap();
        let path = temp.path().join("local");
        let cancellation = CancellationToken::new();
        let completed_task = tokio::spawn(async {});
        tokio::task::yield_now().await;
        assert!(completed_task.is_finished());
        let mut workers = HashMap::from([(
            "peer-a".to_owned(),
            PeerWorker {
                cancellation,
                task: completed_task,
            },
        )]);
        let config = FederationConfig {
            interval: "10ms".into(),
            peers: vec![PeerEntry {
                name: "peer-a".into(),
                endpoint: "http://127.0.0.1:1".into(),
                ..Default::default()
            }],
            ..Default::default()
        };

        reconcile_peer_workers(
            "local",
            &path,
            &config,
            Duration::from_millis(10),
            &mut workers,
        )
        .await;

        assert_eq!(workers.len(), 1);
        assert!(!workers["peer-a"].task.is_finished());
        stop_peer_workers(&mut workers).await;
    }
}
