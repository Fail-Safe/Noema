use super::*;

#[tokio::test]
async fn semantic_partial_progress_is_explicit_and_keeps_hygiene_proposals() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("test", temp.path()).unwrap();
    let root = temp.path().join("test");
    let app = axum::Router::new().route(
        "/v1/chat/completions",
        axum::routing::post(|| async {
            axum::Json(json!({"choices":[{"finish_reason":"length","message":{"content":null}}]}))
        }),
    );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let endpoint = format!("http://{}/v1", listener.local_addr().unwrap());
    let worker = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    let mut cx = Cortex::open("test", &root).unwrap();
    let mut trace = Trace::new(
        "Review partial MCP",
        "note",
        "",
        vec!["Rust Notes".into()],
        "Rust concurrency",
    );
    cx.add(&mut trace).unwrap();
    cx.manifest.tag_review = Some(crate::tag_review::TagReviewConfig {
        mode: crate::tag_review::ModelMode::Separate,
        model_name: "fixture".into(),
        endpoint,
        api_key_env: String::new(),
    });
    crate::cortex::write_manifest(&root, &cx.manifest).unwrap();
    drop(cx);
    let server = NoemaServer::new("test", &root, false).unwrap();
    let output = server
        .tag_review_analyze(Parameters(TagReviewAnalyzeParams {
            model: true,
            offset: 0,
        }))
        .await
        .unwrap();
    worker.abort();
    let output: serde_json::Value = serde_json::from_str(&output).unwrap();
    assert_eq!(output["hygiene_queued"], 1);
    assert_eq!(output["model"]["status"], "partial");
    assert_eq!(output["model"]["unfinished"], 1);
    assert_eq!(output["model"]["analyzed"], 0);
    assert_eq!(output["model"]["queued"], 0);
    assert!(
        output["model"]["warning"]
            .as_str()
            .unwrap()
            .contains("truncated")
    );
    let cx = server.open().await.unwrap();
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

#[tokio::test]
async fn tag_review_tools_require_writable_server_and_explicit_confirmation() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("test", temp.path()).unwrap();
    let root = temp.path().join("test");
    let cx = Cortex::open("test", &root).unwrap();
    let mut trace = Trace::new(
        "Review MCP",
        "note",
        "",
        vec!["Rust Notes".into()],
        "Rust notes",
    );
    cx.add(&mut trace).unwrap();
    let id = trace.frontmatter.id;
    drop(cx);
    let mut read_only = NoemaServer::new("test", &root, true).unwrap();
    read_only.federation_mode = "publish".into();
    assert!(read_only.tag_review_queue().await.is_ok());
    assert!(
        read_only
            .tag_review_analyze(Parameters(TagReviewAnalyzeParams {
                model: false,
                offset: 0
            }))
            .await
            .is_err()
    );
    assert!(
        read_only
            .tag_review_decide(Parameters(TagReviewDecideParams {
                id: "missing".into(),
                action: "apply".into(),
                confirmed: true,
                tags: None,
            }))
            .await
            .is_err()
    );
    let server = NoemaServer::new("test", &root, false).unwrap();
    let output = server
        .tag_review_analyze(Parameters(TagReviewAnalyzeParams {
            model: false,
            offset: 0,
        }))
        .await
        .unwrap();
    let output: serde_json::Value = serde_json::from_str(&output).unwrap();
    assert_eq!(output["hygiene_queued"], 1);
    let cx = server.open().await.unwrap();
    let proposal = cx.tag_reviews().unwrap().remove(0);
    assert_eq!(
        cx.get_trace(&id).unwrap().1.frontmatter.tags,
        ["Rust Notes"]
    );
    drop(cx);
    assert!(
        server
            .tag_review_decide(Parameters(TagReviewDecideParams {
                id: proposal.id.clone(),
                action: "apply".into(),
                confirmed: false,
                tags: None,
            }))
            .await
            .is_err()
    );
    server
        .tag_review_decide(Parameters(TagReviewDecideParams {
            id: proposal.id.clone(),
            action: "apply".into(),
            confirmed: true,
            tags: None,
        }))
        .await
        .unwrap();
    assert_eq!(
        server.open().await.unwrap().get(&id).unwrap().tags,
        ["rust-notes"]
    );
    server
        .tag_review_decide(Parameters(TagReviewDecideParams {
            id: proposal.id,
            action: "undo".into(),
            confirmed: true,
            tags: None,
        }))
        .await
        .unwrap();
    assert_eq!(
        server
            .open()
            .await
            .unwrap()
            .get_trace(&id)
            .unwrap()
            .1
            .frontmatter
            .tags,
        ["Rust Notes"]
    );
}
