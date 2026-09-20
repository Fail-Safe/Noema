use std::{fs, process::Command};

use noema::{cortex::Cortex, trace::Trace};

#[test]
fn sync_reports_all_invalid_files_without_overwriting_them() {
    let temp = tempfile::tempdir().unwrap();
    let config_home = temp.path().join("config");
    let cortex_parent = temp.path().join("cortexes");
    let binary = env!("CARGO_BIN_EXE_noema");

    let init = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .args([
            "init",
            "--name",
            "sync-test",
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

    let root = cortex_parent.join("sync-test");
    let cortex = Cortex::open("sync-test", &root).unwrap();

    let mut malformed = Trace::new("Malformed existing", "fact", "", vec![], "original");
    cortex.add(&mut malformed).unwrap();
    let malformed_id = malformed.frontmatter.id.clone();
    let malformed_path = cortex.trace_file(&malformed_id, false);
    let malformed_bytes = b"frontmatter is temporarily malformed\n";
    fs::write(&malformed_path, malformed_bytes).unwrap();

    let mut invalid = Trace::new("Legacy incident", "incident", "", vec![], "invalid");
    invalid.frontmatter.origin = "sync-test".into();
    let invalid_id = invalid.frontmatter.id.clone();
    let invalid_path = cortex.trace_file(&invalid_id, false);
    invalid.write_preserving_updated(&invalid_path).unwrap();
    let invalid_bytes = fs::read(&invalid_path).unwrap();

    let mut valid = Trace::new("Valid drop-in", "note", "", vec![], "valid");
    valid.frontmatter.origin = "sync-test".into();
    let valid_id = valid.frontmatter.id.clone();
    valid
        .write_preserving_updated(&cortex.trace_file(&valid_id, false))
        .unwrap();

    let mut orphan = Trace::new("Missing file", "note", "", vec![], "orphaned");
    cortex.add(&mut orphan).unwrap();
    let orphan_id = orphan.frontmatter.id.clone();
    fs::remove_file(cortex.trace_file(&orphan_id, false)).unwrap();
    drop(cortex);

    let output = Command::new(binary)
        .env("XDG_CONFIG_HOME", &config_home)
        .args(["--cortex", "sync-test", "sync"])
        .output()
        .unwrap();

    assert!(!output.status.success());
    let stdout = String::from_utf8(output.stdout).unwrap();
    let stderr = String::from_utf8(output.stderr).unwrap();
    assert!(stdout.contains(&format!("INVALID traces/{malformed_id}.md")));
    assert!(stdout.contains(&format!("INVALID traces/{invalid_id}.md")));
    assert!(stdout.contains("unrecognized type \"incident\""));
    assert!(stdout.contains("expected one of: fact, decision, preference"));
    assert!(stdout.contains("Scanned: 1  Added: 1  Changed: 0  Unchanged: 0"));
    assert!(stdout.contains("Drifted: 0  Invalid: 2  Recovered: 0  Orphaned: 1"));
    assert!(stdout.contains("Orphaned IDs:"));
    assert!(stdout.contains(&format!("        {orphan_id}")));
    assert!(stderr.contains(
        "Error: sync completed with 2 invalid trace file(s); invalid files were not changed"
    ));

    assert_eq!(fs::read(malformed_path).unwrap(), malformed_bytes);
    assert_eq!(fs::read(invalid_path).unwrap(), invalid_bytes);
    let cortex = Cortex::open("sync-test", &root).unwrap();
    assert!(cortex.get(&malformed_id).is_ok());
    assert!(cortex.get(&invalid_id).is_err());
    assert!(cortex.get(&valid_id).is_ok());
    assert!(cortex.get(&orphan_id).is_ok());
}
