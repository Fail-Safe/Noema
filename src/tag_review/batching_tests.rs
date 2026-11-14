use std::{
    future::Future,
    sync::{Arc, Mutex},
};

use axum::{Json, Router, http::StatusCode, routing::post};
use serde_json::Value;

use super::*;
use crate::trace::Trace;

fn fixture(count: usize) -> (tempfile::TempDir, Cortex) {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("test", temp.path()).unwrap();
    let cx = Cortex::open("test", temp.path().join("test")).unwrap();
    for i in 0..count {
        let mut trace = Trace::new(
            format!("Concurrency fixture {i}"),
            "note",
            "",
            vec!["rust".into()],
            "Rust concurrency notes",
        );
        cx.add(&mut trace).unwrap();
    }
    (temp, cx)
}

fn configure(cx: &mut Cortex, endpoint: String) {
    cx.manifest.tag_review = Some(TagReviewConfig {
        mode: ModelMode::Separate,
        model_name: "fixture-review".into(),
        endpoint,
        api_key_env: String::new(),
    });
}

async fn mock<F, Fut>(responder: F) -> (String, tokio::task::JoinHandle<()>)
where
    F: Fn(Value) -> Fut + Send + Sync + 'static,
    Fut: Future<Output = (StatusCode, Value)> + Send + 'static,
{
    let responder = Arc::new(responder);
    let app = Router::new().route(
        "/v1/chat/completions",
        post(move |Json(request): Json<Value>| {
            let responder = Arc::clone(&responder);
            async move {
                let (status, body) = responder(request).await;
                (status, Json(body))
            }
        }),
    );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let endpoint = format!("http://{}/v1", listener.local_addr().unwrap());
    let handle = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (endpoint, handle)
}

fn ids(request: &Value) -> Vec<String> {
    assert_eq!(request["model"], "fixture-review");
    assert_eq!(request["max_tokens"], 6000);
    assert!(
        request["messages"][0]["content"]
            .as_str()
            .unwrap()
            .contains("Omit unchanged traces entirely")
    );
    let input: Value =
        serde_json::from_str(request["messages"][1]["content"].as_str().unwrap()).unwrap();
    input["traces"]
        .as_array()
        .unwrap()
        .iter()
        .map(|trace| trace["id"].as_str().unwrap().to_owned())
        .collect()
}

fn complete(ids: &[String]) -> Value {
    let suggestions: Vec<_> = ids.iter().map(|id| json!({"trace_id":id,"after":["rust","concurrency"],"rationale":"Body discusses concurrency."})).collect();
    json!({"choices":[{"finish_reason":"stop","message":{"content":json!({"suggestions":suggestions}).to_string()}}]})
}

#[tokio::test]
async fn forty_trace_window_uses_four_small_requests() {
    let (_temp, mut cx) = fixture(40);
    let sizes = Arc::new(Mutex::new(Vec::new()));
    let observed = Arc::clone(&sizes);
    let (endpoint, server) = mock(move |request| {
        let ids = ids(&request);
        observed.lock().unwrap().push(ids.len());
        async move { (StatusCode::OK, complete(&ids)) }
    })
    .await;
    configure(&mut cx, endpoint);
    let path = cx.dir.clone();
    let result = analyze(cx, 0).await.unwrap();
    server.abort();
    assert_eq!(*sizes.lock().unwrap(), [10, 10, 10, 10]);
    assert_eq!(result.status, "completed");
    assert_eq!(result.requested, 40);
    assert_eq!(result.analyzed, 40);
    assert_eq!(result.unfinished, 0);
    assert_eq!(result.queued, 40);
    assert_eq!(result.requests, 4);
    let cx = Cortex::open("test", path).unwrap();
    assert_eq!(cx.tag_reviews().unwrap().len(), 40);
    assert!(
        cx.list(&ListOptions::default())
            .unwrap()
            .iter()
            .all(|row| row.tags == ["rust"])
    );
}

#[tokio::test]
async fn truncated_responses_are_split_and_never_salvaged() {
    let (_temp, mut cx) = fixture(10);
    let sizes = Arc::new(Mutex::new(Vec::new()));
    let observed = Arc::clone(&sizes);
    let (endpoint, server) = mock(move |request| {
        let ids = ids(&request);
        observed.lock().unwrap().push(ids.len());
        async move {
            let response = if ids.len() > 2 {
                // Even a parseable prefix must not become a proposal if the
                // provider says its generation was incomplete.
                let unsafe_output = json!({"suggestions":[{"trace_id":ids[0],"after":["must-not-be-queued"],"rationale":"Incomplete output"}]}).to_string();
                json!({"choices":[{"finish_reason":"length","message":{"content":unsafe_output}}]})
            } else { complete(&ids) };
            (StatusCode::OK, response)
        }
    }).await;
    configure(&mut cx, endpoint);
    let path = cx.dir.clone();
    let result = analyze(cx, 0).await.unwrap();
    server.abort();
    assert_eq!(*sizes.lock().unwrap(), [10, 5, 2, 3, 1, 2, 5, 2, 3, 1, 2]);
    assert_eq!(result.status, "completed");
    assert_eq!(result.split_retries, 5);
    assert_eq!(result.requests, 11);
    assert_eq!(result.analyzed, 10);
    assert_eq!(result.queued, 10);
    let reviews = Cortex::open("test", path).unwrap().tag_reviews().unwrap();
    assert!(
        reviews
            .iter()
            .all(|review| review.after == ["rust", "concurrency"])
    );
}

#[tokio::test]
async fn later_provider_failure_retains_completed_proposals_and_retry_deduplicates() {
    let (_temp, mut cx) = fixture(12);
    let (endpoint, server) = mock(|request| async move {
        let ids = ids(&request);
        if ids.len() == 10 {
            (StatusCode::OK, complete(&ids))
        } else {
            (
                StatusCode::SERVICE_UNAVAILABLE,
                json!({"error":"synthetic-private-provider-echo"}),
            )
        }
    })
    .await;
    configure(&mut cx, endpoint);
    let path = cx.dir.clone();
    let result = analyze(cx, 0).await.unwrap();
    server.abort();
    assert_eq!(result.status, "partial");
    assert_eq!(result.requested, 12);
    assert_eq!(result.analyzed, 10);
    assert_eq!(result.queued, 10);
    assert_eq!(result.unfinished, 2);
    assert!(result.warning.as_ref().unwrap().contains("503"));
    assert!(
        !serde_json::to_string(&result)
            .unwrap()
            .contains("synthetic-private-provider-echo")
    );
    let mut cx = Cortex::open("test", &path).unwrap();
    assert_eq!(cx.tag_reviews().unwrap().len(), 10);
    let (endpoint, server) =
        mock(|request| async move { (StatusCode::OK, complete(&ids(&request))) }).await;
    configure(&mut cx, endpoint);
    let result = analyze(cx, 0).await.unwrap();
    server.abort();
    assert_eq!(result.status, "completed");
    assert_eq!(result.queued, 2);
    assert_eq!(result.skipped, 10);
    assert_eq!(
        Cortex::open("test", path)
            .unwrap()
            .tag_reviews()
            .unwrap()
            .len(),
        12
    );
}

#[tokio::test]
async fn reasoning_only_single_trace_truncation_is_reported_as_unfinished() {
    let (_temp, mut cx) = fixture(3);
    let (endpoint, server) = mock(|_| async {
        (StatusCode::OK, json!({"choices":[{"finish_reason":"length","message":{"content":null,"reasoning_content":"synthetic-private-reasoning"}}]}))
    }).await;
    configure(&mut cx, endpoint);
    let result = analyze(cx, 0).await.unwrap();
    server.abort();
    assert_eq!(result.status, "partial");
    assert_eq!(result.requests, 5);
    assert_eq!(result.unfinished, 3);
    assert_eq!(result.queued, 0);
    assert!(result.warning.as_ref().unwrap().contains("one trace"));
    assert!(
        !serde_json::to_string(&result)
            .unwrap()
            .contains("synthetic-private-reasoning")
    );
}

#[tokio::test]
async fn repeated_truncation_has_a_bounded_request_budget() {
    let (_temp, mut cx) = fixture(40);
    let (endpoint, server) = mock(|_| async {
        (
            StatusCode::OK,
            json!({"choices":[{"finish_reason":"length"}]}),
        )
    })
    .await;
    configure(&mut cx, endpoint);
    let result = analyze(cx, 0).await.unwrap();
    server.abort();
    assert_eq!(result.requests, 76); // four full binary split trees of ten leaves
    assert!(result.requests <= MAX_REQUESTS);
    assert_eq!(result.status, "partial");
    assert_eq!(result.unfinished, 40);
    assert!(result.warning.as_ref().unwrap().contains("one trace"));
}

#[tokio::test]
async fn timeout_retains_completed_progress_and_cancels_the_next_request() {
    let (_temp, mut cx) = fixture(11);
    let (endpoint, server) = mock(|request| async move {
        let ids = ids(&request);
        if ids.len() == 1 {
            tokio::time::sleep(Duration::from_secs(10)).await;
        }
        (StatusCode::OK, complete(&ids))
    })
    .await;
    configure(&mut cx, endpoint);
    let path = cx.dir.clone();
    let config = resolve_model(&cx.manifest).unwrap();
    let result = analyze_with_timeout(cx, 0, config, Duration::from_secs(2))
        .await
        .unwrap();
    server.abort();
    assert_eq!(result.status, "partial");
    assert_eq!(result.queued, 10);
    assert_eq!(result.analyzed, 10);
    assert_eq!(result.unfinished, 1);
    assert!(result.warning.as_ref().unwrap().contains("time budget"));
    assert_eq!(
        Cortex::open("test", path)
            .unwrap()
            .tag_reviews()
            .unwrap()
            .len(),
        10
    );
}

#[tokio::test]
async fn oversized_replies_also_retry_in_smaller_groups() {
    let (_temp, mut cx) = fixture(2);
    let (endpoint, server) = mock(|request| async move {
        let ids = ids(&request);
        let body = if ids.len() > 1 {
            json!({"choices":[{"finish_reason":"stop","message":{"content":"x".repeat(MAX_RESPONSE + 1)}}]})
        } else { complete(&ids) };
        (StatusCode::OK, body)
    }).await;
    configure(&mut cx, endpoint);
    let result = analyze(cx, 0).await.unwrap();
    server.abort();
    assert_eq!(result.status, "completed");
    assert_eq!(result.requests, 3);
    assert_eq!(result.split_retries, 1);
    assert_eq!(result.queued, 2);
}
