use std::{fs, process::Command};

use noema::{cortex::Cortex, trace::Trace};

#[cfg(unix)]
use std::os::unix::fs::PermissionsExt;

#[test]
fn reconcile_cli_previews_then_restores_long_term_file() {
    let temp = tempfile::tempdir().unwrap();
    let config_home = temp.path().join("config");
    let cortex_parent = temp.path().join("cortexes");
    let binary = env!("CARGO_BIN_EXE_noema");
    let init = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .args([
            "init",
            "--name",
            "reconcile-test",
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

    let root = cortex_parent.join("reconcile-test");
    let cortex = Cortex::open("reconcile-test", &root).unwrap();
    let mut original = Trace::new("Historical policy", "preference", "", vec![], "canonical");
    cortex.add(&mut original).unwrap();
    let id = original.frontmatter.id.clone();
    cortex.promote(&id, "mid").unwrap();
    cortex.promote(&id, "long").unwrap();
    cortex.archive(&id).unwrap();
    let path = cortex.trace_file(&id, true);

    let mut successor = Trace::new("Current policy", "preference", "", vec![], "current");
    successor.frontmatter.derived_from = vec![id.clone()];
    let successor_id = successor.frontmatter.id.clone();
    cortex.add(&mut successor).unwrap();

    let mut drifted = Trace::parse_file(&path).unwrap();
    drifted.body = "edited outside Noema".into();
    drifted.frontmatter.content_hash = noema::trace::content_hash(&drifted.body);
    drifted.write_preserving_updated(&path).unwrap();
    let drifted_bytes = fs::read(&path).unwrap();
    drop(cortex);

    let preview = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .args(["--cortex", "reconcile-test", "memory", "reconcile", &id])
        .output()
        .unwrap();
    assert!(
        preview.status.success(),
        "{}",
        String::from_utf8_lossy(&preview.stderr)
    );
    let preview_text = String::from_utf8(preview.stdout).unwrap();
    assert!(preview_text.contains("Classification: restore-canonical"));
    assert!(preview_text.contains(&format!("File: archive/traces/{id}.md")));
    assert!(preview_text.contains("Drifted fields: body"));
    assert!(preview_text.contains(&successor_id));
    assert!(preview_text.contains("Preview only"));
    assert_eq!(fs::read(&path).unwrap(), drifted_bytes);

    let apply = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .args([
            "--cortex",
            "reconcile-test",
            "memory",
            "reconcile",
            &id,
            "--apply",
            "--yes",
        ])
        .output()
        .unwrap();
    assert!(
        apply.status.success(),
        "{}",
        String::from_utf8_lossy(&apply.stderr)
    );
    let apply_text = String::from_utf8(apply.stdout).unwrap();
    assert!(apply_text.contains("Reconciled"));
    assert!(apply_text.contains("Recovery artifact: db/reconciliations/"));
    assert_eq!(Trace::parse_file(&path).unwrap().body, "canonical");

    let cortex = Cortex::open("reconcile-test", &root).unwrap();
    assert_eq!(cortex.sync().unwrap().drifted, 0);
    assert!(!cortex.get(&id).unwrap().archived_at.is_empty());
    assert_eq!(cortex.lineage(&id).unwrap().1, vec![successor_id]);

    let artifact_directory = root.join("db/reconciliations");
    let artifacts: Vec<_> = fs::read_dir(&artifact_directory)
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .collect();
    assert_eq!(artifacts.len(), 1);
    assert_eq!(fs::read(&artifacts[0]).unwrap(), drifted_bytes);
    #[cfg(unix)]
    {
        assert_eq!(
            fs::metadata(&artifact_directory)
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o700
        );
        assert_eq!(
            fs::metadata(&artifacts[0]).unwrap().permissions().mode() & 0o777,
            0o600
        );
    }

    let history = cortex.history(&id).unwrap();
    let audit = history.last().unwrap();
    assert_eq!(audit.action, "divergence_long_term");
    assert_eq!(
        audit.data["kind"].as_str(),
        Some("local_file_reconciliation")
    );
    assert_eq!(audit.data["resolution"].as_str(), Some("restore_canonical"));
    assert_eq!(
        audit.data["recovery_artifact"].as_str(),
        Some(artifacts[0].strip_prefix(&root).unwrap().to_str().unwrap())
    );
    let audit_count = history
        .iter()
        .filter(|event| event.action == "divergence_long_term")
        .count();
    drop(cortex);

    let clean_preview = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .args(["--cortex", "reconcile-test", "memory", "reconcile", &id])
        .output()
        .unwrap();
    assert!(clean_preview.status.success());
    let clean_preview_text = String::from_utf8(clean_preview.stdout).unwrap();
    assert!(clean_preview_text.contains("Classification: clean"));
    assert!(clean_preview_text.contains("No repair is needed"));

    let second_apply = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .args([
            "--cortex",
            "reconcile-test",
            "memory",
            "reconcile",
            &id,
            "--apply",
            "--yes",
        ])
        .output()
        .unwrap();
    assert!(second_apply.status.success());
    let second_apply_text = String::from_utf8(second_apply.stdout).unwrap();
    assert!(second_apply_text.contains("Classification: clean"));
    assert!(second_apply_text.contains("No repair is needed"));
    assert_eq!(fs::read_dir(&artifact_directory).unwrap().count(), 1);

    let cortex = Cortex::open("reconcile-test", &root).unwrap();
    assert_eq!(
        cortex
            .history(&id)
            .unwrap()
            .iter()
            .filter(|event| event.action == "divergence_long_term")
            .count(),
        audit_count
    );
}
