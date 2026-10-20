use super::*;

fn params(revision: Option<String>) -> RecallContextParams {
    RecallContextParams {
        queries: Vec::new(),
        include_preferences: Some(true),
        known_preferences_revision: revision,
        limit_per_query: None,
        max_body_chars: Some(4000),
        all: false,
        mode: "lexical".into(),
        record_usage: false,
    }
}

async fn recall(server: &NoemaServer, revision: Option<String>) -> RecallContextOutput {
    server
        .recall_context(Parameters(params(revision)))
        .await
        .unwrap()
        .0
}

async fn fixture() -> (tempfile::TempDir, NoemaServer, Trace) {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("test", temp.path()).unwrap();
    let server = NoemaServer::new("test", temp.path().join("test"), false).unwrap();
    let mut trace = Trace::new(
        "Revision preference",
        "preference",
        "test",
        vec!["user-preference".into()],
        "Original policy",
    );
    server.open().await.unwrap().add(&mut trace).unwrap();
    (temp, server, trace)
}

#[cfg(unix)]
#[tokio::test]
async fn matching_revision_omits_bodies_but_preserves_queries_and_usage() {
    let (_temp, server, _trace) = fixture().await;
    let first = recall(&server, None).await;
    assert!(!first.preferences_unchanged);
    assert_eq!(first.preferences.len(), 1);
    let reads = server.open().await.unwrap().recall_cache_counts();
    let unchanged = recall(&server, first.preferences_revision.clone()).await;
    assert!(unchanged.preferences_unchanged);
    assert!(unchanged.preferences.is_empty());
    assert_eq!(unchanged.preferences_revision, first.preferences_revision);
    assert_eq!(server.open().await.unwrap().recall_cache_counts(), reads);

    let mut fact = Trace::new("Task evidence", "fact", "test", vec![], "ULTRAVIOLET");
    server.open().await.unwrap().add(&mut fact).unwrap();
    let mut request = params(first.preferences_revision.clone());
    request.queries = vec!["ULTRAVIOLET".into()];
    request.record_usage = true;
    let queried = server.recall_context(Parameters(request)).await.unwrap().0;
    assert!(queried.preferences_unchanged);
    assert!(queried.preferences.is_empty());
    assert_eq!(queried.results[0].matches[0].body, "ULTRAVIOLET");
    let usage = server
        .open()
        .await
        .unwrap()
        .local_usage_since("", 100)
        .unwrap();
    assert_eq!(
        usage
            .iter()
            .find(|row| row.trace_id == first.preferences[0].id)
            .unwrap()
            .read_count,
        1
    );
}

#[tokio::test]
async fn revisions_invalidate_on_metadata_membership_body_and_limit_changes() {
    let (_temp, server, mut trace) = fixture().await;
    let first = recall(&server, None).await;
    let id = trace.frontmatter.id.clone();
    server
        .open()
        .await
        .unwrap()
        .set_tags(&id, vec!["user-preference".into(), "new-tag".into()], false)
        .unwrap();
    let tagged = recall(&server, first.preferences_revision).await;
    assert!(!tagged.preferences_unchanged);
    assert!(tagged.preferences[0].tags.contains(&"new-tag".into()));
    assert_eq!(
        tagged.preferences[0].content_hash,
        trace.frontmatter.content_hash
    );

    trace.frontmatter.title = "New metadata".into();
    server
        .open()
        .await
        .unwrap()
        .update_trace(&id, &mut trace, false)
        .unwrap();
    let metadata = recall(&server, tagged.preferences_revision).await;
    assert!(!metadata.preferences_unchanged);
    assert_eq!(metadata.preferences[0].title, "New metadata");
    assert_eq!(metadata.preferences[0].body, "Original policy");

    trace.body = "Changed policy".into();
    server
        .open()
        .await
        .unwrap()
        .update_trace(&id, &mut trace, false)
        .unwrap();
    let changed = recall(&server, metadata.preferences_revision).await;
    assert!(!changed.preferences_unchanged);
    assert_eq!(changed.preferences[0].body, "Changed policy");
    let mut request = params(changed.preferences_revision.clone());
    request.max_body_chars = Some(3);
    let short = server.recall_context(Parameters(request)).await.unwrap().0;
    assert!(!short.preferences_unchanged);
    assert_eq!(short.preferences[0].body, "Cha");
    assert!(short.preferences[0].body_truncated);

    server.open().await.unwrap().archive(&id).unwrap();
    let empty = recall(&server, changed.preferences_revision).await;
    assert!(!empty.preferences_unchanged);
    assert!(empty.preferences.is_empty());
    server.open().await.unwrap().unarchive(&id).unwrap();
    let restored = recall(&server, empty.preferences_revision).await;
    assert!(!restored.preferences_unchanged);
    assert_eq!(restored.preferences.len(), 1);
    server
        .open()
        .await
        .unwrap()
        .set_tags(&id, Vec::new(), false)
        .unwrap();
    let removed = recall(&server, restored.preferences_revision).await;
    assert!(!removed.preferences_unchanged);
    assert!(removed.preferences.is_empty());
}

#[tokio::test]
async fn revisions_are_scoped_and_excluded_preferences_have_no_revision() {
    let (_temp, first, _trace) = fixture().await;
    let (_other_temp, other, _other_trace) = fixture().await;
    let loaded = recall(&first, None).await;
    let other_loaded = recall(&other, loaded.preferences_revision.clone()).await;
    assert!(!other_loaded.preferences_unchanged);
    assert_ne!(
        other_loaded.preferences_revision,
        loaded.preferences_revision
    );
    let mut request = params(loaded.preferences_revision);
    request.include_preferences = Some(false);
    request.queries = vec!["Original".into()];
    let excluded = first.recall_context(Parameters(request)).await.unwrap().0;
    assert_eq!(excluded.preferences_revision, None);
    assert!(!excluded.preferences_unchanged);
    assert!(excluded.preferences.is_empty());
    let stale = recall(&first, Some("unknown-revision".into())).await;
    assert!(!stale.preferences_unchanged);
    assert_eq!(stale.preferences.len(), 1);
}

#[tokio::test]
async fn external_edits_invalidate_revision_before_index_reconciliation() {
    let (_temp, server, trace) = fixture().await;
    let first = recall(&server, None).await;
    let path = {
        let cx = server.open().await.unwrap();
        cx.file_path(&cx.get(&trace.frontmatter.id).unwrap())
    };
    let modified = std::fs::metadata(&path).unwrap().modified().unwrap();
    let bytes = std::fs::read_to_string(&path)
        .unwrap()
        .replace("Original policy", "Modified policy");
    std::fs::write(&path, bytes).unwrap();
    std::fs::File::options()
        .write(true)
        .open(&path)
        .unwrap()
        .set_modified(modified)
        .unwrap();
    let changed = recall(&server, first.preferences_revision).await;
    assert!(!changed.preferences_unchanged);
    assert_eq!(changed.preferences[0].body, "Modified policy");
    std::fs::remove_file(&path).unwrap();
    assert!(
        server
            .recall_context(Parameters(params(changed.preferences_revision)))
            .await
            .is_err()
    );
}

#[cfg(unix)]
#[tokio::test]
async fn truncated_preference_sets_and_additions_remain_explicit() {
    let (_temp, server, _trace) = fixture().await;
    for index in 0..MAX_RECALL_PREFERENCES {
        let mut trace = Trace::new(
            format!("Preference {index}"),
            "preference",
            "test",
            vec!["user-preference".into()],
            "Policy",
        );
        server.open().await.unwrap().add(&mut trace).unwrap();
    }
    let full = recall(&server, None).await;
    assert!(full.preference_results_truncated);
    assert_eq!(full.preferences.len(), MAX_RECALL_PREFERENCES);
    let unchanged = recall(&server, full.preferences_revision).await;
    assert!(unchanged.preferences_unchanged);
    assert!(unchanged.preference_results_truncated);
    let mut added = Trace::new(
        "Added preference",
        "preference",
        "test",
        vec!["user-preference".into()],
        "New policy",
    );
    server.open().await.unwrap().add(&mut added).unwrap();
    let changed = recall(&server, unchanged.preferences_revision).await;
    assert!(!changed.preferences_unchanged);
    assert!(changed.preference_results_truncated);
}

#[tokio::test]
async fn agent_guidance_explains_conditional_reuse_and_older_servers() {
    let (_temp, server, _trace) = fixture().await;
    let cortex = server.open().await.unwrap();
    let markdown = render_instructions(&cortex.manifest);
    for rule in [
        "known_preferences_revision",
        "preferences_unchanged=true",
        "including an empty array",
        "compaction/context loss",
        "cortex or server",
        "application memory alone",
        "server returns no revision",
    ] {
        assert!(markdown.contains(rule), "missing startup rule: {rule}");
    }
    let usage = build_cortex_usage(&cortex).unwrap();
    let startup = usage.startup;
    assert_eq!(
        startup["preferred_preference_sequence"][0]["tool"],
        "recall_context"
    );
    assert_eq!(
        startup["preferred_preference_sequence"][0]["arguments"]["record_usage"],
        false
    );
    assert!(
        startup["preference_revision_policy"]
            .as_str()
            .unwrap()
            .contains("Application memory alone is insufficient")
    );
    assert_eq!(startup["preference_sequence"][0]["tool"], "list_traces");
}

#[tokio::test]
async fn cli_snapshot_revision_matches_mcp_view_and_reports_truncation() {
    let (_temp, server, _trace) = fixture().await;
    let cortex = server.open().await.unwrap();
    let snapshot = preference_snapshot(&cortex, 12000).await.unwrap();
    assert_eq!(snapshot["cortex_id"], cortex.manifest.id);
    let revision = snapshot["recall_context"]["preferences_revision"]
        .as_str()
        .unwrap();
    drop(cortex);
    let mut request = params(Some(revision.into()));
    request.max_body_chars = Some(12000);
    let output = server.recall_context(Parameters(request)).await.unwrap().0;
    assert!(output.preferences_unchanged);
    assert!(output.preferences.is_empty());
    let cortex = server.open().await.unwrap();
    let bounded = preference_snapshot(&cortex, 1).await.unwrap();
    assert_eq!(
        bounded["recall_context"]["preferences"][0]["body_truncated"],
        true
    );
}
