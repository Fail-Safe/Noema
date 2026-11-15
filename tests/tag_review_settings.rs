use std::{
    fs,
    path::Path,
    process::{Command, Output},
};

use noema::cortex::{Cortex, read_manifest, write_manifest};

fn cli(config: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_noema"))
        .env("XDG_CONFIG_HOME", config)
        .env_remove("NOEMA_CORTEX")
        .args(args)
        .output()
        .unwrap()
}

#[test]
fn cli_switches_review_models_without_touching_consolidation() {
    let temp = tempfile::tempdir().unwrap();
    let config = temp.path().join("config");
    let parent = temp.path().join("cortexes");
    assert!(
        cli(
            &config,
            &["init", "--name", "test", "--path", parent.to_str().unwrap()]
        )
        .status
        .success()
    );
    let root = parent.join("test");
    let mut manifest = read_manifest(&root).unwrap();
    manifest.consolidation = Some(serde_yaml::from_str("llm_enabled: true\nlocal_llm_endpoint: http://127.0.0.1:8000/v1\nmodel_name: fixture-9b\n").unwrap());
    write_manifest(&root, &manifest).unwrap();
    let original = fs::read(root.join("cortex.md")).unwrap();
    let output = cli(&config, &["tags", "review", "settings"]);
    assert!(output.status.success());
    let status: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(status["mode"], "shared");
    assert!(
        status["semantic_model"]
            .as_str()
            .unwrap()
            .contains("fixture-9b")
    );
    assert_eq!(fs::read(root.join("cortex.md")).unwrap(), original);

    for mode in ["separate", "shared", "separate"] {
        let mut args = vec!["tags", "review", "settings", "--mode", mode];
        if mode == "separate" {
            args.extend([
                "--model",
                "fixture-35b",
                "--endpoint",
                "http://127.0.0.1:8001/v1",
            ]);
        }
        let output = cli(&config, &args);
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        let status: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
        assert_eq!(status["mode"], mode);
        assert!(
            status["semantic_model"]
                .as_str()
                .unwrap()
                .contains(if mode == "shared" {
                    "fixture-9b"
                } else {
                    "fixture-35b"
                })
        );
        assert_eq!(status["saved_separate_model"], "fixture-35b");
        assert_eq!(
            read_manifest(&root).unwrap().consolidation,
            manifest.consolidation
        );
    }
    assert!(
        Cortex::open("test", root)
            .unwrap()
            .tag_reviews()
            .unwrap()
            .is_empty(),
        "settings changes must not run analysis"
    );
}

#[test]
fn incomplete_or_unsafe_cli_settings_are_nonmutating_and_redacted() {
    let temp = tempfile::tempdir().unwrap();
    let config = temp.path().join("config");
    let parent = temp.path().join("cortexes");
    assert!(
        cli(
            &config,
            &["init", "--name", "test", "--path", parent.to_str().unwrap()]
        )
        .status
        .success()
    );
    let path = parent.join("test/cortex.md");
    let before = fs::read(&path).unwrap();
    for args in [
        vec!["tags", "review", "settings", "--mode", "separate"],
        vec![
            "tags",
            "review",
            "settings",
            "--mode",
            "separate",
            "--model",
            "fixture-35b",
            "--endpoint",
            "https://user:synthetic-secret@example.com/v1",
        ],
        vec![
            "tags",
            "review",
            "settings",
            "--api-key-env",
            "synthetic-secret-not-a-variable",
        ],
    ] {
        let output = cli(&config, &args);
        assert!(!output.status.success());
        assert_eq!(fs::read(&path).unwrap(), before);
        assert!(!String::from_utf8_lossy(&output.stderr).contains("synthetic-secret"));
        assert!(!String::from_utf8_lossy(&output.stdout).contains("synthetic-secret"));
    }
}

#[tokio::test]
async fn incomplete_semantic_cli_run_reports_progress_and_exits_unsuccessfully() {
    let temp = tempfile::tempdir().unwrap();
    let config = temp.path().join("config");
    let parent = temp.path().join("cortexes");
    assert!(
        cli(
            &config,
            &["init", "--name", "test", "--path", parent.to_str().unwrap()]
        )
        .status
        .success()
    );
    let root = parent.join("test");
    let app = axum::Router::new().route("/v1/chat/completions", axum::routing::post(|| async {
        axum::Json(serde_json::json!({"choices":[{"finish_reason":"length","message":{"content":"synthetic-private-truncated-output"}}]}))
    }));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let endpoint = format!("http://{}/v1", listener.local_addr().unwrap());
    let worker = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    let mut cx = Cortex::open("test", &root).unwrap();
    let mut trace = noema::trace::Trace::new(
        "CLI partial review",
        "note",
        "",
        vec!["Rust Notes".into()],
        "Concurrency notes",
    );
    cx.add(&mut trace).unwrap();
    cx.manifest.tag_review = Some(noema::tag_review::TagReviewConfig {
        mode: noema::tag_review::ModelMode::Separate,
        model_name: "fixture-review".into(),
        endpoint,
        api_key_env: String::new(),
    });
    write_manifest(&root, &cx.manifest).unwrap();
    drop(cx);
    let output = tokio::task::spawn_blocking(move || {
        cli(&config, &["tags", "review", "analyze", "--model"])
    })
    .await
    .unwrap();
    worker.abort();
    assert!(!output.status.success());
    let stdout = String::from_utf8_lossy(&output.stdout);
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(stdout.contains("\"status\": \"partial\""));
    assert!(stdout.contains("\"unfinished\": 1"));
    assert!(stderr.contains("proposals retained"));
    assert!(!stdout.contains("synthetic-private-truncated-output"));
    assert!(!stderr.contains("synthetic-private-truncated-output"));
    let cx = Cortex::open("test", root).unwrap();
    assert_eq!(cx.tag_reviews().unwrap().len(), 1);
    assert_eq!(
        cx.get_trace(&trace.frontmatter.id)
            .unwrap()
            .1
            .frontmatter
            .tags,
        ["Rust Notes"]
    );
}
