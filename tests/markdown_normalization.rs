use std::fs;

use noema::{
    cortex::Cortex,
    markdown_normalization::{MarkdownNormalizationPlan, MarkdownOperation, MarkdownTracePlan},
    trace::{self, Trace},
};

#[test]
fn normalization_preserves_metadata_audits_recovery_and_replays() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("source", temp.path()).unwrap();
    Cortex::create("peer", temp.path()).unwrap();
    let source_root = temp.path().join("source");
    let peer_root = temp.path().join("peer");
    let source = Cortex::open("source", &source_root).unwrap();
    let peer = Cortex::open("peer", &peer_root).unwrap();

    let mut original = Trace::new(
        "Palette reference",
        "preference",
        "tester",
        vec!["theme".into()],
        "Color #abcdef.\nConfiguration:\nkey = value\n#disabled = true\nDone.\n",
    );
    original.frontmatter.extra.insert(
        "custom".into(),
        serde_yaml::Value::String("preserved".into()),
    );
    source.add(&mut original).unwrap();
    let id = original.frontmatter.id.clone();
    source.promote(&id, "mid").unwrap();
    source.promote(&id, "long").unwrap();
    let before = source.get_trace(&id).unwrap().1;
    let before_bytes = fs::read(source.trace_file(&id, false)).unwrap();
    let expected_body =
        "Color `#abcdef`.\nConfiguration:\n\n```ini\nkey = value\n#disabled = true\n```\n\nDone.\n";
    let plan = MarkdownNormalizationPlan {
        schema_version: 1,
        cortex_id: source.id.clone(),
        traces: vec![MarkdownTracePlan {
            trace_id: id.clone(),
            relative_path: format!("traces/{id}.md"),
            tier: "long".into(),
            expected_content_hash: trace::content_hash(&before.body),
            expected_result_hash: trace::content_hash(expected_body),
            operations: vec![
                MarkdownOperation::InlineCode {
                    line: 1,
                    column: 7,
                    literal: "#abcdef".into(),
                },
                MarkdownOperation::FencedBlock {
                    start_line: 3,
                    end_line: 4,
                    language: "ini".into(),
                },
            ],
        }],
    };

    let preview = source.markdown_normalization_preview(&plan).unwrap();
    assert_eq!(preview.len(), 1);
    assert_eq!(preview[0].tier, "long");
    assert_eq!(source.get_trace(&id).unwrap().1, before);

    let results = source.normalize_markdown(&plan).unwrap();
    let (_, after) = source.get_trace(&id).unwrap();
    assert_eq!(after.body, expected_body);
    assert_eq!(after.frontmatter.title, before.frontmatter.title);
    assert_eq!(after.frontmatter.trace_type, before.frontmatter.trace_type);
    assert_eq!(after.frontmatter.tier, before.frontmatter.tier);
    assert_eq!(after.frontmatter.author, before.frontmatter.author);
    assert_eq!(after.frontmatter.tags, before.frontmatter.tags);
    assert_eq!(
        after.frontmatter.derived_from,
        before.frontmatter.derived_from
    );
    assert_eq!(after.frontmatter.origin, before.frontmatter.origin);
    assert_eq!(after.frontmatter.created, before.frontmatter.created);
    assert_eq!(
        after.frontmatter.source_hash,
        before.frontmatter.source_hash
    );
    assert_eq!(
        after.frontmatter.source_locked,
        before.frontmatter.source_locked
    );
    assert_eq!(after.frontmatter.extra, before.frontmatter.extra);
    assert_eq!(
        fs::read(source_root.join(&results[0].recovery_artifact)).unwrap(),
        before_bytes
    );

    let audit = source.history(&id).unwrap().pop().unwrap();
    assert_eq!(audit.action, "divergence_long_term");
    assert_eq!(
        audit.data["normalization"]["kind"].as_str(),
        Some("obsidian_body_tag_normalization")
    );
    assert_eq!(
        audit.data["normalization"]["before_content_hash"].as_str(),
        Some(trace::content_hash(&before.body).as_str())
    );

    for event in source.events_since("", 100).unwrap() {
        peer.replay_event(&event).unwrap();
    }
    let peer_trace = peer.get_trace(&id).unwrap().1;
    assert_eq!(peer_trace.body, before.body);
    assert_eq!(peer_trace.frontmatter.tier, "long");
    assert_eq!(peer_trace.frontmatter.extra, Default::default());
    assert_eq!(
        peer.history(&id).unwrap().last().unwrap().action,
        "divergence_long_term"
    );
    assert!(
        source
            .long_term_reconciliation_plan(&id)
            .unwrap()
            .drift_fields
            .is_empty()
    );

    let mut forbidden = after.clone();
    forbidden.body.push_str("ordinary edit");
    assert!(source.update_trace(&id, &mut forbidden, false).is_err());
}

#[test]
fn short_normalization_uses_compatible_update_event_and_replays() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("source", temp.path()).unwrap();
    Cortex::create("peer", temp.path()).unwrap();
    let source = Cortex::open("source", temp.path().join("source")).unwrap();
    let peer = Cortex::open("peer", temp.path().join("peer")).unwrap();
    let mut trace = Trace::new("Color", "note", "", vec![], "Color #abcdef\n");
    source.add(&mut trace).unwrap();
    let id = trace.frontmatter.id.clone();
    let expected_body = "Color `#abcdef`\n";
    let plan = MarkdownNormalizationPlan {
        schema_version: 1,
        cortex_id: source.id.clone(),
        traces: vec![MarkdownTracePlan {
            trace_id: id.clone(),
            relative_path: format!("traces/{id}.md"),
            tier: "short".into(),
            expected_content_hash: trace::content_hash(&trace.body),
            expected_result_hash: trace::content_hash(expected_body),
            operations: vec![MarkdownOperation::InlineCode {
                line: 1,
                column: 7,
                literal: "#abcdef".into(),
            }],
        }],
    };

    source.normalize_markdown(&plan).unwrap();
    assert_eq!(
        source.history(&id).unwrap().last().unwrap().action,
        "update"
    );
    for event in source.events_since("", 100).unwrap() {
        peer.replay_event(&event).unwrap();
    }
    assert_eq!(peer.get_trace(&id).unwrap().1.body, expected_body);
}

#[test]
fn normalization_preflights_every_trace_before_mutating_any() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("preflight", temp.path()).unwrap();
    let root = temp.path().join("preflight");
    let cortex = Cortex::open("preflight", &root).unwrap();
    let mut first = Trace::new("First", "note", "", vec![], "Color #abcdef\n");
    let mut second = Trace::new("Second", "note", "", vec![], "Color #123456\n");
    cortex.add(&mut first).unwrap();
    cortex.add(&mut second).unwrap();
    let first_before = fs::read(cortex.trace_file(&first.frontmatter.id, false)).unwrap();
    let plan_for = |trace: &Trace, expected: &str| MarkdownTracePlan {
        trace_id: trace.frontmatter.id.clone(),
        relative_path: format!("traces/{}.md", trace.frontmatter.id),
        tier: "short".into(),
        expected_content_hash: expected.into(),
        expected_result_hash: trace::content_hash(&format!("Color `{}`\n", &trace.body[6..13])),
        operations: vec![MarkdownOperation::InlineCode {
            line: 1,
            column: 7,
            literal: trace.body[6..13].into(),
        }],
    };
    let plan = MarkdownNormalizationPlan {
        schema_version: 1,
        cortex_id: cortex.id.clone(),
        traces: vec![
            plan_for(&first, &trace::content_hash(&first.body)),
            plan_for(&second, "sha256:stale"),
        ],
    };

    assert!(cortex.normalize_markdown(&plan).is_err());
    assert_eq!(
        fs::read(cortex.trace_file(&first.frontmatter.id, false)).unwrap(),
        first_before
    );
    assert!(source_artifacts(&root).is_empty());
}

fn source_artifacts(root: &std::path::Path) -> Vec<std::path::PathBuf> {
    let directory = root.join("db/markdown-normalizations");
    fs::read_dir(directory)
        .map(|entries| entries.map(|entry| entry.unwrap().path()).collect())
        .unwrap_or_default()
}
