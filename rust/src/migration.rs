use std::{
    collections::{BTreeMap, BTreeSet},
    fs::{self, File, OpenOptions},
    io::{Read, Write},
    path::{Path, PathBuf},
};

use anyhow::{Context, Result, bail};
use chrono::Utc;
use fs2::FileExt;
use rusqlite::{OptionalExtension, Transaction, params};
use serde::{Deserialize, Serialize};

use crate::{
    config::{Config, CortexEntry},
    cortex::{MANIFEST_VERSION, Manifest, read_manifest, write_manifest},
    db,
    trace::sync_directory,
};

const JOURNAL_NAME: &str = ".cortex-id-migration.json";
const JOURNAL_TEMP_NAME: &str = ".cortex-id-migration.json.tmp";
const LOCK_NAME: &str = ".cortex-id-migration.lock";

#[derive(Debug, Clone, Serialize, Deserialize)]
struct MigrationJournal {
    version: u32,
    name: String,
    new_id: String,
    reset: bool,
    stamp: String,
    phase: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct MigrationResult {
    pub new_id: String,
    pub traces_updated: usize,
    pub events_updated: usize,
    pub clock_moved: usize,
    pub peers_cleared: usize,
    pub pins_cleared: usize,
    pub cursors_cleared: usize,
    pub stamp: String,
    pub already_current: bool,
}

pub fn has_pending_migration(dir: &Path) -> bool {
    dir.join(JOURNAL_NAME).exists()
}

pub fn planned_identity(dir: &Path, name: &str, reset: bool) -> Result<String> {
    if let Some(journal) = read_journal(&dir.join(JOURNAL_NAME))? {
        if journal.version != 1 || journal.name != name || journal.reset != reset {
            bail!("pending cortex-id migration does not match this request");
        }
        if !is_ulid(&journal.new_id) {
            bail!("pending cortex-id migration contains an invalid identity");
        }
        return Ok(journal.new_id);
    }
    let manifest = read_manifest(dir).context("reading cortex.md")?;
    if !reset && !manifest.id.is_empty() {
        return Ok(manifest.id);
    }
    Ok(ulid::Ulid::new().to_string())
}

struct MigrationLock(File);

impl MigrationLock {
    fn acquire(dir: &Path) -> Result<Self> {
        let path = dir.join(LOCK_NAME);
        let file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(&path)
            .with_context(|| format!("opening migration lock {}", path.display()))?;
        set_owner_only(&path)?;
        file.try_lock_exclusive()
            .context("another cortex-id migration is already running")?;
        Ok(Self(file))
    }
}

impl Drop for MigrationLock {
    fn drop(&mut self) {
        let _ = FileExt::unlock(&self.0);
    }
}

pub fn migrate_cortex_id(
    config: &mut Config,
    name: &str,
    entry: &CortexEntry,
    reset: bool,
    proposed_id: &str,
    config_path: Option<&Path>,
) -> Result<MigrationResult> {
    let dir = &entry.path;
    let _lock = MigrationLock::acquire(dir)?;
    let journal_path = dir.join(JOURNAL_NAME);
    let existing_journal = read_journal(&journal_path)?;
    let manifest = read_manifest(dir).context("reading cortex.md")?;

    if existing_journal.is_none()
        && !reset
        && manifest.version >= MANIFEST_VERSION
        && !manifest.id.is_empty()
    {
        return Ok(MigrationResult {
            new_id: manifest.id,
            already_current: true,
            ..Default::default()
        });
    }

    let mut journal = if let Some(journal) = existing_journal {
        if journal.version != 1 || journal.name != name || journal.reset != reset {
            bail!("pending cortex-id migration does not match this request");
        }
        if !is_ulid(&journal.new_id) {
            bail!("pending cortex-id migration contains an invalid identity");
        }
        journal
    } else {
        if !is_ulid(proposed_id) {
            bail!("proposed cortex identity is not a valid ULID");
        }
        let journal = MigrationJournal {
            version: 1,
            name: name.to_owned(),
            new_id: proposed_id.to_owned(),
            reset,
            stamp: Utc::now().format("%Y%m%dT%H%M%SZ").to_string(),
            phase: "prepared".into(),
        };
        write_journal(dir, &journal)?;
        journal
    };

    db::checkpoint_wal(dir).context("checkpointing database before backup")?;
    let manifest_path = dir.join("cortex.md");
    let database_path = dir.join("db/noema.db");
    copy_backup_atomic(
        &manifest_path,
        &PathBuf::from(format!("{}.{}.bak", manifest_path.display(), journal.stamp)),
    )?;
    if database_path.exists() {
        copy_backup_atomic(
            &database_path,
            &PathBuf::from(format!("{}.{}.bak", database_path.display(), journal.stamp)),
        )?;
    }
    journal.phase = "backed-up".into();
    write_journal(dir, &journal)?;

    let connection = db::open(dir).context("opening database")?;
    let tx = connection
        .unchecked_transaction()
        .context("starting migration transaction")?;
    let mut result = migrate_database(&tx, name, &journal.new_id, &manifest, reset)?;
    tx.commit().context("committing migration")?;
    drop(connection);
    journal.phase = "database-committed".into();
    write_journal(dir, &journal)?;

    let mut updated_manifest = manifest;
    updated_manifest.id = journal.new_id.clone();
    updated_manifest.version = MANIFEST_VERSION;
    write_manifest(dir, &updated_manifest).context("writing cortex.md")?;
    journal.phase = "manifest-saved".into();
    write_journal(dir, &journal)?;

    let mut updated_entry = entry.clone();
    updated_entry.id = journal.new_id.clone();
    config.cortexes.insert(name.to_owned(), updated_entry);
    if let Some(path) = config_path {
        config.save_to_path(path)?;
    } else {
        config.save()?;
    }
    journal.phase = "config-saved".into();
    write_journal(dir, &journal)?;

    fs::remove_file(&journal_path).context("removing completed migration journal")?;
    sync_directory(dir)?;
    result.new_id = journal.new_id;
    result.stamp = journal.stamp;
    Ok(result)
}

fn migrate_database(
    tx: &Transaction<'_>,
    name: &str,
    new_id: &str,
    manifest: &Manifest,
    reset: bool,
) -> Result<MigrationResult> {
    let mut traces_updated = tx.execute(
        "UPDATE traces SET cortex_id=?1 WHERE cortex_id='' AND (origin=?2 OR origin='')",
        params![new_id, name],
    )?;
    let mut events_updated = tx.execute(
        "UPDATE events SET cortex_id=?1 WHERE cortex_id='' AND origin=?2",
        params![new_id, name],
    )?;
    if reset {
        traces_updated += tx.execute(
            "UPDATE traces SET cortex_id=?1 WHERE origin=?2 AND cortex_id!=?1",
            params![new_id, name],
        )?;
        events_updated += tx.execute(
            "UPDATE events SET cortex_id=?1 WHERE origin=?2 AND cortex_id!=?1",
            params![new_id, name],
        )?;
    }

    let peer_names = manifest
        .federation
        .as_ref()
        .map(|federation| {
            federation
                .peers
                .iter()
                .map(|peer| peer.name.clone())
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    let (clock_moved, peers_cleared) = rekey_vector_clock(tx, name, new_id, &peer_names, reset)?;
    let pins_cleared = tx.execute(
        "DELETE FROM federation_state WHERE key LIKE 'peer:%:cortex_id'",
        [],
    )?;
    let cursors_cleared = if reset {
        tx.execute(
            "DELETE FROM federation_state WHERE key LIKE 'peer:%:last_event' OR key LIKE 'peer:%:last_seen'",
            [],
        )?
    } else {
        0
    };
    Ok(MigrationResult {
        traces_updated,
        events_updated,
        clock_moved,
        peers_cleared,
        pins_cleared,
        cursors_cleared,
        ..Default::default()
    })
}

fn rekey_vector_clock(
    tx: &Transaction<'_>,
    old_name: &str,
    new_id: &str,
    peer_names: &[String],
    reset: bool,
) -> Result<(usize, usize)> {
    let raw: Option<String> = tx
        .query_row(
            "SELECT value FROM federation_state WHERE key='vclock'",
            [],
            |row| row.get(0),
        )
        .optional()?;
    let Some(raw) = raw else {
        return Ok((0, 0));
    };
    let mut clock: BTreeMap<String, u64> =
        serde_json::from_str(&raw).context("parsing existing vclock")?;
    let mut moved = 0;
    let mut peers_cleared = 0;
    if let Some(counter) = clock.remove(old_name) {
        clock
            .entry(new_id.to_owned())
            .and_modify(|value| *value = (*value).max(counter))
            .or_insert(counter);
        moved += 1;
    }
    let peers = peer_names
        .iter()
        .map(String::as_str)
        .collect::<BTreeSet<_>>();
    for peer in peers {
        if peer != new_id && clock.remove(peer).is_some() {
            peers_cleared += 1;
        }
    }
    if reset {
        let legacy = clock
            .keys()
            .filter(|key| key.as_str() != new_id && key.len() != 26)
            .cloned()
            .collect::<Vec<_>>();
        for key in legacy {
            let counter = clock.remove(&key).unwrap_or_default();
            clock
                .entry(new_id.to_owned())
                .and_modify(|value| *value = (*value).max(counter))
                .or_insert(counter);
            moved += 1;
        }
    }
    tx.execute(
        "INSERT INTO federation_state(key,value) VALUES ('vclock',?1) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
        [serde_json::to_string(&clock)?],
    )?;
    Ok((moved, peers_cleared))
}

fn read_journal(path: &Path) -> Result<Option<MigrationJournal>> {
    match fs::read(path) {
        Ok(data) => serde_json::from_slice(&data)
            .context("parsing pending cortex-id migration")
            .map(Some),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(error).context("reading pending cortex-id migration"),
    }
}

fn write_journal(dir: &Path, journal: &MigrationJournal) -> Result<()> {
    let path = dir.join(JOURNAL_NAME);
    let temporary = dir.join(JOURNAL_TEMP_NAME);
    if let Ok(metadata) = fs::symlink_metadata(&temporary) {
        if !metadata.file_type().is_file() {
            bail!("refusing to replace non-file migration journal temporary artifact");
        }
        fs::remove_file(&temporary)?;
    }
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&temporary)?;
    set_owner_only(&temporary)?;
    file.write_all(&serde_json::to_vec(journal)?)?;
    file.sync_all()?;
    drop(file);
    fs::rename(&temporary, path)?;
    sync_directory(dir)?;
    Ok(())
}

fn copy_backup_atomic(source: &Path, destination: &Path) -> Result<()> {
    if destination.exists() {
        return Ok(());
    }
    let temporary = destination.with_extension("bak.tmp");
    if let Ok(metadata) = fs::symlink_metadata(&temporary) {
        if !metadata.file_type().is_file() {
            bail!("refusing to replace non-file migration backup temporary artifact");
        }
        fs::remove_file(&temporary)?;
    }
    let mut input = File::open(source)?;
    let mut output = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&temporary)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&temporary, fs::Permissions::from_mode(0o640))?;
    }
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let read = input.read(&mut buffer)?;
        if read == 0 {
            break;
        }
        output.write_all(&buffer[..read])?;
    }
    output.sync_all()?;
    drop(output);
    fs::rename(&temporary, destination)?;
    sync_directory(destination.parent().unwrap())?;
    Ok(())
}

fn is_ulid(value: &str) -> bool {
    value.len() == 26 && ulid::Ulid::from_string(value).is_ok()
}

#[cfg(unix)]
fn set_owner_only(path: &Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o600))?;
    Ok(())
}

#[cfg(not(unix))]
fn set_owner_only(_path: &Path) -> Result<()> {
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        cortex::{Cortex, FederationConfig, PeerEntry},
        trace::Trace,
    };

    fn fixture(name: &str) -> (tempfile::TempDir, PathBuf, String) {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create(name, temp.path()).unwrap();
        let dir = temp.path().join(name);
        let cx = Cortex::open(name, &dir).unwrap();
        let mut trace = Trace::new("Migration subject", "fact", "", vec![], "body");
        cx.add(&mut trace).unwrap();
        drop(cx);
        (temp, dir, trace.frontmatter.id)
    }

    fn degrade_to_v1(dir: &Path, name: &str) {
        let mut manifest = read_manifest(dir).unwrap();
        manifest.id.clear();
        manifest.version = 1;
        manifest.federation = Some(FederationConfig {
            peers: vec![
                PeerEntry {
                    name: "peer-a".into(),
                    endpoint: "https://peer-a.example".into(),
                    ..Default::default()
                },
                PeerEntry {
                    name: "peer-b".into(),
                    endpoint: "https://peer-b.example".into(),
                    ..Default::default()
                },
            ],
            ..Default::default()
        });
        write_manifest(dir, &manifest).unwrap();
        let connection = db::open(dir).unwrap();
        connection
            .execute("UPDATE traces SET cortex_id=''", [])
            .unwrap();
        connection
            .execute("UPDATE events SET cortex_id=''", [])
            .unwrap();
        connection
            .execute(
                "INSERT INTO federation_state(key,value) VALUES ('vclock',?1) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                [serde_json::json!({name:1,"peer-a":5,"peer-b":7}).to_string()],
            )
            .unwrap();
        for (key, value) in [
            ("peer:peer-a:cortex_id", "01OLDPEER00000000000000000"),
            ("peer:peer-a:last_event", "01OLDEVENT0000000000000000"),
            ("peer:peer-a:last_seen", "2026-01-01T00:00:00Z"),
        ] {
            connection
                .execute(
                    "INSERT INTO federation_state(key,value) VALUES (?1,?2)",
                    params![key, value],
                )
                .unwrap();
        }
    }

    fn config_for(name: &str, dir: &Path) -> Config {
        Config {
            default: name.into(),
            cortexes: [(
                name.into(),
                CortexEntry {
                    path: dir.to_path_buf(),
                    id: String::new(),
                },
            )]
            .into_iter()
            .collect(),
            ..Default::default()
        }
    }

    #[test]
    fn migrates_v1_identity_and_preserves_non_reset_cursors() {
        let (temp, dir, trace_id) = fixture("migration-target");
        degrade_to_v1(&dir, "migration-target");
        let mut config = config_for("migration-target", &dir);
        let entry = config.cortexes["migration-target"].clone();
        let config_path = temp.path().join("config/config.yaml");
        let new_id = ulid::Ulid::new().to_string();

        let result = migrate_cortex_id(
            &mut config,
            "migration-target",
            &entry,
            false,
            &new_id,
            Some(&config_path),
        )
        .unwrap();
        assert_eq!(result.new_id, new_id);
        assert_eq!(result.traces_updated, 1);
        assert!(result.events_updated >= 1);
        assert_eq!(result.clock_moved, 1);
        assert_eq!(result.peers_cleared, 2);
        assert_eq!(result.pins_cleared, 1);
        assert_eq!(result.cursors_cleared, 0);

        let manifest = read_manifest(&dir).unwrap();
        assert_eq!(
            (manifest.version, manifest.id.as_str()),
            (2, new_id.as_str())
        );
        let persisted: Config = serde_yaml::from_slice(&fs::read(&config_path).unwrap()).unwrap();
        assert_eq!(persisted.cortexes["migration-target"].id, new_id);
        assert!(
            dir.join(format!("cortex.md.{}.bak", result.stamp))
                .is_file()
        );
        assert!(
            dir.join(format!("db/noema.db.{}.bak", result.stamp))
                .is_file()
        );
        assert!(!dir.join(JOURNAL_NAME).exists());

        let connection = db::open(&dir).unwrap();
        let row_id: String = connection
            .query_row(
                "SELECT cortex_id FROM traces WHERE id=?1",
                [&trace_id],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(row_id, new_id);
        let event_ids: i64 = connection
            .query_row(
                "SELECT COUNT(*) FROM events WHERE trace_id=?1 AND cortex_id=?2",
                params![trace_id, new_id],
                |row| row.get(0),
            )
            .unwrap();
        assert!(event_ids >= 1);
        let clock: String = connection
            .query_row(
                "SELECT value FROM federation_state WHERE key='vclock'",
                [],
                |row| row.get(0),
            )
            .unwrap();
        let clock: BTreeMap<String, u64> = serde_json::from_str(&clock).unwrap();
        assert_eq!(clock.get(&new_id), Some(&1));
        assert!(!clock.contains_key("migration-target"));
        assert!(!clock.contains_key("peer-a"));
        let cursors: i64 = connection
            .query_row(
                "SELECT COUNT(*) FROM federation_state WHERE key LIKE 'peer:%:last_event' OR key LIKE 'peer:%:last_seen'",
                [],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(cursors, 2);
    }

    #[test]
    fn reset_rekeys_owned_rows_and_clears_cursors() {
        let (temp, dir, trace_id) = fixture("reset-target");
        let old_id = read_manifest(&dir).unwrap().id;
        let connection = db::open(&dir).unwrap();
        for (key, value) in [
            ("peer:peer-a:cortex_id", "01OLDPEER00000000000000000"),
            ("peer:peer-a:last_event", "01OLDEVENT0000000000000000"),
            ("peer:peer-a:last_seen", "2026-01-01T00:00:00Z"),
        ] {
            connection
                .execute(
                    "INSERT INTO federation_state(key,value) VALUES (?1,?2)",
                    params![key, value],
                )
                .unwrap();
        }
        drop(connection);
        let mut config = config_for("reset-target", &dir);
        config.cortexes.get_mut("reset-target").unwrap().id = old_id;
        let entry = config.cortexes["reset-target"].clone();
        let new_id = ulid::Ulid::new().to_string();
        let result = migrate_cortex_id(
            &mut config,
            "reset-target",
            &entry,
            true,
            &new_id,
            Some(&temp.path().join("config.yaml")),
        )
        .unwrap();
        assert_eq!(result.cursors_cleared, 2);
        assert_eq!(result.pins_cleared, 1);
        let connection = db::open(&dir).unwrap();
        let owner: String = connection
            .query_row(
                "SELECT cortex_id FROM traces WHERE id=?1",
                [trace_id],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(owner, new_id);
        let stale: i64 = connection
            .query_row(
                "SELECT COUNT(*) FROM federation_state WHERE key LIKE 'peer:%:cortex_id' OR key LIKE 'peer:%:last_event' OR key LIKE 'peer:%:last_seen'",
                [],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(stale, 0);
    }

    #[test]
    fn resumes_database_committed_migration_with_same_identity() {
        let (temp, dir, _) = fixture("resume-target");
        degrade_to_v1(&dir, "resume-target");
        let mut config = config_for("resume-target", &dir);
        let entry = config.cortexes["resume-target"].clone();
        let new_id = ulid::Ulid::new().to_string();
        let mut journal = MigrationJournal {
            version: 1,
            name: "resume-target".into(),
            new_id: new_id.clone(),
            reset: false,
            stamp: "20260816T120000Z".into(),
            phase: "backed-up".into(),
        };
        write_journal(&dir, &journal).unwrap();
        copy_backup_atomic(
            &dir.join("cortex.md"),
            &dir.join("cortex.md.20260816T120000Z.bak"),
        )
        .unwrap();
        copy_backup_atomic(
            &dir.join("db/noema.db"),
            &dir.join("db/noema.db.20260816T120000Z.bak"),
        )
        .unwrap();
        let connection = db::open(&dir).unwrap();
        let tx = connection.unchecked_transaction().unwrap();
        migrate_database(
            &tx,
            "resume-target",
            &new_id,
            &read_manifest(&dir).unwrap(),
            false,
        )
        .unwrap();
        tx.commit().unwrap();
        drop(connection);
        journal.phase = "database-committed".into();
        write_journal(&dir, &journal).unwrap();

        let planned = planned_identity(&dir, "resume-target", false).unwrap();
        assert_eq!(planned, new_id);
        let result = migrate_cortex_id(
            &mut config,
            "resume-target",
            &entry,
            false,
            &ulid::Ulid::new().to_string(),
            Some(&temp.path().join("config.yaml")),
        )
        .unwrap();
        assert_eq!(result.new_id, new_id);
        assert_eq!(read_manifest(&dir).unwrap().id, new_id);
        assert!(!dir.join(JOURNAL_NAME).exists());
    }
}
