use std::process::Command;

use noema::{cortex::Cortex, trace::Trace};

#[test]
fn remove_force_overrides_source_lock_but_remains_recoverable() {
    let temp = tempfile::tempdir().unwrap();
    let config_home = temp.path().join("config");
    let cortex_parent = temp.path().join("cortexes");
    let binary = env!("CARGO_BIN_EXE_noema");

    let init = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .args([
            "init",
            "--name",
            "remove-test",
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

    let root = cortex_parent.join("remove-test");
    let id = {
        let cortex = Cortex::open("remove-test", &root).unwrap();
        let mut trace = Trace::new("Forced trash", "fact", "", vec![], "recoverable");
        trace.frontmatter.origin = "upstream".into();
        trace.frontmatter.source_locked = true;
        cortex.add(&mut trace).unwrap();
        trace.frontmatter.id
    };

    let refused = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .args(["--cortex", "remove-test", "remove", &id])
        .output()
        .unwrap();
    assert!(!refused.status.success());

    let removed = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .args(["--cortex", "remove-test", "remove", &id, "--force"])
        .output()
        .unwrap();
    assert!(
        removed.status.success(),
        "{}",
        String::from_utf8_lossy(&removed.stderr)
    );
    let stdout = String::from_utf8(removed.stdout).unwrap();
    assert!(stdout.contains("moved to trash"));
    assert!(!stdout.contains("permanently"));

    {
        let cortex = Cortex::open("remove-test", &root).unwrap();
        let row = cortex.get(&id).unwrap();
        assert!(!row.trashed_at.is_empty());
        assert!(cortex.trash_dir().join(format!("{id}.md")).is_file());
        assert!(
            cortex
                .history(&id)
                .unwrap()
                .iter()
                .any(|event| event.action == "trash")
        );
    }

    let refused_recovery = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .args(["--cortex", "remove-test", "recover", &id])
        .output()
        .unwrap();
    assert!(!refused_recovery.status.success());

    let recovered = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .args(["--cortex", "remove-test", "recover", &id, "--force"])
        .output()
        .unwrap();
    assert!(
        recovered.status.success(),
        "{}",
        String::from_utf8_lossy(&recovered.stderr)
    );

    let cortex = Cortex::open("remove-test", &root).unwrap();
    assert!(cortex.get(&id).unwrap().trashed_at.is_empty());
    assert!(cortex.trace_file(&id, false).is_file());
}
