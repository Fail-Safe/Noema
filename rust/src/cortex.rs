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

use crate::{
    config::{Config, CortexEntry},
    db,
    event::Event,
    eventsig,
    trace::{self, Trace},
};

pub const MANIFEST_VERSION: u32 = 2;
pub const MAX_SEARCH_QUERY_LEN: usize = 1000;

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
    let metadata = fs::metadata(&path)
        .with_context(|| format!("reading signing key metadata {}", path.display()))?;
    if !metadata.is_file() || metadata.len() > 4096 {
        bail!("signing key file must be a regular file no larger than 4096 bytes");
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if metadata.permissions().mode() & 0o077 != 0 {
            bail!("signing key file permissions are too broad; expected 0600");
        }
    }
    let contents = fs::read_to_string(&path)?;
    let seed = contents
        .lines()
        .map(str::trim)
        .find(|line| !line.is_empty())
        .ok_or_else(|| anyhow::anyhow!("signing key file is empty"))?;
    let key = eventsig::signing_key_from_seed(seed)?;
    let public = eventsig::encode_public(&key.verifying_key());
    if !config.public_key.is_empty() && config.public_key.trim() != public {
        bail!("signing key does not match cortex.md public_key");
    }
    Ok(Some(key))
}

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
    json!({
        "title":f.title,"type":f.trace_type,"author":f.author,"tags":f.tags,
        "derived_from":f.derived_from,"origin":f.origin,"tier":trace.effective_tier(),
        "body":trace.body,"content_hash":f.content_hash,"source_hash":f.source_hash,
        "source_locked":f.source_locked
    })
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
    fn manifest_framing_matches_go_shape() {
        let temp = tempfile::tempdir().unwrap();
        let manifest = Cortex::create("test", temp.path()).unwrap();
        let parsed = read_manifest(&temp.path().join("test")).unwrap();
        assert_eq!(parsed.id, manifest.id);
        assert_eq!(parsed.version, 2);
    }
}
