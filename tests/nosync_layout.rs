use std::{
    fs,
    path::Path,
    process::{Command, Stdio},
    thread,
    time::{Duration, Instant},
};

use noema::{
    cortex::Cortex,
    db,
    storage::{self, DatabaseStorage},
    trace::Trace,
};

fn cli(config: &Path, args: &[&str]) {
    let output = Command::new(env!("CARGO_BIN_EXE_noema"))
        .env("XDG_CONFIG_HOME", config)
        .args(args)
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
}

#[test]
fn nosync_preserves_history_and_round_trips_through_backup_restore() {
    let temp = tempfile::tempdir().unwrap();
    let config = temp.path().join("config");
    cli(
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
    let cx = Cortex::open("sample", &root).unwrap();
    let mut trace = Trace::new("Storage trial", "fact", "", vec![], "preserved body");
    cx.add(&mut trace).unwrap();
    let id = trace.frontmatter.id.clone();
    let history = cx.history(&id).unwrap().len();
    cx.set_federation_state("trial-cursor", "preserved cursor")
        .unwrap();
    drop(cx);
    cli(
        &config,
        &[
            "cortex",
            "storage",
            "sample",
            "--database",
            "nosync",
            "--backup",
            temp.path().join("before.tar.gz").to_str().unwrap(),
        ],
    );
    cli(&config, &["verify", "cortex"]);
    let archive = temp.path().join("backup.tar.gz");
    cli(
        &config,
        &[
            "cortex",
            "backup",
            "sample",
            "--output",
            archive.to_str().unwrap(),
        ],
    );
    let destination = temp.path().join("restored");
    cli(
        &temp.path().join("restore-config"),
        &[
            "cortex",
            "restore",
            archive.to_str().unwrap(),
            "--path",
            destination.to_str().unwrap(),
            "--name",
            "restored",
        ],
    );
    let restored = destination.join("restored");
    let cx = Cortex::open("restored", &restored).unwrap();
    assert_eq!(cx.get_trace(&id).unwrap().1.body, "preserved body");
    assert_eq!(cx.history(&id).unwrap().len(), history);
    assert_eq!(
        cx.federation_state("trial-cursor").unwrap(),
        "preserved cursor"
    );
    assert!(restored.join("db.nosync/noema.db").is_file());
    assert!(restored.join("db/noema.db").is_file());
    assert!(root.join("db/noema.db").is_file());
    drop(cx);
    storage::migrate(
        &restored,
        Some(DatabaseStorage::Default),
        Some(&temp.path().join("reverse.tar.gz")),
        false,
    )
    .unwrap();
    let cx = Cortex::open("restored", &restored).unwrap();
    assert_eq!(cx.history(&id).unwrap().len(), history);
    assert_eq!(
        cx.federation_state("trial-cursor").unwrap(),
        "preserved cursor"
    );
    assert!(!restored.join("db.nosync").exists());
}

#[test]
fn migration_refuses_live_clients_and_preserves_configuration_bytes() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("sample", temp.path()).unwrap();
    let root = temp.path().join("sample");
    let manifest = fs::read(root.join("cortex.md")).unwrap();
    fs::write(
        root.join("storage.yaml"),
        "# Storage preference\nfuture: {keep: true}\ndatabase: default # local choice\n",
    )
    .unwrap();
    let backup = temp.path().join("before.tar.gz");
    let cx = Cortex::open("sample", &root).unwrap();
    let error =
        storage::migrate(&root, Some(DatabaseStorage::Nosync), Some(&backup), false).unwrap_err();
    assert!(format!("{error:#}").contains("storage is in use"));
    assert!(!backup.exists());
    drop(cx);
    storage::migrate(&root, Some(DatabaseStorage::Nosync), Some(&backup), false).unwrap();
    assert_eq!(fs::read(root.join("cortex.md")).unwrap(), manifest);
    assert_eq!(
        fs::read_to_string(root.join("storage.yaml")).unwrap(),
        "# Storage preference\nfuture: {keep: true}\ndatabase: nosync # local choice\n"
    );
    // A pre-feature client opening the legacy SQLite path cannot initialize a second database.
    let old = rusqlite::Connection::open(root.join("db/noema.db")).unwrap();
    assert!(
        old.execute_batch("PRAGMA journal_mode=WAL; CREATE TABLE accidental(id);")
            .is_err()
    );
    drop(old);
    storage::migrate(&root, Some(DatabaseStorage::Nosync), None, false).unwrap();
}

#[test]
fn trial_layout_is_adopted_and_missing_local_database_fails_closed() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("sample", temp.path()).unwrap();
    let root = temp.path().join("sample");
    fs::rename(root.join("db"), root.join("db.nosync")).unwrap();
    let before = fs::read(root.join("db.nosync/noema.db")).unwrap();
    storage::migrate(
        &root,
        Some(DatabaseStorage::Nosync),
        Some(&temp.path().join("before.tar.gz")),
        false,
    )
    .unwrap();
    assert_eq!(fs::read(root.join("db.nosync/noema.db")).unwrap(), before);
    fs::rename(root.join("db.nosync"), temp.path().join("saved-db")).unwrap();
    assert!(
        format!("{:#}", Cortex::open("sample", &root).err().unwrap())
            .contains("restore a full Noema backup")
    );
    assert!(!root.join("db.nosync").exists());
}

#[test]
fn migration_requires_external_backup_and_rejects_conflicting_layouts() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("sample", temp.path()).unwrap();
    let root = temp.path().join("sample");
    assert!(storage::migrate(&root, Some(DatabaseStorage::Nosync), None, false).is_err());
    assert!(
        storage::migrate(
            &root,
            Some(DatabaseStorage::Nosync),
            Some(&root.join("backup.tar.gz")),
            false
        )
        .is_err()
    );
    assert!(!root.join(".noema-storage-migration.json").exists());
    fs::create_dir(root.join("db.nosync")).unwrap();
    assert!(
        storage::migrate(
            &root,
            Some(DatabaseStorage::Nosync),
            Some(&temp.path().join("backup.tar.gz")),
            false
        )
        .is_err()
    );
    assert!(root.join("db/noema.db").is_file());
}

#[test]
fn unsupported_yaml_edit_preserves_unknown_keys_and_database() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("sample", temp.path()).unwrap();
    let root = temp.path().join("sample");
    let config = "database:future: retained\n";
    fs::write(root.join("storage.yaml"), config).unwrap();
    let backup = temp.path().join("before.tar.gz");
    assert!(storage::migrate(&root, Some(DatabaseStorage::Nosync), Some(&backup), false).is_err());
    assert_eq!(
        fs::read_to_string(root.join("storage.yaml")).unwrap(),
        config
    );
    assert!(root.join("db/noema.db").is_file());
    assert!(!backup.exists());
    assert!(!root.join(".noema-storage-migration.json").exists());
}

#[test]
#[cfg(debug_assertions)]
fn killed_storage_migrations_resume_in_both_directions() {
    for direction in ["nosync", "default"] {
        for phase in ["journal", "moved", "configured"] {
            let temp = tempfile::tempdir().unwrap();
            let config = temp.path().join("config");
            cli(
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
            let cx = Cortex::open("sample", &root).unwrap();
            cx.set_federation_state("storage-test", "durable cursor")
                .unwrap();
            drop(cx);
            if direction == "default" {
                storage::migrate(
                    &root,
                    Some(DatabaseStorage::Nosync),
                    Some(&temp.path().join("initial.tar.gz")),
                    false,
                )
                .unwrap();
            }
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                let path = root.join("storage.yaml");
                if !path.exists() {
                    fs::write(&path, "database: default\n").unwrap();
                }
                fs::set_permissions(path, fs::Permissions::from_mode(0o600)).unwrap();
            }
            let marker = temp.path().join("paused");
            let mut child = Command::new(env!("CARGO_BIN_EXE_noema"))
                .env("XDG_CONFIG_HOME", &config)
                .env("NOEMA_TEST_STORAGE_PHASE", phase)
                .env("NOEMA_TEST_STORAGE_PAUSE", &marker)
                .args([
                    "cortex",
                    "storage",
                    "sample",
                    "--database",
                    direction,
                    "--backup",
                    temp.path().join("before.tar.gz").to_str().unwrap(),
                ])
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .spawn()
                .unwrap();
            let deadline = Instant::now() + Duration::from_secs(15);
            while !marker.exists() && Instant::now() < deadline {
                assert!(
                    child.try_wait().unwrap().is_none(),
                    "migration exited before {phase}"
                );
                thread::sleep(Duration::from_millis(20));
            }
            assert!(marker.exists(), "migration never reached {phase}");
            assert!(Cortex::open("sample", &root).is_err());
            child.kill().unwrap();
            child.wait().unwrap();
            assert!(
                format!("{:#}", Cortex::open("sample", &root).err().unwrap()).contains("--resume")
            );
            cli(&config, &["cortex", "storage", "sample", "--resume"]);
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                assert_eq!(
                    fs::metadata(root.join("storage.yaml"))
                        .unwrap()
                        .permissions()
                        .mode()
                        & 0o777,
                    0o600
                );
            }
            let cx = Cortex::open("sample", &root).unwrap();
            assert_eq!(
                cx.federation_state("storage-test").unwrap(),
                "durable cursor"
            );
            assert_eq!(
                cx.db_dir.file_name().unwrap(),
                if direction == "nosync" {
                    "db.nosync"
                } else {
                    "db"
                }
            );
            assert!(!root.join(".noema-storage-migration.json").exists());
        }
    }
}

#[test]
fn ambiguous_directories_fail_without_creating_a_database() {
    let temp = tempfile::tempdir().unwrap();
    fs::create_dir(temp.path().join("db")).unwrap();
    fs::create_dir(temp.path().join("db.nosync")).unwrap();
    let error = db::open(temp.path()).unwrap_err();
    assert!(error.to_string().contains("both db and db.nosync"));
    assert!(!temp.path().join("db/noema.db").exists());
    assert!(!temp.path().join("db.nosync/noema.db").exists());
}

#[test]
fn recovery_artifact_uses_the_selected_directory() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("sample", temp.path()).unwrap();
    let root = temp.path().join("sample");
    fs::rename(root.join("db"), root.join("db.nosync")).unwrap();
    let cx = Cortex::open("sample", &root).unwrap();
    let mut trace = Trace::new("Canonical policy", "fact", "", vec![], "canonical body");
    cx.add(&mut trace).unwrap();
    let id = trace.frontmatter.id.clone();
    cx.promote(&id, "mid").unwrap();
    cx.promote(&id, "long").unwrap();
    let path = cx.trace_file(&id, false);
    let mut drifted = Trace::parse_file(&path).unwrap();
    drifted.body = "external edit".into();
    drifted.write_preserving_updated(&path).unwrap();
    let original = fs::read(&path).unwrap();
    let result = cx.reconcile_long_term(&id).unwrap();
    assert!(
        result
            .recovery_artifact
            .starts_with("db.nosync/reconciliations/")
    );
    assert_eq!(
        fs::read(root.join(result.recovery_artifact)).unwrap(),
        original
    );
    assert!(!root.join("db").exists());
}

#[test]
#[cfg(debug_assertions)]
fn interrupted_mutation_recovers_from_nosync_database() {
    let temp = tempfile::tempdir().unwrap();
    let config = temp.path().join("config");
    cli(
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
    fs::rename(root.join("db"), root.join("db.nosync")).unwrap();
    let cx = Cortex::open("sample", &root).unwrap();
    let mut trace = Trace::new("Recovery trial", "fact", "", vec![], "original body");
    cx.add(&mut trace).unwrap();
    let id = trace.frontmatter.id.clone();
    let path = cx.trace_file(&id, false);
    let original = fs::read(&path).unwrap();
    drop(cx);
    let marker = temp.path().join("mutation-complete");
    let mut child = Command::new(env!("CARGO_BIN_EXE_noema"))
        .env("XDG_CONFIG_HOME", &config)
        .env("NOEMA_DURABILITY", "strong")
        .env("NOEMA_RUST_TEST_PAUSE_AFTER_FILESYSTEM_MUTATION", &marker)
        .args(["append", &id, "--content", "interrupted change"])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    while !marker.exists() && Instant::now() < deadline {
        assert!(
            child.try_wait().unwrap().is_none(),
            "child exited before mutation"
        );
        thread::sleep(Duration::from_millis(10));
    }
    if !marker.exists() {
        let _ = child.kill();
        let _ = child.wait();
        panic!("mutation marker timed out");
    }
    assert_ne!(fs::read(&path).unwrap(), original);
    child.kill().unwrap();
    child.wait().unwrap();
    let recovered = Cortex::open("sample", &root).unwrap();
    assert_eq!(recovered.get_trace(&id).unwrap().1.body, "original body");
    assert_eq!(fs::read(path).unwrap(), original);
    assert!(!root.join("db").exists());
}

#[test]
#[cfg(unix)]
fn migration_preserves_private_configuration_permissions_in_both_directions() {
    use std::os::unix::fs::PermissionsExt;
    for mode in [0o600, 0o640] {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("sample", temp.path()).unwrap();
        let root = temp.path().join("sample");
        let path = root.join("storage.yaml");
        fs::write(&path, "database: default\n").unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(mode)).unwrap();
        for target in [DatabaseStorage::Nosync, DatabaseStorage::Default] {
            storage::migrate(
                &root,
                Some(target),
                Some(&temp.path().join(format!("{}.tar.gz", target.as_str()))),
                false,
            )
            .unwrap();
            assert_eq!(
                fs::metadata(&path).unwrap().permissions().mode() & 0o777,
                mode
            );
        }
    }
}

#[test]
#[cfg(unix)]
fn replaced_legacy_lock_cannot_bypass_live_clients_even_through_path_alias() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("sample", temp.path()).unwrap();
    let root = temp.path().join("sample");
    let alias = temp.path().join("alias");
    std::os::unix::fs::symlink(&root, &alias).unwrap();
    let cx = Cortex::open("sample", &root).unwrap();
    fs::rename(
        root.join(".noema-storage.lock"),
        temp.path().join("old-lock"),
    )
    .unwrap();
    fs::write(root.join(".noema-storage.lock"), "").unwrap();
    for path in [&root, &alias] {
        let backup = temp.path().join("blocked.tar.gz");
        let error = storage::migrate(path, Some(DatabaseStorage::Nosync), Some(&backup), false)
            .unwrap_err();
        assert!(format!("{error:#}").contains("storage is in use"));
        let error = noema::maintenance::compact(path, &backup).unwrap_err();
        assert!(format!("{error:#}").contains("storage is in use"));
        assert!(!backup.exists());
        assert!(!root.join(".noema-storage-migration.json").exists());
        assert!(root.join("db/noema.db").is_file());
    }
    drop(cx);
    storage::migrate(
        &root,
        Some(DatabaseStorage::Nosync),
        Some(&temp.path().join("allowed.tar.gz")),
        false,
    )
    .unwrap();
    noema::maintenance::compact(&root, &temp.path().join("compact.tar.gz")).unwrap();
}

#[test]
fn legacy_clients_still_block_maintenance() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("sample", temp.path()).unwrap();
    let root = temp.path().join("sample");
    let file = fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open(root.join(".noema-storage.lock"))
        .unwrap();
    fs2::FileExt::try_lock_shared(&file).unwrap();
    let backup = temp.path().join("allowed.tar.gz");
    let error =
        storage::migrate(&root, Some(DatabaseStorage::Nosync), Some(&backup), false).unwrap_err();
    assert!(format!("{error:#}").contains("storage is in use"));
    assert!(!backup.exists());
    drop(file);
    storage::migrate(&root, Some(DatabaseStorage::Nosync), Some(&backup), false).unwrap();
}
