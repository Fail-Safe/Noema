#![cfg(debug_assertions)]

use std::{
    fs,
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    thread,
    time::{Duration, Instant},
};

use noema::{cortex::Cortex, trace::Trace};
use rusqlite::Connection;

fn initialize_cortex() -> (tempfile::TempDir, PathBuf, PathBuf) {
    let temp = tempfile::tempdir().unwrap();
    let config_home = temp.path().join("config");
    let cortex_parent = temp.path().join("cortexes");
    let init = Command::new(env!("CARGO_BIN_EXE_noema"))
        .env("XDG_CONFIG_HOME", &config_home)
        .args([
            "init",
            "--name",
            "crash-test",
            "--path",
            cortex_parent.to_str().unwrap(),
        ])
        .output()
        .unwrap();
    assert!(
        init.status.success(),
        "{}",
        String::from_utf8_lossy(&init.stderr)
    );
    let root = cortex_parent.join("crash-test");
    (temp, config_home, root)
}

fn wait_for_fault_marker(child: &mut Child, marker: &Path) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !marker.exists() && Instant::now() < deadline {
        if let Some(status) = child.try_wait().unwrap() {
            panic!("fault-injected child exited before pause: {status}");
        }
        thread::sleep(Duration::from_millis(10));
    }
    if !marker.exists() {
        let _ = child.kill();
        let _ = child.wait();
        panic!("timed out waiting for post-mutation fault marker");
    }
}

#[test]
#[ignore = "subprocess entry point"]
fn external_delete_recovery_child() {
    let Some(root) = std::env::var_os("NOEMA_RUST_TEST_EXTERNAL_DELETE_ROOT") else {
        return;
    };
    let id = std::env::var("NOEMA_RUST_TEST_EXTERNAL_DELETE_ID").unwrap();
    let cortex = Cortex::open("crash-test", PathBuf::from(root)).unwrap();
    cortex.ingest_external_delete(&id).unwrap();
}

#[test]
fn killed_update_is_rolled_back_on_next_open() {
    let (temp, config_home, root) = initialize_cortex();
    let binary = env!("CARGO_BIN_EXE_noema");
    let (id, path, original_bytes, original_permissions, original_events) = {
        let cortex = Cortex::open("crash-test", &root).unwrap();
        let mut trace = Trace::new("Crash boundary", "fact", "", vec![], "original body");
        cortex.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        let path = cortex.trace_file(&id, false);
        (
            id.clone(),
            path.clone(),
            fs::read(&path).unwrap(),
            fs::metadata(&path).unwrap().permissions(),
            cortex.history(&id).unwrap().len(),
        )
    };
    let marker = temp.path().join("replacement-complete");
    let mut child = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .env("NOEMA_DURABILITY", "strong")
        .env("NOEMA_RUST_TEST_PAUSE_AFTER_FILESYSTEM_MUTATION", &marker)
        .args([
            "--cortex",
            "crash-test",
            "append",
            &id,
            "--content",
            "uncommitted replacement",
        ])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();

    wait_for_fault_marker(&mut child, &marker);
    assert_ne!(fs::read(&path).unwrap(), original_bytes);
    child.kill().unwrap();
    let status = child.wait().unwrap();
    assert!(!status.success());

    let recovered = Cortex::open("crash-test", &root).unwrap();
    assert_eq!(fs::read(&path).unwrap(), original_bytes);
    assert_eq!(
        fs::metadata(&path).unwrap().permissions(),
        original_permissions
    );
    assert_eq!(recovered.get_trace(&id).unwrap().1.body, "original body");
    assert_eq!(recovered.history(&id).unwrap().len(), original_events);
    drop(recovered);

    let reopened = Cortex::open("crash-test", &root).unwrap();
    assert_eq!(reopened.get_trace(&id).unwrap().1.body, "original body");
    drop(reopened);
    let database = Connection::open(root.join("db/noema.db")).unwrap();
    let pending: i64 = database
        .query_row(
            "SELECT COUNT(*) FROM federation_state WHERE key LIKE 'rust_pending_mutation:%'",
            [],
            |row| row.get(0),
        )
        .unwrap();
    assert_eq!(pending, 0);
}

#[test]
fn killed_archive_is_moved_back_on_next_open() {
    let (temp, config_home, root) = initialize_cortex();
    let (id, active, archived, original_bytes, original_events) = {
        let cortex = Cortex::open("crash-test", &root).unwrap();
        let mut trace = Trace::new("Crash move", "fact", "", vec![], "active body");
        cortex.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        let active = cortex.trace_file(&id, false);
        (
            id.clone(),
            active.clone(),
            cortex.trace_file(&id, true),
            fs::read(active).unwrap(),
            cortex.history(&id).unwrap().len(),
        )
    };
    let marker = temp.path().join("move-complete");
    let mut child = Command::new(env!("CARGO_BIN_EXE_noema"))
        .env("XDG_CONFIG_HOME", &config_home)
        .env("NOEMA_DURABILITY", "strong")
        .env("NOEMA_RUST_TEST_PAUSE_AFTER_FILESYSTEM_MUTATION", &marker)
        .args(["--cortex", "crash-test", "archive", &id])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();

    wait_for_fault_marker(&mut child, &marker);
    assert!(!active.exists());
    assert_eq!(fs::read(&archived).unwrap(), original_bytes);
    let concurrent = Cortex::open("crash-test", &root).unwrap();
    assert!(concurrent.get(&id).unwrap().archived_at.is_empty());
    assert!(!active.exists());
    drop(concurrent);
    child.kill().unwrap();
    assert!(!child.wait().unwrap().success());

    let recovered = Cortex::open("crash-test", &root).unwrap();
    assert_eq!(fs::read(&active).unwrap(), original_bytes);
    assert!(!archived.exists());
    assert!(recovered.get(&id).unwrap().archived_at.is_empty());
    assert_eq!(recovered.history(&id).unwrap().len(), original_events);
}

#[test]
fn killed_create_is_removed_on_next_open() {
    let (temp, config_home, root) = initialize_cortex();
    let marker = temp.path().join("create-complete");
    let mut child = Command::new(env!("CARGO_BIN_EXE_noema"))
        .env("XDG_CONFIG_HOME", &config_home)
        .env("NOEMA_DURABILITY", "strong")
        .env("NOEMA_RUST_TEST_PAUSE_AFTER_FILESYSTEM_MUTATION", &marker)
        .args([
            "--cortex",
            "crash-test",
            "add",
            "--title",
            "Crash create",
            "--body",
            "uncommitted body",
        ])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();

    wait_for_fault_marker(&mut child, &marker);
    let files: Vec<_> = fs::read_dir(root.join("traces"))
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .filter(|path| path.extension().and_then(|extension| extension.to_str()) == Some("md"))
        .collect();
    assert_eq!(files.len(), 1);
    let database = Connection::open(root.join("db/noema.db")).unwrap();
    let traces: i64 = database
        .query_row("SELECT COUNT(*) FROM traces", [], |row| row.get(0))
        .unwrap();
    assert_eq!(traces, 0);
    drop(database);
    child.kill().unwrap();
    assert!(!child.wait().unwrap().success());

    let recovered = Cortex::open("crash-test", &root).unwrap();
    assert!(recovered.list(&Default::default()).unwrap().is_empty());
    assert!(!files[0].exists());
    assert!(recovered.events_since("", 10).unwrap().is_empty());
}

#[test]
fn killed_hard_delete_is_restored_on_next_open() {
    let (temp, config_home, root) = initialize_cortex();
    let (id, path, original_bytes, original_permissions, original_events) = {
        let cortex = Cortex::open("crash-test", &root).unwrap();
        let mut trace = Trace::new("Crash delete", "fact", "", vec![], "preserved body");
        cortex.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        let path = cortex.trace_file(&id, false);
        (
            id.clone(),
            path.clone(),
            fs::read(&path).unwrap(),
            fs::metadata(&path).unwrap().permissions(),
            cortex.history(&id).unwrap().len(),
        )
    };
    let marker = temp.path().join("delete-complete");
    let mut child = Command::new(env!("CARGO_BIN_EXE_noema"))
        .env("XDG_CONFIG_HOME", &config_home)
        .env("NOEMA_DURABILITY", "strong")
        .env("NOEMA_RUST_TEST_PAUSE_AFTER_FILESYSTEM_MUTATION", &marker)
        .args([
            "--cortex",
            "crash-test",
            "memory",
            "purge",
            &id,
            "--tier",
            "short",
            "--reason",
            "crash-test",
            "--confirm",
            "--hard",
        ])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();

    wait_for_fault_marker(&mut child, &marker);
    assert!(!path.exists());
    let database = Connection::open(root.join("db/noema.db")).unwrap();
    let traces: i64 = database
        .query_row("SELECT COUNT(*) FROM traces WHERE id=?1", [&id], |row| {
            row.get(0)
        })
        .unwrap();
    assert_eq!(traces, 1);
    drop(database);
    child.kill().unwrap();
    assert!(!child.wait().unwrap().success());

    let recovered = Cortex::open("crash-test", &root).unwrap();
    assert_eq!(fs::read(&path).unwrap(), original_bytes);
    assert_eq!(
        fs::metadata(&path).unwrap().permissions(),
        original_permissions
    );
    assert!(recovered.get(&id).is_ok());
    assert_eq!(recovered.history(&id).unwrap().len(), original_events);
}

#[test]
fn killed_external_delete_reconstruction_is_removed_on_next_open() {
    let (temp, _config_home, root) = initialize_cortex();
    let (id, active, trash, original_events) = {
        let cortex = Cortex::open("crash-test", &root).unwrap();
        let mut trace = Trace::new(
            "Crash external delete",
            "fact",
            "",
            vec![],
            "reconstructed body",
        );
        cortex.add(&mut trace).unwrap();
        let id = trace.frontmatter.id.clone();
        (
            id.clone(),
            cortex.trace_file(&id, false),
            cortex.trash_dir().join(format!("{id}.md")),
            cortex.history(&id).unwrap().len(),
        )
    };
    fs::remove_file(&active).unwrap();
    let marker = temp.path().join("external-delete-reconstruction-complete");
    let mut child = Command::new(std::env::current_exe().unwrap())
        .env("NOEMA_DURABILITY", "strong")
        .env("NOEMA_RUST_TEST_EXTERNAL_DELETE_ROOT", &root)
        .env("NOEMA_RUST_TEST_EXTERNAL_DELETE_ID", &id)
        .env("NOEMA_RUST_TEST_PAUSE_AFTER_FILESYSTEM_MUTATION", &marker)
        .args(["--ignored", "--exact", "external_delete_recovery_child"])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();

    wait_for_fault_marker(&mut child, &marker);
    assert!(trash.exists());
    assert!(
        Cortex::open("crash-test", &root)
            .unwrap()
            .get(&id)
            .unwrap()
            .trashed_at
            .is_empty()
    );
    child.kill().unwrap();
    assert!(!child.wait().unwrap().success());

    let recovered = Cortex::open("crash-test", &root).unwrap();
    assert!(!active.exists());
    assert!(!trash.exists());
    assert!(recovered.get(&id).unwrap().trashed_at.is_empty());
    assert_eq!(recovered.history(&id).unwrap().len(), original_events);
    recovered.ingest_external_delete(&id).unwrap();
    assert!(trash.exists());
    assert!(!recovered.get(&id).unwrap().trashed_at.is_empty());
}
