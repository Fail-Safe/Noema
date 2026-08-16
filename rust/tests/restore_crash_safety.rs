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

fn transaction_id(fixture: &Fixture) -> String {
    let directory = fixture.config_home.join("noema/restore-transactions");
    let records = fs::read_dir(directory)
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .filter(|path| path.extension().and_then(|value| value.to_str()) == Some("json"))
        .collect::<Vec<_>>();
    assert_eq!(records.len(), 1);
    records[0].file_stem().unwrap().to_str().unwrap().to_owned()
}

#[cfg(unix)]
fn assert_private_transaction_storage(fixture: &Fixture, id: &str) {
    use std::os::unix::fs::PermissionsExt;

    let directory = fixture.config_home.join("noema/restore-transactions");
    assert_eq!(
        fs::metadata(&directory).unwrap().permissions().mode() & 0o777,
        0o700
    );
    assert_eq!(
        fs::metadata(directory.join(format!("{id}.json")))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o600
    );
}

fn restore_status(fixture: &Fixture) -> String {
    let output = Command::new(env!("CARGO_BIN_EXE_noema-rs"))
        .env("XDG_CONFIG_HOME", &fixture.config_home)
        .args(["cortex", "restore-status"])
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout).unwrap()
}

fn recovery_output(fixture: &Fixture, id: &str, action: &str) -> std::process::Output {
    Command::new(env!("CARGO_BIN_EXE_noema-rs"))
        .env("XDG_CONFIG_HOME", &fixture.config_home)
        .args(["cortex", "restore-recover", id, "--action", action])
        .output()
        .unwrap()
}

fn recover(fixture: &Fixture, id: &str, action: &str) {
    let output = recovery_output(fixture, id, action);
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
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

    let id = transaction_id(&fixture);
    #[cfg(unix)]
    assert_private_transaction_storage(&fixture, &id);
    let status = restore_status(&fixture);
    assert!(status.contains(&id));
    assert!(status.contains("state=resumable"));
    assert!(!status.contains(fixture.destination_parent.to_str().unwrap()));
    assert!(!status.contains("sha256:"));
    recover(&fixture, &id, "resume");
    assert_restored_destination(&fixture);
    assert!(
        load_saved_config(&fixture)
            .unwrap()
            .cortexes
            .contains_key("source")
    );
    assert!(transaction_paths(&fixture.destination_parent, "backup").is_empty());
    assert!(transaction_paths(&fixture.destination_parent, "incoming").is_empty());
    assert_eq!(restore_status(&fixture), "Restore transactions: clean\n");
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

    let id = transaction_id(&fixture);
    assert!(restore_status(&fixture).contains("state=resumable"));
    recover(&fixture, &id, "rollback");
    assert_old_destination(&fixture.destination);
    assert!(load_saved_config(&fixture).is_none());
    assert!(transaction_paths(&fixture.destination_parent, "backup").is_empty());
    assert_eq!(restore_status(&fixture), "Restore transactions: clean\n");
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

    let id = transaction_id(&fixture);
    assert!(restore_status(&fixture).contains("state=committed-cleanup"));
    recover(&fixture, &id, "resume");
    assert_restored_destination(&fixture);
    assert!(transaction_paths(&fixture.destination_parent, "backup").is_empty());
    assert_eq!(restore_status(&fixture), "Restore transactions: clean\n");
}

#[test]
fn committed_restore_can_be_explicitly_rolled_back() {
    let fixture = fixture();
    kill_at(&fixture, "config-saved");
    let id = transaction_id(&fixture);

    recover(&fixture, &id, "rollback");
    assert_old_destination(&fixture.destination);
    let config = load_saved_config(&fixture).unwrap();
    assert!(!config.cortexes.contains_key("source"));
    assert!(config.default.is_empty());
    assert!(transaction_paths(&fixture.destination_parent, "backup").is_empty());
    assert_eq!(restore_status(&fixture), "Restore transactions: clean\n");
}

#[test]
fn restore_into_empty_destination_can_be_rolled_back_after_process_death() {
    let fixture = fixture();
    fs::remove_dir_all(&fixture.destination).unwrap();
    kill_at(&fixture, "restore-placed");
    let id = transaction_id(&fixture);

    recover(&fixture, &id, "rollback");
    assert!(!fixture.destination.exists());
    assert!(load_saved_config(&fixture).is_none());
    assert_eq!(restore_status(&fixture), "Restore transactions: clean\n");
}

#[test]
fn tampered_incoming_tree_blocks_resume_without_removing_evidence() {
    let fixture = fixture();
    kill_at(&fixture, "destination-preserved");
    let id = transaction_id(&fixture);
    let incoming = transaction_paths(&fixture.destination_parent, "incoming");
    let backups = transaction_paths(&fixture.destination_parent, "backup");
    assert_eq!(incoming.len(), 1);
    assert_eq!(backups.len(), 1);
    fs::write(incoming[0].join("cortex.md"), b"tampered\n").unwrap();

    assert!(restore_status(&fixture).contains("state=rollback-only"));
    let output = recovery_output(&fixture, &id, "resume");
    assert!(!output.status.success());
    let stderr = String::from_utf8(output.stderr).unwrap();
    assert!(!stderr.contains("sha256:"));
    assert!(!stderr.contains(fixture.destination_parent.to_str().unwrap()));
    assert_old_destination(&backups[0]);
    assert!(incoming[0].exists());
    assert_eq!(transaction_id(&fixture), id);
}

#[test]
fn malformed_restore_transaction_status_redacts_record_and_path() {
    let fixture = fixture();
    let directory = fixture.config_home.join("noema/restore-transactions");
    fs::create_dir_all(&directory).unwrap();
    let record = directory.join("01M00000000000000000000000.json");
    let sensitive = "private restore transaction content";
    fs::write(&record, sensitive).unwrap();

    let status = restore_status(&fixture);
    assert_eq!(status, "Malformed restore transaction record(s): 1\n");
    assert!(!status.contains(sensitive));
    assert!(!status.contains(record.to_str().unwrap()));
    assert!(!status.contains(fixture.destination_parent.to_str().unwrap()));
}

#[test]
fn concurrent_restore_to_same_destination_is_rejected_before_a_second_journal() {
    let fixture = fixture();
    let marker = fixture._temp.path().join("concurrent.marker");
    let mut first = spawn_restore(&fixture, "destination-preserved", &marker);
    wait_for_marker(&mut first, &marker);

    let second = Command::new(env!("CARGO_BIN_EXE_noema-rs"))
        .env("XDG_CONFIG_HOME", &fixture.config_home)
        .args([
            "cortex",
            "restore",
            fixture.archive.to_str().unwrap(),
            "--path",
            fixture.destination_parent.to_str().unwrap(),
            "--force",
        ])
        .output()
        .unwrap();
    assert!(!second.status.success());
    assert!(
        String::from_utf8(second.stderr)
            .unwrap()
            .contains("already in progress")
    );
    let id = transaction_id(&fixture);
    assert_eq!(restore_status(&fixture).matches(&id).count(), 1);

    first.kill().unwrap();
    assert!(!first.wait().unwrap().success());
    recover(&fixture, &id, "resume");
    assert_restored_destination(&fixture);
    assert_eq!(restore_status(&fixture), "Restore transactions: clean\n");
}
