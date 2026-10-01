use std::sync::{
    Arc, Mutex,
    atomic::{AtomicU16, AtomicUsize, Ordering},
};

use axum::{Json, Router, extract::State, http::StatusCode, routing::post};
use noema::{
    cortex::{Cortex, EmbedBackfillOptions, SearchConfig},
    db,
    embedding::{HttpEmbedder, InputSizeError},
    trace::Trace,
};
use serde_json::{Value, json};

#[derive(Clone, Copy)]
enum Limit {
    Context(usize),
    Physical(usize),
    Request(usize),
}

struct Mock {
    limit: Limit,
    tokenizer: bool,
    status: AtomicU16,
    requests: AtomicUsize,
    tokenizations: AtomicUsize,
    accepted: Mutex<Vec<String>>,
}

struct Server {
    endpoint: String,
    state: Arc<Mock>,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Server {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl Server {
    async fn start(limit: Limit, tokenizer: bool) -> Self {
        let state = Arc::new(Mock {
            limit,
            tokenizer,
            status: AtomicU16::new(200),
            requests: AtomicUsize::new(0),
            tokenizations: AtomicUsize::new(0),
            accepted: Mutex::new(Vec::new()),
        });
        let app = Router::new()
            .route("/v1/embeddings", post(embeddings))
            .route("/tokenize", post(tokenize))
            .route("/custom/tokenize", post(tokenize))
            .with_state(state.clone());
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = format!("http://{}/v1", listener.local_addr().unwrap());
        let task = tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        Self {
            endpoint,
            state,
            task,
        }
    }

    fn embedder(&self) -> HttpEmbedder {
        HttpEmbedder::new(&self.endpoint, "").unwrap()
    }
}

fn token_count(text: &str) -> usize {
    text.chars().count() + 2
}

async fn tokenize(
    State(state): State<Arc<Mock>>,
    Json(request): Json<Value>,
) -> (StatusCode, Json<Value>) {
    state.tokenizations.fetch_add(1, Ordering::SeqCst);
    if !state.tokenizer {
        return (StatusCode::NOT_FOUND, Json(json!({})));
    }
    if state.status.load(Ordering::SeqCst) == 299 {
        return (
            StatusCode::OK,
            Json(json!({"tokens":"private-response-marker"})),
        );
    }
    assert_eq!(request["add_special"], true);
    let count = token_count(request["content"].as_str().unwrap());
    (StatusCode::OK, Json(json!({"tokens":vec![1;count]})))
}

async fn embeddings(
    State(state): State<Arc<Mock>>,
    Json(request): Json<Value>,
) -> (StatusCode, Json<Value>) {
    state.requests.fetch_add(1, Ordering::SeqCst);
    let status = state.status.load(Ordering::SeqCst);
    if status == 299 {
        return (
            StatusCode::OK,
            Json(json!({"data":[{"embedding":"private-response-marker"}]})),
        );
    }
    if status != 200 {
        return (
            StatusCode::from_u16(status).unwrap(),
            Json(
                json!({"error":{"message":"private-response-marker input (999 tokens) is larger than the max context size (20)"}}),
            ),
        );
    }
    let inputs: Vec<&str> = request["input"]
        .as_array()
        .unwrap()
        .iter()
        .map(|value| value.as_str().unwrap())
        .collect();
    let largest = inputs.iter().map(|input| token_count(input)).max().unwrap();
    let too_large = match state.limit {
        Limit::Context(limit) | Limit::Physical(limit) => largest > limit,
        Limit::Request(limit) => {
            inputs.iter().map(|input| token_count(input)).sum::<usize>() > limit
        }
    };
    if too_large {
        let (status, message) = match state.limit {
            Limit::Context(limit) => (
                StatusCode::BAD_REQUEST,
                format!(
                    "input ({largest} tokens) is larger than the max context size ({limit}); private-response-marker"
                ),
            ),
            _ => (
                StatusCode::INTERNAL_SERVER_ERROR,
                format!(
                    "input ({largest} tokens) is too large to process. increase the physical batch size; private-response-marker"
                ),
            ),
        };
        return (status, Json(json!({"error":{"message":message}})));
    }
    if inputs.iter().any(|input| input.contains("outage")) {
        return (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(json!({"error":{"message":"private-response-marker"}})),
        );
    }
    state
        .accepted
        .lock()
        .unwrap()
        .extend(inputs.iter().map(|input| (*input).to_owned()));
    // Reverse rows to exercise index mapping during successful split batches.
    let data: Vec<_> = inputs
        .iter()
        .enumerate()
        .rev()
        .map(
            |(index, input)| json!({"index":index,"embedding":[input.chars().count() as f32, 1.0]}),
        )
        .collect();
    (StatusCode::OK, Json(json!({"data":data})))
}

fn cortex(root: &std::path::Path, server: &Server) -> Cortex {
    Cortex::create("sample", root).unwrap();
    let mut cx = Cortex::open("sample", root.join("sample")).unwrap();
    cx.manifest.search = Some(SearchConfig {
        semantic_enabled: true,
        embedding_model: "test-model".into(),
        embedding_endpoint: server.endpoint.clone(),
        ..Default::default()
    });
    cx
}

fn add(cx: &Cortex, title: &str, body: &str, order: usize) -> String {
    let mut trace = Trace::new(title, "fact", "", Vec::new(), body);
    trace.frontmatter.created = format!("2026-01-{:02}T00:00:00Z", order + 1);
    cx.add(&mut trace).unwrap();
    trace.frontmatter.id
}

#[tokio::test]
async fn oversized_first_trace_is_trimmed_and_neighbors_are_saved() {
    let server = Server::start(Limit::Context(32), true).await;
    let temp = tempfile::tempdir().unwrap();
    let mut cx = cortex(temp.path(), &server);
    let body = "界é🦀".repeat(50);
    let id = add(&cx, "long", &body, 0);
    add(&cx, "short", "small", 1);
    let result = cx
        .embed_backfill(&server.embedder(), "test-model", &Default::default())
        .await
        .unwrap();
    assert_eq!(
        (result.embedded, result.truncated, result.failures.len()),
        (2, 1, 0)
    );
    assert_eq!(cx.get_trace(&id).unwrap().1.body, body);
    let status = cx.embedding_status("test-model").unwrap();
    assert_eq!(
        (status.embedded, status.truncated, status.missing),
        (2, 1, 0)
    );
    let database = db::open(&temp.path().join("sample")).unwrap();
    let (tokens, input_hash): (i64, String) = database
        .query_row(
            "SELECT input_tokens,input_hash FROM trace_embeddings WHERE trace_id=?1",
            [&id],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )
        .unwrap();
    assert!(tokens <= 32);
    assert!(!input_hash.is_empty());
    assert!(
        server
            .state
            .accepted
            .lock()
            .unwrap()
            .iter()
            .all(|input| token_count(input) <= 32)
    );
    let requests = server.state.requests.load(Ordering::SeqCst);
    assert_eq!(
        cx.embed_backfill(&server.embedder(), "test-model", &Default::default())
            .await
            .unwrap()
            .considered,
        0
    );
    assert_eq!(server.state.requests.load(Ordering::SeqCst), requests);
}

#[tokio::test]
async fn physical_limit_and_aggregate_batch_limit_have_distinct_recovery() {
    for (limit, expected_truncated) in [(Limit::Physical(32), 1), (Limit::Request(200), 0)] {
        let server = Server::start(limit, true).await;
        let temp = tempfile::tempdir().unwrap();
        let mut cx = cortex(temp.path(), &server);
        add(&cx, "first", &"x".repeat(150), 0);
        add(&cx, "second", &"y".repeat(20), 1);
        add(&cx, "third", &"z".repeat(20), 2);
        let result = cx
            .embed_backfill(&server.embedder(), "test-model", &Default::default())
            .await
            .unwrap();
        assert_eq!(result.embedded, 3);
        assert_eq!(result.truncated, expected_truncated);
        assert!(result.failures.is_empty());
        if expected_truncated == 0 {
            assert_eq!(server.state.tokenizations.load(Ordering::SeqCst), 0);
        }
    }
}

#[tokio::test]
async fn cooldown_is_applied_before_limit_and_force_retries() {
    let server = Server::start(Limit::Context(32), false).await;
    let temp = tempfile::tempdir().unwrap();
    let mut cx = cortex(temp.path(), &server);
    let id = add(&cx, "long", &"x".repeat(100), 0);
    add(&cx, "short", "small", 1);
    let options = EmbedBackfillOptions {
        limit: 1,
        ..Default::default()
    };
    let first = cx
        .embed_backfill(&server.embedder(), "test-model", &options)
        .await
        .unwrap();
    assert_eq!((first.embedded, first.failures.len()), (0, 1));
    assert_eq!(first.failures[0].trace_id, id);
    assert!(first.failures[0].reason.contains("limit=32"));
    assert!(!first.failures[0].reason.contains("private-response-marker"));
    let second = cx
        .embed_backfill(&server.embedder(), "test-model", &options)
        .await
        .unwrap();
    assert_eq!(
        (second.considered, second.embedded, second.deferred),
        (1, 1, 1)
    );
    assert_eq!(cx.embedding_status("test-model").unwrap().deferred, 1);
    let forced = cx
        .embed_backfill(
            &server.embedder(),
            "test-model",
            &EmbedBackfillOptions {
                force: true,
                ..options
            },
        )
        .await
        .unwrap();
    assert_eq!(forced.failures.len(), 1);
    // Expiry also restores eligibility without force.
    db::open(&temp.path().join("sample"))
        .unwrap()
        .execute(
            "UPDATE embedding_failures SET retry_after='2000-01-01T00:00:00Z'",
            [],
        )
        .unwrap();
    assert_eq!(
        cx.embed_backfill(&server.embedder(), "test-model", &options)
            .await
            .unwrap()
            .failures
            .len(),
        1
    );
}

#[tokio::test]
async fn explicit_budget_is_measured_and_policy_changes_invalidate_vectors() {
    let server = Server::start(Limit::Context(1000), true).await;
    let temp = tempfile::tempdir().unwrap();
    let mut cx = cortex(temp.path(), &server);
    let id = add(&cx, "title", &"é🦀".repeat(40), 0);
    cx.manifest.search.as_mut().unwrap().max_tokens = 24;
    cx.manifest.search.as_mut().unwrap().tokenizer_path = "/custom/tokenize".into();
    let first = cx
        .embed_backfill(&server.embedder(), "test-model", &Default::default())
        .await
        .unwrap();
    assert_eq!((first.embedded, first.truncated), (1, 1));
    assert_eq!(server.state.requests.load(Ordering::SeqCst), 1);
    assert!(token_count(&server.state.accepted.lock().unwrap()[0]) <= 24);
    cx.manifest.search.as_mut().unwrap().max_tokens = 48;
    assert_eq!(cx.embedding_status("test-model").unwrap().stale, 1);
    assert_eq!(
        cx.embed_backfill(&server.embedder(), "test-model", &Default::default())
            .await
            .unwrap()
            .embedded,
        1
    );
    // Title changes affect embedding input even when the body hash is unchanged.
    db::open(&temp.path().join("sample"))
        .unwrap()
        .execute("UPDATE traces SET title='renamed' WHERE id=?1", [&id])
        .unwrap();
    assert_eq!(cx.embedding_status("test-model").unwrap().stale, 1);
    assert_eq!(
        cx.embed_backfill(&server.embedder(), "test-model", &Default::default())
            .await
            .unwrap()
            .embedded,
        1
    );
}

#[tokio::test]
async fn unavailable_explicit_tokenizer_and_auth_errors_are_run_failures() {
    let server = Server::start(Limit::Context(32), false).await;
    let temp = tempfile::tempdir().unwrap();
    let mut cx = cortex(temp.path(), &server);
    add(&cx, "short", "small", 0);
    cx.manifest.search.as_mut().unwrap().max_tokens = 24;
    let error = cx
        .embed_backfill(&server.embedder(), "test-model", &Default::default())
        .await
        .unwrap_err();
    assert!(format!("{error:#}").contains("no compatible /tokenize"));
    assert_eq!(server.state.requests.load(Ordering::SeqCst), 0);
    cx.manifest.search.as_mut().unwrap().max_tokens = 0;
    for status in [401, 403, 429, 503] {
        server.state.status.store(status, Ordering::SeqCst);
        let error = cx
            .embed_backfill(&server.embedder(), "test-model", &Default::default())
            .await
            .unwrap_err();
        assert!(!error.is::<InputSizeError>());
        let message = format!("{error:#}");
        assert!(message.contains(&status.to_string()));
        assert!(!message.contains("private-response-marker"));
        assert_eq!(cx.embedding_status("test-model").unwrap().deferred, 0);
    }
}

#[tokio::test]
async fn successful_splits_survive_a_later_outage() {
    let server = Server::start(Limit::Context(32), true).await;
    let temp = tempfile::tempdir().unwrap();
    let mut cx = cortex(temp.path(), &server);
    add(&cx, "long", &"x".repeat(100), 0);
    add(&cx, "outage", "small", 1);
    let error = cx
        .embed_backfill(&server.embedder(), "test-model", &Default::default())
        .await
        .unwrap_err();
    assert!(format!("{error:#}").contains("1 embeddings saved"));
    assert_eq!(cx.embedding_status("test-model").unwrap().embedded, 1);
}

#[tokio::test]
async fn malformed_provider_responses_do_not_echo_private_values() {
    let server = Server::start(Limit::Context(32), true).await;
    server.state.status.store(299, Ordering::SeqCst);
    let temp = tempfile::tempdir().unwrap();
    let mut cx = cortex(temp.path(), &server);
    add(&cx, "short", "small", 0);
    for budget in [0, 24] {
        cx.manifest.search.as_mut().unwrap().max_tokens = budget;
        let error = cx
            .embed_backfill(&server.embedder(), "test-model", &Default::default())
            .await
            .unwrap_err();
        let message = format!("{error:#}");
        assert!(message.contains("invalid response"), "{message}");
        assert!(!message.contains("private-response-marker"));
        assert_eq!(cx.embedding_status("test-model").unwrap().deferred, 0);
    }
}

#[tokio::test]
async fn edited_failed_trace_bypasses_cooldown_and_clears_failure() {
    let server = Server::start(Limit::Context(32), false).await;
    let temp = tempfile::tempdir().unwrap();
    let mut cx = cortex(temp.path(), &server);
    let id = add(&cx, "long", &"x".repeat(100), 0);
    add(&cx, "short", "small", 1);
    let first = cx
        .embed_backfill(&server.embedder(), "test-model", &Default::default())
        .await
        .unwrap();
    assert_eq!((first.embedded, first.failures.len()), (1, 1));
    let (_, mut trace) = cx.get_trace(&id).unwrap();
    trace.body = "shortened by the user".into();
    cx.update_trace(&id, &mut trace, false).unwrap();
    let next = cx
        .embed_backfill(&server.embedder(), "test-model", &Default::default())
        .await
        .unwrap();
    assert_eq!(
        (next.embedded, next.deferred, next.failures.len()),
        (1, 0, 0)
    );
    let count: i64 = db::open(&cx.dir)
        .unwrap()
        .query_row("SELECT COUNT(*) FROM embedding_failures", [], |row| {
            row.get(0)
        })
        .unwrap();
    assert_eq!(count, 0);
}

#[tokio::test]
async fn cli_reports_partial_progress_and_policy_change_recovers_deferred_trace() {
    let server = Server::start(Limit::Context(32), false).await;
    let temp = tempfile::tempdir().unwrap();
    let mut cx = cortex(temp.path(), &server);
    let id = add(&cx, "long", &"x".repeat(100), 0);
    add(&cx, "short", "small", 1);
    noema::cortex::write_manifest(&cx.dir, &cx.manifest).unwrap();
    let config = temp.path().join("config");
    std::fs::create_dir_all(config.join("noema")).unwrap();
    std::fs::write(
        config.join("noema/config.yaml"),
        json!({"default":"sample","cortexes":{"sample":{"path":cx.dir}}}).to_string(),
    )
    .unwrap();
    async fn cli(config: &std::path::Path, args: &[&str]) -> std::process::Output {
        tokio::process::Command::new(env!("CARGO_BIN_EXE_noema"))
            .env("XDG_CONFIG_HOME", config)
            .env("NOEMA_CORTEX", "sample")
            .args(args)
            .output()
            .await
            .unwrap()
    }
    let output = cli(&config, &["embeddings", "backfill"]).await;
    assert!(!output.status.success());
    let stdout = String::from_utf8_lossy(&output.stdout);
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(stdout.contains("1 embedded"), "{stdout}");
    assert!(
        stderr.contains(&id) && stderr.contains("limit=32"),
        "{stderr}"
    );
    assert!(!stderr.contains("private-response-marker"));
    let status = cli(&config, &["embeddings", "status"]).await;
    assert!(status.status.success());
    assert!(String::from_utf8_lossy(&status.stdout).contains("deferred (retry cooldown): 1"));
    cx.manifest.search.as_mut().unwrap().max_chars = 20;
    noema::cortex::write_manifest(&cx.dir, &cx.manifest).unwrap();
    let output = cli(&config, &["embeddings", "backfill"]).await;
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(String::from_utf8_lossy(&output.stdout).contains("2 embedded, 1 truncated, 0 failed"));
    assert_eq!(cx.get_trace(&id).unwrap().1.body, "x".repeat(100));
}
