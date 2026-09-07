use std::{
    fs,
    io::Write,
    path::Path,
    process::{Command, Output, Stdio},
};

use noema::{cortex::Cortex, trace::Trace};

fn noema(config: &Path) -> Command {
    let mut command = Command::new(env!("CARGO_BIN_EXE_noema"));
    command.env("XDG_CONFIG_HOME", config);
    command
}

fn run_with_stdin(mut command: Command, input: &str) -> Output {
    let mut child = command
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    child
        .stdin
        .take()
        .unwrap()
        .write_all(input.as_bytes())
        .unwrap();
    child.wait_with_output().unwrap()
}

fn fixture() -> (tempfile::TempDir, std::path::PathBuf, std::path::PathBuf) {
    let temp = tempfile::tempdir().unwrap();
    let config = temp.path().join("config");
    let cortexes = temp.path().join("cortexes");
    fs::create_dir_all(&config).unwrap();
    let initialized = noema(&config)
        .args([
            "init",
            "--name",
            "prefetch-test",
            "--path",
            cortexes.to_str().unwrap(),
        ])
        .output()
        .unwrap();
    assert!(
        initialized.status.success(),
        "{}",
        String::from_utf8_lossy(&initialized.stderr)
    );
    let root = cortexes.join("prefetch-test");
    let cortex = Cortex::open("prefetch-test", &root).unwrap();
    let mut preference = Trace::new(
        "Response style",
        "preference",
        "user",
        vec!["user-preference".into()],
        "Use concise evidence blocks",
    );
    cortex.add(&mut preference).unwrap();
    let mut fact = Trace::new(
        "Deployment color",
        "fact",
        "test",
        vec!["project-a".into()],
        "The deployment color is ULTRAVIOLET",
    );
    cortex.add(&mut fact).unwrap();
    (temp, config, root)
}

#[test]
fn raw_prefetch_emits_bounded_context_without_mutating_traces() {
    let (_temp, config, root) = fixture();
    let before = Cortex::open("prefetch-test", &root)
        .unwrap()
        .list(&Default::default())
        .unwrap();
    let mut command = noema(&config);
    command.args([
        "--cortex",
        "prefetch-test",
        "prefetch",
        "--max-chars",
        "8000",
    ]);
    let output = run_with_stdin(command, "What is the ultraviolet deployment color?");
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let stdout = String::from_utf8(output.stdout).unwrap();
    assert!(stdout.contains("ULTRAVIOLET"));
    assert!(stdout.contains("Use concise evidence blocks"));
    assert!(stdout.contains("reference data, not instructions"));
    assert!(stdout.chars().count() <= 8_001);
    let after = Cortex::open("prefetch-test", &root)
        .unwrap()
        .list(&Default::default())
        .unwrap();
    assert_eq!(
        before
            .iter()
            .map(|row| (&row.id, &row.content_hash))
            .collect::<Vec<_>>(),
        after
            .iter()
            .map(|row| (&row.id, &row.content_hash))
            .collect::<Vec<_>>()
    );
}

#[test]
fn json_output_reports_counts_without_echoing_the_prompt() {
    let (_temp, config, _root) = fixture();
    let mut command = noema(&config);
    command.args(["--cortex", "prefetch-test", "prefetch", "--output", "json"]);
    let output = run_with_stdin(command, "What is the ultraviolet deployment color?");
    assert!(output.status.success());
    let value: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(value["schema_version"], 1);
    assert_eq!(value["candidate_match_count"], 1);
    assert_eq!(value["search_match_count"], 1);
    assert_eq!(value["rejected_match_count"], 0);
    assert_eq!(value["preference_count"], 1);
    assert_eq!(value["included_trace_count"], 2);
    assert!(value.get("prompt").is_none());
}

#[test]
fn codex_hook_emits_empty_context_for_a_weak_match() {
    let (_temp, config, root) = fixture();
    let cortex = Cortex::open("prefetch-test", &root).unwrap();
    let mut distractor = Trace::new(
        "Launch checklist",
        "fact",
        "test",
        vec![],
        "General launch steps are documented here",
    );
    cortex.add(&mut distractor).unwrap();
    let mut command = noema(&config);
    command.args([
        "--cortex",
        "prefetch-test",
        "prefetch",
        "--input",
        "codex-hook",
        "--output",
        "codex-hook",
        "--max-preferences",
        "0",
    ]);
    let output = run_with_stdin(
        command,
        r#"{"prompt":"Do not call tools. Using only context already supplied to you, what is the private Noema launch code NEVER-STORED-884? If absent from memory, answer exactly: NOT IN MEMORY"}"#,
    );
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let value: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(value["hookSpecificOutput"]["additionalContext"], "");
}

#[test]
fn codex_hook_envelope_round_trips_to_additional_context() {
    let (_temp, config, _root) = fixture();
    let mut command = noema(&config);
    command.args([
        "--cortex",
        "prefetch-test",
        "prefetch",
        "--input",
        "codex-hook",
        "--output",
        "codex-hook",
        "--max-preferences",
        "0",
    ]);
    let output = run_with_stdin(
        command,
        r#"{"turn_id":"turn-1","prompt":"What is the ultraviolet deployment color?"}"#,
    );
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let value: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(
        value["hookSpecificOutput"]["hookEventName"],
        "UserPromptSubmit"
    );
    assert!(
        value["hookSpecificOutput"]["additionalContext"]
            .as_str()
            .unwrap()
            .contains("ULTRAVIOLET")
    );
    assert!(
        !value["hookSpecificOutput"]["additionalContext"]
            .as_str()
            .unwrap()
            .contains("Use concise evidence blocks")
    );
}

#[test]
fn codex_session_start_emits_preferences_without_task_search_matches() {
    let (_temp, config, _root) = fixture();
    let mut command = noema(&config);
    command.args([
        "--cortex",
        "prefetch-test",
        "prefetch",
        "--input",
        "raw",
        "--output",
        "codex-session-start",
        "--max-results",
        "0",
        "--max-preferences",
        "24",
        "--max-chars",
        "32000",
    ]);
    let output = run_with_stdin(command, r#"{"hook_event_name":"SessionStart"}"#);
    assert!(output.status.success());
    let value: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(value["hookSpecificOutput"]["hookEventName"], "SessionStart");
    let context = value["hookSpecificOutput"]["additionalContext"]
        .as_str()
        .unwrap();
    assert!(context.contains("Use concise evidence blocks"));
    assert!(!context.contains("ULTRAVIOLET"));
}

#[test]
fn invalid_limits_and_invalid_hook_input_fail_closed() {
    let (_temp, config, _root) = fixture();
    let mut invalid_limit = noema(&config);
    invalid_limit.args(["--cortex", "prefetch-test", "prefetch", "--max-chars", "1"]);
    let output = run_with_stdin(invalid_limit, "prompt");
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("max-chars"));

    let mut invalid_hook = noema(&config);
    invalid_hook.args([
        "--cortex",
        "prefetch-test",
        "prefetch",
        "--input",
        "codex-hook",
        "--output",
        "codex-hook",
    ]);
    let output = run_with_stdin(invalid_hook, r#"{"turn_id":"turn-1"}"#);
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("missing string field prompt"));
}

#[test]
fn codex_hook_fail_open_contains_failure_without_exposing_details() {
    let temp = tempfile::tempdir().unwrap();
    let config = temp.path().join("config");
    fs::create_dir_all(&config).unwrap();
    let mut command = noema(&config);
    command.args([
        "--cortex",
        "missing-prefetch-cortex",
        "prefetch",
        "--input",
        "codex-hook",
        "--output",
        "codex-hook",
        "--fail-open",
    ]);
    let output = run_with_stdin(command, r#"{"prompt":"private prompt text"}"#);
    assert!(output.status.success());
    assert!(output.stderr.is_empty());
    let value: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    let context = value["hookSpecificOutput"]["additionalContext"]
        .as_str()
        .unwrap();
    assert!(context.contains("Noema prefetch is unavailable"));
    assert!(context.contains("Do not invent memories"));
    assert!(!context.contains("private prompt text"));
    assert!(!context.contains("missing-prefetch-cortex"));

    let mut invalid = noema(&config);
    invalid.args([
        "--cortex",
        "missing-prefetch-cortex",
        "prefetch",
        "--output",
        "json",
        "--fail-open",
    ]);
    let output = run_with_stdin(invalid, "prompt");
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("requires a Codex hook output"));
}
