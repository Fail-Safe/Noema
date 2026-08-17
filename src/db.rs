use std::{fs, path::Path, time::Duration};

use anyhow::{Context, Result};
use include_dir::{Dir, include_dir};
use rusqlite::{Connection, OpenFlags, OptionalExtension, params};

static MIGRATIONS: Dir<'_> = include_dir!("$CARGO_MANIFEST_DIR/migrations");

pub fn open(cortex_dir: &Path) -> Result<Connection> {
    let db_dir = cortex_dir.join("db");
    fs::create_dir_all(&db_dir)?;
    let connection = Connection::open(db_dir.join("noema.db"))?;
    configure(&connection)?;
    migrate(&connection)?;
    Ok(connection)
}

pub fn open_existing_without_migrations(cortex_dir: &Path) -> Result<Option<Connection>> {
    let path = cortex_dir.join("db/noema.db");
    if !path.exists() {
        return Ok(None);
    }
    let connection = Connection::open_with_flags(
        path,
        OpenFlags::SQLITE_OPEN_READ_WRITE | OpenFlags::SQLITE_OPEN_URI,
    )?;
    connection.busy_timeout(Duration::from_secs(5))?;
    Ok(Some(connection))
}

fn configure(connection: &Connection) -> Result<()> {
    connection.busy_timeout(Duration::from_secs(5))?;
    connection.execute_batch(
        "PRAGMA journal_mode = WAL;
         PRAGMA foreign_keys = ON;",
    )?;
    Ok(())
}

pub fn migrate(connection: &Connection) -> Result<()> {
    connection.execute(
        "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER NOT NULL PRIMARY KEY)",
        [],
    )?;
    let mut files: Vec<_> = MIGRATIONS.files().collect();
    files.sort_by_key(|file| file.path().to_owned());
    for file in files {
        let Some(name) = file.path().file_name().and_then(|name| name.to_str()) else {
            continue;
        };
        let Some(version) = name
            .split('_')
            .next()
            .and_then(|prefix| prefix.parse::<i64>().ok())
        else {
            continue;
        };
        let applied: Option<i64> = connection
            .query_row(
                "SELECT version FROM schema_migrations WHERE version = ?1",
                [version],
                |row| row.get(0),
            )
            .optional()?;
        if applied.is_some() {
            continue;
        }
        let sql = file
            .contents_utf8()
            .with_context(|| format!("migration {name} is not UTF-8"))?;
        let tx = connection.unchecked_transaction()?;
        tx.execute_batch(sql)
            .with_context(|| format!("applying migration {name}"))?;
        tx.execute(
            "INSERT INTO schema_migrations(version) VALUES (?1)",
            params![version],
        )?;
        tx.commit()?;
    }
    Ok(())
}

pub fn checkpoint_wal(cortex_dir: &Path) -> Result<()> {
    let Some(connection) = open_existing_without_migrations(cortex_dir)? else {
        return Ok(());
    };
    connection
        .execute_batch("PRAGMA wal_checkpoint(TRUNCATE)")
        .context("checkpointing WAL")?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn applies_complete_migration_history() {
        let temp = tempfile::tempdir().unwrap();
        let connection = open(temp.path()).unwrap();
        let version: i64 = connection
            .query_row("SELECT MAX(version) FROM schema_migrations", [], |row| {
                row.get(0)
            })
            .unwrap();
        assert_eq!(version, 19);
        let fts: String = connection
            .query_row(
                "SELECT sql FROM sqlite_master WHERE name='traces_fts'",
                [],
                |row| row.get(0),
            )
            .unwrap();
        assert!(fts.contains("fts5"));
    }
}
