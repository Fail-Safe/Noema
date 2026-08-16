#![cfg(debug_assertions)]

use std::{
    fs,
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    thread,
    time::{Duration, Instant},
};

use noema_rs::{config::Config, cortex::Cortex, restore, trace::Trace};

struct Fixture {
    _temp: tempfile::TempDir,
    config_home: PathBuf,
    archive: PathBuf,
    destination_parent: PathBuf,
    destination: PathBuf,
    trace_id: String,
}

fn fixture() -> Fixture {
    let temp = tempfile::tempdir().unwrap();
    let source_parent = temp.path().join("source-cortexes");
    Cortex::create("source", &source_parent).unwrap();
    let source = source_parent.join("source");
    let cortex = Cortex::open("source", &source).unwrap();
    let mut trace = Trace::new("Restore crash", "fact", "", vec![], "restored body");
    cortex.add(&mut trace).unwrap();
    let trace_id = trace.frontmatter.id;
    drop(cortex);
    let archive = temp.path().join("source.tar.gz");
    restore::backup(&source, &archive, false).unwrap();

    let destination_parent = temp.path().join("restored-cortexes");
    let destination = destination_parent.join("source");
    fs::create_dir_all(&destination).unwrap();
    fs::write(
        destination.join("operator-data"),
        b"preserved old destination\n",
    )
    .unwrap();

    Fixture {
        config_home: temp.path().join("config"),
        archive,
        destination_parent,
        destination,
        trace_id,
        _temp: temp,
    }
}

fn spawn_restore(fixture: &Fixture, point: &str, marker: &Path) -> Child {
    Command::new(env!("CARGO_BIN_EXE_noema-rs"))
        .env("XDG_CONFIG_HOME", &fixture.config_home)
        .env("NOEMA_RUST_TEST_RESTORE_PAUSE_POINT", point)
        .env("NOEMA_RUST_TEST_RESTORE_PAUSE_MARKER", marker)
        .args([
            "cortex",
            "restore",
            fixture.archive.to_str().unwrap(),
            "--path",
            fixture.destination_parent.to_str().unwrap(),
            "--force",
        ])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap()
}

fn wait_for_marker(child: &mut Child, marker: &Path) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !marker.exists() && Instant::now() < deadline {
        if let Some(status) = child.try_wait().unwrap() {
            panic!("fault-injected restore exited before pause: {status}");
        }
        thread::sleep(Duration::from_millis(10));
    }
    if !marker.exists() {
        let _ = child.kill();
        let _ = child.wait();
        panic!("timed out waiting for restore fault marker");
    }
}

fn kill_at(fixture: &Fixture, point: &str) {
    let marker = fixture._temp.path().join(format!("{point}.marker"));
    let mut child = spawn_restore(fixture, point, &marker);
    wait_for_marker(&mut child, &marker);
    child.kill().unwrap();
    assert!(!child.wait().unwrap().success());
}

fn transaction_paths(parent: &Path, purpose: &str) -> Vec<PathBuf> {
    let prefix = format!(".noema-restore-{purpose}-");
    fs::read_dir(parent)
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .filter(|path| {
            path.file_name()
                .and_then(|name| name.to_str())
                .is_some_and(|name| name.starts_with(&prefix))
        })
        .collect()
}

fn load_saved_config(fixture: &Fixture) -> Option<Config> {
    let path = fixture.config_home.join("noema/config.yaml");
    fs::read(path)
        .ok()
        .map(|bytes| serde_yaml::from_slice(&bytes).unwrap())
}

fn assert_old_destination(path: &Path) {
    assert_eq!(
        fs::read(path.join("operator-data")).unwrap(),
        b"preserved old destination\n"
    );
}

fn assert_restored_destination(fixture: &Fixture) {
    let cortex = Cortex::open("source", &fixture.destination).unwrap();
    assert_eq!(
        cortex.get_trace(&fixture.trace_id).unwrap().1.body,
        "restored body"
    );
}

#[test]
fn killed_after_destination_preservation_keeps_old_and_incoming_trees() {
    let fixture = fixture();
    kill_at(&fixture, "destination-preserved");

    assert!(!fixture.destination.exists());
    let backups = transaction_paths(&fixture.destination_parent, "backup");
    let incoming = transaction_paths(&fixture.destination_parent, "incoming");
    assert_eq!(backups.len(), 1);
    assert_eq!(incoming.len(), 1);
    assert_old_destination(&backups[0]);
    assert!(incoming[0].join("cortex.md").is_file());
    assert!(load_saved_config(&fixture).is_none());
}

#[test]
fn killed_after_placement_keeps_new_tree_old_backup_and_config_unchanged() {
    let fixture = fixture();
    kill_at(&fixture, "restore-placed");

    assert_restored_destination(&fixture);
    let backups = transaction_paths(&fixture.destination_parent, "backup");
    assert_eq!(backups.len(), 1);
    assert_old_destination(&backups[0]);
    assert!(load_saved_config(&fixture).is_none());
}

#[test]
fn killed_after_config_save_keeps_registered_restore_and_old_backup() {
    let fixture = fixture();
    kill_at(&fixture, "config-saved");

    assert_restored_destination(&fixture);
    let config = load_saved_config(&fixture).unwrap();
    assert_eq!(
        config.cortexes.get("source").unwrap().path,
        fixture.destination
    );
    let backups = transaction_paths(&fixture.destination_parent, "backup");
    assert_eq!(backups.len(), 1);
    assert_old_destination(&backups[0]);
}
