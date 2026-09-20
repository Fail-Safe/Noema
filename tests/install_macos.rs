#![cfg(target_os = "macos")]

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::process::Command;

fn xml_escape(value: &str) -> String {
    value
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&apos;")
}

fn plist(destination: &Path, keepalive: &str) -> String {
    format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.example.noema.sample</string>
  <key>ProgramArguments</key>
  <array>
    <string>{}</string>
    <string>serve</string>
    <string>--cortex</string>
    <string>sample</string>
  </array>
  <key>KeepAlive</key>{keepalive}
  <key>StandardOutPath</key><string>/tmp/preserved.log</string>
  <key>UnrelatedSetting</key><string>preserved</string>
</dict>
</plist>
"#,
        xml_escape(&destination.display().to_string())
    )
}

fn plist_with_localhost(destination: &Path) -> String {
    plist(destination, "<true/>").replace(
        "    <string>sample</string>\n",
        "    <string>sample</string>\n    <string>--host</string>\n    <string>localhost</string>\n    <string>--host-dynamic</string>\n    <string>192.0.2.10</string>\n",
    )
}

fn program_arguments(path: &Path) -> Vec<String> {
    let output = Command::new("plutil")
        .args(["-extract", "ProgramArguments", "json", "-o", "-"])
        .arg(path)
        .output()
        .unwrap();
    assert!(output.status.success());
    serde_json::from_slice(&output.stdout).unwrap()
}

fn test_environment() -> (tempfile::TempDir, std::path::PathBuf, std::path::PathBuf) {
    let temp = tempfile::tempdir().unwrap();
    let agents = temp.path().join("Library/LaunchAgents");
    let fake_bin = temp.path().join("bin");
    fs::create_dir_all(&agents).unwrap();
    fs::create_dir_all(&fake_bin).unwrap();
    let launchctl = fake_bin.join("launchctl");
    fs::write(
        &launchctl,
        "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$NOEMA_LAUNCHCTL_LOG\"\nif [ \"$1\" = bootstrap ] && [ -n \"${NOEMA_LAUNCHCTL_FAIL_ONCE:-}\" ] && [ ! -e \"$NOEMA_LAUNCHCTL_FAIL_ONCE\" ]; then\n  touch \"$NOEMA_LAUNCHCTL_FAIL_ONCE\"\n  exit 5\nfi\nexit 0\n",
    )
    .unwrap();
    let mut permissions = fs::metadata(&launchctl).unwrap().permissions();
    permissions.set_mode(0o755);
    fs::set_permissions(&launchctl, permissions).unwrap();
    (temp, agents, fake_bin)
}

fn run_reconcile(
    destination: &Path,
    agents: &Path,
    fake_bin: &Path,
    log: &Path,
    fail_bootstrap_once: bool,
) -> std::process::Output {
    let path = format!(
        "{}:{}",
        fake_bin.display(),
        std::env::var("PATH").unwrap_or_default()
    );
    let mut command = Command::new(format!(
        "{}/scripts/reconcile-launchd-agents.zsh",
        env!("CARGO_MANIFEST_DIR")
    ));
    command
        .arg(destination)
        .env("NOEMA_LAUNCH_AGENTS_DIR", agents)
        .env("NOEMA_LAUNCHD_DOMAIN", "gui/999")
        .env("NOEMA_LAUNCHCTL_LOG", log)
        .env("PATH", path);
    if fail_bootstrap_once {
        command.env(
            "NOEMA_LAUNCHCTL_FAIL_ONCE",
            log.with_extension("bootstrap-failed-once"),
        );
    }
    command.output().unwrap()
}

#[test]
fn migrates_exact_legacy_policy_and_reloads_loaded_agent() {
    let (_temp, agents, fake_bin) = test_environment();
    let destination = agents.parent().unwrap().join("bin/noema");
    let path = agents.join("com.example.noema.sample.plist");
    let log = agents.join("launchctl.log");
    let original = plist(
        &destination,
        "<dict><key>SuccessfulExit</key><false/></dict>",
    );
    fs::write(&path, &original).unwrap();

    let first = run_reconcile(&destination, &agents, &fake_bin, &log, false);
    assert!(
        first.status.success(),
        "{}",
        String::from_utf8_lossy(&first.stderr)
    );
    let migrated = fs::read_to_string(&path).unwrap();
    assert!(migrated.contains("<key>KeepAlive</key>\n\t<true/>"));
    assert!(migrated.contains("<string>/tmp/preserved.log</string>"));
    assert!(migrated.contains("<key>UnrelatedSetting</key>"));
    assert_eq!(
        fs::read_to_string(path.with_extension("plist.pre-noema-keepalive")).unwrap(),
        original
    );
    assert_eq!(
        fs::read_to_string(&log).unwrap(),
        format!(
            "print gui/999/com.example.noema.sample\nbootout gui/999/com.example.noema.sample\nbootstrap gui/999 {}\n",
            path.display()
        )
    );

    let backup_before =
        fs::read_to_string(path.with_extension("plist.pre-noema-keepalive")).unwrap();
    let second = run_reconcile(&destination, &agents, &fake_bin, &log, false);
    assert!(second.status.success());
    assert_eq!(
        fs::read_to_string(path.with_extension("plist.pre-noema-keepalive")).unwrap(),
        backup_before
    );
}

#[test]
fn preserves_custom_keepalive_policy() {
    let (_temp, agents, fake_bin) = test_environment();
    let destination = agents.parent().unwrap().join("bin/noema");
    let path = agents.join("com.example.noema.sample.plist");
    let log = agents.join("launchctl.log");
    let original = plist(
        &destination,
        "<dict><key>SuccessfulExit</key><false/><key>NetworkState</key><true/></dict>",
    );
    fs::write(&path, &original).unwrap();

    let output = run_reconcile(&destination, &agents, &fake_bin, &log, false);
    assert!(output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("preserving custom KeepAlive policy"));
    assert_eq!(fs::read_to_string(&path).unwrap(), original);
    assert!(!path.with_extension("plist.pre-noema-keepalive").exists());
}

#[test]
fn replaces_localhost_with_both_explicit_loopbacks() {
    let (_temp, agents, fake_bin) = test_environment();
    let destination = agents.parent().unwrap().join("bin/noema");
    let path = agents.join("com.example.noema.sample.plist");
    let log = agents.join("launchctl.log");
    let original = plist_with_localhost(&destination);
    fs::write(&path, &original).unwrap();

    let output = run_reconcile(&destination, &agents, &fake_bin, &log, true);
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(String::from_utf8_lossy(&output.stdout).contains("Normalized launchd loopback"));
    assert!(String::from_utf8_lossy(&output.stderr).contains("bootstrap attempt 1 failed"));
    assert_eq!(
        program_arguments(&path),
        [
            destination.display().to_string(),
            "serve".into(),
            "--cortex".into(),
            "sample".into(),
            "--host".into(),
            "127.0.0.1".into(),
            "--host".into(),
            "::1".into(),
            "--host-dynamic".into(),
            "192.0.2.10".into(),
        ]
    );
    assert_eq!(
        fs::read_to_string(format!("{}.pre-noema-loopback", path.display())).unwrap(),
        original
    );
    assert!(
        fs::read_to_string(&path)
            .unwrap()
            .contains("<key>UnrelatedSetting</key>")
    );
    assert_eq!(
        fs::read_to_string(&log)
            .unwrap()
            .lines()
            .filter(|line| line.starts_with("bootstrap "))
            .count(),
        2
    );
}

#[test]
fn ignores_launch_agents_for_other_binaries() {
    let (_temp, agents, fake_bin) = test_environment();
    let destination = agents.parent().unwrap().join("bin/noema");
    let other_destination = agents.parent().unwrap().join("bin/other-noema");
    let path = agents.join("com.example.noema.sample.plist");
    let log = agents.join("launchctl.log");
    let original = plist(
        &other_destination,
        "<dict><key>SuccessfulExit</key><false/></dict>",
    );
    fs::write(&path, &original).unwrap();

    let output = run_reconcile(&destination, &agents, &fake_bin, &log, false);
    assert!(output.status.success());
    assert_eq!(fs::read_to_string(&path).unwrap(), original);
    assert!(!log.exists());
}
