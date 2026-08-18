use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
    fs::{self, File, OpenOptions},
    io::Write,
    path::{Component, Path, PathBuf},
};

use anyhow::{Context, Result, bail};
use base64::{Engine as _, engine::general_purpose::STANDARD as BASE64};
use chrono::{DateTime, Duration, Utc};
use ed25519_dalek::SigningKey;
use fs2::FileExt;
use rusqlite::{
    Connection, OpenFlags, OptionalExtension, Transaction, params, params_from_iter, types::Value,
};
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::{Digest, Sha256};

use crate::{
    config::{Config, CortexEntry},
    db,
    embedding::{self, HttpEmbedder},
    event::Event,
    eventsig,
    federation::{self, Relation},
    trace::{self, Trace},
};

pub const MANIFEST_VERSION: u32 = 2;
pub const MAX_SEARCH_QUERY_LEN: usize = 1000;
pub const ACCESS_KEY_ENV: &str = "NOEMA_MCP_KEY";
const PENDING_MUTATION_PREFIX: &str = "rust_pending_mutation:";

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Manifest {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub id: String,
    pub name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub purpose: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub owner: String,
    pub created: String,
    pub version: u32,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub access: Option<AccessConfig>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub federation: Option<FederationConfig>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub signing: Option<SigningConfig>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub watch: Option<WatchConfig>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub consolidation: Option<serde_yaml::Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub search: Option<SearchConfig>,
    #[serde(skip)]
    pub body: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct AccessConfig {
    #[serde(default)]
    pub shared_key_file: String,
    #[serde(default)]
    pub tls_cert_path: String,
    #[serde(default)]
    pub tls_key_path: String,
}

#[derive(Debug, Clone, Default, Deserialize)]
pub struct ConsolidationConfig {
    #[serde(default)]
    pub enabled: bool,
    #[serde(default)]
    pub cron: String,
    #[serde(default)]
    pub idle_minutes: i64,
    #[serde(default)]
    pub threshold_short: i64,
    #[serde(default)]
    pub window_hours: i64,
    #[serde(default)]
    pub llm_enabled: bool,
    #[serde(default)]
    pub auto_distillation_enabled: bool,
    #[serde(default)]
    pub model_tier: String,
    #[serde(default)]
    pub local_llm_endpoint: String,
    #[serde(default)]
    pub model_name: String,
    #[serde(default)]
    pub api_key_env: String,
    #[serde(default)]
    pub watchdog_timeout: String,
    #[serde(default)]
    pub graduation: Option<GraduationConfig>,
}

impl ConsolidationConfig {
    pub fn has_trigger(&self) -> bool {
        !self.cron.is_empty() || self.idle_minutes != 0 || self.threshold_short != 0
    }

    pub fn effective_model_tier(&self) -> &str {
        if self.model_tier.is_empty() {
            "large"
        } else {
            &self.model_tier
        }
    }

    pub fn effective_window(&self) -> std::time::Duration {
        let hours = if self.window_hours > 0 {
            self.window_hours as u64
        } else {
            24
        };
        std::time::Duration::from_secs(hours.saturating_mul(60 * 60))
    }
}

#[derive(Debug, Clone, Default, Deserialize)]
pub struct GraduationConfig {
    #[serde(default)]
    pub enabled: Option<bool>,
    #[serde(default)]
    pub min_age_days: i64,
    #[serde(default)]
    pub min_read_count: i64,
    #[serde(default)]
    pub require_unmodified: Option<bool>,
}

impl GraduationConfig {
    pub fn effective_enabled(&self) -> bool {
        self.enabled.unwrap_or(true)
    }

    pub fn effective_min_age(&self) -> std::time::Duration {
        let days = if self.min_age_days > 0 {
            self.min_age_days as u64
        } else {
            14
        };
        std::time::Duration::from_secs(days.saturating_mul(24 * 60 * 60))
    }

    pub fn effective_min_read_count(&self) -> i64 {
        if self.min_read_count > 0 {
            self.min_read_count
        } else {
            3
        }
    }

    pub fn effective_require_unmodified(&self) -> bool {
        self.require_unmodified.unwrap_or(true)
    }
}

impl Manifest {
    pub fn consolidation_config(&self) -> Result<Option<ConsolidationConfig>> {
        let config = self
            .consolidation
            .as_ref()
            .map(|value| {
                serde_yaml::from_value::<ConsolidationConfig>(value.clone())
                    .context("parsing consolidation configuration")
            })
            .transpose()?;
        if let Some(config) = &config
            && config.enabled
            && config.auto_distillation_enabled
        {
            if !config.llm_enabled {
                bail!(
                    "consolidation.auto_distillation_enabled requires consolidation.llm_enabled: true"
                );
            }
            if config.local_llm_endpoint.is_empty() {
                bail!(
                    "consolidation.auto_distillation_enabled requires consolidation.local_llm_endpoint to be set"
                );
            }
            if config.model_name.is_empty() {
                bail!(
                    "consolidation.auto_distillation_enabled requires consolidation.model_name to be set"
                );
            }
        }
        Ok(config)
    }

    pub fn resolved_embedding_endpoint(&self) -> Result<String> {
        if let Some(search) = &self.search
            && !search.embedding_endpoint.is_empty()
        {
            return Ok(search.embedding_endpoint.clone());
        }
        Ok(self
            .consolidation_config()?
            .map(|config| config.local_llm_endpoint)
            .unwrap_or_default())
    }

    pub fn resolved_embedding_api_key_env(&self) -> Result<String> {
        if let Some(search) = &self.search
            && !search.api_key_env.is_empty()
        {
            return Ok(search.api_key_env.clone());
        }
        Ok(self
            .consolidation_config()?
            .map(|config| config.api_key_env)
            .unwrap_or_default())
    }
}

#[derive(Clone, Default)]
pub struct AccessKey {
    pub value: String,
    pub source: String,
    pub path: PathBuf,
    pub fingerprint: String,
}

impl AccessKey {
    pub fn keyed(&self) -> bool {
        !self.value.is_empty()
    }

    pub fn env_override(&self) -> bool {
        self.source == "env" && !self.path.as_os_str().is_empty()
    }
}

pub fn load_access_key(dir: &Path, config: Option<&AccessConfig>) -> Result<AccessKey> {
    let configured_path = config
        .map(|access| access.shared_key_file.as_str())
        .filter(|path| !path.is_empty())
        .map(PathBuf::from)
        .map(|path| {
            if path.is_absolute() {
                path
            } else {
                dir.join(path)
            }
        })
        .unwrap_or_default();

    if let Some(raw) = std::env::var_os(ACCESS_KEY_ENV).filter(|value| !value.is_empty()) {
        let raw = raw
            .into_string()
            .map_err(|_| anyhow::anyhow!("{ACCESS_KEY_ENV} is not valid UTF-8"))?;
        let value = raw.trim().to_owned();
        if value.is_empty() {
            bail!("{ACCESS_KEY_ENV} is set but contains only whitespace");
        }
        return Ok(AccessKey {
            fingerprint: key_fingerprint(&value),
            value,
            source: "env".into(),
            path: configured_path,
        });
    }

    if configured_path.as_os_str().is_empty() {
        return Ok(AccessKey::default());
    }
    let value = load_sidecar_line(&configured_path, "access key file")?;
    Ok(AccessKey {
        fingerprint: key_fingerprint(&value),
        value,
        source: "file".into(),
        path: configured_path,
    })
}

pub fn resolve_tls_paths(dir: &Path, config: Option<&AccessConfig>) -> (PathBuf, PathBuf) {
    let resolve = |value: &str| {
        let path = PathBuf::from(value);
        if value.is_empty() || path.is_absolute() {
            path
        } else {
            dir.join(path)
        }
    };
    config
        .map(|access| {
            (
                resolve(&access.tls_cert_path),
                resolve(&access.tls_key_path),
            )
        })
        .unwrap_or_default()
}

pub fn key_fingerprint(value: &str) -> String {
    let digest = Sha256::digest(value.as_bytes());
    format!(
        "SHA256:{}",
        digest
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<Vec<_>>()
            .join(":")
    )
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct SigningConfig {
    #[serde(default)]
    pub public_key: String,
    #[serde(default)]
    pub private_key_file: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct FederationConfig {
    #[serde(default)]
    pub mode: String,
    #[serde(default)]
    pub peers: Vec<PeerEntry>,
    #[serde(default)]
    pub interval: String,
    #[serde(default)]
    pub verify: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct PeerEntry {
    pub name: String,
    pub endpoint: String,
    #[serde(default)]
    pub ca: String,
    #[serde(default)]
    pub mode: String,
    #[serde(default)]
    pub pubkey: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct WatchConfig {
    pub enabled: Option<bool>,
    #[serde(default)]
    pub debounce_ms: u64,
    pub auto_onboard: Option<bool>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct SearchConfig {
    #[serde(default)]
    pub semantic_enabled: bool,
    #[serde(default)]
    pub embedding_endpoint: String,
    #[serde(default)]
    pub embedding_model: String,
    #[serde(default)]
    pub api_key_env: String,
    #[serde(default)]
    pub default_mode: String,
    #[serde(default)]
    pub hybrid_weight: f64,
    #[serde(default)]
    pub max_chars: usize,
    #[serde(default)]
    pub embed_interval_seconds: u64,
}

impl SearchConfig {
    pub fn effective_default_mode(&self) -> &str {
        if self.default_mode.is_empty() {
            "lexical"
        } else {
            &self.default_mode
        }
    }

    pub fn effective_hybrid_weight(&self) -> f64 {
        if self.hybrid_weight == 0.0 {
            0.5
        } else {
            self.hybrid_weight.clamp(0.0, 1.0)
        }
    }

    pub fn effective_max_chars(&self) -> usize {
        if self.max_chars == 0 {
            32_000
        } else {
            self.max_chars
        }
    }
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct Row {
    pub id: String,
    pub title: String,
    #[serde(rename = "type")]
    pub trace_type: String,
    pub tier: String,
    pub author: String,
    pub origin: String,
    pub cortex_id: String,
    pub tags: Vec<String>,
    pub derived_from: Vec<String>,
    pub archived_at: String,
    pub trashed_at: String,
    pub created_at: String,
    pub updated_at: String,
    pub content_hash: String,
    pub source_locked: bool,
    pub source_hash: String,
}

#[derive(Debug)]
pub struct TraceIdExists {
    pub id: String,
    pub state: String,
}

impl fmt::Display for TraceIdExists {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        writeln!(
            formatter,
            "trace id {:?} already exists (currently {}).",
            self.id, self.state
        )?;
        writeln!(formatter, "fix one of:")?;
        writeln!(
            formatter,
            "  - vary the title (different slug -> different id)"
        )?;
        match self.state.as_str() {
            "trashed" => write!(
                formatter,
                "  - noema recover {}\n  - noema memory purge {}",
                self.id, self.id
            ),
            "archived" => write!(
                formatter,
                "  - noema unarchive {}\n  - noema memory purge {}",
                self.id, self.id
            ),
            "purged" => write!(
                formatter,
                "  - noema memory purge --hard {} (only this frees the slot)",
                self.id
            ),
            _ => write!(formatter, "  - read it first: noema get {}", self.id),
        }
    }
}

impl std::error::Error for TraceIdExists {}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct PromotionCandidate {
    pub id: String,
    pub tier: String,
    #[serde(rename = "type")]
    pub trace_type: String,
    pub read_count: i64,
    pub modify_count: i64,
    pub search_hit_count: i64,
    pub tier_votes: i64,
    pub derived_from_count: i64,
    pub source_count: i64,
    pub created_at: String,
}

#[derive(Debug, Clone, Default)]
pub struct DistilledTraceSpec {
    pub title: String,
    pub body: String,
    pub tags: Vec<String>,
    pub author: String,
    pub source_ids: Vec<String>,
    pub model_name: String,
    pub model_tier_profile: String,
    pub cohesion_confidence: f64,
}

#[derive(Debug, Clone, Default)]
pub struct ListOptions {
    pub trace_type: String,
    pub author: String,
    pub tag: String,
    pub origin: String,
    pub tiers: Vec<String>,
    pub archived: bool,
    pub trashed: bool,
    pub all: bool,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct EmbeddingStatus {
    pub model: String,
    pub embeddable: usize,
    pub embedded: usize,
    pub stale: usize,
    pub missing: usize,
}

#[derive(Debug, Clone, Default)]
pub struct EmbedBackfillOptions {
    pub force: bool,
    pub limit: usize,
    pub max_chars: usize,
    pub batch_size: usize,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct EmbedBackfillResult {
    pub considered: usize,
    pub embedded: usize,
}

#[derive(Debug, Clone, Default)]
pub struct SemanticOptions {
    pub model: String,
    pub limit: usize,
    pub include_archived: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct ScoredRow {
    #[serde(flatten)]
    pub row: Row,
    pub score: f64,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct PopularTrace {
    pub id: String,
    pub title: String,
    #[serde(rename = "type")]
    pub trace_type: String,
    pub tier: String,
    pub search_hits: i64,
    pub read_count: i64,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct TagSummary {
    pub tag: String,
    pub trace_count: i64,
    pub search_hits: i64,
    pub read_count: i64,
    pub modify_count: i64,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "PascalCase")]
pub struct TierStats {
    pub short: i64,
    pub mid: i64,
    pub long: i64,
    pub purged: i64,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "PascalCase")]
pub struct EngagementStats {
    pub total_reads: i64,
    pub total_search_hits: i64,
    pub total_modifies: i64,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "PascalCase")]
pub struct MidLineageBreakdown {
    pub no_sources: i64,
    pub single_source: i64,
    pub multi_source: i64,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "PascalCase")]
pub struct MidEngagementSnapshot {
    pub zero_engagement: i64,
    pub zero_engagement_older: i64,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct ConsolidationDay {
    pub date: String,
    pub success: i64,
    pub fail: i64,
    pub lost_election: i64,
    pub claim: i64,
    pub promote: i64,
    pub distill: i64,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct ConsolidationTotals {
    pub success: i64,
    pub fail: i64,
    pub lost_election: i64,
    pub claim: i64,
    pub promote: i64,
    pub distill: i64,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct ConsolidationActivity {
    pub since: String,
    pub since_start: String,
    pub daily: Vec<ConsolidationDay>,
    pub totals: ConsolidationTotals,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct PromotionStats {
    pub count: usize,
    pub p50: String,
    pub p95: String,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct PromotionLatency {
    pub short_to_mid: PromotionStats,
    pub mid_to_long: PromotionStats,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct OneSourceMidCount {
    pub current: i64,
    pub promoted_last_7d: i64,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct SyncResult {
    pub added: usize,
    pub updated: usize,
    pub recovered: usize,
    pub orphaned: usize,
    pub drifted: usize,
    pub drifted_ids: Vec<String>,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct EventBackfillResult {
    pub backfilled_ids: Vec<String>,
    pub skipped_ids: Vec<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CoordinationClaim {
    pub window_id: String,
    pub winner_id: String,
    pub timestamp: String,
}

pub struct Cortex {
    pub id: String,
    pub name: String,
    pub dir: PathBuf,
    pub manifest: Manifest,
    connection: Connection,
    force_source_lock: bool,
    signing_key: Option<SigningKey>,
    durability: DurabilityProfile,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum DurabilityProfile {
    Strong,
    Standard,
}

impl DurabilityProfile {
    fn parse(value: Option<&str>) -> Result<Self> {
        match value.unwrap_or("").trim().to_ascii_lowercase().as_str() {
            "" | "standard" => Ok(Self::Standard),
            "strong" => Ok(Self::Strong),
            value => {
                bail!("invalid NOEMA_DURABILITY value {value:?}; expected standard or strong")
            }
        }
    }

    fn from_environment() -> Result<Self> {
        Self::parse(std::env::var("NOEMA_DURABILITY").ok().as_deref())
    }

    fn name(self) -> &'static str {
        match self {
            Self::Strong => "strong",
            Self::Standard => "standard",
        }
    }
}

struct FileSnapshot {
    bytes: Vec<u8>,
    permissions: fs::Permissions,
}

#[derive(Serialize, Deserialize)]
struct PendingFileMutation {
    version: u32,
    kind: String,
    trace_id: String,
    #[serde(default)]
    relative_path: String,
    #[serde(default)]
    source_path: String,
    #[serde(default)]
    target_path: String,
    #[serde(default)]
    original_bytes: String,
    #[serde(default)]
    replacement_hash: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    original_mode: Option<u32>,
    #[serde(default)]
    original_readonly: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RecoveryStatus {
    Clean,
    Pending { records: usize },
    MalformedJournal { records: usize },
    UnreadableDatabase,
}

struct PendingMutationGuard {
    key: String,
    lock_path: PathBuf,
    lock_file: File,
}

impl PendingMutationGuard {
    fn cleanup(self) {
        let Self {
            lock_path,
            lock_file,
            ..
        } = self;
        drop(lock_file);
        let _ = fs::remove_file(lock_path);
    }
}

impl FileSnapshot {
    fn capture(path: &Path) -> Result<Self> {
        Ok(Self {
            bytes: fs::read(path)
                .with_context(|| format!("reading {} for rollback", path.display()))?,
            permissions: fs::metadata(path)
                .with_context(|| format!("reading permissions for {}", path.display()))?
                .permissions(),
        })
    }

    fn restore(self, path: &Path) -> Result<()> {
        trace::write_bytes_atomic(path, &self.bytes)?;
        fs::set_permissions(path, self.permissions)?;
        Ok(())
    }

    #[cfg(unix)]
    fn mode(&self) -> Option<u32> {
        use std::os::unix::fs::PermissionsExt;
        Some(self.permissions.mode())
    }

    #[cfg(not(unix))]
    fn mode(&self) -> Option<u32> {
        None
    }
}

fn bytes_hash(bytes: &[u8]) -> String {
    format!("sha256:{:x}", Sha256::digest(bytes))
}

fn trace_lock_name(trace_id: &str) -> String {
    format!("trace-{:x}.lock", Sha256::digest(trace_id.as_bytes()))
}

fn remove_created_replacement(path: &Path, replacement_hash: &str) -> Result<()> {
    match fs::read(path) {
        Ok(bytes) if bytes_hash(&bytes) == replacement_hash => {
            fs::remove_file(path)?;
            Ok(())
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Ok(_) => bail!(
            "refusing to remove independently changed trace {} during crash recovery",
            path.display()
        ),
        Err(error) => Err(error).context("reading pending trace creation"),
    }
}

fn remove_file_durable(path: &Path) -> Result<()> {
    fs::remove_file(path).with_context(|| format!("removing {}", path.display()))?;
    let directory = path
        .parent()
        .ok_or_else(|| anyhow::anyhow!("trace path has no parent directory"))?;
    trace::sync_directory(directory)
}

fn rename_trace_durable(source: &Path, target: &Path) -> Result<()> {
    fs::rename(source, target)
        .with_context(|| format!("moving {} to {}", source.display(), target.display()))?;
    let source_directory = source
        .parent()
        .ok_or_else(|| anyhow::anyhow!("trace source has no parent directory"))?;
    let target_directory = target
        .parent()
        .ok_or_else(|| anyhow::anyhow!("trace target has no parent directory"))?;
    trace::sync_directory(source_directory)?;
    if target_directory != source_directory {
        trace::sync_directory(target_directory)?;
    }
    Ok(())
}

fn restore_moved_file(source: &Path, target: &Path, file_hash: &str) -> Result<()> {
    match (fs::read(source), fs::read(target)) {
        (Ok(source_bytes), Err(target_error))
            if target_error.kind() == std::io::ErrorKind::NotFound
                && bytes_hash(&source_bytes) == file_hash =>
        {
            Ok(())
        }
        (Err(source_error), Ok(target_bytes))
            if source_error.kind() == std::io::ErrorKind::NotFound
                && bytes_hash(&target_bytes) == file_hash =>
        {
            rename_trace_durable(target, source)
        }
        (Ok(_), Err(target_error)) if target_error.kind() == std::io::ErrorKind::NotFound => {
            bail!("source trace changed independently before move recovery")
        }
        (Err(source_error), Ok(_)) if source_error.kind() == std::io::ErrorKind::NotFound => {
            bail!("target trace changed independently before move recovery")
        }
        (Ok(_), Ok(_)) => bail!("both source and target traces exist during move recovery"),
        (Ok(_), Err(target_error)) => Err(target_error).context("reading move recovery target"),
        (Err(source_error), Ok(_)) => Err(source_error).context("reading move recovery source"),
        (Err(source_error), Err(target_error)) => Err(source_error).context(format!(
            "neither move endpoint is readable; target error: {target_error}"
        )),
    }
}

fn validate_relative_trace_path(path: &Path) -> Result<()> {
    if !path
        .components()
        .all(|component| matches!(component, Component::Normal(_)))
        || path.extension().and_then(|extension| extension.to_str()) != Some("md")
        || !(path.starts_with("traces")
            || path.starts_with("archive/traces")
            || path.starts_with("trash/traces"))
    {
        bail!("invalid pending mutation trace path")
    }
    Ok(())
}

fn trace_id_from_path(path: &Path) -> Result<String> {
    let id = path
        .file_stem()
        .and_then(|value| value.to_str())
        .ok_or_else(|| anyhow::anyhow!("trace path has no valid UTF-8 file stem"))?;
    if !trace::is_valid_id(id) {
        bail!("trace path has an invalid trace ID")
    }
    Ok(id.to_owned())
}

pub fn inspect_recovery_status(dir: &Path) -> RecoveryStatus {
    inspect_recovery_status_inner(dir).unwrap_or(RecoveryStatus::UnreadableDatabase)
}

fn inspect_recovery_status_inner(dir: &Path) -> Result<RecoveryStatus> {
    let database_path = dir.join("db/noema.db");
    if !fs::metadata(&database_path)?.is_file() {
        bail!("cortex database is not a regular file")
    }
    let connection = Connection::open_with_flags(
        database_path,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_URI,
    )?;
    connection.busy_timeout(std::time::Duration::from_secs(5))?;
    let integrity: String = connection.query_row("PRAGMA quick_check(1)", [], |row| row.get(0))?;
    if integrity != "ok" {
        bail!("cortex database quick check failed")
    }
    let pattern = format!("{PENDING_MUTATION_PREFIX}*");
    let pending: Vec<(String, String)> = {
        let mut statement = connection
            .prepare("SELECT key,value FROM federation_state WHERE key GLOB ?1 ORDER BY key")?;
        statement
            .query_map([pattern], |row| Ok((row.get(0)?, row.get(1)?)))?
            .collect::<rusqlite::Result<_>>()?
    };
    if pending.is_empty() {
        return Ok(RecoveryStatus::Clean);
    }
    let records = pending.len();
    if pending
        .iter()
        .any(|(key, value)| !pending_record_is_well_formed(key, value))
    {
        return Ok(RecoveryStatus::MalformedJournal { records });
    }
    Ok(RecoveryStatus::Pending { records })
}

fn pending_record_is_well_formed(key: &str, value: &str) -> bool {
    let Some(record_id) = key.strip_prefix(PENDING_MUTATION_PREFIX) else {
        return false;
    };
    if ulid::Ulid::from_string(record_id).is_err() {
        return false;
    }
    let Ok(mutation) = serde_json::from_str::<PendingFileMutation>(value) else {
        return false;
    };
    if mutation.version != 1 || !trace::is_valid_id(&mutation.trace_id) {
        return false;
    }
    let path_matches = |value: &str| {
        let path = Path::new(value);
        validate_relative_trace_path(path).is_ok()
            && trace_id_from_path(path).is_ok_and(|id| id == mutation.trace_id)
    };
    match mutation.kind.as_str() {
        "replace" | "delete" => {
            path_matches(&mutation.relative_path) && BASE64.decode(&mutation.original_bytes).is_ok()
        }
        "create" => path_matches(&mutation.relative_path),
        "move" => path_matches(&mutation.source_path) && path_matches(&mutation.target_path),
        _ => false,
    }
}

#[cfg(debug_assertions)]
fn pause_after_filesystem_mutation_for_test() -> Result<()> {
    let Some(marker) = std::env::var_os("NOEMA_RUST_TEST_PAUSE_AFTER_FILESYSTEM_MUTATION") else {
        return Ok(());
    };
    fs::write(marker, b"filesystem mutation complete\n")?;
    loop {
        std::thread::park();
    }
}

#[cfg(not(debug_assertions))]
fn pause_after_filesystem_mutation_for_test() -> Result<()> {
    Ok(())
}

impl Cortex {
    pub fn create(name: &str, parent: &Path) -> Result<Manifest> {
        let root = parent.join(name);
        if root.exists() {
            bail!("cortex already exists at {}", root.display());
        }
        for relative in ["traces", "archive/traces", "trash/traces", "db"] {
            fs::create_dir_all(root.join(relative))?;
        }
        let manifest = Manifest {
            id: ulid::Ulid::new().to_string(),
            name: name.to_owned(),
            created: Utc::now().format("%Y-%m-%d").to_string(),
            version: MANIFEST_VERSION,
            ..Manifest::default()
        };
        write_manifest(&root, &manifest)?;
        fs::write(root.join("AGENTS.md"), agents_md(&manifest))?;
        let connection = db::open(&root)?;
        drop(connection);
        Ok(manifest)
    }

    pub fn open(name: impl Into<String>, dir: impl Into<PathBuf>) -> Result<Self> {
        let name = name.into();
        let dir = dir.into();
        let durability = DurabilityProfile::from_environment()?;
        for relative in ["traces", "archive/traces", "trash/traces"] {
            fs::create_dir_all(dir.join(relative))?;
        }
        let manifest = read_manifest(&dir)?;
        if manifest.version > 0 && manifest.version < MANIFEST_VERSION {
            bail!(
                "cortex {:?} is at manifest version {} but this binary requires version {}",
                name,
                manifest.version,
                MANIFEST_VERSION
            );
        }
        let connection = db::open(&dir)?;
        let signing_key = load_signing_key(&dir, &manifest)?;
        let mut cortex = Self {
            id: manifest.id.clone(),
            name,
            dir,
            manifest,
            connection,
            force_source_lock: false,
            signing_key,
            durability,
        };
        cortex.recover_pending_mutations()?;
        cortex.rebuild_fts_if_stale()?;
        let days = Config::load()
            .map(|cfg| {
                if cfg.trash_days == 0 {
                    30
                } else {
                    cfg.trash_days
                }
            })
            .unwrap_or(30);
        let _ = cortex.purge_expired(days);
        Ok(cortex)
    }

    fn begin_pending_replace(
        &self,
        path: &Path,
        snapshot: &FileSnapshot,
        replacement: &[u8],
    ) -> Result<PendingMutationGuard> {
        let relative_path = self.relative_trace_path(path)?;
        let pending = PendingFileMutation {
            version: 1,
            kind: "replace".into(),
            trace_id: trace_id_from_path(path)?,
            relative_path,
            source_path: String::new(),
            target_path: String::new(),
            original_bytes: BASE64.encode(&snapshot.bytes),
            replacement_hash: bytes_hash(replacement),
            original_mode: snapshot.mode(),
            original_readonly: snapshot.permissions.readonly(),
        };
        self.begin_pending_mutation(&pending)
    }

    fn acquire_trace_mutation_lock(&self, path: &Path) -> Result<File> {
        let trace_id = trace_id_from_path(path)?;
        let lock_directory = self.pending_mutation_lock_directory();
        fs::create_dir_all(&lock_directory)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&lock_directory, fs::Permissions::from_mode(0o700))?;
        }
        let path_lock_path = lock_directory.join(trace_lock_name(&trace_id));
        let path_lock_file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(&path_lock_path)?;
        path_lock_file.try_lock_exclusive().with_context(|| {
            format!(
                "trace mutation is already in progress ({})",
                path_lock_path.display()
            )
        })?;
        Ok(path_lock_file)
    }

    fn begin_pending_create(
        &self,
        path: &Path,
        replacement: &[u8],
    ) -> Result<PendingMutationGuard> {
        let pending = PendingFileMutation {
            version: 1,
            kind: "create".into(),
            trace_id: trace_id_from_path(path)?,
            relative_path: self.relative_trace_path(path)?,
            source_path: String::new(),
            target_path: String::new(),
            original_bytes: String::new(),
            replacement_hash: bytes_hash(replacement),
            original_mode: None,
            original_readonly: false,
        };
        self.begin_pending_mutation(&pending)
    }

    fn begin_pending_move(&self, source: &Path, target: &Path) -> Result<PendingMutationGuard> {
        let pending = PendingFileMutation {
            version: 1,
            kind: "move".into(),
            trace_id: trace_id_from_path(source)?,
            relative_path: String::new(),
            source_path: self.relative_trace_path(source)?,
            target_path: self.relative_trace_path(target)?,
            original_bytes: String::new(),
            replacement_hash: bytes_hash(
                &fs::read(source)
                    .with_context(|| format!("reading {} before move", source.display()))?,
            ),
            original_mode: None,
            original_readonly: false,
        };
        self.begin_pending_mutation(&pending)
    }

    fn begin_pending_delete(
        &self,
        path: &Path,
        snapshot: &FileSnapshot,
    ) -> Result<PendingMutationGuard> {
        let pending = PendingFileMutation {
            version: 1,
            kind: "delete".into(),
            trace_id: trace_id_from_path(path)?,
            relative_path: self.relative_trace_path(path)?,
            source_path: String::new(),
            target_path: String::new(),
            original_bytes: BASE64.encode(&snapshot.bytes),
            replacement_hash: String::new(),
            original_mode: snapshot.mode(),
            original_readonly: snapshot.permissions.readonly(),
        };
        self.begin_pending_mutation(&pending)
    }

    fn begin_pending_mutation(
        &self,
        pending: &PendingFileMutation,
    ) -> Result<PendingMutationGuard> {
        let id = ulid::Ulid::new().to_string();
        let key = format!("{PENDING_MUTATION_PREFIX}{id}");
        let lock_directory = self.pending_mutation_lock_directory();
        fs::create_dir_all(&lock_directory)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&lock_directory, fs::Permissions::from_mode(0o700))?;
        }
        let lock_path = lock_directory.join(format!("{id}.lock"));
        let lock_file = OpenOptions::new()
            .create_new(true)
            .read(true)
            .write(true)
            .open(&lock_path)?;
        lock_file.try_lock_exclusive()?;
        let insert = self.connection.execute(
            "INSERT INTO federation_state(key,value) VALUES (?1,?2)",
            params![key, serde_json::to_string(&pending)?],
        );
        if let Err(error) = insert {
            drop(lock_file);
            let _ = fs::remove_file(&lock_path);
            return Err(error.into());
        }
        Ok(PendingMutationGuard {
            key,
            lock_path,
            lock_file,
        })
    }

    fn finish_pending_replace<T>(
        &self,
        guard: PendingMutationGuard,
        snapshot: FileSnapshot,
        path: &Path,
        result: Result<T>,
    ) -> Result<T> {
        let Err(error) = result else {
            guard.cleanup();
            return result;
        };
        if let Err(rollback_error) = snapshot.restore(path) {
            guard.cleanup();
            return Err(error.context(format!(
                "also failed to restore {} after mutation error: {rollback_error}",
                path.display()
            )));
        }
        let clear = self
            .connection
            .execute("DELETE FROM federation_state WHERE key=?1", [&guard.key]);
        guard.cleanup();
        match clear {
            Ok(_) => Err(error),
            Err(clear_error) => Err(error.context(format!(
                "filesystem rollback succeeded but clearing its recovery record failed: {clear_error}"
            ))),
        }
    }

    fn finish_pending_create<T>(
        &self,
        guard: PendingMutationGuard,
        path: &Path,
        replacement_hash: &str,
        result: Result<T>,
    ) -> Result<T> {
        let Err(error) = result else {
            guard.cleanup();
            return result;
        };
        if let Err(rollback_error) = remove_created_replacement(path, replacement_hash) {
            guard.cleanup();
            return Err(error.context(format!(
                "also failed to remove {} after mutation error: {rollback_error}",
                path.display()
            )));
        }
        let clear = self
            .connection
            .execute("DELETE FROM federation_state WHERE key=?1", [&guard.key]);
        guard.cleanup();
        match clear {
            Ok(_) => Err(error),
            Err(clear_error) => Err(error.context(format!(
                "filesystem rollback succeeded but clearing its recovery record failed: {clear_error}"
            ))),
        }
    }

    fn finish_pending_move<T>(
        &self,
        guard: PendingMutationGuard,
        source: &Path,
        target: &Path,
        file_hash: &str,
        result: Result<T>,
    ) -> Result<T> {
        let Err(error) = result else {
            guard.cleanup();
            return result;
        };
        if let Err(rollback_error) = restore_moved_file(source, target, file_hash) {
            guard.cleanup();
            return Err(error.context(format!(
                "also failed to move {} back to {}: {rollback_error}",
                target.display(),
                source.display()
            )));
        }
        let clear = self
            .connection
            .execute("DELETE FROM federation_state WHERE key=?1", [&guard.key]);
        guard.cleanup();
        match clear {
            Ok(_) => Err(error),
            Err(clear_error) => Err(error.context(format!(
                "filesystem rollback succeeded but clearing its recovery record failed: {clear_error}"
            ))),
        }
    }

    fn finish_pending_delete<T>(
        &self,
        guard: PendingMutationGuard,
        snapshot: FileSnapshot,
        path: &Path,
        result: Result<T>,
    ) -> Result<T> {
        let Err(error) = result else {
            guard.cleanup();
            return result;
        };
        if let Err(rollback_error) = snapshot.restore(path) {
            guard.cleanup();
            return Err(error.context(format!(
                "also failed to restore deleted trace {}: {rollback_error}",
                path.display()
            )));
        }
        let clear = self
            .connection
            .execute("DELETE FROM federation_state WHERE key=?1", [&guard.key]);
        guard.cleanup();
        match clear {
            Ok(_) => Err(error),
            Err(clear_error) => Err(error.context(format!(
                "filesystem rollback succeeded but clearing its recovery record failed: {clear_error}"
            ))),
        }
    }

    fn replace_trace_transactionally<F>(
        &self,
        path: &Path,
        trace: &Trace,
        database: F,
    ) -> Result<()>
    where
        F: FnOnce(&str) -> Result<()>,
    {
        if self.durability == DurabilityProfile::Standard {
            trace.write_preserving_updated_compatible(path)?;
            return database("");
        }
        let _trace_lock = self.acquire_trace_mutation_lock(path)?;
        let snapshot = FileSnapshot::capture(path)?;
        let replacement = trace.encoded()?;
        let pending = self.begin_pending_replace(path, &snapshot, &replacement)?;
        if let Err(error) = trace.write_preserving_updated(path) {
            return self.finish_pending_replace(pending, snapshot, path, Err(error));
        }
        if let Err(error) = pause_after_filesystem_mutation_for_test() {
            return self.finish_pending_replace(pending, snapshot, path, Err(error));
        }
        let database_result = database(&pending.key);
        self.finish_pending_replace(pending, snapshot, path, database_result)
    }

    fn create_trace_transactionally<F>(&self, path: &Path, trace: &Trace, database: F) -> Result<()>
    where
        F: FnOnce(&str) -> Result<()>,
    {
        if self.durability == DurabilityProfile::Standard {
            trace.write_preserving_updated_compatible(path)?;
            let result = database("");
            if result.is_err() {
                let _ = fs::remove_file(path);
            }
            return result;
        }
        let _trace_lock = self.acquire_trace_mutation_lock(path)?;
        let replacement = trace.encoded()?;
        let replacement_hash = bytes_hash(&replacement);
        let pending = self.begin_pending_create(path, &replacement)?;
        if let Err(error) = trace.write_preserving_updated(path) {
            return self.finish_pending_create(pending, path, &replacement_hash, Err(error));
        }
        if let Err(error) = pause_after_filesystem_mutation_for_test() {
            return self.finish_pending_create(pending, path, &replacement_hash, Err(error));
        }
        let database_result = database(&pending.key);
        self.finish_pending_create(pending, path, &replacement_hash, database_result)
    }

    fn move_trace_transactionally<F>(&self, source: &Path, target: &Path, database: F) -> Result<()>
    where
        F: FnOnce(Option<&str>) -> Result<()>,
    {
        if source == target {
            return database(None);
        }
        if self.durability == DurabilityProfile::Standard {
            fs::rename(source, target)
                .with_context(|| format!("moving {} to {}", source.display(), target.display()))?;
            return database(None);
        }
        let _trace_lock = self.acquire_trace_mutation_lock(source)?;
        let file_hash = bytes_hash(
            &fs::read(source)
                .with_context(|| format!("reading {} before move", source.display()))?,
        );
        let pending = self.begin_pending_move(source, target)?;
        if let Err(error) = rename_trace_durable(source, target) {
            return self.finish_pending_move(pending, source, target, &file_hash, Err(error));
        }
        if let Err(error) = pause_after_filesystem_mutation_for_test() {
            return self.finish_pending_move(pending, source, target, &file_hash, Err(error));
        }
        let database_result = database(Some(&pending.key));
        self.finish_pending_move(pending, source, target, &file_hash, database_result)
    }

    fn delete_trace_transactionally<F>(&self, path: &Path, database: F) -> Result<()>
    where
        F: FnOnce(Option<&str>) -> Result<()>,
    {
        if self.durability == DurabilityProfile::Standard {
            if path.exists() {
                fs::remove_file(path).with_context(|| format!("removing {}", path.display()))?;
            }
            return database(None);
        }
        let _trace_lock = self.acquire_trace_mutation_lock(path)?;
        if !path.exists() {
            return database(None);
        }
        let snapshot = FileSnapshot::capture(path)?;
        let pending = self.begin_pending_delete(path, &snapshot)?;
        if let Err(error) = remove_file_durable(path) {
            return self.finish_pending_delete(pending, snapshot, path, Err(error));
        }
        if let Err(error) = pause_after_filesystem_mutation_for_test() {
            return self.finish_pending_delete(pending, snapshot, path, Err(error));
        }
        let database_result = database(Some(&pending.key));
        self.finish_pending_delete(pending, snapshot, path, database_result)
    }

    fn clear_pending_in_transaction(tx: &Transaction<'_>, key: &str) -> Result<()> {
        if key.is_empty() {
            return Ok(());
        }
        tx.execute("DELETE FROM federation_state WHERE key=?1", [key])?;
        Ok(())
    }

    pub fn durability_profile(&self) -> &'static str {
        self.durability.name()
    }

    fn recover_pending_mutations(&mut self) -> Result<()> {
        let pattern = format!("{PENDING_MUTATION_PREFIX}*");
        let pending: Vec<(String, String)> = {
            let mut statement = self
                .connection
                .prepare("SELECT key,value FROM federation_state WHERE key GLOB ?1 ORDER BY key")?;
            statement
                .query_map([pattern], |row| Ok((row.get(0)?, row.get(1)?)))?
                .collect::<rusqlite::Result<_>>()?
        };
        if pending.is_empty() {
            return Ok(());
        }
        let lock_directory = self.pending_mutation_lock_directory();
        fs::create_dir_all(&lock_directory)?;
        for (key, value) in pending {
            let id = key
                .strip_prefix(PENDING_MUTATION_PREFIX)
                .ok_or_else(|| anyhow::anyhow!("invalid pending mutation key"))?;
            ulid::Ulid::from_string(id).context("invalid pending mutation ID")?;
            let lock_path = lock_directory.join(format!("{id}.lock"));
            let lock_file = OpenOptions::new()
                .create(true)
                .truncate(false)
                .read(true)
                .write(true)
                .open(&lock_path)?;
            match lock_file.try_lock_exclusive() {
                Ok(()) => {}
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => continue,
                Err(error) => return Err(error).context("locking pending mutation recovery"),
            }
            let mutation: PendingFileMutation =
                serde_json::from_str(&value).context("parsing pending mutation recovery record")?;
            if !trace::is_valid_id(&mutation.trace_id) {
                bail!("pending mutation has an invalid trace ID")
            }
            let path_lock_path = lock_directory.join(trace_lock_name(&mutation.trace_id));
            let path_lock_file = OpenOptions::new()
                .create(true)
                .truncate(false)
                .read(true)
                .write(true)
                .open(&path_lock_path)?;
            match path_lock_file.try_lock_exclusive() {
                Ok(()) => {}
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => continue,
                Err(error) => return Err(error).context("locking pending trace recovery"),
            }
            match mutation.kind.as_str() {
                "replace" => self.recover_pending_replace(&mutation)?,
                "create" => self.recover_pending_create(&mutation)?,
                "move" => self.recover_pending_move(&mutation)?,
                "delete" => self.recover_pending_delete(&mutation)?,
                kind => bail!("unsupported pending mutation kind {kind:?}"),
            }
            self.connection
                .execute("DELETE FROM federation_state WHERE key=?1", [&key])?;
            drop(path_lock_file);
            drop(lock_file);
            let _ = fs::remove_file(lock_path);
        }
        Ok(())
    }

    fn recover_pending_replace(&self, mutation: &PendingFileMutation) -> Result<()> {
        if mutation.version != 1 {
            bail!(
                "unsupported pending mutation record version {}",
                mutation.version
            );
        }
        let path = self.resolve_relative_trace_path(&mutation.relative_path)?;
        if trace_id_from_path(&path)? != mutation.trace_id {
            bail!("pending replacement path does not match its trace ID")
        }
        let original = BASE64
            .decode(&mutation.original_bytes)
            .context("decoding pending mutation backup")?;
        match fs::read(&path) {
            Ok(current) if current == original => {}
            Ok(current) if bytes_hash(&current) == mutation.replacement_hash => {
                trace::write_bytes_atomic(&path, &original)?;
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                trace::write_bytes_atomic(&path, &original)?;
            }
            Ok(_) => bail!(
                "refusing to overwrite independently changed trace {} during crash recovery",
                path.display()
            ),
            Err(error) => return Err(error).context("reading pending mutation target"),
        }
        let mut permissions = fs::metadata(&path)?.permissions();
        permissions.set_readonly(mutation.original_readonly);
        #[cfg(unix)]
        if let Some(mode) = mutation.original_mode {
            use std::os::unix::fs::PermissionsExt;
            permissions.set_mode(mode);
        }
        fs::set_permissions(&path, permissions)?;
        Ok(())
    }

    fn recover_pending_create(&self, mutation: &PendingFileMutation) -> Result<()> {
        if mutation.version != 1 {
            bail!(
                "unsupported pending mutation record version {}",
                mutation.version
            );
        }
        let path = self.resolve_relative_trace_path(&mutation.relative_path)?;
        if trace_id_from_path(&path)? != mutation.trace_id {
            bail!("pending creation path does not match its trace ID")
        }
        remove_created_replacement(&path, &mutation.replacement_hash)
    }

    fn recover_pending_move(&self, mutation: &PendingFileMutation) -> Result<()> {
        if mutation.version != 1 {
            bail!(
                "unsupported pending mutation record version {}",
                mutation.version
            );
        }
        let source = self.resolve_relative_trace_path(&mutation.source_path)?;
        let target = self.resolve_relative_trace_path(&mutation.target_path)?;
        if trace_id_from_path(&source)? != mutation.trace_id
            || trace_id_from_path(&target)? != mutation.trace_id
        {
            bail!("pending move paths do not match their trace ID")
        }
        restore_moved_file(&source, &target, &mutation.replacement_hash)
    }

    fn recover_pending_delete(&self, mutation: &PendingFileMutation) -> Result<()> {
        if mutation.version != 1 {
            bail!(
                "unsupported pending mutation record version {}",
                mutation.version
            );
        }
        let path = self.resolve_relative_trace_path(&mutation.relative_path)?;
        if trace_id_from_path(&path)? != mutation.trace_id {
            bail!("pending deletion path does not match its trace ID")
        }
        let original = BASE64
            .decode(&mutation.original_bytes)
            .context("decoding pending deletion backup")?;
        match fs::read(&path) {
            Ok(current) if current == original => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                trace::write_bytes_atomic(&path, &original)?;
            }
            Ok(_) => bail!(
                "refusing to overwrite independently recreated trace {} during crash recovery",
                path.display()
            ),
            Err(error) => return Err(error).context("reading pending deletion path"),
        }
        let mut permissions = fs::metadata(&path)?.permissions();
        permissions.set_readonly(mutation.original_readonly);
        #[cfg(unix)]
        if let Some(mode) = mutation.original_mode {
            use std::os::unix::fs::PermissionsExt;
            permissions.set_mode(mode);
        }
        fs::set_permissions(path, permissions)?;
        Ok(())
    }

    fn relative_trace_path(&self, path: &Path) -> Result<String> {
        let relative = path
            .strip_prefix(&self.dir)
            .with_context(|| format!("trace path {} is outside the cortex", path.display()))?;
        validate_relative_trace_path(relative)?;
        relative
            .to_str()
            .map(str::to_owned)
            .ok_or_else(|| anyhow::anyhow!("trace path is not valid UTF-8"))
    }

    fn resolve_relative_trace_path(&self, relative: &str) -> Result<PathBuf> {
        let relative = Path::new(relative);
        validate_relative_trace_path(relative)?;
        Ok(self.dir.join(relative))
    }

    fn pending_mutation_lock_directory(&self) -> PathBuf {
        self.dir.join("db/pending-mutations")
    }

    pub fn resolve(name_override: Option<&str>) -> Result<Self> {
        let mut config = Config::load()?;
        let selected = name_override
            .map(str::to_owned)
            .or_else(|| std::env::var("NOEMA_CORTEX").ok())
            .filter(|name| !name.is_empty())
            .or_else(|| (!config.default.is_empty()).then(|| config.default.clone()))
            .or_else(|| {
                (config.cortexes.len() == 1).then(|| config.cortexes.keys().next().unwrap().clone())
            })
            .ok_or_else(|| anyhow::anyhow!("no default cortex set; run `noema use <name>`"))?;
        if config.default.is_empty() && config.cortexes.len() == 1 {
            config.default = selected.clone();
            config.save()?;
        }
        let entry = config.cortexes.get(&selected).ok_or_else(|| {
            anyhow::anyhow!(
                "unknown cortex {selected:?} — run `noema init --name {selected}` first"
            )
        })?;
        Self::open(selected, &entry.path)
    }

    pub fn register_created(name: &str, parent: &Path, manifest: &Manifest) -> Result<PathBuf> {
        let path = parent.join(name).canonicalize()?;
        let mut config = Config::load()?;
        config.cortexes.insert(
            name.to_owned(),
            CortexEntry {
                path: path.clone(),
                id: manifest.id.clone(),
            },
        );
        if config.default.is_empty() {
            config.default = name.to_owned();
        }
        config.save()?;
        Ok(path)
    }

    pub fn set_force_source_lock(&mut self, force: bool) {
        self.force_source_lock = force;
    }

    pub fn traces_dir(&self) -> PathBuf {
        self.dir.join("traces")
    }

    pub fn archive_dir(&self) -> PathBuf {
        self.dir.join("archive/traces")
    }

    pub fn trash_dir(&self) -> PathBuf {
        self.dir.join("trash/traces")
    }

    pub fn trace_file(&self, id: &str, archived: bool) -> PathBuf {
        if archived {
            self.archive_dir()
        } else {
            self.traces_dir()
        }
        .join(format!("{id}.md"))
    }

    pub fn file_path(&self, row: &Row) -> PathBuf {
        if !row.trashed_at.is_empty() {
            self.trash_dir().join(format!("{}.md", row.id))
        } else {
            self.trace_file(&row.id, !row.archived_at.is_empty())
        }
    }

    pub fn add(&self, trace: &mut Trace) -> Result<()> {
        trace.validate()?;
        if trace.frontmatter.origin.is_empty() {
            trace.frontmatter.origin = self.name.clone();
        }
        if trace.frontmatter.tier.is_empty() {
            trace.frontmatter.tier = "short".into();
        }
        trace.frontmatter.content_hash = trace::content_hash(&trace.body);
        if let Some((content_hash, state)) = self.trace_id_collision(&trace.frontmatter.id)? {
            if content_hash == trace.frontmatter.content_hash {
                return Ok(());
            }
            return Err(TraceIdExists {
                id: trace.frontmatter.id.clone(),
                state,
            }
            .into());
        }
        let path = self.trace_file(&trace.frontmatter.id, false);
        trace.frontmatter.updated = trace::now_rfc3339();
        if path.exists() {
            return self.replace_trace_transactionally(&path, trace, |pending_key| {
                self.insert_trace_with_pending(trace, true, Some(pending_key))
            });
        }
        self.create_trace_transactionally(&path, trace, |pending_key| {
            self.insert_trace_with_pending(trace, true, Some(pending_key))
        })
    }

    fn trace_id_collision(&self, id: &str) -> Result<Option<(String, String)>> {
        let existing: Option<(String, String, String, String)> = self
            .connection
            .query_row(
                "SELECT COALESCE(content_hash,''),COALESCE(archived_at,''),COALESCE(trashed_at,''),COALESCE(purged_at,'') FROM traces WHERE id=?1",
                [id],
                |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
            )
            .optional()?;
        Ok(existing.map(|(hash, archived, trashed, purged)| {
            let state = if !purged.is_empty() {
                "purged"
            } else if !trashed.is_empty() {
                "trashed"
            } else if !archived.is_empty() {
                "archived"
            } else {
                "active"
            };
            (hash, state.into())
        }))
    }

    fn insert_trace_with_pending(
        &self,
        trace: &Trace,
        emit: bool,
        pending_key: Option<&str>,
    ) -> Result<()> {
        let f = &trace.frontmatter;
        let tx = self.connection.unchecked_transaction()?;
        tx.execute(
            "INSERT INTO traces (id,title,type,tier,author,origin,cortex_id,created_at,updated_at,content_hash,source_locked,source_hash)
             VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12)",
            params![f.id, f.title, f.trace_type, trace.effective_tier(), f.author, f.origin, self.id, f.created, f.updated, f.content_hash, i64::from(f.source_locked), nullable(&f.source_hash)],
        )?;
        insert_tags(&tx, &f.id, &f.tags)?;
        insert_lineage(&tx, &f.id, &f.derived_from)?;
        insert_fts(&tx, &f.id, &f.title, &trace.body, &f.tags)?;
        if emit {
            self.emit_event(&tx, "create", &f.id, &f.created, trace_snapshot(trace))?;
        }
        if let Some(pending_key) = pending_key {
            Self::clear_pending_in_transaction(&tx, pending_key)?;
        }
        tx.commit()?;
        Ok(())
    }

    pub fn get(&self, id: &str) -> Result<Row> {
        if !trace::is_valid_id(id) {
            bail!("invalid trace ID");
        }
        let mut row = self.connection.query_row(
            "SELECT id,title,type,tier,author,origin,cortex_id,archived_at,trashed_at,created_at,updated_at,content_hash,source_locked,source_hash FROM traces WHERE id=?1",
            [id],
            scan_row,
        )?;
        row.tags = self.tags_for(id)?;
        row.derived_from = self.lineage_for(id)?;
        Ok(row)
    }

    pub fn get_trace(&self, id: &str) -> Result<(Row, Trace)> {
        let row = self.get(id)?;
        let trace = Trace::parse_file(&self.file_path(&row))?;
        Ok((row, trace))
    }

    pub fn list(&self, options: &ListOptions) -> Result<Vec<Row>> {
        let (sql, values) = list_query(options, false, None)?;
        self.query_rows(&sql, values)
    }

    pub fn search(&self, query: &str, options: &ListOptions) -> Result<Vec<Row>> {
        if query.len() > MAX_SEARCH_QUERY_LEN {
            bail!("search query too long");
        }
        let fts = sanitize_fts5_query(query);
        if fts.trim().is_empty() {
            return Ok(Vec::new());
        }
        let (sql, values) = list_query(options, true, Some(fts))?;
        self.query_rows(&sql, values)
    }

    pub fn top_searched_traces(&self, limit: usize) -> Result<Vec<PopularTrace>> {
        let limit = if limit == 0 { 10 } else { limit };
        let mut statement = self.connection.prepare(
            "SELECT t.id,t.title,t.type,t.tier,
                    COALESCE(SUM(u.search_hit_count),0) AS hits,
                    COALESCE(SUM(u.read_count),0) AS reads
             FROM traces t
             LEFT JOIN trace_usage u ON u.trace_id=t.id
             WHERE t.archived_at IS NULL
               AND t.trashed_at IS NULL
               AND t.purged_at IS NULL
             GROUP BY t.id
             HAVING hits>0 OR reads>0
             ORDER BY hits DESC,reads DESC,t.id ASC
             LIMIT ?1",
        )?;
        Ok(statement
            .query_map([limit as i64], |row| {
                Ok(PopularTrace {
                    id: row.get(0)?,
                    title: row.get(1)?,
                    trace_type: row.get(2)?,
                    tier: row.get(3)?,
                    search_hits: row.get(4)?,
                    read_count: row.get(5)?,
                })
            })?
            .collect::<rusqlite::Result<Vec<_>>>()?)
    }

    pub fn tag_activity(&self, limit: usize) -> Result<Vec<TagSummary>> {
        let limit = if limit == 0 { 20 } else { limit };
        let mut statement = self.connection.prepare(
            "SELECT tt.tag,
                    COUNT(DISTINCT t.id) AS trace_count,
                    COALESCE(SUM(u.search_hit_count),0) AS hits,
                    COALESCE(SUM(u.read_count),0) AS reads,
                    COALESCE(SUM(u.modify_count),0) AS mods
             FROM trace_tags tt
             JOIN traces t ON t.id=tt.trace_id
             LEFT JOIN trace_usage u ON u.trace_id=t.id
             WHERE t.archived_at IS NULL
               AND t.trashed_at IS NULL
               AND t.purged_at IS NULL
             GROUP BY tt.tag
             ORDER BY hits DESC,reads DESC,mods DESC,tt.tag ASC
             LIMIT ?1",
        )?;
        Ok(statement
            .query_map([limit as i64], |row| {
                Ok(TagSummary {
                    tag: row.get(0)?,
                    trace_count: row.get(1)?,
                    search_hits: row.get(2)?,
                    read_count: row.get(3)?,
                    modify_count: row.get(4)?,
                })
            })?
            .collect::<rusqlite::Result<Vec<_>>>()?)
    }

    pub fn tier_stats(&self) -> Result<TierStats> {
        let mut stats = TierStats::default();
        let mut statement = self.connection.prepare(
            "SELECT tier,COUNT(*) FROM traces
             WHERE archived_at IS NULL AND trashed_at IS NULL AND purged_at IS NULL
             GROUP BY tier",
        )?;
        let rows = statement.query_map([], |row| {
            Ok((row.get::<_, String>(0)?, row.get::<_, i64>(1)?))
        })?;
        for row in rows {
            let (tier, count) = row?;
            match tier.as_str() {
                "short" => stats.short = count,
                "mid" => stats.mid = count,
                "long" => stats.long = count,
                _ => {}
            }
        }
        stats.purged = self.connection.query_row(
            "SELECT COUNT(*) FROM traces WHERE purged_at IS NOT NULL",
            [],
            |row| row.get(0),
        )?;
        Ok(stats)
    }

    pub fn engagement_stats(&self) -> Result<EngagementStats> {
        Ok(self.connection.query_row(
            "SELECT COALESCE(SUM(u.read_count),0),COALESCE(SUM(u.search_hit_count),0),COALESCE(SUM(u.modify_count),0)
             FROM trace_usage u JOIN traces t ON t.id=u.trace_id
             WHERE t.archived_at IS NULL AND t.trashed_at IS NULL AND t.purged_at IS NULL",
            [],
            |row| Ok(EngagementStats {
                total_reads: row.get(0)?,
                total_search_hits: row.get(1)?,
                total_modifies: row.get(2)?,
            }),
        )?)
    }

    pub fn mid_lineage_breakdown(&self) -> Result<MidLineageBreakdown> {
        Ok(self.connection.query_row(
            "SELECT
               COALESCE(SUM(CASE WHEN COALESCE(l.n,0)=0 THEN 1 ELSE 0 END),0),
               COALESCE(SUM(CASE WHEN COALESCE(l.n,0)=1 THEN 1 ELSE 0 END),0),
               COALESCE(SUM(CASE WHEN COALESCE(l.n,0)>=2 THEN 1 ELSE 0 END),0)
             FROM traces t
             LEFT JOIN (SELECT trace_id,COUNT(*) n FROM trace_lineage GROUP BY trace_id) l ON l.trace_id=t.id
             WHERE t.tier='mid' AND t.archived_at IS NULL AND t.trashed_at IS NULL AND t.purged_at IS NULL",
            [],
            |row| Ok(MidLineageBreakdown {
                no_sources: row.get(0)?,
                single_source: row.get(1)?,
                multi_source: row.get(2)?,
            }),
        )?)
    }

    pub fn mid_engagement_snapshot(&self, older_than: Duration) -> Result<MidEngagementSnapshot> {
        let cutoff = (Utc::now() - older_than).to_rfc3339_opts(chrono::SecondsFormat::Secs, true);
        Ok(self.connection.query_row(
            "SELECT COUNT(*),COALESCE(SUM(CASE WHEN t.created_at<=?1 THEN 1 ELSE 0 END),0)
             FROM traces t
             LEFT JOIN (
               SELECT trace_id,SUM(read_count) reads,SUM(search_hit_count) hits,SUM(modify_count) mods
               FROM trace_usage GROUP BY trace_id
             ) u ON u.trace_id=t.id
             WHERE t.tier='mid' AND t.archived_at IS NULL AND t.trashed_at IS NULL AND t.purged_at IS NULL
               AND COALESCE(u.reads,0)=0 AND COALESCE(u.hits,0)=0 AND COALESCE(u.mods,0)=0",
            [cutoff],
            |row| Ok(MidEngagementSnapshot {
                zero_engagement: row.get(0)?,
                zero_engagement_older: row.get(1)?,
            }),
        )?)
    }

    pub fn consolidation_activity(&self, since: Duration) -> Result<ConsolidationActivity> {
        let mut output = ConsolidationActivity {
            since: format_duration_label(since),
            since_start: "0001-01-01T00:00:00Z".into(),
            daily: Vec::new(),
            ..Default::default()
        };
        let actions = [
            "consolidation_success",
            "consolidation_fail",
            "consolidation_claim",
            "promote",
            "consolidate",
        ];
        let mut sql = String::from(
            "SELECT substr(timestamp,1,10) AS date,action,
                    COALESCE(json_extract(data,'$.reason'),'') AS reason,
                    COUNT(*)
             FROM events
             WHERE action IN (?1,?2,?3,?4,?5)",
        );
        let cutoff = if since > Duration::zero() {
            let cutoff = Utc::now() - since;
            let formatted = cutoff.to_rfc3339_opts(chrono::SecondsFormat::Secs, true);
            output.since_start = formatted.clone();
            sql.push_str(" AND timestamp>=?6");
            Some(formatted)
        } else {
            None
        };
        sql.push_str(" GROUP BY date,action,reason ORDER BY date");

        let mut statement = self.connection.prepare(&sql)?;
        let mut by_date: BTreeMap<String, ConsolidationDay> = BTreeMap::new();
        let mut rows = if let Some(cutoff) = cutoff {
            statement.query(params![
                actions[0], actions[1], actions[2], actions[3], actions[4], cutoff
            ])?
        } else {
            statement.query(params![
                actions[0], actions[1], actions[2], actions[3], actions[4]
            ])?
        };
        while let Some(row) = rows.next()? {
            let date: String = row.get(0)?;
            let action: String = row.get(1)?;
            let reason: String = row.get(2)?;
            let count: i64 = row.get(3)?;
            let day = by_date
                .entry(date.clone())
                .or_insert_with(|| ConsolidationDay {
                    date,
                    ..Default::default()
                });
            match action.as_str() {
                "consolidation_success" => {
                    day.success += count;
                    output.totals.success += count;
                }
                "consolidation_fail"
                    if matches!(
                        reason.as_str(),
                        "peer_outranked" | "no_winner_at_recheck" | "context_canceled"
                    ) =>
                {
                    day.lost_election += count;
                    output.totals.lost_election += count;
                }
                "consolidation_fail" => {
                    day.fail += count;
                    output.totals.fail += count;
                }
                "consolidation_claim" => {
                    day.claim += count;
                    output.totals.claim += count;
                }
                "promote" => {
                    day.promote += count;
                    output.totals.promote += count;
                }
                "consolidate" => {
                    day.distill += count;
                    output.totals.distill += count;
                }
                _ => {}
            }
        }
        output.daily = by_date.into_values().collect();
        Ok(output)
    }

    pub fn promotion_latency(&self) -> Result<PromotionLatency> {
        #[derive(Clone)]
        struct Promotion {
            timestamp: DateTime<Utc>,
            from: String,
            to: String,
            created: DateTime<Utc>,
        }

        let mut statement = self.connection.prepare(
            "SELECT e.trace_id,e.timestamp,
                    COALESCE(json_extract(e.data,'$.from'),''),
                    COALESCE(json_extract(e.data,'$.to'),''),t.created_at
             FROM events e
             JOIN traces t ON t.id=e.trace_id
             WHERE e.action='promote'
             ORDER BY e.trace_id,e.timestamp",
        )?;
        let mut rows = statement.query([])?;
        let mut by_trace: BTreeMap<String, Vec<Promotion>> = BTreeMap::new();
        while let Some(row) = rows.next()? {
            let timestamp: String = row.get(1)?;
            let created: String = row.get(4)?;
            let (Ok(timestamp), Ok(created)) = (
                DateTime::parse_from_rfc3339(&timestamp),
                DateTime::parse_from_rfc3339(&created),
            ) else {
                continue;
            };
            by_trace.entry(row.get(0)?).or_default().push(Promotion {
                timestamp: timestamp.with_timezone(&Utc),
                from: row.get(2)?,
                to: row.get(3)?,
                created: created.with_timezone(&Utc),
            });
        }

        let mut short_to_mid = Vec::new();
        let mut mid_to_long = Vec::new();
        for promotions in by_trace.into_values() {
            let mut mid_entry = None;
            for promotion in promotions {
                if promotion.from == "short" && promotion.to == "mid" {
                    short_to_mid.push(promotion.timestamp - promotion.created);
                    mid_entry = Some(promotion.timestamp);
                }
                if promotion.from == "mid" && promotion.to == "long" {
                    mid_to_long.push(promotion.timestamp - mid_entry.unwrap_or(promotion.created));
                }
            }
        }
        Ok(PromotionLatency {
            short_to_mid: summarize_durations(&mut short_to_mid),
            mid_to_long: summarize_durations(&mut mid_to_long),
        })
    }

    pub fn one_source_mid_count(&self) -> Result<OneSourceMidCount> {
        let current = self.connection.query_row(
            "SELECT COUNT(*)
             FROM traces t
             JOIN (SELECT trace_id,COUNT(*) AS n FROM trace_lineage GROUP BY trace_id) l
               ON l.trace_id=t.id
             WHERE t.tier='mid'
               AND t.archived_at IS NULL
               AND t.trashed_at IS NULL
               AND t.purged_at IS NULL
               AND l.n=1",
            [],
            |row| row.get(0),
        )?;
        let cutoff =
            (Utc::now() - Duration::days(7)).to_rfc3339_opts(chrono::SecondsFormat::Secs, true);
        let promoted_last_7d = self.connection.query_row(
            "SELECT COUNT(DISTINCT e.trace_id)
             FROM events e
             JOIN (SELECT trace_id,COUNT(*) AS n FROM trace_lineage GROUP BY trace_id) l
               ON l.trace_id=e.trace_id
             WHERE e.action='promote'
               AND COALESCE(json_extract(e.data,'$.to'),'')='mid'
               AND e.timestamp>=?1
               AND l.n=1",
            [cutoff],
            |row| row.get(0),
        )?;
        Ok(OneSourceMidCount {
            current,
            promoted_last_7d,
        })
    }

    pub fn embedding_status(&self, model: &str) -> Result<EmbeddingStatus> {
        let embeddable: i64 = self.connection.query_row(
            "SELECT COUNT(*) FROM traces WHERE trashed_at IS NULL",
            [],
            |row| row.get(0),
        )?;
        let with_row: i64 = self.connection.query_row(
            "SELECT COUNT(*) FROM traces t JOIN trace_embeddings te ON te.trace_id=t.id WHERE t.trashed_at IS NULL",
            [],
            |row| row.get(0),
        )?;
        let embedded: i64 = self.connection.query_row(
            "SELECT COUNT(*) FROM traces t JOIN trace_embeddings te ON te.trace_id=t.id
             WHERE t.trashed_at IS NULL AND te.embedding_model=?1 AND te.source_hash=t.content_hash",
            [model],
            |row| row.get(0),
        )?;
        Ok(EmbeddingStatus {
            model: model.to_owned(),
            embeddable: embeddable as usize,
            embedded: embedded as usize,
            stale: (with_row - embedded) as usize,
            missing: (embeddable - with_row) as usize,
        })
    }

    pub async fn embed_backfill(
        &mut self,
        embedder: &HttpEmbedder,
        model: &str,
        options: &EmbedBackfillOptions,
    ) -> Result<EmbedBackfillResult> {
        if model.is_empty() {
            bail!("embedding model is empty");
        }
        let batch_size = if options.batch_size == 0 {
            64
        } else {
            options.batch_size
        };
        let max_chars = if options.max_chars == 0 {
            32_000
        } else {
            options.max_chars
        };
        let mut sql = String::from(
            "SELECT t.id,COALESCE(t.content_hash,'') FROM traces t
             LEFT JOIN trace_embeddings te ON te.trace_id=t.id
             WHERE t.trashed_at IS NULL",
        );
        let mut values = Vec::new();
        if !options.force {
            sql.push_str(
                " AND (te.trace_id IS NULL OR te.embedding_model!=? OR te.source_hash!=t.content_hash OR t.content_hash IS NULL OR t.content_hash='')",
            );
            values.push(Value::Text(model.to_owned()));
        }
        sql.push_str(" ORDER BY t.created_at");
        if options.limit > 0 {
            sql.push_str(" LIMIT ?");
            values.push(Value::Integer(options.limit as i64));
        }
        let candidates = {
            let mut statement = self.connection.prepare(&sql)?;
            statement
                .query_map(params_from_iter(values), |row| {
                    Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?))
                })?
                .collect::<rusqlite::Result<Vec<_>>>()?
        };
        let mut result = EmbedBackfillResult {
            considered: candidates.len(),
            ..Default::default()
        };
        let updated_at = trace::now_rfc3339();
        for batch in candidates.chunks(batch_size) {
            let mut inputs = Vec::with_capacity(batch.len());
            let mut ids = Vec::with_capacity(batch.len());
            let mut hashes = Vec::with_capacity(batch.len());
            for (id, indexed_hash) in batch {
                let Ok((row, parsed)) = self.get_trace(id) else {
                    continue;
                };
                let source_hash = if indexed_hash.is_empty() {
                    let hash = trace::content_hash(&parsed.body);
                    self.connection.execute(
                        "UPDATE traces SET content_hash=?1 WHERE id=?2 AND (content_hash IS NULL OR content_hash='')",
                        params![hash, id],
                    )?;
                    hash
                } else {
                    indexed_hash.clone()
                };
                inputs.push(embedding::text(&row.title, &parsed.body, max_chars));
                ids.push(id.clone());
                hashes.push(source_hash);
            }
            if inputs.is_empty() {
                continue;
            }
            let vectors = embedder
                .embed(model, &inputs)
                .await
                .context("embed batch")?;
            if vectors.len() != inputs.len() {
                bail!(
                    "embedder returned {} vectors for {} inputs",
                    vectors.len(),
                    inputs.len()
                );
            }
            let tx = self.connection.unchecked_transaction()?;
            for ((id, source_hash), mut vector) in ids.iter().zip(&hashes).zip(vectors) {
                embedding::normalize(&mut vector);
                tx.execute(
                    "INSERT INTO trace_embeddings(trace_id,embedding_model,dim,embedding,source_hash,updated_at)
                     VALUES (?1,?2,?3,?4,?5,?6)
                     ON CONFLICT(trace_id) DO UPDATE SET
                       embedding_model=excluded.embedding_model,
                       dim=excluded.dim,
                       embedding=excluded.embedding,
                       source_hash=excluded.source_hash,
                       updated_at=excluded.updated_at",
                    params![id, model, vector.len() as i64, embedding::encode(&vector), source_hash, updated_at],
                )?;
                result.embedded += 1;
            }
            tx.commit()?;
        }
        Ok(result)
    }

    pub async fn semantic_search(
        &mut self,
        embedder: &HttpEmbedder,
        query: &str,
        options: &SemanticOptions,
    ) -> Result<Vec<ScoredRow>> {
        let query = query.trim();
        if query.is_empty() {
            return Ok(Vec::new());
        }
        if query.len() > MAX_SEARCH_QUERY_LEN {
            bail!("search query too long");
        }
        if options.model.is_empty() {
            bail!("embedding model is empty");
        }
        let mut vectors = embedder
            .embed(&options.model, &[query.to_owned()])
            .await
            .context("embedding query")?;
        if vectors.len() != 1 || vectors[0].is_empty() {
            bail!("embedder returned no query vector");
        }
        embedding::normalize(&mut vectors[0]);
        let candidates =
            self.load_embedded_candidates(&options.model, options.include_archived, "")?;
        Ok(top_k_cosine(
            &vectors[0],
            candidates,
            effective_limit(options.limit),
        ))
    }

    pub fn semantic_similar(
        &self,
        trace_id: &str,
        options: &SemanticOptions,
    ) -> Result<Vec<ScoredRow>> {
        let query = self.source_vector(trace_id, &options.model)?;
        let candidates =
            self.load_embedded_candidates(&options.model, options.include_archived, trace_id)?;
        Ok(top_k_cosine(
            &query,
            candidates,
            effective_limit(options.limit),
        ))
    }

    pub async fn hybrid_search(
        &mut self,
        embedder: &HttpEmbedder,
        query: &str,
        options: &SemanticOptions,
        weight: f64,
    ) -> Result<Vec<ScoredRow>> {
        let mut semantic_options = options.clone();
        semantic_options.limit = usize::MAX;
        let semantic = self
            .semantic_search(embedder, query, &semantic_options)
            .await?;
        if query.trim().is_empty() {
            return Ok(Vec::new());
        }
        let lexical = self.search(
            query,
            &ListOptions {
                all: options.include_archived,
                ..Default::default()
            },
        )?;
        Ok(rrf_fuse(
            lexical,
            semantic,
            weight,
            effective_limit(options.limit),
        ))
    }

    pub fn hybrid_similar(
        &self,
        trace_id: &str,
        options: &SemanticOptions,
        weight: f64,
    ) -> Result<Vec<ScoredRow>> {
        let mut semantic_options = options.clone();
        semantic_options.limit = usize::MAX;
        let semantic = self.semantic_similar(trace_id, &semantic_options)?;
        let lexical = self.find_similar(trace_id, 50, options.include_archived)?;
        Ok(rrf_fuse(
            lexical,
            semantic,
            weight,
            effective_limit(options.limit),
        ))
    }

    pub fn find_similar(
        &self,
        trace_id: &str,
        limit: usize,
        include_archived: bool,
    ) -> Result<Vec<Row>> {
        let (source, parsed) = self.get_trace(trace_id)?;
        let terms = distinctive_terms(
            &format!("{} {} {}", source.title, parsed.body, source.tags.join(" ")),
            25,
        );
        if terms.is_empty() {
            return Ok(Vec::new());
        }
        let mut query = terms.join(" OR ");
        if query.len() > MAX_SEARCH_QUERY_LEN {
            query.truncate(MAX_SEARCH_QUERY_LEN);
            query = query.trim_end_matches([' ', 'O', 'R']).to_owned();
        }
        let mut rows = self.search(
            &query,
            &ListOptions {
                all: include_archived,
                ..Default::default()
            },
        )?;
        rows.retain(|row| row.id != trace_id);
        rows.truncate(if limit == 0 { 10 } else { limit });
        Ok(rows)
    }

    fn source_vector(&self, trace_id: &str, model: &str) -> Result<Vec<f32>> {
        if model.is_empty() {
            bail!("embedding model is empty");
        }
        let blob = self
            .connection
            .query_row(
                "SELECT embedding FROM trace_embeddings WHERE trace_id=?1 AND embedding_model=?2",
                params![trace_id, model],
                |row| row.get::<_, Vec<u8>>(0),
            )
            .optional()?
            .with_context(|| {
                format!(
                    "trace {trace_id} has no {model} embedding yet (run: noema embeddings backfill)"
                )
            })?;
        embedding::decode(&blob)
    }

    fn load_embedded_candidates(
        &self,
        model: &str,
        include_archived: bool,
        exclude_id: &str,
    ) -> Result<Vec<(Row, Vec<f32>)>> {
        let columns = "t.id,t.title,t.type,t.tier,t.author,t.origin,t.cortex_id,t.archived_at,t.trashed_at,t.created_at,t.updated_at,t.content_hash,t.source_locked,t.source_hash";
        let mut sql = format!(
            "SELECT {columns},te.embedding FROM trace_embeddings te
             JOIN traces t ON t.id=te.trace_id
             WHERE te.embedding_model=? AND t.trashed_at IS NULL"
        );
        let mut values = vec![Value::Text(model.to_owned())];
        if !include_archived {
            sql.push_str(" AND t.archived_at IS NULL");
        }
        if !exclude_id.is_empty() {
            sql.push_str(" AND t.id!=?");
            values.push(Value::Text(exclude_id.to_owned()));
        }
        let mut candidates = {
            let mut statement = self.connection.prepare(&sql)?;
            statement
                .query_map(params_from_iter(values), |raw| {
                    Ok((scan_row(raw)?, raw.get::<_, Vec<u8>>(14)?))
                })?
                .collect::<rusqlite::Result<Vec<_>>>()?
        };
        let mut output = Vec::with_capacity(candidates.len());
        for (mut row, blob) in candidates.drain(..) {
            let Ok(vector) = embedding::decode(&blob) else {
                continue;
            };
            if !vector.iter().all(|value| value.is_finite()) {
                continue;
            }
            row.tags = self.tags_for(&row.id)?;
            row.derived_from = self.lineage_for(&row.id)?;
            output.push((row, vector));
        }
        Ok(output)
    }

    pub fn short_tier_count(&self) -> Result<i64> {
        Ok(self.connection.query_row(
            "SELECT COUNT(*) FROM traces
             WHERE tier='short'
               AND archived_at IS NULL
               AND trashed_at IS NULL
               AND purged_at IS NULL",
            [],
            |row| row.get(0),
        )?)
    }

    pub fn promotion_candidates(
        &self,
        tier: &str,
        window: std::time::Duration,
    ) -> Result<Vec<PromotionCandidate>> {
        let window = chrono::Duration::from_std(window).context("promotion window is too large")?;
        let cutoff = (Utc::now() - window).to_rfc3339_opts(chrono::SecondsFormat::Secs, true);
        let mut statement = self.connection.prepare(
            "SELECT
                t.id,
                t.tier,
                t.type,
                COALESCE(u.total_reads, 0),
                COALESCE(u.total_modifies, 0),
                COALESCE(u.total_search_hits, 0),
                t.tier_votes,
                COALESCE(v.n, 0),
                COALESCE(s.n, 0),
                t.created_at
             FROM traces t
             LEFT JOIN (
                SELECT trace_id,
                       SUM(read_count) AS total_reads,
                       SUM(modify_count) AS total_modifies,
                       SUM(search_hit_count) AS total_search_hits
                FROM trace_usage
                GROUP BY trace_id
             ) u ON u.trace_id=t.id
             LEFT JOIN v_derived_from_count v ON v.trace_id=t.id
             LEFT JOIN (
                SELECT trace_id, COUNT(*) AS n
                FROM trace_lineage
                GROUP BY trace_id
             ) s ON s.trace_id=t.id
             WHERE t.tier=?1
               AND t.archived_at IS NULL
               AND t.trashed_at IS NULL
               AND t.purged_at IS NULL
               AND t.created_at>=?2
               AND t.id!=''
             ORDER BY t.created_at DESC",
        )?;
        let rows = statement.query_map(params![tier, cutoff], |row| {
            Ok(PromotionCandidate {
                id: row.get(0)?,
                tier: row.get(1)?,
                trace_type: row.get(2)?,
                read_count: row.get(3)?,
                modify_count: row.get(4)?,
                search_hit_count: row.get(5)?,
                tier_votes: row.get(6)?,
                derived_from_count: row.get(7)?,
                source_count: row.get(8)?,
                created_at: row.get(9)?,
            })
        })?;
        Ok(rows.collect::<rusqlite::Result<Vec<_>>>()?)
    }

    pub fn llm_candidates(&self, window: std::time::Duration) -> Result<Vec<PromotionCandidate>> {
        let window = chrono::Duration::from_std(window).context("LLM window is too large")?;
        let cutoff = (Utc::now() - window).to_rfc3339_opts(chrono::SecondsFormat::Secs, true);
        let mut statement = self.connection.prepare(
            "SELECT
                t.id,
                t.tier,
                t.type,
                COALESCE(u.total_reads, 0),
                COALESCE(u.total_modifies, 0),
                COALESCE(u.total_search_hits, 0),
                t.tier_votes,
                COALESCE(v.n, 0),
                COALESCE(s.n, 0),
                t.created_at
             FROM traces t
             LEFT JOIN (
                SELECT trace_id,
                       SUM(read_count) AS total_reads,
                       SUM(modify_count) AS total_modifies,
                       SUM(search_hit_count) AS total_search_hits
                FROM trace_usage
                GROUP BY trace_id
             ) u ON u.trace_id=t.id
             LEFT JOIN v_derived_from_count v ON v.trace_id=t.id
             LEFT JOIN (
                SELECT trace_id, COUNT(*) AS n
                FROM trace_lineage
                GROUP BY trace_id
             ) s ON s.trace_id=t.id
             WHERE t.tier='short'
               AND t.archived_at IS NULL
               AND t.trashed_at IS NULL
               AND t.purged_at IS NULL
               AND t.created_at>=?1
               AND t.id!=''
               AND t.id NOT IN (
                   SELECT je.value
                   FROM events e, json_each(json_extract(e.data, '$.source_ids')) je
                   WHERE e.action='consolidate'
               )
             ORDER BY t.created_at DESC",
        )?;
        let rows = statement.query_map([cutoff], |row| {
            Ok(PromotionCandidate {
                id: row.get(0)?,
                tier: row.get(1)?,
                trace_type: row.get(2)?,
                read_count: row.get(3)?,
                modify_count: row.get(4)?,
                search_hit_count: row.get(5)?,
                tier_votes: row.get(6)?,
                derived_from_count: row.get(7)?,
                source_count: row.get(8)?,
                created_at: row.get(9)?,
            })
        })?;
        Ok(rows.collect::<rusqlite::Result<Vec<_>>>()?)
    }

    pub fn create_distilled_trace(&self, spec: DistilledTraceSpec) -> Result<String> {
        if spec.title.is_empty() {
            bail!("distilled trace: title is required");
        }
        if spec.body.is_empty() {
            bail!("distilled trace: body is required");
        }
        if spec.source_ids.len() < 2 {
            bail!("distilled trace requires >= 2 source IDs");
        }
        for id in &spec.source_ids {
            self.get(id)
                .with_context(|| format!("source trace not found: {id}"))?;
        }

        let mut trace = Trace::new(spec.title, "observation", spec.author, spec.tags, spec.body);
        trace.frontmatter.tier = "mid".into();
        trace.frontmatter.derived_from = spec.source_ids.clone();
        self.add(&mut trace)?;

        let mut data = serde_json::Map::new();
        data.insert("source_ids".into(), json!(spec.source_ids));
        data.insert("distilled_id".into(), json!(trace.frontmatter.id));
        if !spec.model_name.is_empty() {
            data.insert("model_name".into(), json!(spec.model_name));
        }
        if !spec.model_tier_profile.is_empty() {
            data.insert("model_tier_profile".into(), json!(spec.model_tier_profile));
        }
        if spec.cohesion_confidence != 0.0 {
            data.insert(
                "cohesion_confidence".into(),
                json!(spec.cohesion_confidence),
            );
        }
        let tx = self.connection.unchecked_transaction()?;
        self.emit_event(
            &tx,
            "consolidate",
            &trace.frontmatter.id,
            &trace::now_rfc3339(),
            serde_json::Value::Object(data),
        )?;
        tx.commit()?;
        Ok(trace.frontmatter.id)
    }

    pub fn graduation_candidates(
        &self,
        min_age: std::time::Duration,
    ) -> Result<Vec<PromotionCandidate>> {
        let min_age =
            chrono::Duration::from_std(min_age).context("graduation minimum age is too large")?;
        let cutoff = (Utc::now() - min_age).to_rfc3339_opts(chrono::SecondsFormat::Secs, true);
        let mut statement = self.connection.prepare(
            "SELECT
                t.id,
                t.tier,
                t.type,
                COALESCE(u.total_reads, 0),
                COALESCE(u.total_modifies, 0),
                COALESCE(u.total_search_hits, 0),
                t.tier_votes,
                COALESCE(v.n, 0),
                COALESCE(s.n, 0),
                t.created_at
             FROM traces t
             LEFT JOIN (
                SELECT trace_id,
                       SUM(read_count) AS total_reads,
                       SUM(modify_count) AS total_modifies,
                       SUM(search_hit_count) AS total_search_hits
                FROM trace_usage
                GROUP BY trace_id
             ) u ON u.trace_id=t.id
             LEFT JOIN v_derived_from_count v ON v.trace_id=t.id
             LEFT JOIN (
                SELECT trace_id, COUNT(*) AS n
                FROM trace_lineage
                GROUP BY trace_id
             ) s ON s.trace_id=t.id
             WHERE t.tier='mid'
               AND t.archived_at IS NULL
               AND t.trashed_at IS NULL
               AND t.purged_at IS NULL
               AND t.created_at<=?1
               AND t.id!=''
             ORDER BY t.created_at ASC",
        )?;
        let rows = statement.query_map([cutoff], |row| {
            Ok(PromotionCandidate {
                id: row.get(0)?,
                tier: row.get(1)?,
                trace_type: row.get(2)?,
                read_count: row.get(3)?,
                modify_count: row.get(4)?,
                search_hit_count: row.get(5)?,
                tier_votes: row.get(6)?,
                derived_from_count: row.get(7)?,
                source_count: row.get(8)?,
                created_at: row.get(9)?,
            })
        })?;
        Ok(rows.collect::<rusqlite::Result<Vec<_>>>()?)
    }

    pub fn last_mutation_time(&self) -> Result<Option<DateTime<Utc>>> {
        let timestamp: Option<String> =
            self.connection
                .query_row("SELECT MAX(timestamp) FROM events", [], |row| row.get(0))?;
        timestamp
            .map(|timestamp| {
                DateTime::parse_from_rfc3339(&timestamp)
                    .map(|value| value.with_timezone(&Utc))
                    .context("parsing latest event timestamp")
            })
            .transpose()
    }

    pub fn has_consolidation_success_after(&self, cutoff: DateTime<Utc>) -> Result<bool> {
        Ok(self.connection.query_row(
            "SELECT EXISTS(
                SELECT 1 FROM events
                WHERE action='consolidation_success' AND timestamp>?1
             )",
            [cutoff.to_rfc3339_opts(chrono::SecondsFormat::Secs, true)],
            |row| row.get(0),
        )?)
    }

    pub fn bump_search_hits(&self, rows: &[Row]) {
        for row in rows.iter().take(3).filter(|row| row.tier != "long") {
            let now = trace::now_rfc3339();
            if let Err(error) = self.connection.execute(
                "INSERT INTO trace_usage(trace_id,peer_cortex_id,read_count,modify_count,search_hit_count,last_read_at,updated_at)
                 VALUES (?1,?2,0,0,1,NULL,?3)
                 ON CONFLICT(trace_id,peer_cortex_id) DO UPDATE SET
                   search_hit_count=search_hit_count+1,
                   updated_at=excluded.updated_at",
                params![row.id, self.id, now],
            ) {
                eprintln!(
                    "search-hit instrumentation warning for trace {}: {error}",
                    row.id
                );
            }
        }
    }

    fn query_rows(&self, sql: &str, values: Vec<Value>) -> Result<Vec<Row>> {
        let mut statement = self.connection.prepare(sql)?;
        let mapped = statement.query_map(params_from_iter(values), scan_row)?;
        let mut rows = mapped.collect::<rusqlite::Result<Vec<_>>>()?;
        drop(statement);
        for row in &mut rows {
            row.tags = self.tags_for(&row.id)?;
            row.derived_from = self.lineage_for(&row.id)?;
        }
        Ok(rows)
    }

    pub fn update_trace(&self, id: &str, trace: &mut Trace, actor_agent: bool) -> Result<()> {
        let existing = self.get(id)?;
        self.check_source_lock(&existing)?;
        if existing.tier == "long" {
            bail!("long-term trace is immutable");
        }
        trace.frontmatter.id = id.to_owned();
        trace.frontmatter.origin = existing.origin.clone();
        trace.frontmatter.created = existing.created_at.clone();
        trace.frontmatter.tier = existing.tier.clone();
        trace.frontmatter.updated = trace::now_rfc3339();
        trace.frontmatter.content_hash = trace::content_hash(&trace.body);
        trace.validate()?;
        let path = self.file_path(&existing);
        self.replace_trace_transactionally(&path, trace, |pending_key| {
            let tx = self.connection.unchecked_transaction()?;
            let f = &trace.frontmatter;
            tx.execute(
                "UPDATE traces SET title=?1,type=?2,author=?3,updated_at=?4,content_hash=?5,source_locked=?6,source_hash=?7 WHERE id=?8",
                params![f.title, f.trace_type, f.author, f.updated, f.content_hash, i64::from(f.source_locked), nullable(&f.source_hash), id],
            )?;
            replace_tags(&tx, id, &f.tags)?;
            replace_lineage(&tx, id, &f.derived_from)?;
            upsert_fts(&tx, id, &f.title, &trace.body, &f.tags)?;
            self.emit_event(&tx, "update", id, &f.updated, trace_snapshot(trace))?;
            Self::clear_pending_in_transaction(&tx, pending_key)?;
            tx.commit()?;
            Ok(())
        })?;
        if actor_agent {
            self.bump_usage(id, false, true)?;
        }
        Ok(())
    }

    pub fn update_from_file(&self, id: &str) -> Result<()> {
        let existing = self.get(id)?;
        self.check_source_lock(&existing)?;
        if existing.tier == "long" {
            bail!("long-term trace is immutable");
        }
        let mut trace = Trace::parse_file(&self.file_path(&existing))?;
        trace.frontmatter.tier = existing.tier;
        trace.frontmatter.updated = trace::now_rfc3339();
        trace.frontmatter.content_hash = trace::content_hash(&trace.body);
        trace.validate()?;

        let tx = self.connection.unchecked_transaction()?;
        let f = &trace.frontmatter;
        tx.execute(
            "UPDATE traces SET title=?1,type=?2,author=?3,origin=?4,updated_at=?5,content_hash=?6,source_locked=?7,source_hash=?8 WHERE id=?9",
            params![f.title, f.trace_type, f.author, f.origin, f.updated, f.content_hash, i64::from(f.source_locked), nullable(&f.source_hash), id],
        )?;
        replace_tags(&tx, id, &f.tags)?;
        replace_lineage(&tx, id, &f.derived_from)?;
        upsert_fts(&tx, id, &f.title, &trace.body, &f.tags)?;
        self.emit_event(&tx, "update", id, &f.updated, trace_snapshot(&trace))?;
        tx.commit()?;
        Ok(())
    }

    pub fn append(&self, id: &str, content: &str, actor_agent: bool) -> Result<()> {
        let (_, mut trace) = self.get_trace(id)?;
        if !trace.body.is_empty() && !trace.body.ends_with('\n') {
            trace.body.push('\n');
        }
        trace.body.push_str(content);
        self.update_trace(id, &mut trace, actor_agent)
    }

    pub fn set_tags(&self, id: &str, tags: Vec<String>, actor_agent: bool) -> Result<()> {
        let (_, mut trace) = self.get_trace(id)?;
        trace.frontmatter.tags = dedupe(tags);
        self.update_trace(id, &mut trace, actor_agent)
    }

    pub fn append_tags(
        &self,
        id: &str,
        tags: Vec<String>,
        actor_agent: bool,
    ) -> Result<Vec<String>> {
        let (_, mut trace) = self.get_trace(id)?;
        trace.frontmatter.tags.extend(tags);
        trace.frontmatter.tags = dedupe(trace.frontmatter.tags);
        let result = trace.frontmatter.tags.clone();
        self.update_trace(id, &mut trace, actor_agent)?;
        Ok(result)
    }

    pub fn archive(&self, id: &str) -> Result<()> {
        self.move_trace(id, Visibility::Archive)
    }

    pub fn unarchive(&self, id: &str) -> Result<()> {
        self.move_trace(id, Visibility::Active)
    }

    pub fn trash(&self, id: &str) -> Result<()> {
        self.move_trace(id, Visibility::Trash)
    }

    pub fn recover(&self, id: &str) -> Result<()> {
        self.move_trace(id, Visibility::Active)
    }

    pub fn mark_archived_no_move(&self, id: &str) -> Result<()> {
        let row = self.get(id)?;
        self.check_source_lock(&row)?;
        if !row.trashed_at.is_empty() {
            bail!("trace {id} is in trash");
        }
        if row.archived_at.is_empty() {
            self.mark_visibility(id, "archive", Some(trace::now_rfc3339()), None)?;
        }
        Ok(())
    }

    pub fn mark_unarchived_no_move(&self, id: &str) -> Result<()> {
        let row = self.get(id)?;
        self.check_source_lock(&row)?;
        if !row.archived_at.is_empty() {
            self.mark_visibility(id, "unarchive", None, None)?;
        }
        Ok(())
    }

    pub fn mark_trashed_no_move(&self, id: &str) -> Result<()> {
        let row = self.get(id)?;
        self.check_source_lock(&row)?;
        if row.trashed_at.is_empty() {
            self.mark_visibility(id, "trash", None, Some(trace::now_rfc3339()))?;
        }
        Ok(())
    }

    pub fn mark_recovered_no_move(&self, id: &str) -> Result<()> {
        let row = self.get(id)?;
        self.check_source_lock(&row)?;
        if !row.trashed_at.is_empty() {
            self.mark_visibility(id, "recover", None, None)?;
        }
        Ok(())
    }

    fn mark_visibility(
        &self,
        id: &str,
        action: &str,
        archived_at: Option<String>,
        trashed_at: Option<String>,
    ) -> Result<()> {
        self.mark_visibility_with_pending(id, action, archived_at, trashed_at, None)
    }

    fn mark_visibility_with_pending(
        &self,
        id: &str,
        action: &str,
        archived_at: Option<String>,
        trashed_at: Option<String>,
        pending_key: Option<&str>,
    ) -> Result<()> {
        let now = trace::now_rfc3339();
        let tx = self.connection.unchecked_transaction()?;
        tx.execute(
            "UPDATE traces SET archived_at=?1,trashed_at=?2 WHERE id=?3",
            params![archived_at, trashed_at, id],
        )?;
        self.emit_event(&tx, action, id, &now, json!({}))?;
        if let Some(pending_key) = pending_key {
            Self::clear_pending_in_transaction(&tx, pending_key)?;
        }
        tx.commit()?;
        Ok(())
    }

    pub fn ingest_external_delete(&self, id: &str) -> Result<()> {
        let row = self.get(id)?;
        self.check_source_lock(&row)?;
        if !row.trashed_at.is_empty() {
            return Ok(());
        }

        if let Some(event) = self
            .history(id)?
            .into_iter()
            .rev()
            .find(|event| matches!(event.action.as_str(), "create" | "update"))
        {
            let data: TraceEventData = serde_json::from_value(event.data)?;
            let trace = Trace {
                frontmatter: crate::trace::Frontmatter {
                    id: id.to_owned(),
                    title: data.title,
                    trace_type: data.trace_type,
                    tier: row.tier,
                    author: data.author,
                    tags: data.tags,
                    derived_from: data.derived_from,
                    origin: data.origin,
                    created: row.created_at,
                    updated: event.timestamp,
                    content_hash: data.content_hash,
                    source_hash: data.source_hash,
                    source_locked: data.source_locked,
                },
                body: data.body,
            };
            let path = self.trash_dir().join(format!("{id}.md"));
            let trashed_at = trace::now_rfc3339();
            if path.exists() {
                return self.replace_trace_transactionally(&path, &trace, |pending_key| {
                    self.mark_visibility_with_pending(
                        id,
                        "trash",
                        None,
                        Some(trashed_at),
                        Some(pending_key),
                    )
                });
            }
            return self.create_trace_transactionally(&path, &trace, |pending_key| {
                self.mark_visibility_with_pending(
                    id,
                    "trash",
                    None,
                    Some(trashed_at),
                    Some(pending_key),
                )
            });
        }
        self.mark_visibility(id, "trash", None, Some(trace::now_rfc3339()))
    }

    pub fn apply_external_purge(&self, id: &str) -> Result<()> {
        let row = self.get(id)?;
        if row.trashed_at.is_empty() {
            bail!("trace {id} is not in trash");
        }
        let now = trace::now_rfc3339();
        let tx = self.connection.unchecked_transaction()?;
        tx.execute("DELETE FROM traces WHERE id=?1", [id])?;
        self.emit_event(&tx, "purge", id, &now, json!({}))?;
        tx.commit()?;
        Ok(())
    }

    fn move_trace(&self, id: &str, destination: Visibility) -> Result<()> {
        let row = self.get(id)?;
        self.check_source_lock(&row)?;
        let source = self.file_path(&row);
        let target = match destination {
            Visibility::Active => self.trace_file(id, false),
            Visibility::Archive => self.trace_file(id, true),
            Visibility::Trash => self.trash_dir().join(format!("{id}.md")),
        };
        let now = trace::now_rfc3339();
        let (action, archived, trashed) = match destination {
            Visibility::Active if !row.trashed_at.is_empty() => ("recover", None, None),
            Visibility::Active => ("unarchive", None, None),
            Visibility::Archive => ("archive", Some(now.as_str()), None),
            Visibility::Trash => ("trash", None, Some(now.as_str())),
        };
        self.move_trace_transactionally(&source, &target, |pending_key| {
            let tx = self.connection.unchecked_transaction()?;
            tx.execute(
                "UPDATE traces SET archived_at=?1,trashed_at=?2 WHERE id=?3",
                params![archived, trashed, id],
            )?;
            self.emit_event(&tx, action, id, &now, json!({}))?;
            if let Some(pending_key) = pending_key {
                Self::clear_pending_in_transaction(&tx, pending_key)?;
            }
            tx.commit()?;
            Ok(())
        })
    }

    pub fn remove_hard(&self, id: &str) -> Result<()> {
        let row = self.get(id)?;
        self.check_source_lock(&row)?;
        let path = self.file_path(&row);
        self.delete_trace_transactionally(&path, |pending_key| {
            let tx = self.connection.unchecked_transaction()?;
            tx.execute("DELETE FROM traces WHERE id=?1", [id])?;
            if let Some(pending_key) = pending_key {
                Self::clear_pending_in_transaction(&tx, pending_key)?;
            }
            tx.commit()?;
            Ok(())
        })
    }

    pub fn admin_purge(
        &self,
        id: &str,
        reason: &str,
        expected_tier: &str,
        hard: bool,
    ) -> Result<()> {
        let row = self.get(id)?;
        if row.tier != expected_tier {
            bail!(
                "tier mismatch: trace is {:?}, caller asserted {:?}",
                row.tier,
                expected_tier
            );
        }
        if reason.is_empty() {
            bail!("purge requires a reason (audit trail needs it)");
        }
        let path = self.file_path(&row);
        let now = trace::now_rfc3339();
        let action = if hard {
            "purge_hard"
        } else if row.tier == "long" {
            "purge_long_term"
        } else {
            "purge"
        };
        let mut event_data = serde_json::Map::new();
        event_data.insert("reason".into(), json!(reason));
        event_data.insert("tier".into(), json!(&row.tier));
        if !row.content_hash.is_empty() {
            event_data.insert("content_hash".into(), json!(&row.content_hash));
        }
        event_data.insert("actor".into(), json!("human"));
        if hard {
            event_data.insert("hard".into(), json!(true));
        }
        self.delete_trace_transactionally(&path, |pending_key| {
            let tx = self.connection.unchecked_transaction()?;
            let update_trigger: Option<String> = if row.tier == "long" {
                tx.query_row(
                    "SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trg_long_term_immutable'",
                    [],
                    |result| result.get(0),
                )
                .optional()?
            } else {
                None
            };
            let delete_trigger: Option<String> = if row.tier == "long" && hard {
                tx.query_row(
                    "SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trg_long_term_no_delete'",
                    [],
                    |result| result.get(0),
                )
                .optional()?
            } else {
                None
            };
            if row.tier == "long" {
                tx.execute("DROP TRIGGER IF EXISTS trg_long_term_immutable", [])?;
                if hard {
                    tx.execute("DROP TRIGGER IF EXISTS trg_long_term_no_delete", [])?;
                }
            }
            if hard {
                tx.execute("DELETE FROM trace_lineage WHERE trace_id=?1", [id])?;
                tx.execute("DELETE FROM trace_lineage WHERE derived_from=?1", [id])?;
                tx.execute("DELETE FROM trace_tags WHERE trace_id=?1", [id])?;
                tx.execute("DELETE FROM traces_fts WHERE id=?1", [id])?;
                tx.execute("DELETE FROM traces WHERE id=?1", [id])?;
            } else {
                tx.execute(
                    "UPDATE traces SET purged_at=?1,purge_reason=?2,updated_at=?1 WHERE id=?3",
                    params![now, reason, id],
                )?;
                tx.execute("DELETE FROM trace_tags WHERE trace_id=?1", [id])?;
                tx.execute("DELETE FROM traces_fts WHERE id=?1", [id])?;
                tx.execute(
                    "INSERT INTO traces_fts(id,title,body) VALUES (?1,?2,?3)",
                    params![id, row.title, format!("[purged: {reason}]")],
                )?;
            }
            if let Some(trigger) = update_trigger {
                tx.execute_batch(&trigger)?;
            }
            if let Some(trigger) = delete_trigger {
                tx.execute_batch(&trigger)?;
            }
            self.emit_event(
                &tx,
                action,
                id,
                &now,
                serde_json::Value::Object(event_data),
            )?;
            if let Some(pending_key) = pending_key {
                Self::clear_pending_in_transaction(&tx, pending_key)?;
            }
            tx.commit()?;
            Ok(())
        })
    }

    pub fn purge_expired(&mut self, days: u32) -> Result<usize> {
        let days = if days == 0 { 30 } else { days };
        let cutoff = (Utc::now() - Duration::days(days.into())).to_rfc3339();
        let ids: Vec<String> = {
            let mut statement = self.connection.prepare(
                "SELECT id FROM traces WHERE trashed_at IS NOT NULL AND trashed_at < ?1",
            )?;
            statement
                .query_map([cutoff], |row| row.get(0))?
                .collect::<rusqlite::Result<_>>()?
        };
        let now = trace::now_rfc3339();
        for id in &ids {
            let path = self.trash_dir().join(format!("{id}.md"));
            self.delete_trace_transactionally(&path, |pending_key| {
                let tx = self.connection.unchecked_transaction()?;
                tx.execute("DELETE FROM traces WHERE id=?1", [id])?;
                self.emit_event(&tx, "purge", id, &now, json!({}))?;
                if let Some(pending_key) = pending_key {
                    Self::clear_pending_in_transaction(&tx, pending_key)?;
                }
                tx.commit()?;
                Ok(())
            })?;
        }
        Ok(ids.len())
    }

    pub fn promote(&self, id: &str, to: &str) -> Result<()> {
        let row = self.get(id)?;
        if !((row.tier == "short" && to == "mid") || (row.tier == "mid" && to == "long")) {
            bail!("invalid promotion {} -> {to}", row.tier);
        }
        self.change_tier(&row, to, "promote")
    }

    pub fn demote(&self, id: &str) -> Result<()> {
        let row = self.get(id)?;
        if row.tier != "mid" {
            bail!("only mid-tier traces can be demoted");
        }
        self.change_tier(&row, "short", "demote")
    }

    fn change_tier(&self, row: &Row, to: &str, action: &str) -> Result<()> {
        let path = self.file_path(row);
        let mut trace = Trace::parse_file(&path)?;
        trace.frontmatter.tier = to.to_owned();
        trace.frontmatter.updated = row.updated_at.clone();
        self.replace_trace_transactionally(&path, &trace, |pending_key| {
            let tx = self.connection.unchecked_transaction()?;
            tx.execute("UPDATE traces SET tier=?1 WHERE id=?2", params![to, row.id])?;
            self.emit_event(
                &tx,
                action,
                &row.id,
                &trace::now_rfc3339(),
                json!({"from": row.tier, "to": to}),
            )?;
            Self::clear_pending_in_transaction(&tx, pending_key)?;
            tx.commit()?;
            Ok(())
        })
    }

    pub fn vote(&self, id: &str, delta: i64, actor: &str) -> Result<()> {
        if delta == 0 || !(-2..=2).contains(&delta) {
            bail!("vote delta must be ±1 or ±2");
        }
        self.get(id)?;
        let tx = self.connection.unchecked_transaction()?;
        tx.execute(
            "UPDATE traces SET tier_votes=tier_votes+?1 WHERE id=?2",
            params![delta, id],
        )?;
        self.emit_event(
            &tx,
            "vote",
            id,
            &trace::now_rfc3339(),
            json!({"delta":delta,"actor":actor}),
        )?;
        tx.commit()?;
        Ok(())
    }

    pub fn tier_votes(&self, id: &str) -> Result<i64> {
        Ok(self
            .connection
            .query_row("SELECT tier_votes FROM traces WHERE id=?1", [id], |row| {
                row.get(0)
            })?)
    }

    pub fn history(&self, id: &str) -> Result<Vec<Event>> {
        let mut statement = self.connection.prepare(
            "SELECT id,action,trace_id,cortex_id,origin,timestamp,data,vclock,signature,pubkey FROM events WHERE trace_id=?1 ORDER BY id",
        )?;
        let rows = statement.query_map([id], scan_event)?;
        Ok(rows.collect::<rusqlite::Result<_>>()?)
    }

    pub fn resolve_divergence(
        &self,
        divergence_id: &str,
        accept_origin: &str,
        custom_body: &str,
    ) -> Result<()> {
        if accept_origin.is_empty() && custom_body.is_empty() {
            bail!("resolution requires either an accept origin or a custom body");
        }
        if !accept_origin.is_empty() && !custom_body.is_empty() {
            bail!("specify only one of accept origin or custom body");
        }

        let (row, divergence) = self
            .get_trace(divergence_id)
            .with_context(|| format!("divergence trace {divergence_id:?} not found"))?;
        if row.trace_type != "divergence" {
            bail!(
                "trace {divergence_id:?} is not a divergence (type={})",
                row.trace_type
            );
        }
        let original_id = row.derived_from.first().ok_or_else(|| {
            anyhow::anyhow!(
                "divergence trace {divergence_id:?} has no derived_from link to original trace"
            )
        })?;

        let resolution = if !custom_body.is_empty() {
            custom_body.to_owned()
        } else {
            let sections = split_divergence_sections(&divergence.body)
                .with_context(|| format!("parsing divergence trace {divergence_id:?}"))?;
            if let Some(section) = sections
                .iter()
                .find(|section| section.matches(accept_origin))
            {
                section.body.trim().to_owned()
            } else {
                let available = sections
                    .iter()
                    .map(DivergenceSection::label)
                    .collect::<Vec<_>>()
                    .join(", ");
                bail!(
                    "origin {accept_origin:?} not found in divergence {divergence_id:?} (available: {available})"
                );
            }
        };

        let (_, mut original) = self
            .get_trace(original_id)
            .with_context(|| format!("original trace {original_id:?} not found"))?;
        original.body = resolution;
        self.update_trace(original_id, &mut original, false)?;
        self.trash(divergence_id)
    }

    pub fn unresolved_coordination_claims_before(
        &self,
        cutoff: &str,
    ) -> Result<Vec<CoordinationClaim>> {
        let mut statement = self.connection.prepare(
            "SELECT trace_id,cortex_id,timestamp
             FROM events
             WHERE action='consolidation_claim'
               AND timestamp<?1
               AND NOT EXISTS (
                   SELECT 1 FROM events resolved
                   WHERE resolved.trace_id=events.trace_id
                     AND resolved.action IN ('consolidation_success','consolidation_fail')
               )
             ORDER BY timestamp",
        )?;
        Ok(statement
            .query_map([cutoff], |row| {
                Ok(CoordinationClaim {
                    window_id: row.get(0)?,
                    winner_id: row.get(1)?,
                    timestamp: row.get(2)?,
                })
            })?
            .collect::<rusqlite::Result<_>>()?)
    }

    pub fn events_since(&self, since: &str, limit: usize) -> Result<Vec<Event>> {
        let sql = if since.is_empty() {
            "SELECT id,action,trace_id,cortex_id,origin,timestamp,data,vclock,signature,pubkey FROM events ORDER BY id LIMIT ?1"
        } else {
            "SELECT id,action,trace_id,cortex_id,origin,timestamp,data,vclock,signature,pubkey FROM events WHERE id>?1 ORDER BY id LIMIT ?2"
        };
        let mut statement = self.connection.prepare(sql)?;
        let events = if since.is_empty() {
            statement
                .query_map([limit as i64], scan_event)?
                .collect::<rusqlite::Result<_>>()?
        } else {
            statement
                .query_map(params![since, limit as i64], scan_event)?
                .collect::<rusqlite::Result<_>>()?
        };
        Ok(events)
    }

    pub fn backfill_create_events(&self, dry_run: bool) -> Result<EventBackfillResult> {
        let candidates: Vec<(String, bool, bool)> = {
            let mut statement = self.connection.prepare(
                "SELECT id,archived_at IS NOT NULL,trashed_at IS NOT NULL FROM traces
                 WHERE id NOT IN (SELECT trace_id FROM events WHERE action='create')
                 ORDER BY created_at,id",
            )?;
            statement
                .query_map([], |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)))?
                .collect::<rusqlite::Result<_>>()?
        };
        let mut result = EventBackfillResult::default();
        for (id, archived, trashed) in candidates {
            if archived || trashed {
                result.skipped_ids.push(id);
                continue;
            }
            if !dry_run {
                let trace = Trace::parse_file(&self.trace_file(&id, false))?;
                let now = trace::now_rfc3339();
                let tx = self.connection.unchecked_transaction()?;
                self.emit_event(&tx, "create", &id, &now, trace_snapshot(&trace))?;
                tx.commit()?;
            }
            result.backfilled_ids.push(id);
        }
        Ok(result)
    }

    pub fn replay_event(&self, event: &Event) -> Result<()> {
        self.verify_replay_event(event)?;
        self.check_replay_source_lock(event)?;
        let exists: bool = self.connection.query_row(
            "SELECT EXISTS(SELECT 1 FROM events WHERE id=?1)",
            [&event.id],
            |row| row.get(0),
        )?;
        if exists {
            return Ok(());
        }
        if matches!(
            event.action.as_str(),
            "consolidation_claim" | "consolidation_success" | "consolidation_fail"
        ) {
            return self.store_remote_event(event);
        }
        if !trace::is_valid_id(&event.trace_id) {
            if event.action == "purge" {
                return self.replay_purge_db_only(event);
            }
            bail!(
                "rejecting remote event with invalid trace ID {:?}",
                event.trace_id
            );
        }
        match event.action.as_str() {
            "create" => {
                if self.get(&event.trace_id).is_ok() {
                    self.store_remote_event(event)
                } else {
                    self.materialize_remote_snapshot(event, None)
                }
            }
            "update" => self.replay_update(event),
            "tag_update" => self.replay_tag_update(event),
            "archive" | "unarchive" | "trash" | "recover" => self.replay_visibility(event),
            "promote" | "demote" => self.replay_tier_change(event),
            "consolidate" => self.replay_consolidate(event),
            "consolidate_fallback" | "divergence_long_term" => self.store_remote_event(event),
            "vote" => self.replay_vote(event),
            "purge" => self.replay_purge(event),
            "purge_long_term" => self.replay_purge_long_term(event),
            "purge_hard" => self.replay_purge_hard(event),
            action => bail!("unknown event action {action:?}"),
        }
    }

    fn federation_verify_mode(&self) -> &str {
        self.manifest
            .federation
            .as_ref()
            .map(|config| config.verify.as_str())
            .filter(|mode| !mode.is_empty())
            .unwrap_or("off")
    }

    fn verify_replay_event(&self, event: &Event) -> Result<()> {
        let mode = self.federation_verify_mode();
        if mode == "off" {
            return Ok(());
        }
        let key_name = format!("cortexkey:{}", event.cortex_id);
        let pinned: Option<String> = self
            .connection
            .query_row(
                "SELECT value FROM federation_state WHERE key=?1",
                [&key_name],
                |row| row.get(0),
            )
            .optional()?;
        let local_public = self
            .manifest
            .signing
            .as_ref()
            .map(|signing| signing.public_key.as_str())
            .unwrap_or("");
        let expected = if event.cortex_id == self.id {
            local_public
        } else {
            pinned.as_deref().unwrap_or(&event.pubkey)
        };
        let problem = if expected.is_empty() {
            Some(anyhow::anyhow!(
                "no signing key is available for event origin"
            ))
        } else if event.signature.is_empty() {
            Some(anyhow::anyhow!("remote event is unsigned"))
        } else if pinned
            .as_deref()
            .is_some_and(|key| !event.pubkey.is_empty() && key.trim() != event.pubkey.trim())
        {
            Some(anyhow::anyhow!(
                "event public key conflicts with pinned key"
            ))
        } else {
            eventsig::verify(expected, event, &event.signature).err()
        };
        if let Some(problem) = problem {
            if mode == "enforce" {
                return Err(problem).context(format!(
                    "rejecting event {} from cortex {}",
                    event.id, event.cortex_id
                ));
            }
            eprintln!(
                "federation signature warning for event {}: {problem:#}",
                event.id
            );
            return Ok(());
        }
        if event.cortex_id != self.id && pinned.is_none() {
            self.connection.execute(
                "INSERT INTO federation_state(key,value) VALUES (?1,?2)",
                params![key_name, event.pubkey],
            )?;
        }
        Ok(())
    }

    fn check_replay_source_lock(&self, event: &Event) -> Result<()> {
        let mode = self.federation_verify_mode();
        if mode == "off"
            || !matches!(
                event.action.as_str(),
                "update" | "tag_update" | "trash" | "purge"
            )
        {
            return Ok(());
        }
        let owner: Option<(i64, String)> = self
            .connection
            .query_row(
                "SELECT source_locked,cortex_id FROM traces WHERE id=?1",
                [&event.trace_id],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .optional()?;
        let Some((source_locked, owner)) = owner else {
            return Ok(());
        };
        if source_locked == 0 || owner == event.cortex_id {
            return Ok(());
        }
        let problem = anyhow::anyhow!(
            "event {} ({}) targets source-locked trace {} owned by cortex {} but originates from cortex {}",
            event.id,
            event.action,
            event.trace_id,
            owner,
            event.cortex_id
        );
        if mode == "enforce" {
            return Err(problem).context("rejecting source-lock violation");
        }
        eprintln!("federation source-lock warning: {problem:#} — accepted (verify=warn)");
        Ok(())
    }

    fn replay_update(&self, event: &Event) -> Result<()> {
        let Ok(row) = self.get(&event.trace_id) else {
            return self.materialize_remote_snapshot(event, None);
        };
        if let Some(local) = self
            .history(&event.trace_id)?
            .into_iter()
            .rev()
            .find(|candidate| matches!(candidate.action.as_str(), "create" | "update"))
            && !local.vclock.is_empty()
            && !event.vclock.is_empty()
        {
            match federation::compare(&local.vclock, &event.vclock) {
                Relation::Concurrent => return self.create_divergence(&row, event),
                Relation::After | Relation::Equal => return self.store_remote_event(event),
                Relation::Before => {}
            }
        }
        self.materialize_remote_snapshot(event, Some(row))
    }

    fn replay_tag_update(&self, event: &Event) -> Result<()> {
        #[derive(Deserialize)]
        struct TagData {
            tags: Vec<String>,
        }
        let data: TagData = serde_json::from_value(event.data.clone())?;
        let row = self.get(&event.trace_id)?;
        let (_, mut trace) = self.get_trace(&event.trace_id)?;
        trace.frontmatter.tags = dedupe(data.tags);
        trace.frontmatter.tier = row.tier.clone();
        trace.frontmatter.updated = row.updated_at.clone();
        let path = self.file_path(&row);
        self.replace_trace_transactionally(&path, &trace, |pending_key| {
            let tx = self.connection.unchecked_transaction()?;
            replace_tags(&tx, &event.trace_id, &trace.frontmatter.tags)?;
            upsert_fts(
                &tx,
                &event.trace_id,
                &row.title,
                &trace.body,
                &trace.frontmatter.tags,
            )?;
            self.store_remote_event_tx(&tx, event)?;
            Self::clear_pending_in_transaction(&tx, pending_key)?;
            tx.commit()?;
            Ok(())
        })
    }

    fn replay_visibility(&self, event: &Event) -> Result<()> {
        let Ok(row) = self.get(&event.trace_id) else {
            return self.store_remote_event(event);
        };
        let already_applied = match event.action.as_str() {
            "archive" => !row.archived_at.is_empty(),
            "unarchive" => row.archived_at.is_empty(),
            "trash" => !row.trashed_at.is_empty(),
            "recover" => row.trashed_at.is_empty(),
            _ => unreachable!(),
        };
        if already_applied {
            return self.store_remote_event(event);
        }
        let (source, target, archived, trashed): (_, _, Option<String>, Option<String>) =
            match event.action.as_str() {
                "archive" => (
                    self.trace_file(&event.trace_id, false),
                    self.trace_file(&event.trace_id, true),
                    Some(event.timestamp.clone()),
                    nullable(&row.trashed_at).map(str::to_owned),
                ),
                "unarchive" => (
                    self.trace_file(&event.trace_id, true),
                    self.trace_file(&event.trace_id, false),
                    None,
                    nullable(&row.trashed_at).map(str::to_owned),
                ),
                "trash" => (
                    self.file_path(&row),
                    self.trash_dir().join(format!("{}.md", event.trace_id)),
                    None,
                    Some(event.timestamp.clone()),
                ),
                "recover" => (
                    self.trash_dir().join(format!("{}.md", event.trace_id)),
                    self.trace_file(&event.trace_id, false),
                    nullable(&row.archived_at).map(str::to_owned),
                    None,
                ),
                _ => unreachable!(),
            };
        self.move_trace_transactionally(&source, &target, |pending_key| {
            let tx = self.connection.unchecked_transaction()?;
            tx.execute(
                "UPDATE traces SET archived_at=?1,trashed_at=?2 WHERE id=?3",
                params![archived, trashed, event.trace_id],
            )?;
            self.store_remote_event_tx(&tx, event)?;
            if let Some(pending_key) = pending_key {
                Self::clear_pending_in_transaction(&tx, pending_key)?;
            }
            tx.commit()?;
            Ok(())
        })
    }

    fn replay_tier_change(&self, event: &Event) -> Result<()> {
        #[derive(Deserialize)]
        struct TierData {
            to: String,
        }
        let data: TierData = serde_json::from_value(event.data.clone())?;
        if !matches!(data.to.as_str(), "short" | "mid" | "long") {
            bail!("tier-change event {} has invalid target", event.id);
        }
        let Ok(row) = self.get(&event.trace_id) else {
            return self.store_remote_event(event);
        };
        let path = self.file_path(&row);
        let mut trace = Trace::parse_file(&path)?;
        trace.frontmatter.tier = data.to.clone();
        trace.frontmatter.updated = row.updated_at.clone();
        self.replace_trace_transactionally(&path, &trace, |pending_key| {
            let tx = self.connection.unchecked_transaction()?;
            tx.execute(
                "UPDATE traces SET tier=?1 WHERE id=?2",
                params![data.to, event.trace_id],
            )?;
            self.store_remote_event_tx(&tx, event)?;
            Self::clear_pending_in_transaction(&tx, pending_key)?;
            tx.commit()?;
            Ok(())
        })
    }

    fn replay_consolidate(&self, event: &Event) -> Result<()> {
        #[derive(Default, Deserialize)]
        struct ConsolidateData {
            #[serde(default)]
            distilled_id: String,
        }
        let data: ConsolidateData = serde_json::from_value(event.data.clone()).unwrap_or_default();
        let id = if data.distilled_id.is_empty() {
            &event.trace_id
        } else {
            &data.distilled_id
        };
        let Ok(row) = self.get(id) else {
            return self.store_remote_event(event);
        };
        if row.tier != "short" {
            return self.store_remote_event(event);
        }
        let path = self.file_path(&row);
        let mut trace = Trace::parse_file(&path)?;
        trace.frontmatter.tier = "mid".into();
        trace.frontmatter.updated = row.updated_at.clone();
        self.replace_trace_transactionally(&path, &trace, |pending_key| {
            let tx = self.connection.unchecked_transaction()?;
            tx.execute("UPDATE traces SET tier='mid' WHERE id=?1", [id])?;
            self.store_remote_event_tx(&tx, event)?;
            Self::clear_pending_in_transaction(&tx, pending_key)?;
            tx.commit()?;
            Ok(())
        })
    }

    fn replay_vote(&self, event: &Event) -> Result<()> {
        #[derive(Deserialize)]
        struct VoteData {
            delta: i64,
        }
        let mut data: VoteData = serde_json::from_value(event.data.clone())?;
        data.delta = data.delta.signum();
        let Ok(_) = self.get(&event.trace_id) else {
            return self.store_remote_event(event);
        };
        let tx = self.connection.unchecked_transaction()?;
        tx.execute(
            "UPDATE traces SET tier_votes=tier_votes+?1 WHERE id=?2",
            params![data.delta, event.trace_id],
        )?;
        self.store_remote_event_tx(&tx, event)?;
        tx.commit()?;
        Ok(())
    }

    fn replay_purge(&self, event: &Event) -> Result<()> {
        let path = self.trash_dir().join(format!("{}.md", event.trace_id));
        self.delete_trace_transactionally(&path, |pending_key| {
            let tx = self.connection.unchecked_transaction()?;
            tx.execute("DELETE FROM traces_fts WHERE id=?1", [&event.trace_id])?;
            tx.execute("DELETE FROM traces WHERE id=?1", [&event.trace_id])?;
            self.store_remote_event_tx(&tx, event)?;
            if let Some(pending_key) = pending_key {
                Self::clear_pending_in_transaction(&tx, pending_key)?;
            }
            tx.commit()?;
            Ok(())
        })
    }

    fn replay_purge_db_only(&self, event: &Event) -> Result<()> {
        let tx = self.connection.unchecked_transaction()?;
        tx.execute("DELETE FROM traces WHERE id=?1", [&event.trace_id])?;
        self.store_remote_event_tx(&tx, event)?;
        tx.commit()?;
        Ok(())
    }

    fn replay_purge_long_term(&self, event: &Event) -> Result<()> {
        if self.get(&event.trace_id).is_err() {
            return self.store_remote_event(event);
        }
        let reason = event
            .data
            .get("reason")
            .and_then(|value| value.as_str())
            .filter(|reason| !reason.is_empty())
            .unwrap_or("remote purge");
        let tx = self.connection.unchecked_transaction()?;
        let trigger: Option<String> = tx
            .query_row(
                "SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trg_long_term_immutable'",
                [],
                |row| row.get(0),
            )
            .optional()?;
        tx.execute("DROP TRIGGER IF EXISTS trg_long_term_immutable", [])?;
        tx.execute(
            "UPDATE traces SET purged_at=?1,purge_reason=?2,updated_at=?1 WHERE id=?3",
            params![event.timestamp, reason, event.trace_id],
        )?;
        tx.execute(
            "DELETE FROM trace_tags WHERE trace_id=?1",
            [&event.trace_id],
        )?;
        tx.execute("DELETE FROM traces_fts WHERE id=?1", [&event.trace_id])?;
        tx.execute(
            "INSERT INTO traces_fts(id,title,body) VALUES (?1,'',?2)",
            params![event.trace_id, format!("[purged: {reason}]")],
        )?;
        if let Some(trigger) = trigger {
            tx.execute_batch(&trigger)?;
        }
        self.store_remote_event_tx(&tx, event)?;
        tx.commit()?;
        self.remove_trace_files_best_effort(&event.trace_id);
        Ok(())
    }

    fn replay_purge_hard(&self, event: &Event) -> Result<()> {
        if self.get(&event.trace_id).is_err() {
            return self.store_remote_event(event);
        }
        let tx = self.connection.unchecked_transaction()?;
        let update_trigger: Option<String> = tx
            .query_row(
                "SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trg_long_term_immutable'",
                [],
                |row| row.get(0),
            )
            .optional()?;
        let delete_trigger: Option<String> = tx
            .query_row(
                "SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trg_long_term_no_delete'",
                [],
                |row| row.get(0),
            )
            .optional()?;
        tx.execute("DROP TRIGGER IF EXISTS trg_long_term_immutable", [])?;
        tx.execute("DROP TRIGGER IF EXISTS trg_long_term_no_delete", [])?;
        tx.execute(
            "DELETE FROM trace_lineage WHERE trace_id=?1",
            [&event.trace_id],
        )?;
        tx.execute(
            "DELETE FROM trace_lineage WHERE derived_from=?1",
            [&event.trace_id],
        )?;
        tx.execute(
            "DELETE FROM trace_tags WHERE trace_id=?1",
            [&event.trace_id],
        )?;
        tx.execute("DELETE FROM traces_fts WHERE id=?1", [&event.trace_id])?;
        tx.execute("DELETE FROM traces WHERE id=?1", [&event.trace_id])?;
        if let Some(trigger) = update_trigger {
            tx.execute_batch(&trigger)?;
        }
        if let Some(trigger) = delete_trigger {
            tx.execute_batch(&trigger)?;
        }
        self.store_remote_event_tx(&tx, event)?;
        tx.commit()?;
        self.remove_trace_files_best_effort(&event.trace_id);
        Ok(())
    }

    fn remove_trace_files_best_effort(&self, id: &str) {
        for path in [
            self.trace_file(id, false),
            self.trace_file(id, true),
            self.trash_dir().join(format!("{id}.md")),
        ] {
            if path.exists() {
                let _ = remove_file_durable(&path);
            }
        }
    }

    fn materialize_remote_snapshot(&self, event: &Event, existing: Option<Row>) -> Result<()> {
        let data: TraceEventData = serde_json::from_value(event.data.clone())?;
        let content_hash = trace::content_hash(&data.body);
        if !data.content_hash.is_empty() && data.content_hash != content_hash {
            bail!("content hash mismatch in remote event {}", event.id);
        }
        let mut tier = existing
            .as_ref()
            .map(|row| row.tier.clone())
            .or_else(|| (!data.tier.is_empty()).then(|| data.tier.clone()))
            .unwrap_or_else(|| "short".into());
        if existing.is_none() {
            for pending in self
                .history(&event.trace_id)?
                .into_iter()
                .filter(|pending| pending.id > event.id)
            {
                match pending.action.as_str() {
                    "consolidate" if tier == "short" => tier = "mid".into(),
                    "promote" | "demote" => {
                        if let Some(target) =
                            pending.data.get("to").and_then(|value| value.as_str())
                            && matches!(target, "short" | "mid" | "long")
                        {
                            tier = target.into();
                        }
                    }
                    _ => {}
                }
            }
        }
        let created = existing
            .as_ref()
            .map(|row| row.created_at.clone())
            .unwrap_or_else(|| event.timestamp.clone());
        let trace = Trace {
            frontmatter: trace::Frontmatter {
                id: event.trace_id.clone(),
                title: data.title,
                trace_type: data.trace_type,
                tier,
                author: data.author,
                tags: dedupe(data.tags),
                derived_from: dedupe(data.derived_from),
                origin: if data.origin.is_empty() {
                    event.origin.clone()
                } else {
                    data.origin
                },
                created,
                updated: event.timestamp.clone(),
                content_hash,
                source_hash: data.source_hash,
                source_locked: data.source_locked && event.cortex_id != self.id,
            },
            body: data.body,
        };
        trace.validate()?;
        let path = existing
            .as_ref()
            .map(|row| self.file_path(row))
            .unwrap_or_else(|| self.trace_file(&event.trace_id, false));
        if path.exists() {
            return self.replace_trace_transactionally(&path, &trace, |pending_key| {
                self.commit_remote_snapshot(event, existing.is_some(), &trace, Some(pending_key))
            });
        }
        self.create_trace_transactionally(&path, &trace, |pending_key| {
            self.commit_remote_snapshot(event, false, &trace, Some(pending_key))
        })
    }

    fn commit_remote_snapshot(
        &self,
        event: &Event,
        existing: bool,
        trace: &Trace,
        pending_key: Option<&str>,
    ) -> Result<()> {
        let tx = self.connection.unchecked_transaction()?;
        let f = &trace.frontmatter;
        if existing {
            tx.execute(
                "UPDATE traces SET title=?1,type=?2,author=?3,origin=?4,updated_at=?5,content_hash=?6,source_locked=?7,source_hash=?8 WHERE id=?9",
                params![f.title,f.trace_type,f.author,f.origin,f.updated,f.content_hash,i64::from(f.source_locked),nullable(&f.source_hash),f.id],
            )?;
        } else {
            tx.execute(
                "INSERT INTO traces(id,title,type,tier,author,origin,cortex_id,created_at,updated_at,content_hash,source_locked,source_hash) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12)",
                params![f.id,f.title,f.trace_type,f.tier,f.author,f.origin,event.cortex_id,f.created,f.updated,f.content_hash,i64::from(f.source_locked),nullable(&f.source_hash)],
            )?;
        }
        replace_tags(&tx, &f.id, &f.tags)?;
        replace_lineage(&tx, &f.id, &f.derived_from)?;
        upsert_fts(&tx, &f.id, &f.title, &trace.body, &f.tags)?;
        self.store_remote_event_tx(&tx, event)?;
        if let Some(pending_key) = pending_key {
            Self::clear_pending_in_transaction(&tx, pending_key)?;
        }
        tx.commit()?;
        Ok(())
    }

    fn create_divergence(&self, local: &Row, remote: &Event) -> Result<()> {
        let (_, local_trace) = self.get_trace(&local.id)?;
        let data: TraceEventData = serde_json::from_value(remote.data.clone())?;
        let local_event = self
            .history(&local.id)?
            .into_iter()
            .rev()
            .find(|event| matches!(event.action.as_str(), "create" | "update"))
            .ok_or_else(|| anyhow::anyhow!("local trace has no mutation event"))?;
        let mut versions = vec![
            (
                self.id.clone(),
                self.name.clone(),
                local_event.vclock,
                local_trace.body,
            ),
            (
                remote.cortex_id.clone(),
                remote.origin.clone(),
                remote.vclock.clone(),
                data.body,
            ),
        ];
        versions.sort_by(|left, right| left.0.cmp(&right.0));
        let mut body = format!(
            "## Concurrent edits detected\n\n**Trace:** {}\n**Conflicting origins:** {}\n",
            local.id,
            versions
                .iter()
                .map(|(id, name, _, _)| format!("{} ({})", name, &id[..id.len().min(8)]))
                .collect::<Vec<_>>()
                .join(", ")
        );
        for (id, name, clock, version_body) in versions {
            body.push_str(&format!(
                "\n### Version from {} ({})\n**Vector clock:** {}\n\n{}\n",
                name,
                &id[..id.len().min(8)],
                serde_json::to_string(&clock)?,
                version_body.trim_end()
            ));
        }
        let mut divergence = Trace::new(
            format!("Divergence: {}", local.title),
            "divergence",
            "system",
            vec!["divergence".into(), "needs-resolution".into()],
            body.trim_end(),
        );
        divergence.frontmatter.derived_from = vec![local.id.clone()];
        self.add(&mut divergence)?;
        self.store_remote_event(remote)
    }

    fn store_remote_event(&self, event: &Event) -> Result<()> {
        let tx = self.connection.unchecked_transaction()?;
        self.store_remote_event_tx(&tx, event)?;
        tx.commit()?;
        Ok(())
    }

    fn store_remote_event_tx(&self, tx: &Transaction<'_>, event: &Event) -> Result<()> {
        tx.execute(
            "INSERT INTO events(id,action,trace_id,cortex_id,origin,timestamp,data,vclock,signature,pubkey) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)",
            params![event.id,event.action,event.trace_id,event.cortex_id,event.origin,event.timestamp,serde_json::to_string(&event.data)?,serde_json::to_string(&event.vclock)?,event.signature,event.pubkey],
        )?;
        let current: Option<String> = tx
            .query_row(
                "SELECT value FROM federation_state WHERE key='vclock'",
                [],
                |row| row.get(0),
            )
            .optional()?;
        let mut clock: BTreeMap<String, u64> = current
            .and_then(|value| serde_json::from_str(&value).ok())
            .unwrap_or_default();
        federation::merge(&mut clock, &event.vclock);
        tx.execute(
            "INSERT INTO federation_state(key,value) VALUES ('vclock',?1) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            [serde_json::to_string(&clock)?],
        )?;
        Ok(())
    }

    pub fn lineage(&self, id: &str) -> Result<(Vec<String>, Vec<String>)> {
        self.get(id)?;
        let parents = self.lineage_for(id)?;
        let mut statement = self.connection.prepare(
            "SELECT trace_id FROM trace_lineage WHERE derived_from=?1 ORDER BY trace_id",
        )?;
        let children = statement
            .query_map([id], |row| row.get(0))?
            .collect::<rusqlite::Result<_>>()?;
        Ok((parents, children))
    }

    pub fn sync(&self) -> Result<SyncResult> {
        self.sync_with_recovery(false)
    }

    pub fn sync_with_recovery(&self, recover: bool) -> Result<SyncResult> {
        let mut result = SyncResult::default();
        let mut found = BTreeSet::new();
        let now = trace::now_rfc3339();
        for (directory, archived, trashed) in [
            (self.traces_dir(), false, false),
            (self.archive_dir(), true, false),
            (self.trash_dir(), false, true),
        ] {
            let entries = match fs::read_dir(&directory) {
                Ok(entries) => entries,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
                Err(error) => return Err(error.into()),
            };
            for entry in entries {
                let path = entry?.path();
                if path.extension().and_then(|ext| ext.to_str()) != Some("md") {
                    continue;
                }
                let mut trace = match Trace::parse_file(&path) {
                    Ok(trace) => trace,
                    Err(_) => continue,
                };
                let id = trace.frontmatter.id.clone();
                found.insert(id.clone());
                let existing = self.get(&id).ok();
                if let Some(row) = &existing
                    && trace.effective_tier() != row.tier
                {
                    trace.frontmatter.tier = row.tier.clone();
                    trace
                        .write_preserving_updated(&path)
                        .with_context(|| format!("repairing tier for {id}"))?;
                }
                trace.validate()?;

                let archived_at = if archived {
                    Some(
                        existing
                            .as_ref()
                            .map(|row| row.archived_at.as_str())
                            .filter(|value| !value.is_empty())
                            .unwrap_or(&now)
                            .to_owned(),
                    )
                } else {
                    None
                };
                let trashed_at = if trashed {
                    Some(
                        existing
                            .as_ref()
                            .map(|row| row.trashed_at.as_str())
                            .filter(|value| !value.is_empty())
                            .unwrap_or(&now)
                            .to_owned(),
                    )
                } else {
                    None
                };

                let cortex_id = existing
                    .as_ref()
                    .map(|row| row.cortex_id.clone())
                    .filter(|value| !value.is_empty())
                    .unwrap_or_else(|| {
                        if trace.frontmatter.origin.is_empty()
                            || trace.frontmatter.origin == self.name
                        {
                            self.id.clone()
                        } else {
                            self.lookup_cortex_id_for_trace(&id)
                        }
                    });
                let content_hash = trace::content_hash(&trace.body);

                if let Some(row) = existing {
                    if row.tier == "long" {
                        let drifted = row.title != trace.frontmatter.title
                            || row.trace_type != trace.frontmatter.trace_type
                            || row.author != trace.frontmatter.author
                            || row.origin != trace.frontmatter.origin
                            || row.cortex_id != cortex_id
                            || row.content_hash != content_hash
                            || row.updated_at != trace.frontmatter.updated
                            || row.created_at != trace.frontmatter.created
                            || row.source_locked != trace.frontmatter.source_locked
                            || row.source_hash != trace.frontmatter.source_hash;
                        self.connection.execute(
                            "UPDATE traces SET archived_at=?1,trashed_at=?2 WHERE id=?3",
                            params![archived_at, trashed_at, id],
                        )?;
                        if drifted {
                            result.drifted += 1;
                            if result.drifted_ids.len() < 10 {
                                result.drifted_ids.push(id);
                            }
                        } else {
                            result.updated += 1;
                        }
                        continue;
                    }

                    let needs_file_repair = trace.frontmatter.content_hash != content_hash;
                    if needs_file_repair {
                        trace.frontmatter.content_hash = content_hash.clone();
                        trace.frontmatter.updated = trace::now_rfc3339();
                    }
                    let update_db = |pending_key: Option<&str>| -> Result<()> {
                        let tx = self.connection.unchecked_transaction()?;
                        let f = &trace.frontmatter;
                        tx.execute(
                            "UPDATE traces SET title=?1,type=?2,author=?3,origin=?4,cortex_id=?5,updated_at=?6,archived_at=?7,trashed_at=?8,content_hash=?9,source_locked=?10,source_hash=?11 WHERE id=?12",
                            params![f.title,f.trace_type,f.author,f.origin,cortex_id,f.updated,archived_at,trashed_at,content_hash,i64::from(f.source_locked),nullable(&f.source_hash),id],
                        )?;
                        replace_tags(&tx, &id, &f.tags)?;
                        replace_lineage(&tx, &id, &f.derived_from)?;
                        upsert_fts(&tx, &id, &f.title, &trace.body, &f.tags)?;
                        if let Some(pending_key) = pending_key {
                            Self::clear_pending_in_transaction(&tx, pending_key)?;
                        }
                        tx.commit()?;
                        Ok(())
                    };
                    if needs_file_repair {
                        self.replace_trace_transactionally(&path, &trace, |pending_key| {
                            update_db(Some(pending_key))
                        })?;
                    } else {
                        update_db(None)?;
                    }
                    result.updated += 1;
                } else {
                    let needs_file_repair = trace.frontmatter.content_hash != content_hash;
                    if needs_file_repair {
                        trace.frontmatter.content_hash = content_hash.clone();
                        trace.frontmatter.updated = trace::now_rfc3339();
                    }
                    let insert_db = |pending_key: Option<&str>| -> Result<()> {
                        let tx = self.connection.unchecked_transaction()?;
                        let f = &trace.frontmatter;
                        tx.execute(
                            "INSERT INTO traces (id,title,type,tier,author,origin,cortex_id,created_at,updated_at,archived_at,trashed_at,content_hash,source_locked,source_hash) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14)",
                            params![id,f.title,f.trace_type,trace.effective_tier(),f.author,f.origin,cortex_id,f.created,f.updated,archived_at,trashed_at,content_hash,i64::from(f.source_locked),nullable(&f.source_hash)],
                        )?;
                        replace_tags(&tx, &id, &f.tags)?;
                        replace_lineage(&tx, &id, &f.derived_from)?;
                        upsert_fts(&tx, &id, &f.title, &trace.body, &f.tags)?;
                        if let Some(pending_key) = pending_key {
                            Self::clear_pending_in_transaction(&tx, pending_key)?;
                        }
                        tx.commit()?;
                        Ok(())
                    };
                    if needs_file_repair {
                        self.replace_trace_transactionally(&path, &trace, |pending_key| {
                            insert_db(Some(pending_key))
                        })?;
                    } else {
                        insert_db(None)?;
                    }
                    result.added += 1;
                }
            }
        }
        let db_ids: Vec<String> = {
            let mut statement = self.connection.prepare("SELECT id FROM traces")?;
            statement
                .query_map([], |row| row.get(0))?
                .collect::<rusqlite::Result<_>>()?
        };
        for id in db_ids.into_iter().filter(|id| !found.contains(id)) {
            if !recover {
                result.orphaned += 1;
                continue;
            }
            if self
                .recover_orphan_from_event_log(&id)
                .with_context(|| format!("recovering {id}"))?
            {
                result.recovered += 1;
            } else {
                result.orphaned += 1;
            }
        }
        Ok(result)
    }

    fn lookup_cortex_id_for_trace(&self, id: &str) -> String {
        self.connection
            .query_row(
                "SELECT cortex_id FROM events WHERE trace_id=?1 AND cortex_id!='' ORDER BY id DESC LIMIT 1",
                [id],
                |row| row.get(0),
            )
            .unwrap_or_default()
    }

    fn recover_orphan_from_event_log(&self, id: &str) -> Result<bool> {
        let Some(event) = self
            .history(id)?
            .into_iter()
            .rev()
            .find(|event| matches!(event.action.as_str(), "create" | "update"))
        else {
            return Ok(false);
        };
        let data: TraceEventData =
            serde_json::from_value(event.data).context("parsing event snapshot")?;
        let row = self.get(id)?;
        let trace = Trace {
            frontmatter: trace::Frontmatter {
                id: id.to_owned(),
                title: data.title,
                trace_type: data.trace_type,
                tier: row.tier,
                author: data.author,
                tags: data.tags,
                derived_from: data.derived_from,
                origin: data.origin,
                created: row.created_at,
                updated: event.timestamp,
                content_hash: data.content_hash,
                source_hash: data.source_hash,
                source_locked: data.source_locked,
            },
            body: data.body,
        };
        let path = if !row.trashed_at.is_empty() {
            self.trash_dir().join(format!("{id}.md"))
        } else {
            self.trace_file(id, !row.archived_at.is_empty())
        };
        if path.exists() {
            self.replace_trace_transactionally(&path, &trace, |pending_key| {
                let tx = self.connection.unchecked_transaction()?;
                Self::clear_pending_in_transaction(&tx, pending_key)?;
                tx.commit()?;
                Ok(())
            })?;
        } else {
            self.create_trace_transactionally(&path, &trace, |pending_key| {
                let tx = self.connection.unchecked_transaction()?;
                Self::clear_pending_in_transaction(&tx, pending_key)?;
                tx.commit()?;
                Ok(())
            })?;
        }
        Ok(true)
    }

    pub fn get_clock(&self) -> Result<BTreeMap<String, u64>> {
        let value: Option<String> = self
            .connection
            .query_row(
                "SELECT value FROM federation_state WHERE key='vclock'",
                [],
                |row| row.get(0),
            )
            .optional()?;
        Ok(value
            .and_then(|value| serde_json::from_str(&value).ok())
            .unwrap_or_default())
    }

    pub fn database_health(&self) -> Result<(i64, String)> {
        let version = self.connection.query_row(
            "SELECT COALESCE(MAX(version),0) FROM schema_migrations",
            [],
            |row| row.get(0),
        )?;
        let journal = self
            .connection
            .query_row("PRAGMA journal_mode", [], |row| row.get(0))?;
        Ok((version, journal))
    }

    pub fn local_usage_since(
        &self,
        since: &str,
        limit: usize,
    ) -> Result<Vec<federation::TraceUsage>> {
        let mut statement = self.connection.prepare(
            "SELECT trace_id,peer_cortex_id,read_count,modify_count,search_hit_count,last_read_at,updated_at
             FROM trace_usage
             WHERE peer_cortex_id=?1 AND updated_at>?2
             ORDER BY updated_at ASC
             LIMIT ?3",
        )?;
        let rows = statement.query_map(params![self.id, since, limit.max(1) as i64], |row| {
            Ok(federation::TraceUsage {
                trace_id: row.get(0)?,
                peer_cortex_id: row.get(1)?,
                read_count: row.get(2)?,
                modify_count: row.get(3)?,
                search_hit_count: row.get(4)?,
                last_read_at: row.get::<_, Option<String>>(5)?.unwrap_or_default(),
                updated_at: row.get(6)?,
            })
        })?;
        Ok(rows.collect::<rusqlite::Result<Vec<_>>>()?)
    }

    pub fn merge_remote_usage(&self, rows: &[federation::TraceUsage]) -> Result<()> {
        if rows.is_empty() {
            return Ok(());
        }
        for row in rows {
            if !trace::is_valid_id(&row.trace_id)
                || row.peer_cortex_id.is_empty()
                || row.updated_at.is_empty()
                || row.read_count < 0
                || row.modify_count < 0
                || row.search_hit_count < 0
            {
                bail!("invalid remote usage row for trace {:?}", row.trace_id);
            }
        }
        let tx = self.connection.unchecked_transaction()?;
        {
            let mut statement = tx.prepare(
                "INSERT INTO trace_usage(trace_id,peer_cortex_id,read_count,modify_count,search_hit_count,last_read_at,updated_at)
                 VALUES (?1,?2,?3,?4,?5,?6,?7)
                 ON CONFLICT(trace_id,peer_cortex_id) DO UPDATE SET
                   read_count=MAX(read_count,excluded.read_count),
                   modify_count=MAX(modify_count,excluded.modify_count),
                   search_hit_count=MAX(search_hit_count,excluded.search_hit_count),
                   last_read_at=MAX(COALESCE(last_read_at,''),COALESCE(excluded.last_read_at,'')),
                   updated_at=MAX(updated_at,excluded.updated_at)",
            )?;
            for row in rows {
                if row.peer_cortex_id == self.id {
                    continue;
                }
                statement.execute(params![
                    row.trace_id,
                    row.peer_cortex_id,
                    row.read_count,
                    row.modify_count,
                    row.search_hit_count,
                    nullable(&row.last_read_at),
                    row.updated_at,
                ])?;
            }
        }
        tx.commit()?;
        Ok(())
    }

    pub fn federation_state(&self, key: &str) -> Result<String> {
        Ok(self
            .connection
            .query_row(
                "SELECT value FROM federation_state WHERE key=?1",
                [key],
                |row| row.get(0),
            )
            .optional()?
            .unwrap_or_default())
    }

    pub fn set_federation_state(&self, key: &str, value: &str) -> Result<()> {
        self.connection.execute(
            "INSERT INTO federation_state(key,value) VALUES (?1,?2) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            params![key, value],
        )?;
        Ok(())
    }

    pub fn emit_coordination_event(
        &self,
        action: &str,
        window_id: &str,
        data: serde_json::Value,
    ) -> Result<()> {
        if !matches!(
            action,
            "consolidation_claim" | "consolidation_success" | "consolidation_fail"
        ) {
            bail!("unsupported coordination event action {action:?}")
        }
        let tx = self.connection.unchecked_transaction()?;
        self.emit_event(&tx, action, window_id, &trace::now_rfc3339(), data)?;
        tx.commit()?;
        Ok(())
    }

    pub fn delete_federation_state(&self, key: &str) -> Result<()> {
        self.connection
            .execute("DELETE FROM federation_state WHERE key=?1", [key])?;
        Ok(())
    }

    pub fn bump_read(&self, id: &str) -> Result<()> {
        let row = self.get(id)?;
        if row.tier != "long" {
            self.bump_usage(id, true, false)?;
        }
        Ok(())
    }

    fn bump_usage(&self, id: &str, read: bool, modify: bool) -> Result<()> {
        let now = trace::now_rfc3339();
        self.connection.execute(
            "INSERT INTO trace_usage(trace_id,peer_cortex_id,read_count,modify_count,last_read_at,updated_at)
             VALUES (?1,?2,?3,?4,?5,?5)
             ON CONFLICT(trace_id,peer_cortex_id) DO UPDATE SET
               read_count=read_count+excluded.read_count,
               modify_count=modify_count+excluded.modify_count,
               last_read_at=CASE WHEN excluded.read_count>0 THEN excluded.last_read_at ELSE last_read_at END,
               updated_at=excluded.updated_at",
            params![id,self.id,i64::from(read),i64::from(modify),now],
        )?;
        Ok(())
    }

    fn check_source_lock(&self, row: &Row) -> Result<()> {
        if row.source_locked && row.origin != self.name && !self.force_source_lock {
            bail!("trace is source-locked by origin {:?}", row.origin);
        }
        Ok(())
    }

    fn tags_for(&self, id: &str) -> Result<Vec<String>> {
        let mut statement = self
            .connection
            .prepare("SELECT tag FROM trace_tags WHERE trace_id=?1 ORDER BY tag")?;
        Ok(statement
            .query_map([id], |row| row.get(0))?
            .collect::<rusqlite::Result<_>>()?)
    }

    fn lineage_for(&self, id: &str) -> Result<Vec<String>> {
        let mut statement = self.connection.prepare(
            "SELECT derived_from FROM trace_lineage WHERE trace_id=?1 ORDER BY derived_from",
        )?;
        Ok(statement
            .query_map([id], |row| row.get(0))?
            .collect::<rusqlite::Result<_>>()?)
    }

    fn emit_event(
        &self,
        tx: &Transaction<'_>,
        action: &str,
        id: &str,
        timestamp: &str,
        data: serde_json::Value,
    ) -> Result<()> {
        let current: Option<String> = tx
            .query_row(
                "SELECT value FROM federation_state WHERE key='vclock'",
                [],
                |row| row.get(0),
            )
            .optional()?;
        let mut clock: BTreeMap<String, u64> = current
            .and_then(|value| serde_json::from_str(&value).ok())
            .unwrap_or_default();
        if !self.id.is_empty() {
            *clock.entry(self.id.clone()).or_default() += 1;
        }
        let mut event = Event::new(action, id, &self.id, &self.name, data, clock.clone());
        event.timestamp = timestamp.to_owned();
        if let Some(key) = &self.signing_key {
            event.signature = eventsig::sign(key, &event);
            event.pubkey = eventsig::encode_public(&key.verifying_key());
        }
        tx.execute(
            "INSERT INTO events(id,action,trace_id,cortex_id,origin,timestamp,data,vclock,signature,pubkey) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)",
            params![event.id,event.action,event.trace_id,event.cortex_id,event.origin,event.timestamp,serde_json::to_string(&event.data)?,serde_json::to_string(&event.vclock)?,event.signature,event.pubkey],
        )?;
        tx.execute(
            "INSERT INTO federation_state(key,value) VALUES ('vclock',?1) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            [serde_json::to_string(&clock)?],
        )?;
        Ok(())
    }

    fn rebuild_fts_if_stale(&mut self) -> Result<()> {
        let traces: i64 = self
            .connection
            .query_row("SELECT COUNT(*) FROM traces", [], |row| row.get(0))?;
        let fts: i64 = self
            .connection
            .query_row("SELECT COUNT(*) FROM traces_fts", [], |row| row.get(0))?;
        if traces == fts {
            return Ok(());
        }
        let tx = self.connection.unchecked_transaction()?;
        tx.execute("DELETE FROM traces_fts", [])?;
        let ids: Vec<String> = {
            let mut statement = tx.prepare("SELECT id,archived_at,trashed_at FROM traces")?;
            statement
                .query_map([], |row| {
                    Ok((
                        row.get::<_, String>(0)?,
                        row.get::<_, Option<String>>(1)?,
                        row.get::<_, Option<String>>(2)?,
                    ))
                })?
                .collect::<rusqlite::Result<Vec<_>>>()?
                .into_iter()
                .filter_map(|(id, archived, trashed)| {
                    let path = if trashed.is_some() {
                        self.trash_dir().join(format!("{id}.md"))
                    } else {
                        self.trace_file(&id, archived.is_some())
                    };
                    path.exists().then_some(id)
                })
                .collect()
        };
        for id in ids {
            let row = self.get_from_tx(&tx, &id)?;
            let trace = Trace::parse_file(&self.file_path(&row))?;
            upsert_fts(
                &tx,
                &id,
                &trace.frontmatter.title,
                &trace.body,
                &trace.frontmatter.tags,
            )?;
        }
        tx.commit()?;
        Ok(())
    }

    fn get_from_tx(&self, tx: &Transaction<'_>, id: &str) -> Result<Row> {
        Ok(tx.query_row(
            "SELECT id,title,type,tier,author,origin,cortex_id,archived_at,trashed_at,created_at,updated_at,content_hash,source_locked,source_hash FROM traces WHERE id=?1",
            [id], scan_row,
        )?)
    }
}

fn load_sidecar_line(path: &Path, label: &str) -> Result<String> {
    let metadata = fs::metadata(path)
        .with_context(|| format!("reading {label} metadata {}", path.display()))?;
    if !metadata.is_file() {
        bail!("{label} {} is not a regular file", path.display());
    }
    if metadata.len() > 4096 {
        bail!(
            "{label} {} is {} bytes; maximum is 4096",
            path.display(),
            metadata.len()
        );
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if metadata.permissions().mode() & 0o077 != 0 {
            bail!(
                "refusing to read {label} {}: permissions are too broad; expected 0600",
                path.display()
            );
        }
    }
    let contents =
        fs::read_to_string(path).with_context(|| format!("reading {label} {}", path.display()))?;
    let lines: Vec<_> = contents
        .lines()
        .map(str::trim)
        .filter(|line| !line.is_empty())
        .collect();
    match lines.as_slice() {
        [] => bail!("{label} {} is empty or whitespace-only", path.display()),
        [line] => Ok((*line).to_owned()),
        _ => bail!(
            "{label} {} contains {} non-empty lines; only one is supported",
            path.display(),
            lines.len()
        ),
    }
}

fn load_signing_key(dir: &Path, manifest: &Manifest) -> Result<Option<SigningKey>> {
    let Some(config) = &manifest.signing else {
        return Ok(None);
    };
    if config.private_key_file.is_empty() {
        return Ok(None);
    }
    let configured = PathBuf::from(&config.private_key_file);
    let path = if configured.is_absolute() {
        configured
    } else {
        dir.join(configured)
    };
    let seed = load_sidecar_line(&path, "signing key file")?;
    let key = eventsig::signing_key_from_seed(&seed)?;
    let public = eventsig::encode_public(&key.verifying_key());
    if !config.public_key.is_empty() && config.public_key.trim() != public {
        bail!("signing key does not match cortex.md public_key");
    }
    Ok(Some(key))
}

#[derive(Debug, Clone, Default, Deserialize)]
struct TraceEventData {
    title: String,
    #[serde(rename = "type")]
    trace_type: String,
    #[serde(default)]
    author: String,
    #[serde(default)]
    tags: Vec<String>,
    #[serde(default)]
    derived_from: Vec<String>,
    #[serde(default)]
    origin: String,
    #[serde(default)]
    tier: String,
    body: String,
    #[serde(default)]
    content_hash: String,
    #[serde(default)]
    source_hash: String,
    #[serde(default)]
    source_locked: bool,
}

#[derive(Clone, Copy)]
enum Visibility {
    Active,
    Archive,
    Trash,
}

pub fn read_manifest(dir: &Path) -> Result<Manifest> {
    let bytes = fs::read(dir.join("cortex.md"))?;
    let source = std::str::from_utf8(&bytes)?;
    if let Some(rest) = source.strip_prefix("---\n")
        && let Some(end) = rest.find("\n---\n")
    {
        let mut manifest: Manifest = serde_yaml::from_str(&rest[..end])?;
        manifest.body = rest[end + 5..]
            .strip_prefix('\n')
            .unwrap_or(&rest[end + 5..])
            .to_owned();
        validate_manifest(&manifest)?;
        return Ok(manifest);
    }
    let manifest: Manifest = serde_yaml::from_str(source)?;
    validate_manifest(&manifest)?;
    Ok(manifest)
}

pub fn write_manifest(dir: &Path, manifest: &Manifest) -> Result<()> {
    let yaml = serde_yaml::to_string(manifest)?;
    let yaml = yaml.strip_prefix("---\n").unwrap_or(&yaml);
    let body = manifest.body.trim_end_matches('\n');
    let suffix = if body.is_empty() {
        String::new()
    } else {
        format!("\n{body}\n")
    };
    let path = dir.join("cortex.md");
    if path.exists() {
        drop(OpenOptions::new().write(true).open(&path)?);
    }
    let temporary = dir.join(format!(".noema-manifest-{}.tmp", ulid::Ulid::new()));
    let result = (|| -> Result<()> {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&temporary, fs::Permissions::from_mode(0o640))?;
        }
        file.write_all(format!("---\n{yaml}---\n{suffix}").as_bytes())?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temporary, &path)?;
        trace::sync_directory(dir)?;
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(temporary);
    }
    result?;
    Ok(())
}

fn validate_manifest(manifest: &Manifest) -> Result<()> {
    if let Some(federation) = &manifest.federation {
        let mode = if federation.mode.is_empty() {
            "sync"
        } else {
            &federation.mode
        };
        if !["sync", "publish", "subscribe"].contains(&mode) {
            bail!("invalid federation mode {mode:?}");
        }
        let verify = if federation.verify.is_empty() {
            "off"
        } else {
            &federation.verify
        };
        if !["off", "warn", "enforce"].contains(&verify) {
            bail!("invalid federation verification mode {verify:?}");
        }
    }
    if let Some(search) = &manifest.search {
        if !search.default_mode.is_empty()
            && !["lexical", "semantic", "hybrid"].contains(&search.default_mode.as_str())
        {
            bail!("invalid search.default_mode {:?}", search.default_mode);
        }
        if search.semantic_enabled {
            if !(0.0..=1.0).contains(&search.hybrid_weight) {
                bail!(
                    "search.hybrid_weight must be in [0,1], got {}",
                    search.hybrid_weight
                );
            }
            if search.embedding_model.is_empty() {
                bail!(
                    "search.semantic_enabled requires search.embedding_model to be set (no default embedding model is assumed)"
                );
            }
            let endpoint = manifest.resolved_embedding_endpoint()?;
            if endpoint.is_empty() {
                bail!(
                    "search.semantic_enabled requires search.embedding_endpoint (or consolidation.local_llm_endpoint) to be set"
                );
            }
            reqwest::Url::parse(&endpoint).context("invalid search.embedding_endpoint")?;
        }
    }
    Ok(())
}

fn scan_row(row: &rusqlite::Row<'_>) -> rusqlite::Result<Row> {
    Ok(Row {
        id: row.get(0)?,
        title: row.get(1)?,
        trace_type: row.get(2)?,
        tier: row.get(3)?,
        author: row.get::<_, Option<String>>(4)?.unwrap_or_default(),
        origin: row.get(5)?,
        cortex_id: row.get(6)?,
        archived_at: row.get::<_, Option<String>>(7)?.unwrap_or_default(),
        trashed_at: row.get::<_, Option<String>>(8)?.unwrap_or_default(),
        created_at: row.get(9)?,
        updated_at: row.get(10)?,
        content_hash: row.get::<_, Option<String>>(11)?.unwrap_or_default(),
        source_locked: row.get::<_, i64>(12)? != 0,
        source_hash: row.get::<_, Option<String>>(13)?.unwrap_or_default(),
        tags: Vec::new(),
        derived_from: Vec::new(),
    })
}

fn scan_event(row: &rusqlite::Row<'_>) -> rusqlite::Result<Event> {
    let data: String = row.get(6)?;
    let clock: String = row.get(7)?;
    Ok(Event {
        id: row.get(0)?,
        action: row.get(1)?,
        trace_id: row.get(2)?,
        cortex_id: row.get(3)?,
        origin: row.get(4)?,
        timestamp: row.get(5)?,
        data: serde_json::from_str(&data).unwrap_or_else(|_| json!({})),
        vclock: serde_json::from_str(&clock).unwrap_or_default(),
        signature: row.get(8)?,
        pubkey: row.get(9)?,
    })
}

fn effective_limit(limit: usize) -> usize {
    if limit == 0 { 10 } else { limit }
}

fn top_k_cosine(query: &[f32], candidates: Vec<(Row, Vec<f32>)>, limit: usize) -> Vec<ScoredRow> {
    let mut output = candidates
        .into_iter()
        .filter_map(|(row, vector)| {
            embedding::cosine(query, &vector).map(|score| ScoredRow { row, score })
        })
        .collect::<Vec<_>>();
    output.sort_by(|left, right| {
        right
            .score
            .partial_cmp(&left.score)
            .unwrap_or(std::cmp::Ordering::Equal)
    });
    if limit != usize::MAX {
        output.truncate(limit);
    }
    output
}

fn rrf_fuse(
    lexical: Vec<Row>,
    semantic: Vec<ScoredRow>,
    weight: f64,
    limit: usize,
) -> Vec<ScoredRow> {
    const RRF_K: f64 = 60.0;
    let weight = weight.clamp(0.0, 1.0);
    let mut positions = BTreeMap::<String, usize>::new();
    let mut output = Vec::<ScoredRow>::new();
    let mut add = |row: Row, contribution: f64| {
        if let Some(position) = positions.get(&row.id) {
            output[*position].score += contribution;
        } else {
            positions.insert(row.id.clone(), output.len());
            output.push(ScoredRow {
                row,
                score: contribution,
            });
        }
    };
    for (index, row) in lexical.into_iter().enumerate() {
        add(row, (1.0 - weight) / (RRF_K + (index + 1) as f64));
    }
    for (index, scored) in semantic.into_iter().enumerate() {
        add(scored.row, weight / (RRF_K + (index + 1) as f64));
    }
    output.sort_by(|left, right| {
        right
            .score
            .partial_cmp(&left.score)
            .unwrap_or(std::cmp::Ordering::Equal)
            .then_with(|| left.row.id.cmp(&right.row.id))
    });
    output.truncate(limit);
    output
}

fn distinctive_terms(text: &str, limit: usize) -> Vec<String> {
    const STOPWORDS: &[&str] = &[
        "the", "and", "for", "are", "but", "not", "you", "all", "can", "had", "her", "was", "one",
        "our", "out", "day", "get", "has", "him", "his", "how", "man", "new", "now", "old", "see",
        "two", "way", "who", "boy", "did", "its", "let", "put", "say", "she", "too", "use", "any",
        "off", "set", "yet", "that", "with", "this", "from", "they", "have", "will", "your",
        "what", "when", "make", "like", "into", "time", "just", "know", "take", "than", "them",
        "well", "were", "been", "more", "some", "only", "over", "such", "very", "also", "back",
        "each", "even", "find", "give", "good", "most", "much", "must", "name", "need", "next",
        "open", "part", "same", "seem", "show", "tell", "then", "there", "their", "would", "could",
        "should", "about", "after", "again", "before", "being", "these", "those", "where", "which",
        "while", "because", "between", "through", "during",
    ];
    let mut counts = BTreeMap::<String, usize>::new();
    for token in text
        .split(|character: char| !character.is_alphanumeric())
        .filter(|token| !token.is_empty())
    {
        let token = token.to_lowercase();
        if token.len() < 3
            || token.chars().all(|character| character.is_ascii_digit())
            || STOPWORDS.contains(&token.as_str())
        {
            continue;
        }
        *counts.entry(token).or_default() += 1;
    }
    let mut counts = counts.into_iter().collect::<Vec<_>>();
    counts.sort_by(|(left_term, left_count), (right_term, right_count)| {
        right_count
            .cmp(left_count)
            .then_with(|| left_term.cmp(right_term))
    });
    counts
        .into_iter()
        .take(limit)
        .map(|(term, _)| term)
        .collect()
}

fn list_query(
    options: &ListOptions,
    fts: bool,
    fts_query: Option<String>,
) -> Result<(String, Vec<Value>)> {
    let columns = "t.id,t.title,t.type,t.tier,t.author,t.origin,t.cortex_id,t.archived_at,t.trashed_at,t.created_at,t.updated_at,t.content_hash,t.source_locked,t.source_hash";
    let mut sql = if fts {
        format!(
            "SELECT {columns} FROM traces t JOIN traces_fts f ON f.id=t.id WHERE traces_fts MATCH ?"
        )
    } else {
        format!("SELECT {columns} FROM traces t WHERE 1=1")
    };
    let mut values = Vec::new();
    if let Some(query) = fts_query {
        values.push(Value::Text(query));
    }
    match (options.trashed, options.all, options.archived) {
        (true, _, _) => sql.push_str(" AND t.trashed_at IS NOT NULL"),
        (_, true, _) => sql.push_str(" AND t.trashed_at IS NULL"),
        (_, _, true) => sql.push_str(" AND t.archived_at IS NOT NULL AND t.trashed_at IS NULL"),
        _ => sql.push_str(" AND t.archived_at IS NULL AND t.trashed_at IS NULL"),
    }
    for (column, value) in [
        ("t.type", &options.trace_type),
        ("t.author", &options.author),
        ("t.origin", &options.origin),
    ] {
        if !value.is_empty() {
            sql.push_str(&format!(" AND {column}=?"));
            values.push(Value::Text(value.clone()));
        }
    }
    if !options.tag.is_empty() {
        sql.push_str(" AND t.id IN (SELECT trace_id FROM trace_tags WHERE tag=?)");
        values.push(Value::Text(options.tag.clone()));
    }
    if !options.tiers.is_empty() {
        sql.push_str(" AND t.tier IN (");
        sql.push_str(
            &std::iter::repeat_n("?", options.tiers.len())
                .collect::<Vec<_>>()
                .join(","),
        );
        sql.push(')');
        values.extend(options.tiers.iter().cloned().map(Value::Text));
    }
    if fts {
        sql.push_str(" ORDER BY bm25(traces_fts)");
    } else {
        sql.push_str(" ORDER BY t.created_at DESC,t.rowid DESC");
    }
    Ok((sql, values))
}

fn replace_tags(tx: &Transaction<'_>, id: &str, tags: &[String]) -> Result<()> {
    tx.execute("DELETE FROM trace_tags WHERE trace_id=?1", [id])?;
    insert_tags(tx, id, tags)
}

fn insert_tags(tx: &Transaction<'_>, id: &str, tags: &[String]) -> Result<()> {
    for tag in dedupe(tags.to_vec()) {
        tx.execute(
            "INSERT INTO trace_tags(trace_id,tag) VALUES (?1,?2)",
            params![id, tag],
        )?;
    }
    Ok(())
}

fn replace_lineage(tx: &Transaction<'_>, id: &str, sources: &[String]) -> Result<()> {
    tx.execute("DELETE FROM trace_lineage WHERE trace_id=?1", [id])?;
    insert_lineage(tx, id, sources)
}

fn insert_lineage(tx: &Transaction<'_>, id: &str, sources: &[String]) -> Result<()> {
    for source in dedupe(sources.to_vec()) {
        tx.execute(
            "INSERT INTO trace_lineage(trace_id,derived_from) VALUES (?1,?2)",
            params![id, source],
        )?;
    }
    Ok(())
}

fn upsert_fts(
    tx: &Transaction<'_>,
    id: &str,
    title: &str,
    body: &str,
    tags: &[String],
) -> Result<()> {
    tx.execute("DELETE FROM traces_fts WHERE id=?1", [id])?;
    insert_fts(tx, id, title, body, tags)
}

fn insert_fts(
    tx: &Transaction<'_>,
    id: &str,
    title: &str,
    body: &str,
    tags: &[String],
) -> Result<()> {
    tx.execute(
        "INSERT INTO traces_fts(id,title,body,tags) VALUES (?1,?2,?3,?4)",
        params![id, title, body, tags.join(" ")],
    )?;
    Ok(())
}

fn trace_snapshot(trace: &Trace) -> serde_json::Value {
    let f = &trace.frontmatter;
    let mut value = serde_json::Map::new();
    value.insert("title".into(), json!(f.title));
    value.insert("type".into(), json!(f.trace_type));
    if !f.author.is_empty() {
        value.insert("author".into(), json!(f.author));
    }
    if !f.tags.is_empty() {
        value.insert("tags".into(), json!(f.tags));
    }
    if !f.derived_from.is_empty() {
        value.insert("derived_from".into(), json!(f.derived_from));
    }
    if !f.origin.is_empty() {
        value.insert("origin".into(), json!(f.origin));
    }
    let tier = trace.effective_tier();
    if !tier.is_empty() {
        value.insert("tier".into(), json!(tier));
    }
    value.insert("body".into(), json!(trace.body));
    if !f.content_hash.is_empty() {
        value.insert("content_hash".into(), json!(f.content_hash));
    }
    if !f.source_hash.is_empty() {
        value.insert("source_hash".into(), json!(f.source_hash));
    }
    if f.source_locked {
        value.insert("source_locked".into(), json!(true));
    }
    value.into()
}

pub fn parse_since(value: &str) -> Result<Duration> {
    let value = value.trim();
    if value.is_empty() {
        return Ok(Duration::zero());
    }
    if value == "0" {
        return Ok(Duration::zero());
    }
    let (sign, mut rest) = value.strip_prefix('-').map_or_else(
        || {
            value
                .strip_prefix('+')
                .map_or((1.0, value), |rest| (1.0, rest))
        },
        |rest| (-1.0, rest),
    );
    let mut nanoseconds = 0.0;
    while !rest.is_empty() {
        let number_end = rest
            .find(|character: char| !character.is_ascii_digit() && character != '.')
            .unwrap_or(rest.len());
        if number_end == 0 || number_end == rest.len() {
            bail!(invalid_duration(value));
        }
        let amount: f64 = rest[..number_end]
            .parse()
            .map_err(|_| anyhow::anyhow!(invalid_duration(value)))?;
        rest = &rest[number_end..];
        let (unit, tail) = ["ms", "us", "µs", "ns", "w", "d", "h", "m", "s"]
            .into_iter()
            .find_map(|unit| rest.strip_prefix(unit).map(|tail| (unit, tail)))
            .ok_or_else(|| anyhow::anyhow!(invalid_duration(value)))?;
        rest = tail;
        nanoseconds += amount
            * match unit {
                "w" => 7.0 * 24.0 * 60.0 * 60.0 * 1_000_000_000.0,
                "d" => 24.0 * 60.0 * 60.0 * 1_000_000_000.0,
                "h" => 60.0 * 60.0 * 1_000_000_000.0,
                "m" => 60.0 * 1_000_000_000.0,
                "s" => 1_000_000_000.0,
                "ms" => 1_000_000.0,
                "us" | "µs" => 1_000.0,
                "ns" => 1.0,
                _ => unreachable!(),
            };
    }
    nanoseconds *= sign;
    if !nanoseconds.is_finite() || nanoseconds > i64::MAX as f64 || nanoseconds < i64::MIN as f64 {
        bail!("duration {value:?} is out of range");
    }
    Ok(Duration::nanoseconds(nanoseconds as i64))
}

fn invalid_duration(value: &str) -> String {
    format!(
        "invalid duration {value:?}: use compact duration syntax (e.g. 24h, 90m) or days/weeks (7d, 2w)"
    )
}

fn format_duration_label(duration: Duration) -> String {
    let total_seconds = duration.num_seconds();
    if total_seconds == 0 {
        return "0s".into();
    }
    let negative = total_seconds < 0;
    let mut seconds = total_seconds.unsigned_abs();
    let hours = seconds / 3600;
    seconds %= 3600;
    let minutes = seconds / 60;
    seconds %= 60;
    let mut output = if negative {
        String::from("-")
    } else {
        String::new()
    };
    if hours > 0 {
        output.push_str(&format!("{hours}h"));
    }
    if minutes > 0 || hours > 0 {
        output.push_str(&format!("{minutes}m"));
    }
    output.push_str(&format!("{seconds}s"));
    output
}
fn summarize_durations(values: &mut [Duration]) -> PromotionStats {
    if values.is_empty() {
        return PromotionStats::default();
    }
    values.sort();
    let pick = |percent: usize| {
        let rank = (percent * values.len()).div_ceil(100);
        values[rank.saturating_sub(1).min(values.len() - 1)]
    };
    PromotionStats {
        count: values.len(),
        p50: format_duration_label(pick(50)),
        p95: format_duration_label(pick(95)),
    }
}

#[derive(Debug, PartialEq, Eq)]
struct DivergenceSection {
    name: String,
    cortex_id: String,
    body: String,
}

impl DivergenceSection {
    fn label(&self) -> String {
        if self.cortex_id.is_empty() {
            self.name.clone()
        } else {
            format!("{} ({})", self.name, self.cortex_id)
        }
    }

    fn matches(&self, accepted: &str) -> bool {
        accepted == self.name
            || (!self.cortex_id.is_empty()
                && (accepted == self.cortex_id || accepted.starts_with(&self.cortex_id)))
    }
}

fn split_divergence_sections(body: &str) -> Result<Vec<DivergenceSection>> {
    const HEADER: &str = "### Version from ";
    let mut sections = Vec::new();
    let mut rest = body;
    while let Some(index) = rest.find(HEADER) {
        rest = &rest[index + HEADER.len()..];
        let (label, after_label) = rest.split_once('\n').unwrap_or((rest, ""));
        rest = after_label;
        if rest.starts_with("**Vector clock:**") {
            rest = rest.split_once('\n').map_or("", |(_, after)| after);
        }
        let (section_body, after_section) = match rest.find(&format!("\n{HEADER}")) {
            Some(next) => (&rest[..next], &rest[next + 1..]),
            None => (rest, ""),
        };
        let label = label.trim();
        let (name, cortex_id) = if let Some(without_suffix) = label.strip_suffix(')') {
            without_suffix
                .rsplit_once(" (")
                .map(|(name, id)| (name.trim(), id.trim()))
                .unwrap_or((label, ""))
        } else {
            (label, "")
        };
        sections.push(DivergenceSection {
            name: name.to_owned(),
            cortex_id: cortex_id.to_owned(),
            body: section_body.to_owned(),
        });
        rest = after_section;
        if rest.is_empty() {
            break;
        }
    }
    if sections.is_empty() {
        bail!("no '### Version from ' headers found");
    }
    Ok(sections)
}

fn nullable(value: &str) -> Option<&str> {
    (!value.is_empty()).then_some(value)
}

fn dedupe(values: Vec<String>) -> Vec<String> {
    let mut seen = BTreeSet::new();
    values
        .into_iter()
        .filter(|value| !value.is_empty() && seen.insert(value.clone()))
        .collect()
}

pub fn sanitize_fts5_query(query: &str) -> String {
    let mut out = Vec::new();
    for token in query.split_whitespace() {
        if ["AND", "OR", "NOT"].contains(&token)
            || token.starts_with('"')
            || token.ends_with('"')
            || bare_fts_token(token)
        {
            out.push(token.to_owned());
        } else {
            out.push(format!("\"{token}\""));
        }
    }
    let mut joined = out.join(" ");
    if joined.matches('"').count() % 2 != 0 {
        joined = joined.replace('"', "");
    }
    joined
}

fn bare_fts_token(token: &str) -> bool {
    let token = token.strip_suffix('*').unwrap_or(token);
    !token.is_empty() && token.chars().all(|ch| ch.is_alphanumeric() || ch == '_')
}

fn agents_md(manifest: &Manifest) -> String {
    format!(
        "# Noema Cortex — Agent Guide\n\n- **Cortex:** {}\n- **Cortex ID:** {}\n\nTrace markdown files are the source of truth. Use the Noema CLI or MCP server to mutate them.\n",
        manifest.name, manifest.id
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn signed_cortex(parent: &Path, name: &str) -> Cortex {
        let mut manifest = Cortex::create(name, parent).unwrap();
        let root = parent.join(name);
        let (_, public, seed) = eventsig::generate().unwrap();
        let key_path = root.join("noema-signing.key");
        fs::write(&key_path, format!("{seed}\n")).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&key_path, fs::Permissions::from_mode(0o600)).unwrap();
        }
        manifest.signing = Some(SigningConfig {
            public_key: public,
            private_key_file: "noema-signing.key".into(),
        });
        manifest.federation = Some(FederationConfig {
            verify: "enforce".into(),
            ..Default::default()
        });
        write_manifest(&root, &manifest).unwrap();
        let mut cortex = Cortex::open(name, root).unwrap();
        cortex.durability = DurabilityProfile::Strong;
        cortex
    }

    fn event_for(cx: &Cortex, trace_id: &str, action: &str, cortex_id: &str) -> Event {
        cx.history(trace_id)
            .unwrap()
            .into_iter()
            .find(|event| event.action == action && event.cortex_id == cortex_id)
            .unwrap()
    }

    fn cortex() -> (tempfile::TempDir, Cortex) {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("test", temp.path()).unwrap();
        let mut cx = Cortex::open("test", temp.path().join("test")).unwrap();
        cx.durability = DurabilityProfile::Strong;
        (temp, cx)
    }

    #[test]
    fn durability_profile_parser_is_explicit_and_fail_closed() {
        assert_eq!(
            DurabilityProfile::parse(None).unwrap(),
            DurabilityProfile::Standard
        );
        assert_eq!(
            DurabilityProfile::parse(Some("standard")).unwrap(),
            DurabilityProfile::Standard
        );
        assert_eq!(
            DurabilityProfile::parse(Some("strong")).unwrap(),
            DurabilityProfile::Strong
        );
        assert!(DurabilityProfile::parse(Some("fast")).is_err());
    }

    #[test]
    fn standard_durability_skips_recovery_records_and_lock_files() {
        let (_temp, mut cx) = cortex();
        cx.durability = DurabilityProfile::Standard;
        let mut trace = Trace::new(
            "Standard durability",
            "fact",
            "",
            vec!["profile".into()],
            "first body",
        );
        cx.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        trace.body = "updated body".into();
        cx.update_trace(&id, &mut trace, false).unwrap();
        cx.archive(&id).unwrap();
        cx.unarchive(&id).unwrap();

        let pending: i64 = cx
            .connection
            .query_row(
                "SELECT COUNT(*) FROM federation_state WHERE key GLOB 'rust_pending_mutation:*'",
                [],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(pending, 0);
        assert!(!cx.pending_mutation_lock_directory().exists());
        assert_eq!(
            Trace::parse_file(&cx.trace_file(&id, false)).unwrap().body,
            "updated body"
        );
        assert_eq!(cx.durability_profile(), "standard");
    }

    #[test]
    fn parses_observability_windows_and_duration_labels() {
        assert_eq!(parse_since("24h").unwrap(), Duration::hours(24));
        assert_eq!(parse_since("7d").unwrap(), Duration::days(7));
        assert_eq!(parse_since("1h30m").unwrap(), Duration::minutes(90));
        assert_eq!(format_duration_label(Duration::minutes(90)), "1h30m0s");
        assert!(parse_since("tomorrow").is_err());
    }

    #[test]
    fn consolidation_window_defaults_to_twenty_four_hours() {
        assert_eq!(
            ConsolidationConfig::default().effective_window(),
            std::time::Duration::from_secs(24 * 60 * 60)
        );
        assert_eq!(
            ConsolidationConfig {
                window_hours: 72,
                ..Default::default()
            }
            .effective_window(),
            std::time::Duration::from_secs(72 * 60 * 60)
        );
    }

    #[test]
    fn observability_aggregates_usage_events_latency_and_leaks() {
        let (_temp, cx) = cortex();
        let mut source = Trace::new("Source", "fact", "", vec!["alpha".into()], "source");
        cx.add(&mut source).unwrap();
        let mut derived = Trace::new(
            "Derived",
            "observation",
            "",
            vec!["alpha".into(), "beta".into()],
            "derived",
        );
        derived.frontmatter.derived_from = vec![source.frontmatter.id.clone()];
        cx.add(&mut derived).unwrap();
        cx.bump_read(&derived.frontmatter.id).unwrap();
        cx.bump_read(&derived.frontmatter.id).unwrap();
        cx.bump_search_hits(&[cx.get(&derived.frontmatter.id).unwrap()]);
        cx.promote(&derived.frontmatter.id, "mid").unwrap();
        cx.emit_coordination_event(
            "consolidation_fail",
            "window-peer",
            json!({"reason":"peer_outranked"}),
        )
        .unwrap();
        cx.emit_coordination_event(
            "consolidation_fail",
            "window-error",
            json!({"reason":"endpoint_error"}),
        )
        .unwrap();

        let popular = cx.top_searched_traces(10).unwrap();
        assert_eq!(popular.len(), 1);
        assert_eq!(popular[0].id, derived.frontmatter.id);
        assert_eq!((popular[0].search_hits, popular[0].read_count), (1, 2));
        let tags = cx.tag_activity(10).unwrap();
        assert_eq!(tags[0].tag, "alpha");
        assert_eq!(tags[0].search_hits, 1);

        let activity = cx.consolidation_activity(Duration::hours(24)).unwrap();
        assert_eq!(activity.totals.promote, 1);
        assert_eq!(activity.totals.lost_election, 1);
        assert_eq!(activity.totals.fail, 1);
        let latency = cx.promotion_latency().unwrap();
        assert_eq!(latency.short_to_mid.count, 1);
        let leaks = cx.one_source_mid_count().unwrap();
        assert_eq!(leaks.current, 1);
        assert_eq!(leaks.promoted_last_7d, 1);
    }

    #[test]
    fn resolves_divergence_by_origin_or_custom_body() {
        let (_temp, cx) = cortex();
        let mut original = Trace::new("Original", "fact", "", vec![], "old body");
        cx.add(&mut original).unwrap();

        let body = format!(
            "## Concurrent edits detected\n\n**Trace:** {}\n**Conflicting origins:** test (LOCAL123), peer (REMOTE12)\n\n### Version from test (LOCAL123)\n**Vector clock:** {{}}\n\nlocal body\n\n### Version from peer (REMOTE12)\n**Vector clock:** {{}}\n\nremote body",
            original.frontmatter.id
        );
        let mut divergence = Trace::new(
            "Divergence: Original",
            "divergence",
            "system",
            vec!["divergence".into()],
            body,
        );
        divergence.frontmatter.derived_from = vec![original.frontmatter.id.clone()];
        cx.add(&mut divergence).unwrap();
        cx.resolve_divergence(&divergence.frontmatter.id, "peer", "")
            .unwrap();
        assert_eq!(
            cx.get_trace(&original.frontmatter.id).unwrap().1.body,
            "remote body"
        );
        assert!(
            !cx.get(&divergence.frontmatter.id)
                .unwrap()
                .trashed_at
                .is_empty()
        );

        let mut custom = Trace::new(
            "Divergence: Original custom",
            "divergence",
            "system",
            vec!["divergence".into()],
            "unparsed body",
        );
        custom.frontmatter.derived_from = vec![original.frontmatter.id.clone()];
        cx.add(&mut custom).unwrap();
        cx.resolve_divergence(&custom.frontmatter.id, "", "merged body")
            .unwrap();
        assert_eq!(
            cx.get_trace(&original.frontmatter.id).unwrap().1.body,
            "merged body"
        );
        assert!(
            !cx.get(&custom.frontmatter.id)
                .unwrap()
                .trashed_at
                .is_empty()
        );
    }

    #[test]
    fn distilled_trace_preserves_sources_lineage_and_telemetry() {
        let (_temp, cx) = cortex();
        let mut first = Trace::new("source one", "note", "", vec![], "first body");
        let mut second = Trace::new("source two", "note", "", vec![], "second body");
        cx.add(&mut first).unwrap();
        cx.add(&mut second).unwrap();

        let id = cx
            .create_distilled_trace(DistilledTraceSpec {
                title: "Distilled".into(),
                body: "Combined body".into(),
                tags: vec!["memory".into()],
                source_ids: vec![first.frontmatter.id.clone(), second.frontmatter.id.clone()],
                model_name: "fixture-model".into(),
                model_tier_profile: "frontier".into(),
                cohesion_confidence: 0.85,
                ..DistilledTraceSpec::default()
            })
            .unwrap();

        let row = cx.get(&id).unwrap();
        assert_eq!(row.tier, "mid");
        assert_eq!(row.derived_from.len(), 2);
        assert_eq!(cx.get(&first.frontmatter.id).unwrap().tier, "short");
        assert_eq!(cx.get(&second.frontmatter.id).unwrap().tier, "short");
        let history = cx.history(&id).unwrap();
        assert_eq!(
            history
                .iter()
                .map(|event| event.action.as_str())
                .collect::<Vec<_>>(),
            vec!["create", "consolidate"]
        );
        assert_eq!(history[1].data["model_name"], "fixture-model");
        assert_eq!(history[1].data["cohesion_confidence"], 0.85);

        let candidates = cx
            .llm_candidates(Duration::hours(24).to_std().unwrap())
            .unwrap();
        assert!(candidates.is_empty());
    }

    #[test]
    fn consolidate_replay_is_telemetry_only_for_materialized_mid_trace() {
        let (_temp, cx) = cortex();
        let id = "20260815-distilled-replay";
        let mut trace = Trace::new("Distilled replay", "observation", "", vec![], "body");
        trace.frontmatter.id = id.into();
        trace.frontmatter.tier = "mid".into();
        cx.add(&mut trace).unwrap();
        let event = Event::new(
            "consolidate",
            id,
            "01REMOTE",
            "peer",
            json!({"distilled_id":id,"source_ids":["20260815-source-a","20260815-source-b"]}),
            BTreeMap::new(),
        );
        cx.replay_event(&event).unwrap();
        cx.replay_event(&event).unwrap();
        assert_eq!(cx.get(id).unwrap().tier, "mid");
        assert_eq!(
            cx.history(id)
                .unwrap()
                .into_iter()
                .filter(|item| item.action == "consolidate")
                .count(),
            1
        );
    }

    #[test]
    fn create_replay_folds_a_consolidation_that_arrived_first() {
        let (_temp, cx) = cortex();
        let id = "20260815-pending-consolidation";
        let mut snapshot = Trace::new("Pending", "observation", "", vec![], "body");
        snapshot.frontmatter.id = id.into();
        let mut data = trace_snapshot(&snapshot);
        data.as_object_mut().unwrap().remove("tier");

        let mut create = Event::new("create", id, "01REMOTE", "peer", data, BTreeMap::new());
        create.id = "01JR0000000000000000000001".into();
        let mut consolidate = Event::new(
            "consolidate",
            id,
            "01REMOTE",
            "peer",
            json!({"distilled_id":id}),
            BTreeMap::new(),
        );
        consolidate.id = "01JR0000000000000000000002".into();

        cx.replay_event(&consolidate).unwrap();
        cx.replay_event(&create).unwrap();
        assert_eq!(cx.get(id).unwrap().tier, "mid");
        assert_eq!(cx.history(id).unwrap().len(), 2);
    }

    #[test]
    fn auto_distillation_configuration_requires_the_llm_fields() {
        let valid: serde_yaml::Value = serde_yaml::from_str(
            "enabled: true\nauto_distillation_enabled: true\nllm_enabled: true\nlocal_llm_endpoint: http://127.0.0.1:9000/v1\nmodel_name: fixture\n",
        )
        .unwrap();
        let manifest = Manifest {
            consolidation: Some(valid),
            ..Manifest::default()
        };
        assert_eq!(
            manifest
                .consolidation_config()
                .unwrap()
                .unwrap()
                .effective_model_tier(),
            "large"
        );

        for source in [
            "enabled: true\nauto_distillation_enabled: true\nlocal_llm_endpoint: x\nmodel_name: m\n",
            "enabled: true\nauto_distillation_enabled: true\nllm_enabled: true\nmodel_name: m\n",
            "enabled: true\nauto_distillation_enabled: true\nllm_enabled: true\nlocal_llm_endpoint: x\n",
        ] {
            let manifest = Manifest {
                consolidation: Some(serde_yaml::from_str(source).unwrap()),
                ..Manifest::default()
            };
            assert!(manifest.consolidation_config().is_err());
        }
    }

    #[test]
    fn add_search_update_and_archive() {
        let (_temp, cx) = cortex();
        let mut trace = Trace::new(
            "Why Rust",
            "decision",
            "tester",
            vec!["language".into()],
            "ownership and safety",
        );
        cx.add(&mut trace).unwrap();
        assert_eq!(
            cx.search("ownership", &ListOptions::default())
                .unwrap()
                .len(),
            1
        );
        let id = trace.frontmatter.id.clone();
        trace.body = "ownership, safety, and predictable memory".into();
        cx.update_trace(&id, &mut trace, true).unwrap();
        cx.archive(&id).unwrap();
        assert!(
            cx.list(&ListOptions {
                archived: true,
                ..Default::default()
            })
            .unwrap()[0]
                .archived_at
                .len()
                > 10
        );
        assert!(cx.history(&id).unwrap().len() >= 3);
    }

    #[test]
    fn sync_recovery_rebuilds_latest_snapshot_in_current_visibility() {
        let (_temp, cx) = cortex();
        let mut trace = Trace::new(
            "Recover snapshot",
            "note",
            "agent-1",
            vec!["recovery".into()],
            "original body",
        );
        trace.frontmatter.tier = "mid".into();
        cx.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        trace.body = "latest body".into();
        cx.update_trace(&id, &mut trace, false).unwrap();
        cx.archive(&id).unwrap();
        let path = cx.trace_file(&id, true);
        fs::remove_file(&path).unwrap();

        let ordinary = cx.sync().unwrap();
        assert_eq!((ordinary.recovered, ordinary.orphaned), (0, 1));
        let recovered = cx.sync_with_recovery(true).unwrap();
        assert_eq!((recovered.recovered, recovered.orphaned), (1, 0));

        let rebuilt = Trace::parse_file(&path).unwrap();
        let row = cx.get(&id).unwrap();
        assert_eq!(rebuilt.body, "latest body");
        assert_eq!(rebuilt.frontmatter.author, "agent-1");
        assert_eq!(rebuilt.frontmatter.tags, vec!["recovery"]);
        assert_eq!(rebuilt.frontmatter.tier, "mid");
        assert_eq!(rebuilt.frontmatter.updated, row.updated_at);
        assert!(!row.archived_at.is_empty());
    }

    #[test]
    fn sync_reports_long_tier_drift_without_reindexing_content() {
        let (_temp, cx) = cortex();
        let mut trace = Trace::new("Immutable", "fact", "", vec![], "committed");
        cx.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        cx.promote(&id, "mid").unwrap();
        cx.promote(&id, "long").unwrap();
        let before = cx.get(&id).unwrap();
        let path = cx.trace_file(&id, false);
        let mut edited = Trace::parse_file(&path).unwrap();
        edited.body = "out-of-band edit".into();
        edited.write_preserving_updated(&path).unwrap();

        let result = cx.sync().unwrap();
        assert_eq!(result.drifted, 1);
        assert_eq!(result.drifted_ids, vec![id.clone()]);
        let after = cx.get(&id).unwrap();
        assert_eq!(after.content_hash, before.content_hash);
        assert_eq!(after.updated_at, before.updated_at);
        assert_eq!(Trace::parse_file(&path).unwrap().body, "out-of-band edit");
    }

    #[test]
    fn sync_atomically_heals_hash_for_new_files() {
        let (_temp, cx) = cortex();
        let mut trace = Trace::new("Direct file", "note", "", vec![], "on disk");
        trace.frontmatter.origin = cx.name.clone();
        let id = trace.frontmatter.id.clone();
        let path = cx.trace_file(&id, false);
        trace.write_preserving_updated(&path).unwrap();
        assert!(trace.frontmatter.content_hash.is_empty());

        let result = cx.sync().unwrap();
        assert_eq!(result.added, 1);
        let repaired = Trace::parse_file(&path).unwrap();
        let expected = trace::content_hash("on disk");
        assert_eq!(repaired.frontmatter.content_hash, expected);
        assert_eq!(cx.get(&id).unwrap().content_hash, expected);
    }

    #[test]
    fn configured_signing_key_signs_local_events() {
        let temp = tempfile::tempdir().unwrap();
        let mut manifest = Cortex::create("signed", temp.path()).unwrap();
        let root = temp.path().join("signed");
        let (_, public, seed) = eventsig::generate().unwrap();
        let key_path = root.join("noema-signing.key");
        fs::write(&key_path, format!("{seed}\n")).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&key_path, fs::Permissions::from_mode(0o600)).unwrap();
        }
        manifest.signing = Some(SigningConfig {
            public_key: public.clone(),
            private_key_file: "noema-signing.key".into(),
        });
        write_manifest(&root, &manifest).unwrap();

        let cx = Cortex::open("signed", &root).unwrap();
        let mut trace = Trace::new("Signed", "fact", "", vec![], "authenticated event");
        cx.add(&mut trace).unwrap();
        let event = cx
            .connection
            .query_row(
                "SELECT id,action,trace_id,cortex_id,origin,timestamp,data,vclock,signature,pubkey FROM events WHERE trace_id=?1",
                [&trace.frontmatter.id],
                scan_event,
            )
            .unwrap();
        assert_eq!(event.pubkey, public);
        eventsig::verify(&event.pubkey, &event, &event.signature).unwrap();
    }

    #[test]
    fn signed_two_node_replay_handles_ordering_divergence_and_tampering() {
        let temp = tempfile::tempdir().unwrap();
        let alpha = signed_cortex(temp.path(), "alpha");
        let beta = signed_cortex(temp.path(), "beta");

        let mut shared = Trace::new("Shared replay", "fact", "", vec![], "initial");
        alpha.add(&mut shared).unwrap();
        let shared_id = shared.frontmatter.id.clone();
        let create = event_for(&alpha, &shared_id, "create", &alpha.id);
        beta.replay_event(&create).unwrap();
        beta.replay_event(&create).unwrap();
        assert_eq!(beta.get_trace(&shared_id).unwrap().1.body, "initial");
        assert_eq!(beta.history(&shared_id).unwrap().len(), 1);

        let mut tampered = create.clone();
        tampered.data["body"] = json!("tampered");
        assert!(beta.replay_event(&tampered).is_err());
        assert_eq!(beta.get_trace(&shared_id).unwrap().1.body, "initial");

        shared.body = "causal update".into();
        alpha.update_trace(&shared_id, &mut shared, false).unwrap();
        let causal = event_for(&alpha, &shared_id, "update", &alpha.id);
        beta.replay_event(&causal).unwrap();
        assert_eq!(beta.get_trace(&shared_id).unwrap().1.body, "causal update");

        let mut reordered = Trace::new("Reordered replay", "fact", "", vec![], "old");
        alpha.add(&mut reordered).unwrap();
        let reordered_id = reordered.frontmatter.id.clone();
        let older_create = event_for(&alpha, &reordered_id, "create", &alpha.id);
        reordered.body = "newest snapshot".into();
        alpha
            .update_trace(&reordered_id, &mut reordered, false)
            .unwrap();
        let newer_update = event_for(&alpha, &reordered_id, "update", &alpha.id);
        beta.replay_event(&newer_update).unwrap();
        beta.replay_event(&older_create).unwrap();
        assert_eq!(
            beta.get_trace(&reordered_id).unwrap().1.body,
            "newest snapshot"
        );

        shared.body = "alpha concurrent version".into();
        alpha.update_trace(&shared_id, &mut shared, false).unwrap();
        let (_, mut beta_shared) = beta.get_trace(&shared_id).unwrap();
        beta_shared.body = "beta concurrent version".into();
        beta.update_trace(&shared_id, &mut beta_shared, false)
            .unwrap();
        let beta_update = event_for(&beta, &shared_id, "update", &beta.id);
        alpha.replay_event(&beta_update).unwrap();
        let divergences = alpha
            .list(&ListOptions {
                trace_type: "divergence".into(),
                ..Default::default()
            })
            .unwrap();
        assert_eq!(divergences.len(), 1);
        let divergence = alpha.get_trace(&divergences[0].id).unwrap().1;
        assert!(divergence.body.contains("alpha concurrent version"));
        assert!(divergence.body.contains("beta concurrent version"));

        let mut locked = Trace::new("Locked replay", "fact", "", vec![], "owned");
        locked.frontmatter.source_locked = true;
        alpha.add(&mut locked).unwrap();
        let locked_id = locked.frontmatter.id.clone();
        let locked_create = event_for(&alpha, &locked_id, "create", &alpha.id);
        beta.replay_event(&locked_create).unwrap();
        let (_, mut foreign) = beta.get_trace(&locked_id).unwrap();
        foreign.body = "unauthorized".into();
        assert!(beta.update_trace(&locked_id, &mut foreign, false).is_err());
    }

    #[test]
    fn signed_coordination_events_replay_without_trace_materialization() {
        let temp = tempfile::tempdir().unwrap();
        let alpha = signed_cortex(temp.path(), "alpha");
        let beta = signed_cortex(temp.path(), "beta");
        let window = ulid::Ulid::new().to_string();
        alpha
            .emit_coordination_event(
                "consolidation_claim",
                &window,
                json!({"window_id":window,"cortex_id":alpha.id}),
            )
            .unwrap();
        let event = event_for(&alpha, &window, "consolidation_claim", &alpha.id);
        eventsig::verify(&event.pubkey, &event, &event.signature).unwrap();

        beta.replay_event(&event).unwrap();
        beta.replay_event(&event).unwrap();

        assert_eq!(beta.history(&window).unwrap().len(), 1);
        assert!(beta.get(&window).is_err());
    }

    #[test]
    fn replay_applies_metadata_visibility_tier_vote_and_purge_events() {
        let temp = tempfile::tempdir().unwrap();
        let alpha = signed_cortex(temp.path(), "alpha");
        let beta = signed_cortex(temp.path(), "beta");
        let mut trace = Trace::new("Lifecycle", "fact", "", vec!["old".into()], "body");
        alpha.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        beta.replay_event(&event_for(&alpha, &id, "create", &alpha.id))
            .unwrap();

        let mut tag_update = Event::new(
            "tag_update",
            &id,
            &alpha.id,
            &alpha.name,
            json!({"tags":["new","new","federated"]}),
            alpha.get_clock().unwrap(),
        );
        tag_update.pubkey = alpha.manifest.signing.as_ref().unwrap().public_key.clone();
        tag_update.signature = eventsig::sign(alpha.signing_key.as_ref().unwrap(), &tag_update);
        beta.replay_event(&tag_update).unwrap();
        assert_eq!(
            beta.get(&id).unwrap().tags,
            vec!["federated".to_string(), "new".to_string()]
        );

        alpha.archive(&id).unwrap();
        beta.replay_event(&event_for(&alpha, &id, "archive", &alpha.id))
            .unwrap();
        assert!(!beta.get(&id).unwrap().archived_at.is_empty());
        let mut unrelated_recover = Event::new(
            "recover",
            &id,
            &alpha.id,
            &alpha.name,
            json!({}),
            alpha.get_clock().unwrap(),
        );
        unrelated_recover.pubkey = alpha.manifest.signing.as_ref().unwrap().public_key.clone();
        unrelated_recover.signature =
            eventsig::sign(alpha.signing_key.as_ref().unwrap(), &unrelated_recover);
        beta.replay_event(&unrelated_recover).unwrap();
        assert!(!beta.get(&id).unwrap().archived_at.is_empty());
        alpha.unarchive(&id).unwrap();
        beta.replay_event(&event_for(&alpha, &id, "unarchive", &alpha.id))
            .unwrap();
        assert!(beta.get(&id).unwrap().archived_at.is_empty());

        alpha.promote(&id, "mid").unwrap();
        beta.replay_event(&event_for(&alpha, &id, "promote", &alpha.id))
            .unwrap();
        assert_eq!(beta.get(&id).unwrap().tier, "mid");
        alpha.vote(&id, 1, "human").unwrap();
        beta.replay_event(&event_for(&alpha, &id, "vote", &alpha.id))
            .unwrap();
        let votes: i64 = beta
            .connection
            .query_row("SELECT tier_votes FROM traces WHERE id=?1", [&id], |row| {
                row.get(0)
            })
            .unwrap();
        assert_eq!(votes, 1);

        alpha.trash(&id).unwrap();
        beta.replay_event(&event_for(&alpha, &id, "trash", &alpha.id))
            .unwrap();
        assert!(!beta.get(&id).unwrap().trashed_at.is_empty());
        alpha.recover(&id).unwrap();
        beta.replay_event(&event_for(&alpha, &id, "recover", &alpha.id))
            .unwrap();
        assert!(beta.get(&id).unwrap().trashed_at.is_empty());

        let mut purge = Event::new(
            "purge",
            &id,
            &alpha.id,
            &alpha.name,
            json!({}),
            alpha.get_clock().unwrap(),
        );
        purge.pubkey = alpha.manifest.signing.as_ref().unwrap().public_key.clone();
        purge.signature = eventsig::sign(alpha.signing_key.as_ref().unwrap(), &purge);
        beta.replay_event(&purge).unwrap();
        assert!(beta.get(&id).is_err());
        assert!(
            beta.history(&id)
                .unwrap()
                .iter()
                .any(|event| event.id == purge.id)
        );
    }

    #[test]
    fn admin_purge_enforces_tier_and_preserves_auditable_long_term_semantics() {
        let (_temp, cx) = cortex();
        let mut trace = Trace::new("Ceremonial purge", "fact", "", vec!["keep".into()], "body");
        trace.frontmatter.tier = "long".into();
        cx.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        let path = cx.trace_file(&id, false);

        let mismatch = cx
            .admin_purge(&id, "retention request", "mid", false)
            .unwrap_err();
        assert!(mismatch.to_string().contains("tier mismatch"));
        assert!(path.exists());

        cx.admin_purge(&id, "retention request", "long", false)
            .unwrap();
        let (purged_at, reason): (String, String) = cx
            .connection
            .query_row(
                "SELECT purged_at,purge_reason FROM traces WHERE id=?1",
                [&id],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .unwrap();
        assert!(!purged_at.is_empty());
        assert_eq!(reason, "retention request");
        assert!(!path.exists());
        assert_eq!(
            cx.history(&id).unwrap().last().unwrap().action,
            "purge_long_term"
        );
        assert!(
            cx.connection
                .execute("UPDATE traces SET title='forbidden' WHERE id=?1", [&id])
                .is_err()
        );

        cx.admin_purge(&id, "erase tombstone", "long", true)
            .unwrap();
        assert!(cx.get(&id).is_err());
        assert_eq!(
            cx.history(&id).unwrap().last().unwrap().action,
            "purge_hard"
        );
    }

    #[test]
    fn event_backfill_previews_skips_and_commits_idempotently() {
        let (_temp, cx) = cortex();
        let mut active = Trace::new("Backfill active", "fact", "", vec![], "body");
        let mut archived = Trace::new("Backfill archived", "fact", "", vec![], "body");
        cx.add(&mut active).unwrap();
        cx.add(&mut archived).unwrap();
        cx.archive(&archived.frontmatter.id).unwrap();
        cx.connection.execute("DELETE FROM events", []).unwrap();

        let preview = cx.backfill_create_events(true).unwrap();
        assert_eq!(preview.backfilled_ids, vec![active.frontmatter.id.clone()]);
        assert_eq!(preview.skipped_ids, vec![archived.frontmatter.id.clone()]);
        assert!(cx.history(&active.frontmatter.id).unwrap().is_empty());

        let committed = cx.backfill_create_events(false).unwrap();
        assert_eq!(committed, preview);
        assert_eq!(
            cx.history(&active.frontmatter.id).unwrap()[0].action,
            "create"
        );
        let second = cx.backfill_create_events(false).unwrap();
        assert!(second.backfilled_ids.is_empty());
        assert_eq!(second.skipped_ids, vec![archived.frontmatter.id.clone()]);
    }

    #[test]
    fn expiry_purge_emits_replayable_event_and_zero_days_uses_default_retention() {
        let (_temp, mut cx) = cortex();
        let mut trace = Trace::new("Expired trash", "note", "", vec![], "body");
        cx.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        cx.trash(&id).unwrap();
        cx.connection
            .execute(
                "UPDATE traces SET trashed_at='2000-01-01T00:00:00Z' WHERE id=?1",
                [&id],
            )
            .unwrap();

        assert_eq!(cx.purge_expired(0).unwrap(), 1);
        assert!(cx.get(&id).is_err());
        assert_eq!(cx.history(&id).unwrap().last().unwrap().action, "purge");
        assert!(!cx.trash_dir().join(format!("{id}.md")).exists());
    }

    #[test]
    fn replay_handles_long_term_hard_purge_and_telemetry_actions() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("alpha", temp.path()).unwrap();
        Cortex::create("beta", temp.path()).unwrap();
        let alpha = Cortex::open("alpha", temp.path().join("alpha")).unwrap();
        let beta = Cortex::open("beta", temp.path().join("beta")).unwrap();

        let mut trace = Trace::new("Federated purge", "fact", "", vec![], "body");
        trace.frontmatter.tier = "long".into();
        alpha.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        beta.replay_event(&event_for(&alpha, &id, "create", &alpha.id))
            .unwrap();
        alpha
            .admin_purge(&id, "remote retention", "long", false)
            .unwrap();
        beta.replay_event(&event_for(&alpha, &id, "purge_long_term", &alpha.id))
            .unwrap();
        let purged: String = beta
            .connection
            .query_row("SELECT purged_at FROM traces WHERE id=?1", [&id], |row| {
                row.get(0)
            })
            .unwrap();
        assert!(!purged.is_empty());

        alpha
            .admin_purge(&id, "remote erasure", "long", true)
            .unwrap();
        beta.replay_event(&event_for(&alpha, &id, "purge_hard", &alpha.id))
            .unwrap();
        assert!(beta.get(&id).is_err());

        for action in ["consolidate_fallback", "divergence_long_term"] {
            let event = Event::new(
                action,
                &id,
                &alpha.id,
                &alpha.name,
                json!({}),
                BTreeMap::new(),
            );
            beta.replay_event(&event).unwrap();
            assert!(
                beta.history(&id)
                    .unwrap()
                    .iter()
                    .any(|item| item.id == event.id)
            );
        }
    }

    #[test]
    fn signed_replay_rejects_foreign_mutation_of_source_locked_trace() {
        let temp = tempfile::tempdir().unwrap();
        let alpha = signed_cortex(temp.path(), "alpha");
        let beta = signed_cortex(temp.path(), "beta");
        let gamma = signed_cortex(temp.path(), "gamma");
        let mut trace = Trace::new("Locked source", "fact", "", vec![], "original");
        trace.frontmatter.source_locked = true;
        alpha.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        beta.replay_event(&event_for(&alpha, &id, "create", &alpha.id))
            .unwrap();

        trace.body = "unauthorized".into();
        trace.frontmatter.content_hash = trace::content_hash(&trace.body);
        let mut attack = Event::new(
            "update",
            &id,
            &gamma.id,
            &gamma.name,
            trace_snapshot(&trace),
            gamma.get_clock().unwrap(),
        );
        attack.pubkey = gamma.manifest.signing.as_ref().unwrap().public_key.clone();
        attack.signature = eventsig::sign(gamma.signing_key.as_ref().unwrap(), &attack);
        let error = beta.replay_event(&attack).unwrap_err();
        assert!(error.to_string().contains("source-lock violation"));
        assert_eq!(beta.get_trace(&id).unwrap().1.body, "original");
        assert!(
            !beta
                .history(&id)
                .unwrap()
                .iter()
                .any(|event| event.id == attack.id)
        );
    }

    #[test]
    fn usage_rows_publish_only_local_contributions_and_merge_monotonically() {
        let temp = tempfile::tempdir().unwrap();
        let alpha = signed_cortex(temp.path(), "alpha");
        let beta = signed_cortex(temp.path(), "beta");
        let mut trace = Trace::new("Usage", "fact", "", vec![], "body");
        alpha.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        beta.replay_event(&event_for(&alpha, &id, "create", &alpha.id))
            .unwrap();

        alpha.bump_read(&id).unwrap();
        alpha.bump_search_hits(&[alpha.get(&id).unwrap()]);
        trace.body = "modified".into();
        alpha.update_trace(&id, &mut trace, true).unwrap();

        let rows = alpha.local_usage_since("", 100).unwrap();
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0].peer_cortex_id, alpha.id);
        assert_eq!(rows[0].read_count, 1);
        assert_eq!(rows[0].modify_count, 1);
        assert_eq!(rows[0].search_hit_count, 1);
        beta.merge_remote_usage(&rows).unwrap();

        let counts: (i64, i64, i64) = beta
            .connection
            .query_row(
                "SELECT read_count,modify_count,search_hit_count FROM trace_usage WHERE trace_id=?1 AND peer_cortex_id=?2",
                params![id, alpha.id],
                |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
            )
            .unwrap();
        assert_eq!(counts, (1, 1, 1));

        let mut stale = rows[0].clone();
        stale.read_count = 0;
        stale.modify_count = 0;
        stale.search_hit_count = 0;
        stale.updated_at = "2099-01-01T00:00:00Z".into();
        beta.merge_remote_usage(&[stale]).unwrap();
        let counts: (i64, i64, i64) = beta
            .connection
            .query_row(
                "SELECT read_count,modify_count,search_hit_count FROM trace_usage WHERE trace_id=?1 AND peer_cortex_id=?2",
                params![id, alpha.id],
                |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
            )
            .unwrap();
        assert_eq!(counts, (1, 1, 1));

        beta.bump_read(&id).unwrap();
        let beta_rows = beta.local_usage_since("", 100).unwrap();
        assert_eq!(beta_rows.len(), 1);
        assert_eq!(beta_rows[0].peer_cortex_id, beta.id);
    }

    #[test]
    fn manifest_framing_matches_public_shape() {
        let temp = tempfile::tempdir().unwrap();
        let manifest = Cortex::create("test", temp.path()).unwrap();
        let parsed = read_manifest(&temp.path().join("test")).unwrap();
        assert_eq!(parsed.id, manifest.id);
        assert_eq!(parsed.version, 2);
    }

    #[test]
    fn access_sidecars_are_single_line_private_files_with_safe_fingerprints() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("access.key");
        fs::write(&path, "  shared-secret  \n").unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
        }
        assert_eq!(
            load_sidecar_line(&path, "access key file").unwrap(),
            "shared-secret"
        );
        assert_eq!(
            key_fingerprint("abc"),
            "SHA256:ba:78:16:bf:8f:01:cf:ea:41:41:40:de:5d:ae:22:23:b0:03:61:a3:96:17:7a:9c:b4:10:ff:61:f2:00:15:ad"
        );

        fs::write(&path, "first\nsecond\n").unwrap();
        assert!(load_sidecar_line(&path, "access key file").is_err());
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&path, fs::Permissions::from_mode(0o644)).unwrap();
            assert!(load_sidecar_line(&path, "access key file").is_err());
        }
    }

    #[test]
    fn cosine_ranking_skips_dimension_mismatches() {
        let candidates = vec![
            (
                Row {
                    id: "a".into(),
                    ..Default::default()
                },
                vec![1.0, 0.0, 0.0],
            ),
            (
                Row {
                    id: "b".into(),
                    ..Default::default()
                },
                vec![0.0, 1.0, 0.0],
            ),
            (
                Row {
                    id: "c".into(),
                    ..Default::default()
                },
                vec![0.7, 0.7, 0.0],
            ),
            (
                Row {
                    id: "d".into(),
                    ..Default::default()
                },
                vec![1.0, 0.0],
            ),
        ];
        let ranked = top_k_cosine(&[1.0, 0.0, 0.0], candidates, 2);
        assert_eq!(
            ranked
                .iter()
                .map(|item| item.row.id.as_str())
                .collect::<Vec<_>>(),
            ["a", "c"]
        );
    }

    #[test]
    fn reciprocal_rank_fusion_uses_stable_tie_breaks() {
        let row = |id: &str| Row {
            id: id.into(),
            ..Default::default()
        };
        let lexical = vec![row("a"), row("b"), row("c")];
        let semantic = vec![
            ScoredRow {
                row: row("c"),
                score: 1.0,
            },
            ScoredRow {
                row: row("d"),
                score: 0.5,
            },
            ScoredRow {
                row: row("a"),
                score: 0.1,
            },
        ];
        let fused = rrf_fuse(lexical, semantic, 0.5, 4);
        assert_eq!(
            fused
                .iter()
                .map(|item| item.row.id.as_str())
                .collect::<Vec<_>>(),
            ["a", "c", "b", "d"]
        );
    }

    #[test]
    fn distinctive_terms_match_frequency_then_alphabetical_order() {
        assert_eq!(
            distinctive_terms("the beta alpha beta 123 gamma alpha", 3),
            ["alpha", "beta", "gamma"]
        );
    }

    #[test]
    fn failed_database_update_restores_exact_trace_bytes() {
        let (_temp, cx) = cortex();
        let mut trace = Trace::new("Stable", "fact", "", vec!["before".into()], "original body");
        cx.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        let path = cx.trace_file(&id, false);
        let original_bytes = fs::read(&path).unwrap();
        let original_permissions = fs::metadata(&path).unwrap().permissions();
        let original_row = cx.get(&id).unwrap();
        let original_events = cx.history(&id).unwrap().len();
        cx.connection
            .execute_batch(
                "CREATE TRIGGER fail_trace_update
                 BEFORE UPDATE OF title ON traces
                 BEGIN
                   SELECT RAISE(ABORT, 'injected update failure');
                 END;",
            )
            .unwrap();

        let (_, mut changed) = cx.get_trace(&id).unwrap();
        changed.frontmatter.title = "Must Roll Back".into();
        changed.frontmatter.tags = vec!["after".into()];
        changed.body = "replacement body".into();
        assert!(cx.update_trace(&id, &mut changed, true).is_err());

        assert_eq!(fs::read(&path).unwrap(), original_bytes);
        assert_eq!(
            fs::metadata(&path).unwrap().permissions(),
            original_permissions
        );
        let current = cx.get(&id).unwrap();
        assert_eq!(current.title, original_row.title);
        assert_eq!(current.updated_at, original_row.updated_at);
        assert_eq!(current.content_hash, original_row.content_hash);
        assert_eq!(current.tags, original_row.tags);
        assert_eq!(cx.history(&id).unwrap().len(), original_events);
    }

    #[test]
    fn failed_visibility_transaction_moves_trace_back() {
        let (_temp, cx) = cortex();
        let mut trace = Trace::new("Visible", "fact", "", vec![], "active body");
        cx.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        let active = cx.trace_file(&id, false);
        let archived = cx.trace_file(&id, true);
        let original_bytes = fs::read(&active).unwrap();
        let original_events = cx.history(&id).unwrap().len();
        cx.connection
            .execute_batch(
                "CREATE TRIGGER fail_visibility_update
                 BEFORE UPDATE OF archived_at, trashed_at ON traces
                 BEGIN
                   SELECT RAISE(ABORT, 'injected visibility failure');
                 END;",
            )
            .unwrap();

        assert!(cx.archive(&id).is_err());

        assert_eq!(fs::read(&active).unwrap(), original_bytes);
        assert!(!archived.exists());
        let current = cx.get(&id).unwrap();
        assert!(current.archived_at.is_empty());
        assert!(current.trashed_at.is_empty());
        assert_eq!(cx.history(&id).unwrap().len(), original_events);
    }

    #[test]
    fn failed_tier_transaction_restores_trace_frontmatter() {
        let (_temp, cx) = cortex();
        let mut trace = Trace::new("Tiered", "fact", "", vec![], "short body");
        cx.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        let path = cx.trace_file(&id, false);
        let original_bytes = fs::read(&path).unwrap();
        let original_events = cx.history(&id).unwrap().len();
        cx.connection
            .execute_batch(
                "CREATE TRIGGER fail_tier_update
                 BEFORE UPDATE OF tier ON traces
                 BEGIN
                   SELECT RAISE(ABORT, 'injected tier failure');
                 END;",
            )
            .unwrap();

        assert!(cx.promote(&id, "mid").is_err());

        assert_eq!(fs::read(&path).unwrap(), original_bytes);
        assert_eq!(cx.get(&id).unwrap().tier, "short");
        assert_eq!(cx.history(&id).unwrap().len(), original_events);
    }

    #[test]
    fn failed_remote_update_restores_materialized_trace() {
        let temp = tempfile::tempdir().unwrap();
        let alpha = signed_cortex(temp.path(), "alpha");
        let beta = signed_cortex(temp.path(), "beta");
        let mut trace = Trace::new(
            "Remote stable",
            "fact",
            "",
            vec!["before".into()],
            "original remote body",
        );
        alpha.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        beta.replay_event(&event_for(&alpha, &id, "create", &alpha.id))
            .unwrap();
        let path = beta.trace_file(&id, false);
        let original_bytes = fs::read(&path).unwrap();
        let original_permissions = fs::metadata(&path).unwrap().permissions();
        let original_row = beta.get(&id).unwrap();
        let original_events = beta.history(&id).unwrap().len();

        trace.frontmatter.title = "Remote replacement".into();
        trace.frontmatter.tags = vec!["after".into()];
        trace.body = "replacement remote body".into();
        alpha.update_trace(&id, &mut trace, false).unwrap();
        let update = event_for(&alpha, &id, "update", &alpha.id);
        beta.connection
            .execute_batch(
                "CREATE TRIGGER fail_remote_trace_update
                 BEFORE UPDATE OF title ON traces
                 BEGIN
                   SELECT RAISE(ABORT, 'injected remote update failure');
                 END;",
            )
            .unwrap();

        assert!(beta.replay_event(&update).is_err());

        assert_eq!(fs::read(&path).unwrap(), original_bytes);
        assert_eq!(
            fs::metadata(&path).unwrap().permissions(),
            original_permissions
        );
        let current = beta.get(&id).unwrap();
        assert_eq!(current.title, original_row.title);
        assert_eq!(current.updated_at, original_row.updated_at);
        assert_eq!(current.content_hash, original_row.content_hash);
        assert_eq!(current.tags, original_row.tags);
        assert_eq!(beta.history(&id).unwrap().len(), original_events);
    }

    #[test]
    fn failed_remote_create_removes_newly_materialized_trace() {
        let temp = tempfile::tempdir().unwrap();
        let alpha = signed_cortex(temp.path(), "alpha");
        let beta = signed_cortex(temp.path(), "beta");
        let mut trace = Trace::new("Remote new", "fact", "", vec![], "remote body");
        alpha.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        let create = event_for(&alpha, &id, "create", &alpha.id);
        let path = beta.trace_file(&id, false);
        beta.connection
            .execute_batch(
                "CREATE TRIGGER fail_remote_trace_insert
                 BEFORE INSERT ON traces
                 BEGIN
                   SELECT RAISE(ABORT, 'injected remote create failure');
                 END;",
            )
            .unwrap();

        assert!(beta.replay_event(&create).is_err());

        assert!(!path.exists());
        assert!(beta.get(&id).is_err());
        assert!(beta.history(&id).unwrap().is_empty());

        let orphan_bytes = b"orphan content that must survive rollback\n";
        fs::write(&path, orphan_bytes).unwrap();
        let orphan_permissions = fs::metadata(&path).unwrap().permissions();
        assert!(beta.replay_event(&create).is_err());
        assert_eq!(fs::read(&path).unwrap(), orphan_bytes);
        assert_eq!(
            fs::metadata(&path).unwrap().permissions(),
            orphan_permissions
        );
        assert!(beta.get(&id).is_err());
        assert!(beta.history(&id).unwrap().is_empty());
    }

    #[test]
    fn failed_hard_delete_restores_exact_trace_file() {
        let (_temp, cx) = cortex();
        let mut trace = Trace::new("Delete rollback", "fact", "", vec![], "preserved body");
        cx.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        let path = cx.trace_file(&id, false);
        let original_bytes = fs::read(&path).unwrap();
        let original_permissions = fs::metadata(&path).unwrap().permissions();
        let original_events = cx.history(&id).unwrap().len();
        cx.connection
            .execute_batch(
                "CREATE TRIGGER fail_hard_delete
                 BEFORE DELETE ON traces
                 BEGIN
                   SELECT RAISE(ABORT, 'injected hard delete failure');
                 END;",
            )
            .unwrap();

        assert!(cx.remove_hard(&id).is_err());

        assert_eq!(fs::read(&path).unwrap(), original_bytes);
        assert_eq!(
            fs::metadata(&path).unwrap().permissions(),
            original_permissions
        );
        assert!(cx.get(&id).is_ok());
        assert_eq!(cx.history(&id).unwrap().len(), original_events);
    }

    #[test]
    fn failed_external_delete_reconstruction_removes_uncommitted_trash_file() {
        let (_temp, cx) = cortex();
        let mut trace = Trace::new("External delete", "fact", "", vec![], "recoverable body");
        cx.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        let active = cx.trace_file(&id, false);
        let trash = cx.trash_dir().join(format!("{id}.md"));
        let original_events = cx.history(&id).unwrap().len();
        fs::remove_file(active).unwrap();
        cx.connection
            .execute_batch(
                "CREATE TRIGGER fail_external_delete_visibility
                 BEFORE UPDATE OF archived_at, trashed_at ON traces
                 BEGIN
                   SELECT RAISE(ABORT, 'injected external delete failure');
                 END;",
            )
            .unwrap();

        assert!(cx.ingest_external_delete(&id).is_err());

        assert!(!trash.exists());
        let row = cx.get(&id).unwrap();
        assert!(row.archived_at.is_empty());
        assert!(row.trashed_at.is_empty());
        assert_eq!(cx.history(&id).unwrap().len(), original_events);
    }

    #[test]
    fn failed_remote_purge_restores_exact_trashed_trace() {
        let temp = tempfile::tempdir().unwrap();
        let alpha = signed_cortex(temp.path(), "alpha");
        let beta = signed_cortex(temp.path(), "beta");
        let mut trace = Trace::new("Remote purge", "fact", "", vec![], "trashed body");
        alpha.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        beta.replay_event(&event_for(&alpha, &id, "create", &alpha.id))
            .unwrap();
        alpha.trash(&id).unwrap();
        beta.replay_event(&event_for(&alpha, &id, "trash", &alpha.id))
            .unwrap();
        let path = beta.trash_dir().join(format!("{id}.md"));
        let original_bytes = fs::read(&path).unwrap();
        let original_permissions = fs::metadata(&path).unwrap().permissions();
        let original_events = beta.history(&id).unwrap().len();
        let mut purge = Event::new(
            "purge",
            &id,
            &alpha.id,
            &alpha.name,
            json!({}),
            alpha.get_clock().unwrap(),
        );
        purge.pubkey = alpha.manifest.signing.as_ref().unwrap().public_key.clone();
        purge.signature = eventsig::sign(alpha.signing_key.as_ref().unwrap(), &purge);
        beta.connection
            .execute_batch(
                "CREATE TRIGGER fail_remote_purge
                 BEFORE DELETE ON traces
                 BEGIN
                   SELECT RAISE(ABORT, 'injected remote purge failure');
                 END;",
            )
            .unwrap();

        assert!(beta.replay_event(&purge).is_err());

        assert_eq!(fs::read(&path).unwrap(), original_bytes);
        assert_eq!(
            fs::metadata(&path).unwrap().permissions(),
            original_permissions
        );
        assert!(beta.get(&id).is_ok());
        assert_eq!(beta.history(&id).unwrap().len(), original_events);
    }

    #[test]
    fn strong_durability_rejects_concurrent_same_trace_create_before_file_write() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("shared", temp.path()).unwrap();
        let root = temp.path().join("shared");
        let mut first = Cortex::open("shared", &root).unwrap();
        let mut second = Cortex::open("shared", &root).unwrap();
        first.durability = DurabilityProfile::Strong;
        second.durability = DurabilityProfile::Strong;
        let mut trace = Trace::new("Serialized create", "fact", "", vec![], "body");
        let path = first.trace_file(&trace.frontmatter.id, false);
        let _held = first.acquire_trace_mutation_lock(&path).unwrap();

        assert!(second.add(&mut trace).is_err());
        assert!(!path.exists());
        assert!(first.get(&trace.frontmatter.id).is_err());
    }
}
