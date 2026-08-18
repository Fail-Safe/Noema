#![cfg(debug_assertions)]

use std::{
    collections::BTreeMap,
    fs,
    path::Path,
    process::{Command, Stdio},
    thread,
    time::{Duration, Instant},
};

use noema::config::{Config, CortexEntry};

#[test]
fn killed_config_writer_preserves_old_yaml_and_retry_commits_cleanly() {
    let temp = tempfile::tempdir().unwrap();
    let config_home = temp.path().join("config");
    let config_directory = config_home.join("noema");
    let config_path = config_directory.join("config.yaml");
    let alpha = temp.path().join("alpha");
    let beta = temp.path().join("beta");
    fs::create_dir_all(&config_directory).unwrap();
    fs::create_dir_all(&alpha).unwrap();
    fs::create_dir_all(&beta).unwrap();
    let original = Config {
        default: "alpha".into(),
        cortexes: BTreeMap::from([
            (
                "alpha".into(),
                CortexEntry {
                    path: alpha,
                    id: String::new(),
                },
            ),
            (
                "beta".into(),
                CortexEntry {
                    path: beta,
                    id: String::new(),
                },
            ),
        ]),
        trash_days: 0,
        ui: None,
    };
    let original_bytes = serde_yaml::to_string(&original).unwrap().into_bytes();
    fs::write(&config_path, &original_bytes).unwrap();

    let marker = temp.path().join("config-before-rename.marker");
    let mut child = Command::new(env!("CARGO_BIN_EXE_noema"))
        .env("XDG_CONFIG_HOME", &config_home)
        .env("NOEMA_RUST_TEST_CONFIG_PAUSE_POINT", "before-rename")
        .env("NOEMA_RUST_TEST_CONFIG_PAUSE_MARKER", &marker)
        .args(["use", "beta"])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();
    wait_for_marker(&mut child, &marker);
    child.kill().unwrap();
    assert!(!child.wait().unwrap().success());

    assert_eq!(fs::read(&config_path).unwrap(), original_bytes);
    let parsed: Config = serde_yaml::from_slice(&original_bytes).unwrap();
    assert_eq!(parsed.default, "alpha");
    assert!(config_directory.join(".config.yaml.tmp").is_file());

    let retry = Command::new(env!("CARGO_BIN_EXE_noema"))
        .env("XDG_CONFIG_HOME", &config_home)
        .args(["use", "beta"])
        .output()
        .unwrap();
    assert!(
        retry.status.success(),
        "{}",
        String::from_utf8_lossy(&retry.stderr)
    );
    let committed: Config = serde_yaml::from_slice(&fs::read(&config_path).unwrap()).unwrap();
    assert_eq!(committed.default, "beta");
    assert!(!config_directory.join(".config.yaml.tmp").exists());

    #[cfg(unix)]
    assert_private_modes(&config_directory, &config_path);
}

fn wait_for_marker(child: &mut std::process::Child, marker: &Path) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !marker.exists() && Instant::now() < deadline {
        if let Some(status) = child.try_wait().unwrap() {
            panic!("fault-injected config writer exited before pause: {status}");
        }
        thread::sleep(Duration::from_millis(10));
    }
    if !marker.exists() {
        let _ = child.kill();
        let _ = child.wait();
        panic!("timed out waiting for config fault marker");
    }
}

#[cfg(unix)]
fn assert_private_modes(directory: &Path, config: &Path) {
    use std::os::unix::fs::PermissionsExt;

    assert_eq!(
        fs::metadata(directory.join(".config.lock"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o600
    );
    assert_eq!(
        fs::metadata(config).unwrap().permissions().mode() & 0o777,
        0o640
    );
}
