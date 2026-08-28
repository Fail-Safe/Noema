use anyhow::{Context, Result, bail};
use notify::{Config, Event, EventKind, RecommendedWatcher, RecursiveMode, Watcher};
use serde_yaml::Value;
use std::{
    collections::{BTreeMap, BTreeSet, HashMap},
    fs,
    path::{Path, PathBuf},
    sync::mpsc::{self, RecvTimeoutError, SyncSender},
    thread::JoinHandle,
    time::{Duration, Instant, SystemTime},
};
use tokio_util::sync::CancellationToken;

use crate::{
    cortex::{Cortex, Row},
    trace::{self, Frontmatter, Trace},
};

#[derive(Clone, Copy)]
pub struct Settings {
    pub debounce: Duration,
    pub auto_onboard: bool,
}

pub struct WatchScheduler {
    cancellation: CancellationToken,
    handle: Option<JoinHandle<()>>,
}

impl WatchScheduler {
    pub fn start(
        cortex_name: String,
        cortex_dir: PathBuf,
        settings: Settings,
        cancellation: CancellationToken,
    ) -> Result<Self> {
        let (ready_sender, ready_receiver) = mpsc::sync_channel(1);
        let worker_cancellation = cancellation.clone();
        let handle = std::thread::spawn(move || {
            if let Err(error) = serve(
                cortex_name,
                cortex_dir,
                settings,
                worker_cancellation,
                ready_sender,
            ) {
                eprintln!("Noema watcher stopped: {error:#}");
            }
        });
        match ready_receiver.recv_timeout(Duration::from_secs(5)) {
            Ok(Ok(())) => {}
            Ok(Err(error)) => {
                cancellation.cancel();
                let _ = handle.join();
                bail!("starting watcher: {error}")
            }
            Err(error) => {
                cancellation.cancel();
                let _ = handle.join();
                bail!("waiting for watcher startup: {error}")
            }
        }
        Ok(Self {
            cancellation,
            handle: Some(handle),
        })
    }

    pub fn stop(mut self) {
        self.cancellation.cancel();
        if let Some(handle) = self.handle.take() {
            let _ = handle.join();
        }
    }
}

fn serve(
    cortex_name: String,
    cortex_dir: PathBuf,
    settings: Settings,
    cancellation: CancellationToken,
    ready: SyncSender<std::result::Result<(), String>>,
) -> Result<()> {
    let (sender, receiver) = mpsc::channel();
    let mut watcher = match RecommendedWatcher::new(sender, Config::default()) {
        Ok(watcher) => watcher,
        Err(error) => {
            let _ = ready.send(Err(error.to_string()));
            return Err(error.into());
        }
    };
    for relative in ["traces", "archive/traces", "trash/traces"] {
        if let Err(error) = watcher.watch(&cortex_dir.join(relative), RecursiveMode::Recursive) {
            let _ = ready.send(Err(error.to_string()));
            return Err(error.into());
        }
    }
    let cortex = match Cortex::open(cortex_name, &cortex_dir) {
        Ok(cortex) => cortex,
        Err(error) => {
            let _ = ready.send(Err(format!("{error:#}")));
            return Err(error);
        }
    };
    let mut reconciler = Reconciler::new(cortex, settings);
    let mut pending = HashMap::<PathBuf, Instant>::new();
    let scan_interval = settings
        .debounce
        .saturating_mul(5)
        .max(Duration::from_millis(250))
        .min(Duration::from_secs(2));
    let mut snapshot = match scan_snapshot(&cortex_dir) {
        Ok(snapshot) => snapshot,
        Err(error) => {
            let _ = ready.send(Err(format!("{error:#}")));
            return Err(error);
        }
    };
    let mut next_scan = Instant::now() + scan_interval;
    let _ = ready.send(Ok(()));
    eprintln!(
        "[watch] active, debounce={}ms, dirs=[traces archive trash]",
        settings.debounce.as_millis()
    );

    while !cancellation.is_cancelled() {
        let timeout = pending
            .values()
            .min()
            .map(|due| due.saturating_duration_since(Instant::now()))
            .unwrap_or(Duration::from_millis(50))
            .min(next_scan.saturating_duration_since(Instant::now()))
            .min(Duration::from_millis(50));
        match receiver.recv_timeout(timeout) {
            Ok(Ok(event)) => {
                if event.need_rescan() {
                    next_scan = Instant::now();
                }
                if event_needs_reconcile(&event) {
                    let due = Instant::now() + settings.debounce;
                    for path in event.paths.into_iter().filter(|path| is_trace_file(path)) {
                        pending.insert(path, due);
                    }
                }
            }
            Ok(Err(error)) => eprintln!("[watch] notify error: {error}"),
            Err(RecvTimeoutError::Timeout) => {}
            Err(RecvTimeoutError::Disconnected) => break,
        }

        let now = Instant::now();
        if now >= next_scan {
            match scan_snapshot(&cortex_dir) {
                Ok(current) => {
                    let due = now + settings.debounce;
                    for path in changed_paths(&snapshot, &current) {
                        pending.insert(path, due);
                    }
                    snapshot = current;
                }
                Err(error) => eprintln!("[watch] fallback scan failed: {error:#}"),
            }
            next_scan = now + scan_interval;
        }

        let now = Instant::now();
        let mut ready: Vec<_> = pending
            .iter()
            .filter_map(|(path, due)| (*due <= now).then_some(path.clone()))
            .collect();
        ready.sort();
        let reconciled = !ready.is_empty();
        for path in ready {
            pending.remove(&path);
            if let Err(error) = reconciler.reconcile(&path, &cancellation) {
                eprintln!("[watch] reconcile {}: {error:#}", path.display());
            }
        }
        if reconciled {
            match scan_snapshot(&cortex_dir) {
                Ok(current) => {
                    let due = Instant::now() + settings.debounce;
                    for path in changed_paths(&snapshot, &current) {
                        pending.insert(path, due);
                    }
                    snapshot = current;
                }
                Err(error) => eprintln!("[watch] post-reconcile scan failed: {error:#}"),
            }
        }
    }
    Ok(())
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum TraceDir {
    Active,
    Archive,
    Trash,
}

struct Reconciler {
    cortex: Cortex,
    settings: Settings,
    last_skip: HashMap<PathBuf, String>,
}

impl Reconciler {
    fn new(cortex: Cortex, settings: Settings) -> Self {
        Self {
            cortex,
            settings,
            last_skip: HashMap::new(),
        }
    }

    fn reconcile(&mut self, path: &Path, cancellation: &CancellationToken) -> Result<()> {
        let Some(directory) = self.classify_dir(path) else {
            return Ok(());
        };
        let Some(id) = path.file_stem().and_then(|value| value.to_str()) else {
            return Ok(());
        };
        let exists = match fs::metadata(path) {
            Ok(metadata) => metadata.is_file(),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => false,
            Err(error) => return Err(error).context("reading watched path metadata"),
        };
        let mut row = if trace::is_valid_id(id) {
            get_optional(&self.cortex, id)?
        } else {
            None
        };

        if !exists && row.is_some() {
            let deadline = Instant::now() + self.settings.debounce;
            while Instant::now() < deadline {
                if cancellation.is_cancelled() {
                    return Ok(());
                }
                std::thread::sleep(Duration::from_millis(5));
            }
            if path.exists() {
                return Ok(());
            }
        }

        if exists {
            self.reconcile_existing(path, id, directory, row.take())
        } else {
            self.reconcile_missing(id, directory, row.as_ref())
        }
    }

    fn reconcile_existing(
        &mut self,
        path: &Path,
        id: &str,
        directory: TraceDir,
        row: Option<Row>,
    ) -> Result<()> {
        let parsed = Trace::parse_file(path);
        let valid = parsed
            .as_ref()
            .is_ok_and(|trace| trace.frontmatter.id == id && trace.validate().is_ok());
        if !valid {
            if let (Err(error), Some(existing)) = (&parsed, row.as_ref())
                && directory == TraceDir::Active
                && error.to_string().contains("missing frontmatter delimiter")
                && !source_locked_foreign(existing, &self.cortex.name)
            {
                match self.heal_file(path, existing) {
                    Ok(()) => {
                        self.last_skip.remove(path);
                        eprintln!("[watch] healed frontmatter-wiped trace: {id}");
                    }
                    Err(error) => self.log_skip(path, format!("heal failed: {error:#}")),
                }
                return Ok(());
            }
            if self.settings.auto_onboard && directory == TraceDir::Active && row.is_none() {
                match self.onboard_file(path) {
                    Ok(mut trace) => {
                        self.cortex.add(&mut trace)?;
                        self.last_skip.remove(path);
                        eprintln!("[watch] auto-onboarded {}", trace.frontmatter.id);
                    }
                    Err(error) => self.log_skip(path, format!("auto-onboard failed: {error:#}")),
                }
                return Ok(());
            }
            let reason = match parsed {
                Err(error) => error.to_string(),
                Ok(trace) if trace.frontmatter.id != id => format!(
                    "frontmatter id {:?} does not match filename",
                    trace.frontmatter.id
                ),
                Ok(trace) => trace.validate().unwrap_err().to_string(),
            };
            self.log_skip(path, reason);
            return Ok(());
        }

        let trace = parsed.expect("valid parse checked above");
        let Some(mut existing) = row else {
            if directory != TraceDir::Active {
                self.log_skip(path, "new trace in non-active dir".into());
                return Ok(());
            }
            let mut trace = trace;
            self.cortex.add(&mut trace)?;
            self.last_skip.remove(path);
            eprintln!("[watch] ingested external create: {id}");
            return Ok(());
        };

        if source_locked_foreign(&existing, &self.cortex.name) {
            return Ok(());
        }
        match directory {
            TraceDir::Active => {
                if !existing.trashed_at.is_empty() {
                    self.cortex.mark_recovered_no_move(id)?;
                    existing = self.cortex.get(id)?;
                } else if !existing.archived_at.is_empty() {
                    self.cortex.mark_unarchived_no_move(id)?;
                    existing = self.cortex.get(id)?;
                }
                if trace::content_hash(&trace.body) != existing.content_hash
                    || metadata_drift(&trace, &existing)
                {
                    self.cortex.update_from_file(id)?;
                }
            }
            TraceDir::Archive => {
                if !existing.trashed_at.is_empty() {
                    self.cortex.mark_recovered_no_move(id)?;
                    existing = self.cortex.get(id)?;
                }
                if existing.archived_at.is_empty() {
                    self.cortex.mark_archived_no_move(id)?;
                    existing = self.cortex.get(id)?;
                }
                if trace::content_hash(&trace.body) != existing.content_hash
                    || metadata_drift(&trace, &existing)
                {
                    self.cortex.update_from_file(id)?;
                }
            }
            TraceDir::Trash if existing.trashed_at.is_empty() => {
                self.cortex.mark_trashed_no_move(id)?;
            }
            TraceDir::Trash => {}
        }
        self.last_skip.remove(path);
        Ok(())
    }

    fn reconcile_missing(
        &mut self,
        id: &str,
        directory: TraceDir,
        row: Option<&Row>,
    ) -> Result<()> {
        let Some(row) = row else {
            return Ok(());
        };
        if self.exists_in_any_dir(id) {
            return Ok(());
        }
        match directory {
            TraceDir::Active if row.archived_at.is_empty() && row.trashed_at.is_empty() => {
                self.cortex.ingest_external_delete(id)?;
            }
            TraceDir::Archive if !row.archived_at.is_empty() && row.trashed_at.is_empty() => {
                self.cortex.ingest_external_delete(id)?;
            }
            TraceDir::Trash if !row.trashed_at.is_empty() => {
                self.cortex.apply_external_purge(id)?;
            }
            _ => {}
        }
        Ok(())
    }

    fn classify_dir(&self, path: &Path) -> Option<TraceDir> {
        match path.parent() {
            Some(parent) if parent == self.cortex.traces_dir() => Some(TraceDir::Active),
            Some(parent) if parent == self.cortex.archive_dir() => Some(TraceDir::Archive),
            Some(parent) if parent == self.cortex.trash_dir() => Some(TraceDir::Trash),
            _ => None,
        }
    }

    fn exists_in_any_dir(&self, id: &str) -> bool {
        [
            self.cortex.trace_file(id, false),
            self.cortex.trace_file(id, true),
            self.cortex.trash_dir().join(format!("{id}.md")),
        ]
        .iter()
        .any(|path| path.exists())
    }

    fn log_skip(&mut self, path: &Path, reason: String) {
        if self.last_skip.get(path) == Some(&reason) {
            return;
        }
        self.last_skip.insert(path.to_path_buf(), reason.clone());
        eprintln!("[watch] skipping {}: {reason}", path.display());
    }

    fn onboard_file(&self, path: &Path) -> Result<Trace> {
        let data = fs::read(path)?;
        if data.iter().take(8192).any(|byte| *byte == 0) {
            bail!("file does not look like UTF-8 text");
        }
        let source = std::str::from_utf8(&data).context("file is not UTF-8 text")?;
        if source.trim().is_empty() {
            bail!("file is empty");
        }
        let (partial, body) = partial_trace(source);
        let title = partial
            .title
            .filter(|value| !value.trim().is_empty())
            .or_else(|| extract_h1(body))
            .or_else(|| clean_filename_stem(path))
            .unwrap_or_else(|| "Untitled".into());
        let trace_type = partial
            .trace_type
            .filter(|value| trace::VALID_TYPES.contains(&value.as_str()))
            .unwrap_or_else(|| "note".into());
        let original = path
            .file_name()
            .and_then(|value| value.to_str())
            .unwrap_or("unknown.md");
        let body = format!(
            "> Auto-onboarded from `{original}`\n\n{}",
            body.trim_start_matches('\n')
        );
        let mut trace = Trace::new(
            title,
            trace_type,
            partial.author.unwrap_or_default(),
            partial.tags,
            body,
        );
        trace.frontmatter.extra = partial.extra;
        let target = self.cortex.trace_file(&trace.frontmatter.id, false);
        if target != path && target.exists() {
            bail!("target path already exists: {}", target.display());
        }
        trace.write(&target)?;
        if target != path {
            fs::remove_file(path)?;
        }
        Ok(trace)
    }

    fn heal_file(&self, path: &Path, row: &Row) -> Result<()> {
        let body = fs::read_to_string(path)?;
        if body.trim().is_empty() {
            bail!("file is empty");
        }
        let mut trace = Trace {
            frontmatter: Frontmatter {
                id: row.id.clone(),
                title: row.title.clone(),
                trace_type: row.trace_type.clone(),
                tier: row.tier.clone(),
                author: row.author.clone(),
                tags: row.tags.clone(),
                derived_from: row.derived_from.clone(),
                origin: row.origin.clone(),
                created: row.created_at.clone(),
                updated: row.updated_at.clone(),
                content_hash: trace::content_hash(&body),
                source_hash: row.source_hash.clone(),
                source_locked: row.source_locked,
                extra: Default::default(),
            },
            body,
        };
        trace.write(path)?;
        self.cortex.update_from_file(&row.id)
    }
}

#[derive(Default)]
struct PartialFrontmatter {
    title: Option<String>,
    trace_type: Option<String>,
    author: Option<String>,
    tags: Vec<String>,
    extra: BTreeMap<String, Value>,
}

fn partial_trace(source: &str) -> (PartialFrontmatter, &str) {
    let Some(rest) = source.strip_prefix("---\n") else {
        return (PartialFrontmatter::default(), source);
    };
    let Some(end) = rest.find("\n---\n") else {
        return (PartialFrontmatter::default(), source);
    };
    let partial = serde_yaml::from_str(&rest[..end])
        .ok()
        .and_then(partial_frontmatter)
        .unwrap_or_default();
    let body = rest[end + 5..]
        .strip_prefix('\n')
        .unwrap_or(&rest[end + 5..]);
    (partial, body)
}

fn partial_frontmatter(value: Value) -> Option<PartialFrontmatter> {
    let Value::Mapping(mapping) = value else {
        return None;
    };
    let mut partial = PartialFrontmatter::default();
    for (key, value) in mapping {
        let Some(key) = key.as_str() else {
            continue;
        };
        match key {
            "title" => partial.title = yaml_string(&value),
            "type" => partial.trace_type = yaml_string(&value),
            "author" => {
                let authors = yaml_strings(&value);
                partial.author = (!authors.is_empty()).then(|| authors.join(", "));
            }
            "tags" => partial.tags = yaml_tags(&value),
            "id" | "tier" | "derived_from" | "origin" | "created" | "updated" | "content_hash"
            | "source_hash" | "source_locked" => {}
            _ => {
                partial.extra.insert(key.to_owned(), value);
            }
        }
    }
    Some(partial)
}

fn yaml_string(value: &Value) -> Option<String> {
    value.as_str().map(str::to_owned)
}

fn yaml_strings(value: &Value) -> Vec<String> {
    match value {
        Value::String(value) if !value.is_empty() => vec![value.clone()],
        Value::Sequence(values) => values
            .iter()
            .filter_map(yaml_string)
            .filter(|value| !value.is_empty())
            .collect(),
        _ => Vec::new(),
    }
}

fn yaml_tags(value: &Value) -> Vec<String> {
    match value {
        Value::String(value) => value
            .split(',')
            .map(str::trim)
            .filter(|value| !value.is_empty())
            .map(str::to_owned)
            .collect(),
        _ => yaml_strings(value),
    }
}

fn extract_h1(body: &str) -> Option<String> {
    body.lines().find_map(|line| {
        line.trim()
            .strip_prefix("# ")
            .map(|title| title.trim().trim_end_matches('#').trim().to_owned())
            .filter(|title| !title.is_empty())
    })
}

fn clean_filename_stem(path: &Path) -> Option<String> {
    let stem = path.file_stem()?.to_str()?.trim();
    let timestamp = regex::Regex::new(
        r"(?i)\s*[-_]\s*\d{4}-\d{2}-\d{2}(?:[T ]\d{2}[:-]?\d{2}[:-]?\d{2}(?:[.,]\d+)?(?:[+-]\d{2}:?\d{2}|Z)?)?$",
    )
    .expect("valid watcher timestamp regex");
    let title = timestamp.replace(stem, "").trim().to_owned();
    (!title.is_empty()).then_some(title)
}

fn get_optional(cortex: &Cortex, id: &str) -> Result<Option<Row>> {
    match cortex.get(id) {
        Ok(row) => Ok(Some(row)),
        Err(error)
            if matches!(
                error.downcast_ref::<rusqlite::Error>(),
                Some(rusqlite::Error::QueryReturnedNoRows)
            ) =>
        {
            Ok(None)
        }
        Err(error) => Err(error),
    }
}

fn source_locked_foreign(row: &Row, cortex_name: &str) -> bool {
    row.source_locked && row.origin != cortex_name
}

fn metadata_drift(trace: &Trace, row: &Row) -> bool {
    let trace_tags: BTreeSet<_> = trace.frontmatter.tags.iter().collect();
    let row_tags: BTreeSet<_> = row.tags.iter().collect();
    let trace_sources: BTreeSet<_> = trace.frontmatter.derived_from.iter().collect();
    let row_sources: BTreeSet<_> = row.derived_from.iter().collect();
    trace.frontmatter.title != row.title
        || trace.frontmatter.trace_type != row.trace_type
        || trace.frontmatter.author != row.author
        || trace_tags != row_tags
        || trace_sources != row_sources
}

fn is_trace_file(path: &Path) -> bool {
    let Some(name) = path.file_name().and_then(|value| value.to_str()) else {
        return false;
    };
    !name.starts_with('.') && path.extension().and_then(|value| value.to_str()) == Some("md")
}

fn event_needs_reconcile(event: &Event) -> bool {
    matches!(
        event.kind,
        EventKind::Any | EventKind::Create(_) | EventKind::Modify(_) | EventKind::Remove(_)
    )
}

#[derive(Clone, Copy, PartialEq, Eq)]
struct FileStamp {
    modified: Option<SystemTime>,
    length: u64,
}

fn scan_snapshot(root: &Path) -> Result<BTreeMap<PathBuf, FileStamp>> {
    let mut snapshot = BTreeMap::new();
    for relative in ["traces", "archive/traces", "trash/traces"] {
        let directory = root.join(relative);
        for entry in fs::read_dir(&directory)
            .with_context(|| format!("scanning watched directory {}", directory.display()))?
        {
            let path = entry?.path();
            if !is_trace_file(&path) {
                continue;
            }
            let metadata = fs::metadata(&path)?;
            if metadata.is_file() {
                snapshot.insert(
                    path,
                    FileStamp {
                        modified: metadata.modified().ok(),
                        length: metadata.len(),
                    },
                );
            }
        }
    }
    Ok(snapshot)
}

fn changed_paths(
    previous: &BTreeMap<PathBuf, FileStamp>,
    current: &BTreeMap<PathBuf, FileStamp>,
) -> BTreeSet<PathBuf> {
    previous
        .keys()
        .chain(current.keys())
        .filter(|path| previous.get(*path) != current.get(*path))
        .cloned()
        .collect()
}

#[cfg(test)]
mod tests {
    use super::{
        Reconciler, Settings, changed_paths, clean_filename_stem, event_needs_reconcile,
        extract_h1, is_trace_file, partial_trace, scan_snapshot,
    };
    use crate::{
        cortex::{Cortex, ListOptions},
        trace::{self, Trace},
    };
    use notify::{
        Event, EventKind,
        event::{AccessKind, AccessMode, CreateKind, DataChange, ModifyKind, RemoveKind},
    };
    use std::{fs, path::Path, time::Duration};
    use tempfile::TempDir;
    use tokio_util::sync::CancellationToken;

    #[test]
    fn filters_watched_paths() {
        assert!(is_trace_file(Path::new("20260815-note.md")));
        assert!(!is_trace_file(Path::new(".swap.md")));
        assert!(!is_trace_file(Path::new("note.txt")));
    }

    #[test]
    fn filters_non_mutating_watcher_events() {
        for kind in [
            AccessKind::Read,
            AccessKind::Open(AccessMode::Any),
            AccessKind::Close(AccessMode::Read),
            AccessKind::Close(AccessMode::Write),
        ] {
            let event = Event::new(EventKind::Access(kind));
            assert!(!event_needs_reconcile(&event));
        }
        assert!(!event_needs_reconcile(&Event::new(EventKind::Other)));
        assert!(event_needs_reconcile(&Event::new(EventKind::Any)));
        assert!(event_needs_reconcile(&Event::new(EventKind::Create(
            CreateKind::File,
        ))));
        assert!(event_needs_reconcile(&Event::new(EventKind::Modify(
            ModifyKind::Data(DataChange::Content),
        ))));
        assert!(event_needs_reconcile(&Event::new(EventKind::Remove(
            RemoveKind::File,
        ))));
    }

    #[test]
    fn derives_onboarding_metadata() {
        let source = "---\ntitle: Clipped page\ntags: [docs]\n---\n\n# Ignored heading\n\nBody\n";
        let (partial, body) = partial_trace(source);
        assert_eq!(partial.title.as_deref(), Some("Clipped page"));
        assert_eq!(partial.tags, ["docs"]);
        assert_eq!(extract_h1(body).as_deref(), Some("Ignored heading"));
        assert_eq!(
            clean_filename_stem(Path::new("Clipped page - 2026-04-19T235252-0400.md")).as_deref(),
            Some("Clipped page")
        );
    }

    #[test]
    fn derives_web_clipper_fields_independently() {
        let source = r#"---
title: Clipped page
source: https://example.com/article
author:
  - "[[Author One]]"
  - "[[Author Two]]"
published: 2026-08-27
description: Useful reference
tags:
  - clippings
  - research
---

Body
"#;
        let (partial, body) = partial_trace(source);

        assert_eq!(partial.title.as_deref(), Some("Clipped page"));
        assert_eq!(
            partial.author.as_deref(),
            Some("[[Author One]], [[Author Two]]")
        );
        assert_eq!(partial.tags, ["clippings", "research"]);
        assert_eq!(
            partial.extra.get("source").and_then(|value| value.as_str()),
            Some("https://example.com/article")
        );
        assert_eq!(
            partial
                .extra
                .get("description")
                .and_then(|value| value.as_str()),
            Some("Useful reference")
        );
        assert!(partial.extra.contains_key("published"));
        assert_eq!(body, "Body\n");
    }

    #[test]
    fn unsupported_author_does_not_erase_valid_neighboring_fields() {
        let source = "---\ntitle: Clipped page\nauthor: {name: Writer}\ntags: [docs]\nsource: https://example.com\n---\n\nBody\n";
        let (partial, _) = partial_trace(source);

        assert_eq!(partial.title.as_deref(), Some("Clipped page"));
        assert_eq!(partial.author, None);
        assert_eq!(partial.tags, ["docs"]);
        assert!(partial.extra.contains_key("source"));
    }

    #[test]
    fn fallback_scan_reports_same_second_changes() {
        let temp = tempfile::tempdir().unwrap();
        for relative in ["traces", "archive/traces", "trash/traces"] {
            fs::create_dir_all(temp.path().join(relative)).unwrap();
        }
        let path = temp.path().join("traces/note.md");
        fs::write(&path, "before").unwrap();
        let before = scan_snapshot(temp.path()).unwrap();
        fs::write(&path, "after-change").unwrap();
        let after = scan_snapshot(temp.path()).unwrap();
        assert_eq!(changed_paths(&before, &after), [path].into());
    }

    fn setup() -> (TempDir, Reconciler, String) {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("watch", temp.path()).unwrap();
        let cortex = Cortex::open("watch", temp.path().join("watch")).unwrap();
        let mut trace = Trace::new("Seed", "note", "tester", vec!["old".into()], "body\n");
        let id = trace.frontmatter.id.clone();
        cortex.add(&mut trace).unwrap();
        (
            temp,
            Reconciler::new(
                cortex,
                Settings {
                    debounce: Duration::from_millis(1),
                    auto_onboard: true,
                },
            ),
            id,
        )
    }

    #[test]
    fn external_edit_updates_index_without_rewriting_file() {
        let (_temp, mut reconciler, id) = setup();
        let path = reconciler.cortex.trace_file(&id, false);
        let mut edited = fs::read(&path).unwrap();
        edited.extend_from_slice(b"external words\n");
        fs::write(&path, &edited).unwrap();

        reconciler
            .reconcile(&path, &CancellationToken::new())
            .unwrap();

        assert_eq!(fs::read(&path).unwrap(), edited);
        assert_eq!(
            reconciler.cortex.get(&id).unwrap().content_hash,
            trace::content_hash(&Trace::parse(&edited).unwrap().body)
        );
        assert_eq!(
            reconciler
                .cortex
                .history(&id)
                .unwrap()
                .iter()
                .filter(|event| event.action == "update")
                .count(),
            1
        );
    }

    #[test]
    fn external_move_and_delete_emit_recoverable_state() {
        let (_temp, mut reconciler, id) = setup();
        let active = reconciler.cortex.trace_file(&id, false);
        let archived = reconciler.cortex.trace_file(&id, true);
        fs::rename(&active, &archived).unwrap();
        reconciler
            .reconcile(&archived, &CancellationToken::new())
            .unwrap();
        assert!(!reconciler.cortex.get(&id).unwrap().archived_at.is_empty());

        fs::remove_file(&archived).unwrap();
        reconciler
            .reconcile(&archived, &CancellationToken::new())
            .unwrap();
        assert!(!reconciler.cortex.get(&id).unwrap().trashed_at.is_empty());
        assert!(
            reconciler
                .cortex
                .trash_dir()
                .join(format!("{id}.md"))
                .exists()
        );
        let actions: Vec<_> = reconciler
            .cortex
            .history(&id)
            .unwrap()
            .into_iter()
            .map(|event| event.action)
            .collect();
        assert_eq!(actions, ["create", "archive", "trash"]);
    }

    #[test]
    fn onboards_plain_markdown_and_heals_tracked_frontmatter() {
        let (_temp, mut reconciler, seed_id) = setup();
        let dropped = reconciler.cortex.traces_dir().join("Raw Note.md");
        fs::write(&dropped, "# Imported title\n\nImported body\n").unwrap();
        reconciler
            .reconcile(&dropped, &CancellationToken::new())
            .unwrap();
        let rows = reconciler.cortex.list(&ListOptions::default()).unwrap();
        assert_eq!(rows.len(), 2);
        assert!(rows.iter().any(|row| row.title == "Imported title"));

        let seed_path = reconciler.cortex.trace_file(&seed_id, false);
        fs::write(&seed_path, "plain replacement body\n").unwrap();
        reconciler
            .reconcile(&seed_path, &CancellationToken::new())
            .unwrap();
        let healed = Trace::parse_file(&seed_path).unwrap();
        assert_eq!(healed.frontmatter.title, "Seed");
        assert!(healed.body.contains("plain replacement body"));
        assert_eq!(
            reconciler
                .cortex
                .history(&seed_id)
                .unwrap()
                .iter()
                .filter(|event| event.action == "update")
                .count(),
            1
        );
    }

    #[test]
    fn onboards_web_clipper_frontmatter_without_losing_properties() {
        let (_temp, mut reconciler, _) = setup();
        let dropped = reconciler.cortex.traces_dir().join("Clipped page.md");
        fs::write(
            &dropped,
            r#"---
title: Clipped page
source: https://example.com/article
author:
  - "[[Author One]]"
  - "[[Author Two]]"
published: 2026-08-27
created: 2026-08-27
description: Useful reference
tags:
  - clippings
  - research
---

Clipped body
"#,
        )
        .unwrap();

        reconciler
            .reconcile(&dropped, &CancellationToken::new())
            .unwrap();

        let row = reconciler
            .cortex
            .list(&ListOptions::default())
            .unwrap()
            .into_iter()
            .find(|row| row.title == "Clipped page")
            .unwrap();
        assert_eq!(row.author, "[[Author One]], [[Author Two]]");
        assert_eq!(row.tags, ["clippings", "research"]);
        let stored = Trace::parse_file(&reconciler.cortex.trace_file(&row.id, false)).unwrap();
        assert_eq!(
            stored
                .frontmatter
                .extra
                .get("source")
                .and_then(|value| value.as_str()),
            Some("https://example.com/article")
        );
        assert_eq!(
            stored
                .frontmatter
                .extra
                .get("description")
                .and_then(|value| value.as_str()),
            Some("Useful reference")
        );
        assert!(stored.frontmatter.extra.contains_key("published"));
        assert!(stored.body.contains("Clipped body"));
        assert!(!dropped.exists());
    }
}
