use std::{
    fs::{self, File, OpenOptions},
    path::{Path, PathBuf},
};

use anyhow::{Context, Result, bail};
use fs2::FileExt;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::trace::{sync_directory, write_bytes_atomic, write_bytes_atomic_with_mode};

const CONFIG: &str = "storage.yaml";
const JOURNAL: &str = ".noema-storage-migration.json";
const GUARD: &[u8] = b"Noema database storage is nosync. Upgrade Noema to access db.nosync; do not replace this guard.\n";

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, clap::ValueEnum)]
#[serde(rename_all = "lowercase")]
pub enum DatabaseStorage {
    Default,
    Nosync,
}

impl DatabaseStorage {
    pub fn directory_name(self) -> &'static str {
        match self {
            Self::Default => "db",
            Self::Nosync => "db.nosync",
        }
    }
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Default => "default",
            Self::Nosync => "nosync",
        }
    }
}

#[derive(Debug)]
pub struct StorageLock(Vec<File>);

impl StorageLock {
    pub fn acquire(root: &Path, exclusive: bool) -> Result<Self> {
        let primary_path = runtime_lock_path(root)?;
        let legacy_path = root.join(".noema-storage.lock");
        let mut files = Vec::with_capacity(2);
        // Keep the legacy lock during upgrades; the runtime lock remains authoritative
        // even if a sync provider replaces the file inside the cortex.
        for path in [&primary_path, &legacy_path] {
            exists(path)?;
            let file = OpenOptions::new()
                .read(true)
                .write(true)
                .create(true)
                .truncate(false)
                .open(path)?;
            let result = if exclusive {
                FileExt::try_lock_exclusive(&file)
            } else {
                FileExt::try_lock_shared(&file)
            };
            result.context("database storage is in use; stop Noema servers, watchers, and other clients before changing storage")?;
            files.push(file);
        }
        Ok(Self(files))
    }
}

fn runtime_lock_path(root: &Path) -> Result<PathBuf> {
    let canonical_root =
        fs::canonicalize(root).context("resolving cortex storage lock identity")?;
    let key = format!(
        "{:x}",
        Sha256::digest(canonical_root.as_os_str().as_encoded_bytes())
    );
    let base = std::env::var_os("XDG_RUNTIME_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(std::env::temp_dir);
    let directory = base.join("noema").join("storage-locks").join(key);
    exists(&directory)?;
    fs::create_dir_all(&directory)?;
    if fs::canonicalize(&directory)?.starts_with(&canonical_root) {
        bail!("storage runtime locks must be outside the cortex; choose a local XDG_RUNTIME_DIR");
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&directory, fs::Permissions::from_mode(0o700))?;
    }
    Ok(directory.join("storage.lock"))
}

impl Drop for StorageLock {
    fn drop(&mut self) {
        for file in &self.0 {
            let _ = FileExt::unlock(file);
        }
    }
}

fn exists(path: &Path) -> Result<bool> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() => {
            bail!("storage paths must not be symlinks: {}", path.display())
        }
        Ok(_) => Ok(true),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(error) => Err(error.into()),
    }
}

fn configured(root: &Path) -> Result<Option<DatabaseStorage>> {
    let path = root.join(CONFIG);
    if !exists(&path)? {
        return Ok(None);
    }
    let value: serde_yaml::Value = serde_yaml::from_slice(&fs::read(path)?)?;
    let mapping = value
        .as_mapping()
        .context("storage.yaml must be a YAML mapping")?;
    mapping
        .get(serde_yaml::Value::String("database".into()))
        .map(|value| {
            serde_yaml::from_value(value.clone()).context("database must be default or nosync")
        })
        .transpose()
}

fn guard_directory(root: &Path, allow_empty: bool) -> Result<bool> {
    let path = root.join("db");
    if !exists(&path)? {
        return Ok(false);
    }
    let entries = fs::read_dir(&path)?.collect::<std::io::Result<Vec<_>>>()?;
    if entries.is_empty() {
        return Ok(allow_empty);
    }
    if entries.len() != 1 || entries[0].file_name() != "noema.db" {
        return Ok(false);
    }
    let file = path.join("noema.db");
    Ok(exists(&file)?
        && fs::metadata(&file)?.len() == GUARD.len() as u64
        && fs::read(file)? == GUARD)
}

pub fn directory(root: &Path) -> Result<PathBuf> {
    if exists(&root.join(JOURNAL))? {
        bail!(
            "interrupted storage migration; run noema cortex storage <name> --resume before opening this cortex"
        );
    }
    let regular = exists(&root.join("db"))?;
    let local = exists(&root.join("db.nosync"))?;
    let mode = configured(root)?;
    if local {
        if regular && !guard_directory(root, false)? {
            bail!("both db and db.nosync exist; refusing ambiguous database storage");
        }
        if mode == Some(DatabaseStorage::Default) {
            bail!(
                "storage.yaml selects default but db.nosync exists; use the storage migration command"
            );
        }
        let path = root.join("db.nosync");
        if !path.is_dir() || !exists(&path.join("noema.db"))? {
            bail!("db.nosync database is missing; restore a full Noema backup on this device");
        }
        return Ok(path);
    }
    if mode == Some(DatabaseStorage::Nosync) || guard_directory(root, false)? {
        bail!("db.nosync database is missing; restore a full Noema backup on this device");
    }
    Ok(root.join("db"))
}

// Preserve comments, unknown keys, and ordering rather than serializing the configuration.
fn configuration_bytes(root: &Path, mode: DatabaseStorage) -> Result<Vec<u8>> {
    let path = root.join(CONFIG);
    let previous = if exists(&path)? {
        fs::read_to_string(path)?
    } else {
        String::new()
    };
    let parsed = configured(root)?;
    let mut found = false;
    let mut next = String::new();
    for line in previous.split_inclusive('\n') {
        if let Some(rest) = line.strip_prefix("database:") {
            if found {
                bail!("duplicate database setting");
            }
            found = true;
            next.push_str(&format!("database: {}", mode.as_str()));
            if let Some((_, comment)) = rest.split_once('#') {
                next.push_str(" #");
                next.push_str(comment.trim_end_matches(['\r', '\n']));
            }
            if line.ends_with('\n') {
                next.push('\n');
            }
        } else {
            next.push_str(line);
        }
    }
    if found && parsed.is_none() {
        bail!("cannot safely identify the database setting in storage.yaml");
    }
    if !found {
        if parsed.is_some() {
            bail!("use a top-level database: setting in storage.yaml before migrating");
        }
        if !next.is_empty() && !next.ends_with('\n') {
            next.push('\n');
        }
        next.push_str(&format!("database: {}\n", mode.as_str()));
    }
    let value: serde_yaml::Value = serde_yaml::from_str(&next)?;
    if value.get("database").and_then(|v| v.as_str()) != Some(mode.as_str()) {
        bail!("cannot safely update storage.yaml");
    }
    Ok(next.into_bytes())
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Migration {
    version: u32,
    target: DatabaseStorage,
    config: String,
}

pub fn migrate(
    root: &Path,
    target: Option<DatabaseStorage>,
    backup: Option<&Path>,
    resume: bool,
) -> Result<DatabaseStorage> {
    let _lock = StorageLock::acquire(root, true)?;
    crate::cortex::read_manifest(root)?;
    let journal_path = root.join(JOURNAL);
    let migration = if exists(&journal_path)? {
        if !resume || target.is_some() || backup.is_some() {
            bail!("interrupted storage migration; use --resume alone");
        }
        let journal: Migration = serde_json::from_slice(&fs::read(&journal_path)?)?;
        if journal.version != 1 {
            bail!("unsupported storage migration version");
        }
        let config: serde_yaml::Value = serde_yaml::from_str(&journal.config)?;
        if config.get("database").and_then(|value| value.as_str()) != Some(journal.target.as_str())
        {
            bail!("storage migration configuration does not match its target");
        }
        journal
    } else {
        if resume {
            bail!("no interrupted storage migration");
        }
        let target = target.context("select --database default or --database nosync")?;
        let source = directory(root)?;
        let config = configuration_bytes(root, target)?;
        if source.file_name().and_then(|v| v.to_str()) == Some(target.directory_name())
            && configured(root)? == Some(target)
            && (target == DatabaseStorage::Default || guard_directory(root, false)?)
        {
            return Ok(target);
        }
        let backup = backup.context("--backup <archive.tar.gz> outside the cortex is required before changing database storage")?;
        let database = source.join("noema.db");
        if !exists(&database)? {
            bail!("source database is missing");
        }
        let connection = rusqlite::Connection::open_with_flags(
            &database,
            rusqlite::OpenFlags::SQLITE_OPEN_READ_WRITE,
        )?;
        let integrity: String = connection.query_row("PRAGMA quick_check", [], |row| row.get(0))?;
        if integrity != "ok" {
            bail!("database integrity check failed");
        }
        let busy: i64 =
            connection.query_row("PRAGMA wal_checkpoint(TRUNCATE)", [], |row| row.get(0))?;
        if busy != 0 {
            bail!("database is busy; stop all Noema clients before migrating");
        }
        drop(connection);
        crate::restore::backup_without_storage_lock(root, backup, false)?;
        let journal = Migration {
            version: 1,
            target,
            config: String::from_utf8(config)?,
        };
        write_bytes_atomic(&journal_path, &serde_json::to_vec(&journal)?)?;
        pause_for_test("journal")?;
        journal
    };
    match migration.target {
        DatabaseStorage::Nosync => {
            if !exists(&root.join("db.nosync"))? {
                if guard_directory(root, true)? {
                    bail!("source database is missing; restore the migration backup");
                }
                fs::rename(root.join("db"), root.join("db.nosync"))?;
                sync_directory(root)?;
            }
            if !exists(&root.join("db.nosync/noema.db"))? {
                bail!("nosync database is missing");
            }
            pause_for_test("moved")?;
            if exists(&root.join("db"))? && !guard_directory(root, true)? {
                bail!("unexpected files at db; refusing to overwrite them");
            }
            fs::create_dir_all(root.join("db"))?;
            let guard = root.join(".noema-storage-guard");
            exists(&guard)?;
            write_bytes_atomic(&guard, GUARD)?;
            fs::rename(guard, root.join("db/noema.db"))?;
            sync_directory(&root.join("db"))?;
            sync_directory(root)?;
        }
        DatabaseStorage::Default => {
            if exists(&root.join("db.nosync"))? {
                if exists(&root.join("db"))? {
                    if !guard_directory(root, true)? {
                        bail!("unexpected files at db; refusing to overwrite them");
                    }
                    if exists(&root.join("db/noema.db"))? {
                        fs::remove_file(root.join("db/noema.db"))?;
                        sync_directory(&root.join("db"))?;
                    }
                    fs::remove_dir(root.join("db"))?;
                    sync_directory(root)?;
                }
                fs::rename(root.join("db.nosync"), root.join("db"))?;
                sync_directory(root)?;
            }
            if !exists(&root.join("db/noema.db"))? {
                bail!("default database is missing");
            }
            pause_for_test("moved")?;
        }
    }
    let config_path = root.join(CONFIG);
    let existed = exists(&config_path)?;
    #[cfg(unix)]
    let mode = {
        use std::os::unix::fs::PermissionsExt;
        if existed {
            fs::metadata(&config_path)?.permissions().mode() & 0o7777
        } else {
            0o640
        }
    };
    #[cfg(not(unix))]
    let mode = {
        let _ = existed;
        0o640
    };
    write_bytes_atomic_with_mode(&config_path, migration.config.as_bytes(), mode)?;
    pause_for_test("configured")?;
    fs::remove_file(journal_path)?;
    sync_directory(root)?;
    Ok(migration.target)
}

fn pause_for_test(phase: &str) -> Result<()> {
    #[cfg(debug_assertions)]
    if std::env::var("NOEMA_TEST_STORAGE_PHASE").ok().as_deref() == Some(phase)
        && let Some(marker) = std::env::var_os("NOEMA_TEST_STORAGE_PAUSE")
    {
        fs::write(marker, phase)?;
        loop {
            std::thread::sleep(std::time::Duration::from_millis(50));
        }
    }
    let _ = phase;
    Ok(())
}
