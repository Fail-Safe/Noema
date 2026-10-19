use super::*;

fn preferences_only(max_body_chars: usize) -> RecallContextParams {
    RecallContextParams {
        queries: Vec::new(),
        include_preferences: Some(true),
        known_preferences_revision: None,
        limit_per_query: None,
        max_body_chars: Some(max_body_chars),
        all: false,
        mode: "lexical".into(),
        record_usage: false,
    }
}

#[tokio::test]
async fn cached_recall_keeps_metadata_membership_and_full_bodies_current() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("test", temp.path()).unwrap();
    let root = temp.path().join("test");
    let server = NoemaServer::new("test", &root, false).unwrap();
    let mut trace = Trace::new(
        "Cached preference",
        "preference",
        "test",
        vec!["user-preference".into()],
        "Original full preference body",
    );
    let id = trace.frontmatter.id.clone();
    server.open().await.unwrap().add(&mut trace).unwrap();
    let short = server
        .recall_context(Parameters(preferences_only(8)))
        .await
        .unwrap()
        .0;
    assert_eq!(short.preferences[0].body, "Original");
    assert!(short.preferences[0].body_truncated);
    let full = server
        .recall_context(Parameters(preferences_only(4000)))
        .await
        .unwrap()
        .0;
    assert_eq!(full.preferences[0].body, trace.body);
    #[cfg(unix)]
    assert_eq!(server.open().await.unwrap().recall_cache_counts(), (1, 1));

    let peer = Cortex::open("test", &root).unwrap();
    trace.frontmatter.title = "Metadata-only title".into();
    peer.update_trace(&id, &mut trace, false).unwrap();
    let metadata_only = server
        .recall_context(Parameters(preferences_only(4000)))
        .await
        .unwrap()
        .0;
    assert_eq!(metadata_only.preferences[0].title, "Metadata-only title");
    assert_eq!(
        metadata_only.preferences[0].content_hash,
        full.preferences[0].content_hash
    );
    trace.body = "Updated full preference body".into();
    trace.frontmatter.title = "Updated title".into();
    peer.update_trace(&id, &mut trace, false).unwrap();
    let updated = server
        .recall_context(Parameters(preferences_only(4000)))
        .await
        .unwrap()
        .0;
    assert_eq!(updated.preferences[0].body, trace.body);
    assert_eq!(updated.preferences[0].title, "Updated title");
    assert_ne!(
        updated.preferences[0].content_hash,
        full.preferences[0].content_hash
    );

    peer.set_tags(&id, Vec::new(), false).unwrap();
    assert!(
        server
            .recall_context(Parameters(preferences_only(4000)))
            .await
            .unwrap()
            .0
            .preferences
            .is_empty()
    );
    peer.set_tags(&id, vec!["user-preference".into()], false)
        .unwrap();
    peer.archive(&id).unwrap();
    assert!(
        server
            .recall_context(Parameters(preferences_only(4000)))
            .await
            .unwrap()
            .0
            .preferences
            .is_empty()
    );
    peer.unarchive(&id).unwrap();
    assert_eq!(
        server
            .recall_context(Parameters(preferences_only(4000)))
            .await
            .unwrap()
            .0
            .preferences[0]
            .body,
        trace.body
    );

    let mut added = Trace::new(
        "New preference",
        "preference",
        "test",
        vec!["user-preference".into()],
        "New policy",
    );
    peer.add(&mut added).unwrap();
    assert_eq!(
        server
            .recall_context(Parameters(preferences_only(4000)))
            .await
            .unwrap()
            .0
            .preferences
            .len(),
        2
    );
}

fn distribution(values: &mut [f64]) -> serde_json::Value {
    values.sort_by(f64::total_cmp);
    json!({
        "samples": values.len(),
        "p50_ms": values[values.len() / 2],
        "p95_ms": values[(values.len() * 95).div_ceil(100) - 1],
        "min_ms": values[0],
        "max_ms": values[values.len() - 1],
    })
}

#[tokio::test]
#[ignore = "quantitative release-mode A/B benchmark; prints synthetic measurements"]
async fn preference_cache_ab_benchmark() {
    let mut profiles = Vec::new();
    let mut corpora = [(8usize, 256usize), (8, 4000), (24, 12000)]
        .into_iter()
        .map(|(count, chars)| {
            (
                "synthetic",
                vec!["Evidence policy. ".repeat(chars.div_ceil(17)); count]
                    .into_iter()
                    .map(|body| body.chars().take(chars).collect::<String>())
                    .collect::<Vec<_>>(),
                chars,
            )
        })
        .collect::<Vec<_>>();
    if let Ok(path) = std::env::var("NOEMA_CACHE_BENCH_BODIES") {
        let bodies: Vec<String> = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        assert!(!bodies.is_empty() && bodies.len() <= MAX_RECALL_PREFERENCES);
        corpora.push((
            "local_preference_snapshot",
            bodies,
            DEFAULT_RECALL_BODY_CHARS,
        ));
    }
    for (corpus, bodies, max_body_chars) in corpora {
        let preference_count = bodies.len();
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("benchmark", temp.path()).unwrap();
        let root = temp.path().join("benchmark");
        let server = NoemaServer::new("benchmark", &root, false).unwrap();
        for (index, body) in bodies.iter().enumerate() {
            let mut trace = Trace::new(
                format!("Preference {index}"),
                "preference",
                "benchmark",
                vec!["user-preference".into()],
                body,
            );
            server.open().await.unwrap().add(&mut trace).unwrap();
        }
        let expected = serde_json::to_vec(
            &server
                .recall_context(Parameters(preferences_only(max_body_chars)))
                .await
                .unwrap()
                .0,
        )
        .unwrap();
        let mut warm = [Vec::new(), Vec::new()];
        let mut cold = [Vec::new(), Vec::new()];
        let mut counts = [(0, 0), (0, 0)];
        for block in 0..12 {
            for arm in if block % 2 == 0 { [0, 1] } else { [1, 0] } {
                server
                    .open()
                    .await
                    .unwrap()
                    .set_recall_cache_enabled(arm == 1);
                for sample in 0..51 {
                    let start = Instant::now();
                    let output = server
                        .recall_context(Parameters(preferences_only(max_body_chars)))
                        .await
                        .unwrap()
                        .0;
                    let encoded = serde_json::to_vec(&output).unwrap();
                    let elapsed_ms = start.elapsed().as_secs_f64() * 1000.0;
                    assert_eq!(encoded, expected);
                    if sample == 0 {
                        cold[arm].push(elapsed_ms);
                    } else {
                        warm[arm].push(elapsed_ms);
                    }
                }
                let (hits, reads) = server.open().await.unwrap().recall_cache_counts();
                counts[arm].0 += hits;
                counts[arm].1 += reads;
            }
        }
        assert_eq!(counts[0], (0, 12 * 51 * preference_count));
        #[cfg(unix)]
        assert_eq!(
            counts[1],
            (12 * 50 * preference_count, 12 * preference_count)
        );
        let arms = ["uncached", "cached"]
            .into_iter()
            .enumerate()
            .map(|(arm, name)| {
                (
                    name.to_owned(),
                    json!({
                        "warm": distribution(&mut warm[arm]),
                        "cold": distribution(&mut cold[arm]),
                        "body_cache_hits": counts[arm].0,
                        "body_file_reads": counts[arm].1,
                    }),
                )
            })
            .collect::<serde_json::Map<_, _>>();
        profiles.push(json!({
            "corpus": corpus,
            "preference_count": preference_count,
            "max_returned_body_chars": max_body_chars,
            "stored_body_chars": bodies.iter().map(|body| body.chars().count()).collect::<Vec<_>>(),
            "response_bytes": expected.len(),
            "responses_identical": true,
            "arms": arms,
        }));
    }
    println!(
        "CACHE_AB_JSON={}",
        json!({
            "measurement": "in-process recall_context handler including JSON serialization and operation metrics; no network transport or model evaluation",
            "profiles": profiles,
        })
    );
}
