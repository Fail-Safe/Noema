use std::{
    fs,
    path::{Path, PathBuf},
    time::Duration,
};

use anyhow::{Context, Result, bail};
use rusqlite::{Connection, OpenFlags};
use serde::Serialize;

use crate::storage::{self, DatabaseStorage, StorageLock};

#[derive(Debug, Serialize)]
pub struct StorageStats {
    pub database: DatabaseStorage,
    pub database_path: PathBuf,
    pub database_bytes: u64,
    pub logical_bytes: u64,
    pub wal_bytes: u64,
    pub page_size: u64,
    pub page_count: u64,
    pub free_pages: u64,
    pub reusable_bytes: u64,
    pub reusable_percent: f64,
    pub auto_vacuum: String,
    pub available_disk_bytes: u64,
    pub compact_required_free_bytes: u64,
}

#[derive(Debug, Serialize)]
pub struct CompactResult {
    pub before: StorageStats,
    pub after: StorageStats,
    pub backup_path: PathBuf,
    pub reclaimed_bytes: u64,
}

fn file_size(path: &Path, required: bool) -> Result<u64> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.is_file() => Ok(metadata.len()),
        Ok(_) => bail!(
            "database and sidecars must be regular files: {}",
            path.display()
        ),
        Err(error) if !required && error.kind() == std::io::ErrorKind::NotFound => Ok(0),
        Err(error) => Err(error).with_context(|| format!("reading {}", path.display())),
    }
}

fn database_path(root: &Path) -> Result<PathBuf> {
    let directory = storage::directory(root)?;
    let database = directory.join("noema.db");
    file_size(&database, true)?;
    for sidecar in ["noema.db-wal", "noema.db-shm", "noema.db-journal"] {
        file_size(&directory.join(sidecar), false)?;
    }
    Ok(database)
}

fn read_stats(connection: &Connection, database_path: &Path) -> Result<StorageStats> {
    let tx = connection.unchecked_transaction()?;
    let page_size = u64::from(tx.query_row("PRAGMA page_size", [], |row| row.get::<_, u32>(0))?);
    let page_count = u64::from(tx.query_row("PRAGMA page_count", [], |row| row.get::<_, u32>(0))?);
    let free_pages =
        u64::from(tx.query_row("PRAGMA freelist_count", [], |row| row.get::<_, u32>(0))?);
    let auto_vacuum: u32 = tx.query_row("PRAGMA auto_vacuum", [], |row| row.get(0))?;
    tx.commit()?;
    let directory = database_path
        .parent()
        .context("database has no parent directory")?;
    let logical_bytes = page_size * page_count;
    let database_bytes = file_size(database_path, true)?;
    let compact_required_free_bytes = logical_bytes
        .max(database_bytes)
        .checked_mul(2)
        .context("database is too large to calculate compaction headroom")?;
    Ok(StorageStats {
        database: if directory
            .file_name()
            .is_some_and(|name| name == "db.nosync")
        {
            DatabaseStorage::Nosync
        } else {
            DatabaseStorage::Default
        },
        database_path: database_path.to_owned(),
        database_bytes,
        logical_bytes,
        wal_bytes: file_size(&directory.join("noema.db-wal"), false)?,
        page_size,
        page_count,
        free_pages,
        reusable_bytes: page_size * free_pages,
        reusable_percent: if page_count == 0 {
            0.0
        } else {
            free_pages as f64 * 100.0 / page_count as f64
        },
        auto_vacuum: match auto_vacuum {
            0 => "none".into(),
            1 => "full".into(),
            2 => "incremental".into(),
            other => format!("unknown ({other})"),
        },
        available_disk_bytes: fs2::available_space(directory)?,
        compact_required_free_bytes,
    })
}

pub fn storage_stats(root: &Path) -> Result<StorageStats> {
    let _lock = StorageLock::acquire(root, false)?;
    let path = database_path(root)?;
    let connection = Connection::open_with_flags(&path, OpenFlags::SQLITE_OPEN_READ_ONLY)?;
    connection.busy_timeout(Duration::from_secs(5))?;
    connection.execute_batch("PRAGMA query_only=ON")?;
    read_stats(&connection, &path)
}

fn integrity_check(connection: &Connection) -> Result<()> {
    let rows = connection
        .prepare("PRAGMA integrity_check")?
        .query_map([], |row| row.get::<_, String>(0))?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    if rows != ["ok"] {
        bail!("database integrity check failed");
    }
    Ok(())
}

fn checkpoint(connection: &Connection) -> Result<()> {
    let busy: i64 =
        connection.query_row("PRAGMA wal_checkpoint(TRUNCATE)", [], |row| row.get(0))?;
    if busy != 0 {
        bail!("database checkpoint is busy; stop all Noema clients before compacting");
    }
    Ok(())
}

fn require_headroom(available: u64, required: u64) -> Result<()> {
    if available < required {
        bail!(
            "insufficient disk space for compaction: {required} bytes required, {available} bytes available"
        );
    }
    Ok(())
}

pub fn compact(root: &Path, backup: &Path) -> Result<CompactResult> {
    let _lock = StorageLock::acquire(root, true)?;
    crate::cortex::read_manifest(root)?;
    let path = database_path(root)?;
    let connection = Connection::open_with_flags(&path, OpenFlags::SQLITE_OPEN_READ_WRITE)?;
    connection.busy_timeout(Duration::from_secs(5))?;
    connection.execute_batch("PRAGMA synchronous=FULL")?;
    integrity_check(&connection).context("checking database before compaction")?;
    let before = read_stats(&connection, &path)?;
    require_headroom(
        before.available_disk_bytes,
        before.compact_required_free_bytes,
    )?;
    checkpoint(&connection)?;
    crate::restore::backup_without_storage_lock(root, backup, false)
        .context("creating required compaction backup")?;
    let result = (|| -> Result<StorageStats> {
        // The backup may share this volume, so check again after it has consumed space.
        require_headroom(
            fs2::available_space(path.parent().unwrap())?,
            before.compact_required_free_bytes,
        )?;
        pause_for_test("backed-up")?;
        connection
            .execute_batch("VACUUM")
            .context("vacuuming database")?;
        pause_for_test("vacuumed")?;
        integrity_check(&connection).context("checking database after compaction")?;
        checkpoint(&connection)?;
        read_stats(&connection, &path)
    })();
    let after = result.with_context(|| {
        format!(
            "compaction did not finish verification; backup retained at {}",
            backup.display()
        )
    })?;
    Ok(CompactResult {
        reclaimed_bytes: before.database_bytes.saturating_sub(after.database_bytes),
        before,
        after,
        backup_path: backup.to_owned(),
    })
}

fn pause_for_test(phase: &str) -> Result<()> {
    #[cfg(debug_assertions)]
    if std::env::var("NOEMA_TEST_COMPACT_PHASE").ok().as_deref() == Some(phase)
        && let Some(marker) = std::env::var_os("NOEMA_TEST_COMPACT_PAUSE")
    {
        fs::write(marker, phase)?;
        loop {
            std::thread::sleep(Duration::from_millis(50));
        }
    }
    let _ = phase;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::require_headroom;

    #[test]
    fn insufficient_disk_space_is_rejected() {
        assert!(require_headroom(99, 100).is_err());
        assert!(require_headroom(100, 100).is_ok());
    }
}
