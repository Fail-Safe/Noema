use std::fs;

use noema_rs::{cortex::Cortex, trace::Trace};
use rusqlite::{Connection, params};

fn populated_cortex() -> (
    tempfile::TempDir,
    std::path::PathBuf,
    String,
    std::path::PathBuf,
) {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("recovery", temp.path()).unwrap();
    let root = temp.path().join("recovery");
    let cortex = Cortex::open("recovery", &root).unwrap();
    let mut trace = Trace::new("Recovery safety", "fact", "", vec![], "preserved body");
    cortex.add(&mut trace).unwrap();
    let id = trace.frontmatter.id.clone();
    let path = cortex.trace_file(&id, false);
    drop(cortex);
    (temp, root, id, path)
}

#[test]
fn corrupt_database_fails_without_rewriting_database_or_trace() {
    let (_temp, root, _id, trace_path) = populated_cortex();
    let database_path = root.join("db/noema.db");
    let trace_bytes = fs::read(&trace_path).unwrap();
    let corrupt_bytes = b"not a sqlite database\n";
    fs::write(&database_path, corrupt_bytes).unwrap();

    let error = Cortex::open("recovery", &root).err().unwrap();

    assert!(!error.to_string().contains("not a sqlite database"));
    assert_eq!(fs::read(&database_path).unwrap(), corrupt_bytes);
    assert_eq!(fs::read(&trace_path).unwrap(), trace_bytes);
}

#[test]
fn malformed_pending_record_fails_closed_without_clearing_it() {
    let (_temp, root, _id, trace_path) = populated_cortex();
    let trace_bytes = fs::read(&trace_path).unwrap();
    let database_path = root.join("db/noema.db");
    let database = Connection::open(&database_path).unwrap();
    let key = "rust_pending_mutation:01M00000000000000000000000";
    database
        .execute(
            "INSERT INTO federation_state(key,value) VALUES (?1,?2)",
            params![key, "not-json"],
        )
        .unwrap();
    drop(database);

    assert!(Cortex::open("recovery", &root).is_err());
    assert_eq!(fs::read(&trace_path).unwrap(), trace_bytes);
    let database = Connection::open(database_path).unwrap();
    let count: i64 = database
        .query_row(
            "SELECT COUNT(*) FROM federation_state WHERE key=?1",
            [key],
            |row| row.get(0),
        )
        .unwrap();
    assert_eq!(count, 1);
}

#[test]
fn pending_record_path_traversal_fails_closed() {
    let (_temp, root, id, trace_path) = populated_cortex();
    let trace_bytes = fs::read(&trace_path).unwrap();
    let database_path = root.join("db/noema.db");
    let database = Connection::open(&database_path).unwrap();
    let key = "rust_pending_mutation:01M00000000000000000000000";
    let record = serde_json::json!({
        "version": 1,
        "kind": "delete",
        "trace_id": id,
        "relative_path": "../../outside.md",
        "source_path": "",
        "target_path": "",
        "original_bytes": "",
        "replacement_hash": "",
        "original_mode": null,
        "original_readonly": false
    });
    database
        .execute(
            "INSERT INTO federation_state(key,value) VALUES (?1,?2)",
            params![key, record.to_string()],
        )
        .unwrap();
    drop(database);

    assert!(Cortex::open("recovery", &root).is_err());
    assert_eq!(fs::read(&trace_path).unwrap(), trace_bytes);
    assert!(!root.parent().unwrap().join("outside.md").exists());
}
