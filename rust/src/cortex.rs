use std::{
    collections::{BTreeMap, BTreeSet},
    fs,
    path::{Path, PathBuf},
};

use anyhow::{Context, Result, bail};
use chrono::{Duration, Utc};
use ed25519_dalek::SigningKey;
use rusqlite::{
    Connection, OptionalExtension, Transaction, params, params_from_iter, types::Value,
};
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::{Digest, Sha256};

use crate::{
    config::{Config, CortexEntry},
    db,
    event::Event,
    eventsig,
    federation::{self, Relation},
    trace::{self, Trace},
};

pub const MANIFEST_VERSION: u32 = 2;
pub const MAX_SEARCH_QUERY_LEN: usize = 1000;
pub const ACCESS_KEY_ENV: &str = "NOEMA_MCP_KEY";

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
    pub local_llm_endpoint: String,
    #[serde(default)]
    pub watchdog_timeout: String,
}

impl ConsolidationConfig {
    pub fn has_trigger(&self) -> bool {
        !self.cron.is_empty() || self.idle_minutes != 0 || self.threshold_short != 0
    }
}

impl Manifest {
    pub fn consolidation_config(&self) -> Result<Option<ConsolidationConfig>> {
        self.consolidation
            .as_ref()
            .map(|value| {
                serde_yaml::from_value(value.clone()).context("parsing consolidation configuration")
            })
            .transpose()
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
pub struct SyncResult {
    pub added: usize,
    pub updated: usize,
    pub orphaned: usize,
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
        };
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
                "unknown cortex {:?} — run `noema init --name {selected}` first",
                selected
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
        if self.get(&trace.frontmatter.id).is_ok() {
            bail!("trace ID {:?} already exists", trace.frontmatter.id);
        }
        let path = self.trace_file(&trace.frontmatter.id, false);
        trace.write(&path)?;
        let result = self.insert_trace(trace, true);
        if result.is_err() {
            let _ = fs::remove_file(path);
        }
        result
    }

    fn insert_trace(&self, trace: &Trace, emit: bool) -> Result<()> {
        let f = &trace.frontmatter;
        let tx = self.connection.unchecked_transaction()?;
        tx.execute(
            "INSERT INTO traces (id,title,type,tier,author,origin,cortex_id,created_at,updated_at,content_hash,source_locked,source_hash)
             VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12)",
            params![f.id, f.title, f.trace_type, trace.effective_tier(), f.author, f.origin, self.id, f.created, f.updated, f.content_hash, i64::from(f.source_locked), nullable(&f.source_hash)],
        )?;
        replace_tags(&tx, &f.id, &f.tags)?;
        replace_lineage(&tx, &f.id, &f.derived_from)?;
        upsert_fts(&tx, &f.id, &f.title, &trace.body, &f.tags)?;
        if emit {
            self.emit_event(&tx, "create", &f.id, &f.created, trace_snapshot(trace))?;
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
        if query.chars().count() > MAX_SEARCH_QUERY_LEN {
            bail!("search query too long");
        }
        let fts = sanitize_fts5_query(query);
        if fts.trim().is_empty() {
            return Ok(Vec::new());
        }
        let (sql, values) = list_query(options, true, Some(fts))?;
        self.query_rows(&sql, values)
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
        trace.write_preserving_updated(&path)?;
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
        tx.commit()?;
        if actor_agent {
            self.bump_usage(id, false, true)?;
        }
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

    fn move_trace(&self, id: &str, destination: Visibility) -> Result<()> {
        let row = self.get(id)?;
        self.check_source_lock(&row)?;
        let source = self.file_path(&row);
        let target = match destination {
            Visibility::Active => self.trace_file(id, false),
            Visibility::Archive => self.trace_file(id, true),
            Visibility::Trash => self.trash_dir().join(format!("{id}.md")),
        };
        if source != target {
            fs::rename(&source, &target)
                .with_context(|| format!("moving {} to {}", source.display(), target.display()))?;
        }
        let now = trace::now_rfc3339();
        let (action, archived, trashed) = match destination {
            Visibility::Active if !row.trashed_at.is_empty() => ("recover", None, None),
            Visibility::Active => ("unarchive", None, None),
            Visibility::Archive => ("archive", Some(now.as_str()), None),
            Visibility::Trash => ("trash", None, Some(now.as_str())),
        };
        let tx = self.connection.unchecked_transaction()?;
        tx.execute(
            "UPDATE traces SET archived_at=?1,trashed_at=?2 WHERE id=?3",
            params![archived, trashed, id],
        )?;
        self.emit_event(&tx, action, id, &now, json!({}))?;
        tx.commit()?;
        Ok(())
    }

    pub fn remove_hard(&self, id: &str) -> Result<()> {
        let row = self.get(id)?;
        self.check_source_lock(&row)?;
        let _ = fs::remove_file(self.file_path(&row));
        self.connection
            .execute("DELETE FROM traces WHERE id=?1", [id])?;
        Ok(())
    }

    pub fn purge_expired(&mut self, days: u32) -> Result<usize> {
        let cutoff = (Utc::now() - Duration::days(days.into())).to_rfc3339();
        let ids: Vec<String> = {
            let mut statement = self.connection.prepare("SELECT id FROM traces WHERE trashed_at IS NOT NULL AND trashed_at < ?1 AND tier != 'long'")?;
            statement
                .query_map([cutoff], |row| row.get(0))?
                .collect::<rusqlite::Result<_>>()?
        };
        for id in &ids {
            let _ = fs::remove_file(self.trash_dir().join(format!("{id}.md")));
            self.connection
                .execute("DELETE FROM traces WHERE id=?1", [id])?;
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
        let mut trace = Trace::parse_file(&self.file_path(row))?;
        trace.frontmatter.tier = to.to_owned();
        trace.frontmatter.updated = row.updated_at.clone();
        trace.write_preserving_updated(&self.file_path(row))?;
        let tx = self.connection.unchecked_transaction()?;
        tx.execute("UPDATE traces SET tier=?1 WHERE id=?2", params![to, row.id])?;
        self.emit_event(
            &tx,
            action,
            &row.id,
            &trace::now_rfc3339(),
            json!({"from": row.tier, "to": to}),
        )?;
        tx.commit()?;
        Ok(())
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

    pub fn history(&self, id: &str) -> Result<Vec<Event>> {
        let mut statement = self.connection.prepare(
            "SELECT id,action,trace_id,cortex_id,origin,timestamp,data,vclock,signature,pubkey FROM events WHERE trace_id=?1 ORDER BY id",
        )?;
        let rows = statement.query_map([id], scan_event)?;
        Ok(rows.collect::<rusqlite::Result<_>>()?)
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

    pub fn replay_event(&self, event: &Event) -> Result<()> {
        self.verify_replay_event(event)?;
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
            "vote" => self.replay_vote(event),
            "purge" => self.replay_purge(event),
            action => bail!("federation experiment does not replay action {action:?}"),
        }
    }

    fn verify_replay_event(&self, event: &Event) -> Result<()> {
        let mode = self
            .manifest
            .federation
            .as_ref()
            .map(|config| config.verify.as_str())
            .filter(|mode| !mode.is_empty())
            .unwrap_or("off");
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
        trace.write_preserving_updated(&self.file_path(&row))?;
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
        tx.commit()?;
        Ok(())
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
        if source != target {
            fs::rename(&source, &target)
                .with_context(|| format!("moving {} to {}", source.display(), target.display()))?;
        }
        let tx = self.connection.unchecked_transaction()?;
        tx.execute(
            "UPDATE traces SET archived_at=?1,trashed_at=?2 WHERE id=?3",
            params![archived, trashed, event.trace_id],
        )?;
        self.store_remote_event_tx(&tx, event)?;
        tx.commit()?;
        Ok(())
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
        let mut trace = Trace::parse_file(&self.file_path(&row))?;
        trace.frontmatter.tier = data.to.clone();
        trace.frontmatter.updated = row.updated_at.clone();
        trace.write_preserving_updated(&self.file_path(&row))?;
        let tx = self.connection.unchecked_transaction()?;
        tx.execute(
            "UPDATE traces SET tier=?1 WHERE id=?2",
            params![data.to, event.trace_id],
        )?;
        self.store_remote_event_tx(&tx, event)?;
        tx.commit()?;
        Ok(())
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
        let _ = fs::remove_file(self.trash_dir().join(format!("{}.md", event.trace_id)));
        let tx = self.connection.unchecked_transaction()?;
        tx.execute("DELETE FROM traces_fts WHERE id=?1", [&event.trace_id])?;
        tx.execute("DELETE FROM traces WHERE id=?1", [&event.trace_id])?;
        self.store_remote_event_tx(&tx, event)?;
        tx.commit()?;
        Ok(())
    }

    fn materialize_remote_snapshot(&self, event: &Event, existing: Option<Row>) -> Result<()> {
        let data: TraceEventData = serde_json::from_value(event.data.clone())?;
        let content_hash = trace::content_hash(&data.body);
        if !data.content_hash.is_empty() && data.content_hash != content_hash {
            bail!("content hash mismatch in remote event {}", event.id);
        }
        let tier = existing
            .as_ref()
            .map(|row| row.tier.clone())
            .or_else(|| (!data.tier.is_empty()).then(|| data.tier.clone()))
            .unwrap_or_else(|| "short".into());
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
        trace.write_preserving_updated(&path)?;
        let tx = self.connection.unchecked_transaction()?;
        let f = &trace.frontmatter;
        if existing.is_some() {
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
        let mut result = SyncResult::default();
        let mut found = BTreeSet::new();
        for (directory, archived, trashed) in [
            (self.traces_dir(), false, false),
            (self.archive_dir(), true, false),
            (self.trash_dir(), false, true),
        ] {
            for entry in fs::read_dir(directory)? {
                let path = entry?.path();
                if path.extension().and_then(|ext| ext.to_str()) != Some("md") {
                    continue;
                }
                let trace = Trace::parse_file(&path)?;
                trace.validate()?;
                let id = trace.frontmatter.id.clone();
                found.insert(id.clone());
                if self.get(&id).is_ok() {
                    self.reindex_trace(&trace, archived, trashed)?;
                    result.updated += 1;
                } else {
                    self.insert_trace(&trace, false)?;
                    self.connection.execute(
                        "UPDATE traces SET archived_at=?1,trashed_at=?2 WHERE id=?3",
                        params![
                            archived.then(trace::now_rfc3339),
                            trashed.then(trace::now_rfc3339),
                            id
                        ],
                    )?;
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
        result.orphaned = db_ids.iter().filter(|id| !found.contains(*id)).count();
        Ok(result)
    }

    fn reindex_trace(&self, trace: &Trace, archived: bool, trashed: bool) -> Result<()> {
        let f = &trace.frontmatter;
        let tx = self.connection.unchecked_transaction()?;
        tx.execute(
            "UPDATE traces SET title=?1,type=?2,tier=?3,author=?4,origin=?5,updated_at=?6,content_hash=?7,source_locked=?8,source_hash=?9,archived_at=?10,trashed_at=?11 WHERE id=?12",
            params![f.title,f.trace_type,trace.effective_tier(),f.author,f.origin,f.updated,trace::content_hash(&trace.body),i64::from(f.source_locked),nullable(&f.source_hash),archived.then(trace::now_rfc3339),trashed.then(trace::now_rfc3339),f.id],
        )?;
        replace_tags(&tx, &f.id, &f.tags)?;
        replace_lineage(&tx, &f.id, &f.derived_from)?;
        upsert_fts(&tx, &f.id, &f.title, &trace.body, &f.tags)?;
        tx.commit()?;
        Ok(())
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
    fs::write(dir.join("cortex.md"), format!("---\n{yaml}---\n{suffix}"))?;
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
    if let Some(search) = &manifest.search
        && !search.default_mode.is_empty()
        && !["lexical", "semantic", "hybrid"].contains(&search.default_mode.as_str())
    {
        bail!("invalid search.default_mode {:?}", search.default_mode);
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
        Cortex::open(name, root).unwrap()
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
        let cx = Cortex::open("test", temp.path().join("test")).unwrap();
        (temp, cx)
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
    fn manifest_framing_matches_go_shape() {
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
}
