use std::{
    fs,
    path::Path,
    process::{Command, Stdio},
    thread,
    time::{Duration, Instant},
};

use noema::{
    cortex::Cortex,
    db, embedding, maintenance,
    storage::{self, DatabaseStorage},
    trace::Trace,
};
use rusqlite::{Connection, params};

fn cli(config: &Path, args: &[&str]) -> std::process::Output {
    Command::new(env!("CARGO_BIN_EXE_noema"))
        .env("XDG_CONFIG_HOME", config)
        .args(args)
        .output()
        .unwrap()
}

fn success(config: &Path, args: &[&str]) -> std::process::Output {
    let output = cli(config, args);
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    output
}

fn populate(root: &Path) -> String {
    let cx = Cortex::open("sample", root).unwrap();
    let mut trace = Trace::new(
        "Preserved history",
        "fact",
        "",
        vec!["storage".into()],
        "searchable quartz",
    );
    cx.add(&mut trace).unwrap();
    let id = trace.frontmatter.id.clone();
    cx.set_federation_state("maintenance-test", "durable cursor")
        .unwrap();
    drop(cx);
    let database = db::open(root).unwrap();
    database.execute("INSERT INTO trace_embeddings(trace_id,embedding_model,dim,embedding,source_hash,updated_at) VALUES (?1,'test-model',3,?2,'test-hash','2026-01-01T00:00:00Z')", params![id, embedding::encode(&[1.0, 0.0, 0.0])]).unwrap();
    database
        .execute_batch(
            "CREATE TABLE space_fixture(value BLOB);
        WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<32)
        INSERT INTO space_fixture SELECT zeroblob(65536) FROM n;
        DROP TABLE space_fixture;",
        )
        .unwrap();
    id
}

fn assert_preserved(root: &Path, id: &str, history: usize) {
    let cx = Cortex::open("sample", root).unwrap();
    assert_eq!(cx.get_trace(id).unwrap().1.body, "searchable quartz");
    assert_eq!(cx.history(id).unwrap().len(), history);
    assert_eq!(
        cx.federation_state("maintenance-test").unwrap(),
        "durable cursor"
    );
    let connection = db::open(root).unwrap();
    let vector: Vec<u8> = connection
        .query_row(
            "SELECT embedding FROM trace_embeddings WHERE trace_id=?1",
            [id],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(vector, embedding::encode(&[1.0, 0.0, 0.0]));
    let hits: i64 = connection
        .query_row(
            "SELECT count(*) FROM traces_fts WHERE traces_fts MATCH 'quartz'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(hits, 1);
}

#[test]
fn compact_preserves_data_and_restorable_backup_in_both_layouts() {
    for mode in [DatabaseStorage::Default, DatabaseStorage::Nosync] {
        let temp = tempfile::tempdir().unwrap();
        let config = temp.path().join("config");
        success(
            &config,
            &[
                "init",
                "--name",
                "sample",
                "--path",
                temp.path().to_str().unwrap(),
            ],
        );
        let root = temp.path().join("sample");
        let id = populate(&root);
        let history = Cortex::open("sample", &root)
            .unwrap()
            .history(&id)
            .unwrap()
            .len();
        if mode == DatabaseStorage::Nosync {
            storage::migrate(
                &root,
                Some(mode),
                Some(&temp.path().join("migration.tar.gz")),
                false,
            )
            .unwrap();
        }
        let manifest = fs::read(root.join("cortex.md")).unwrap();
        let trace = fs::read(root.join("traces").join(format!("{id}.md"))).unwrap();
        let before = maintenance::storage_stats(&root).unwrap();
        assert!(before.reusable_bytes > 1024 * 1024);
        let backup = temp.path().join("before-compact.tar.gz");
        let output = success(
            &config,
            &[
                "cortex",
                "compact",
                "sample",
                "--backup",
                backup.to_str().unwrap(),
                "--json",
            ],
        );
        let report: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
        assert!(report["reclaimed_bytes"].as_u64().unwrap() > 1024 * 1024);
        assert_eq!(report["after"]["reusable_bytes"], 0);
        assert_eq!(report["after"]["wal_bytes"], 0);
        assert_eq!(fs::read(root.join("cortex.md")).unwrap(), manifest);
        assert_eq!(
            fs::read(root.join("traces").join(format!("{id}.md"))).unwrap(),
            trace
        );
        assert_preserved(&root, &id, history);
        let restored_parent = temp.path().join("restored");
        success(
            &temp.path().join("restore-config"),
            &[
                "cortex",
                "restore",
                backup.to_str().unwrap(),
                "--path",
                restored_parent.to_str().unwrap(),
                "--name",
                "restored",
            ],
        );
        assert_preserved(&restored_parent.join("restored"), &id, history);
        assert!(
            restored_parent
                .join("restored")
                .join(mode.directory_name())
                .join("noema.db")
                .is_file()
        );
    }
}

#[test]
fn status_is_read_only_while_compact_refuses_live_clients() {
    let temp = tempfile::tempdir().unwrap();
    let config = temp.path().join("config");
    success(
        &config,
        &[
            "init",
            "--name",
            "sample",
            "--path",
            temp.path().to_str().unwrap(),
        ],
    );
    let root = temp.path().join("sample");
    populate(&root);
    let cx = Cortex::open("sample", &root).unwrap();
    let observer = db::open(&root).unwrap();
    let version: i64 = observer
        .query_row("PRAGMA data_version", [], |row| row.get(0))
        .unwrap();
    let stats = success(&config, &["cortex", "storage", "sample", "--json"]);
    let stats: serde_json::Value = serde_json::from_slice(&stats.stdout).unwrap();
    assert_eq!(stats["database"], "default");
    assert!(stats["reusable_bytes"].as_u64().unwrap() > 0);
    let after: i64 = observer
        .query_row("PRAGMA data_version", [], |row| row.get(0))
        .unwrap();
    assert_eq!(version, after);
    let backup = temp.path().join("busy.tar.gz");
    let output = cli(
        &config,
        &[
            "cortex",
            "compact",
            "sample",
            "--backup",
            backup.to_str().unwrap(),
        ],
    );
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("storage is in use"));
    assert!(!backup.exists());
    drop(cx);
}

#[test]
fn compact_refuses_unsafe_backup_and_missing_database() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("sample", temp.path()).unwrap();
    let root = temp.path().join("sample");
    populate(&root);
    let before = maintenance::storage_stats(&root).unwrap();
    assert!(maintenance::compact(&root, &root.join("backup.tar.gz")).is_err());
    let backup = temp.path().join("existing.tar.gz");
    fs::write(&backup, b"preserve existing archive").unwrap();
    assert!(maintenance::compact(&root, &backup).is_err());
    assert_eq!(fs::read(&backup).unwrap(), b"preserve existing archive");
    assert_eq!(
        maintenance::storage_stats(&root).unwrap().reusable_bytes,
        before.reusable_bytes
    );
    fs::rename(root.join("db/noema.db"), temp.path().join("saved.db")).unwrap();
    assert!(maintenance::storage_stats(&root).is_err());
    assert!(maintenance::compact(&root, &temp.path().join("missing.tar.gz")).is_err());
    assert!(!root.join("db/noema.db").exists());
}

#[test]
#[cfg(debug_assertions)]
fn killed_compaction_keeps_database_and_backup_readable() {
    for phase in ["backed-up", "vacuumed"] {
        let temp = tempfile::tempdir().unwrap();
        let config = temp.path().join("config");
        success(
            &config,
            &[
                "init",
                "--name",
                "sample",
                "--path",
                temp.path().to_str().unwrap(),
            ],
        );
        let root = temp.path().join("sample");
        let id = populate(&root);
        let history = Cortex::open("sample", &root)
            .unwrap()
            .history(&id)
            .unwrap()
            .len();
        let marker = temp.path().join("paused");
        let backup = temp.path().join("before.tar.gz");
        let mut child = Command::new(env!("CARGO_BIN_EXE_noema"))
            .env("XDG_CONFIG_HOME", &config)
            .env("NOEMA_TEST_COMPACT_PHASE", phase)
            .env("NOEMA_TEST_COMPACT_PAUSE", &marker)
            .args([
                "cortex",
                "compact",
                "sample",
                "--backup",
                backup.to_str().unwrap(),
            ])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        let deadline = Instant::now() + Duration::from_secs(15);
        while !marker.exists() && Instant::now() < deadline {
            assert!(
                child.try_wait().unwrap().is_none(),
                "compaction exited before {phase}"
            );
            thread::sleep(Duration::from_millis(20));
        }
        assert!(marker.exists());
        assert!(maintenance::storage_stats(&root).is_err());
        child.kill().unwrap();
        child.wait().unwrap();
        assert_preserved(&root, &id, history);
        let restored_parent = temp.path().join("restored");
        success(
            &temp.path().join("restore-config"),
            &[
                "cortex",
                "restore",
                backup.to_str().unwrap(),
                "--path",
                restored_parent.to_str().unwrap(),
                "--name",
                "restored",
            ],
        );
        assert_preserved(&restored_parent.join("restored"), &id, history);
        let connection = Connection::open(root.join("db/noema.db")).unwrap();
        let integrity: String = connection
            .query_row("PRAGMA integrity_check", [], |row| row.get(0))
            .unwrap();
        assert_eq!(integrity, "ok");
    }
}

#[test]
fn compact_refuses_sqlite_writer_outside_noema_lock() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("sample", temp.path()).unwrap();
    let root = temp.path().join("sample");
    populate(&root);
    let writer = Connection::open(root.join("db/noema.db")).unwrap();
    writer.execute_batch("BEGIN IMMEDIATE").unwrap();
    let backup = temp.path().join("busy.tar.gz");
    let error = maintenance::compact(&root, &backup).unwrap_err();
    assert!(format!("{error:#}").contains("busy"));
    assert!(!backup.exists());
    writer.execute_batch("ROLLBACK").unwrap();
}
