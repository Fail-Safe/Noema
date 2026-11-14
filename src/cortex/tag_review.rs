//! Local, human-reviewed taxonomy maintenance. Proposals never mutate traces.
use super::*;

const MAX_TAGS: usize = 64;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TagReview {
    pub id: String,
    pub trace_id: String,
    pub title: String,
    pub before: Vec<String>,
    pub after: Vec<String>,
    pub rationale: String,
    pub source: String,
    pub evidence: String,
    /// Hash of all non-tag trace content, including metadata and visibility.
    pub snapshot: String,
    pub status: String,
}

pub(crate) fn snapshot(row: &Row, trace: &Trace) -> Result<String> {
    let mut copy = trace.clone();
    copy.frontmatter.tags.clear();
    Ok(trace::content_hash(&serde_json::to_string(&json!({
        "trace": String::from_utf8(copy.encoded()?)?,
        "archived": row.archived_at,
        "trashed": row.trashed_at,
        "tier": row.tier,
        "origin": row.origin,
        "source_locked": row.source_locked,
    }))?))
}

pub(crate) fn validate_tags(before: &[String], after: &[String]) -> Result<()> {
    if after.len() > MAX_TAGS {
        bail!("at most {MAX_TAGS} tags are allowed in a review");
    }
    let mut seen = BTreeSet::new();
    for tag in after {
        if tag.is_empty()
            || tag.len() > 128
            || tag.chars().any(char::is_control)
            || !seen.insert(tag)
        {
            bail!("review tags must be nonempty, unique, and at most 128 bytes");
        }
        if !before.contains(tag) && !crate::tag::inspect(tag).is_empty() {
            bail!("new review tags must use canonical spelling");
        }
    }
    // These are selectors consumed by agents, not merely topical labels. Freeze
    // their membership (including malformed variants) in this review workflow.
    let protected = |tags: &[String]| -> BTreeSet<String> {
        tags.iter()
            .filter(|tag| {
                let normalized = crate::tag::suggested_fix(tag).unwrap_or_else(|| (*tag).clone());
                matches!(
                    normalized.as_str(),
                    "user-preference" | "user-level" | "project-level"
                ) || normalized.starts_with("to-")
                    || normalized.starts_with("from-")
            })
            .cloned()
            .collect()
    };
    if protected(before) != protected(after) {
        bail!(
            "Tag Review cannot change behavioral, scope, or routing tags; use explicit tag editing"
        );
    }
    Ok(())
}

impl Cortex {
    pub fn tag_reviews(&self) -> Result<Vec<TagReview>> {
        let mut stmt = self.connection.prepare(
            "SELECT id,status,proposal FROM tag_reviews ORDER BY CASE status WHEN 'pending' THEN 0 WHEN 'deferred' THEN 1 ELSE 2 END,created_at,id"
        )?;
        let rows = stmt.query_map([], |r| {
            Ok((
                r.get::<_, String>(0)?,
                r.get::<_, String>(1)?,
                r.get::<_, String>(2)?,
            ))
        })?;
        rows.map(|r| {
            let (id, status, payload) = r?;
            let mut proposal: TagReview = serde_json::from_str(&payload)?;
            proposal.id = id;
            proposal.status = status;
            Ok(proposal)
        })
        .collect()
    }

    pub fn tag_review(&self, id: &str) -> Result<TagReview> {
        let (status, payload): (String, String) = self
            .connection
            .query_row(
                "SELECT status,proposal FROM tag_reviews WHERE id=?1",
                [id],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .context("unknown tag review")?;
        let mut proposal: TagReview = serde_json::from_str(&payload)?;
        proposal.status = status;
        Ok(proposal)
    }

    /// Capture evidence without incrementing read/engagement signals.
    pub fn propose_tag_review(
        &self,
        id: &str,
        after: Vec<String>,
        rationale: &str,
        source: &str,
    ) -> Result<Option<TagReview>> {
        self.propose_tag_review_checked(id, after, rationale, source, None)
    }

    /// Pin model/doctor output to the evidence it actually reviewed, rather than
    /// accidentally rebasing an old suggestion onto a concurrently edited trace.
    pub(crate) fn propose_tag_review_checked(
        &self,
        id: &str,
        after: Vec<String>,
        rationale: &str,
        source: &str,
        expected: Option<(&[String], &str)>,
    ) -> Result<Option<TagReview>> {
        let (row, trace) = self.get_trace(id)?;
        if !row.trashed_at.is_empty() || (row.source_locked && row.origin != self.name) {
            bail!("trace is trashed or source-locked");
        }
        let before = trace.frontmatter.tags.clone();
        let snapshot = snapshot(&row, &trace)?;
        if let Some((tags, hash)) = expected
            && (tags != before || hash != snapshot)
        {
            bail!("stale review evidence: trace changed before the proposal was queued");
        }
        validate_tags(&before, &after)?;
        let mut before_set = before.clone();
        let mut after_set = after.clone();
        before_set.sort();
        after_set.sort();
        if before_set == after_set {
            return Ok(None);
        }
        if rationale.trim().is_empty() || rationale.len() > 4000 || source.len() > 256 {
            bail!("a bounded rationale and source are required");
        }
        let fingerprint = trace::content_hash(&serde_json::to_string(&(
            &row.id,
            &snapshot,
            &before_set,
            &after_set,
        ))?);
        let id = format!("review-{}", fingerprint.trim_start_matches("sha256:"));
        let proposal = TagReview {
            id: id.clone(),
            trace_id: row.id,
            title: row.title,
            before,
            after,
            rationale: rationale.to_owned(),
            source: source.to_owned(),
            evidence: trace.body.chars().take(2000).collect(),
            snapshot,
            status: "pending".into(),
        };
        let now = trace::now_rfc3339();
        let inserted = self.connection.execute(
            "INSERT OR IGNORE INTO tag_reviews(id,trace_id,fingerprint,proposal,created_at,updated_at) SELECT ?1,?2,?3,?4,?5,?5 WHERE EXISTS (SELECT 1 FROM traces WHERE id=?2 AND purged_at IS NULL)",
            params![id, proposal.trace_id, fingerprint, serde_json::to_string(&proposal)?, now],
        )?;
        Ok((inserted != 0).then_some(proposal))
    }

    /// Existing rejection/defer decisions are not overwritten by another scan.
    pub fn review_tag_hygiene(&self) -> Result<usize> {
        let rows = self.tag_rows()?;
        let report = crate::tag::doctor(&rows);
        let plan = crate::tag::doctor_fix_plan(&rows, &self.name, &report);
        let mut added = 0;
        for change in plan.changes {
            if validate_tags(&change.before, &change.after).is_ok() {
                let (row, current) = self.get_trace(&change.id)?;
                let evidence_snapshot = snapshot(&row, &current)?;
                added += usize::from(
                    self.propose_tag_review_checked(
                        &change.id,
                        change.after,
                        "Normalize tag spelling and formatting (deterministic tag doctor).",
                        "tag-doctor",
                        Some((&change.before, &evidence_snapshot)),
                    )?
                    .is_some(),
                );
            }
        }
        Ok(added)
    }

    pub fn set_tag_review_status(&self, id: &str, status: &str) -> Result<()> {
        if !matches!(status, "pending" | "rejected" | "deferred") {
            bail!("use apply or undo for trace-changing review actions");
        }
        let changed = self.connection.execute(
            "UPDATE tag_reviews SET status=?2,updated_at=?3 WHERE id=?1 AND status IN ('pending','deferred','rejected')",
            params![id, status, trace::now_rfc3339()],
        )?;
        if changed != 1 {
            bail!("review is missing or already applied/reverted");
        }
        Ok(())
    }

    /// Editing creates a fresh proposal and preserves the old decision/evidence.
    pub fn edit_tag_review(&self, id: &str, after: Vec<String>) -> Result<Option<TagReview>> {
        let proposal = self.tag_review(id)?;
        if !matches!(proposal.status.as_str(), "pending" | "deferred") {
            bail!("only pending or deferred reviews may be edited");
        }
        self.validate_tag_review_snapshot(&proposal, false)?;
        validate_tags(&proposal.before, &after)?;
        if after.len() == proposal.after.len()
            && after.iter().all(|tag| proposal.after.contains(tag))
        {
            return Ok(Some(proposal));
        }
        let next = self.propose_tag_review_checked(
            &proposal.trace_id,
            after,
            &proposal.rationale,
            "human-edited",
            Some((&proposal.before, &proposal.snapshot)),
        )?;
        self.set_tag_review_status(id, "rejected")?;
        Ok(next)
    }

    fn validate_tag_review_snapshot(
        &self,
        proposal: &TagReview,
        undo: bool,
    ) -> Result<(Row, Trace)> {
        let (row, trace) = self.get_trace(&proposal.trace_id)?;
        if !row.trashed_at.is_empty() || (row.source_locked && row.origin != self.name) {
            bail!("trace is trashed or source-locked");
        }
        let expected = if undo {
            &proposal.after
        } else {
            &proposal.before
        };
        if &trace.frontmatter.tags != expected || snapshot(&row, &trace)? != proposal.snapshot {
            bail!("stale review: trace changed; analyze again or edit tags explicitly");
        }
        Ok((row, trace))
    }

    /// The queue transition and normal tag_update event commit together. Strong
    /// durability also recovers the file on process death using the usual journal.
    pub fn apply_tag_review(&self, id: &str, undo: bool) -> Result<()> {
        // Serialize review actions, including those from other local clients.
        let lock = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(self.db_dir.join("tag-review.lock"))?;
        lock.try_lock_exclusive()
            .context("another Tag Review action is running")?;
        let proposal = self.tag_review(id)?;
        let allowed = if undo {
            proposal.status == "applied"
        } else {
            matches!(proposal.status.as_str(), "pending" | "deferred")
        };
        if !allowed {
            bail!("review cannot be applied/undone in its current state");
        }
        validate_tags(&proposal.before, &proposal.after)?;
        let path = self.file_path(&self.get(&proposal.trace_id)?);
        let _trace_lock = self.acquire_trace_mutation_lock(&path)?;
        let (row, mut trace) = self.validate_tag_review_snapshot(&proposal, undo)?;
        trace.frontmatter.tags = if undo {
            proposal.before.clone()
        } else {
            proposal.after.clone()
        };
        trace.validate()?;
        let database = |pending_key: &str| {
            let tx = self.connection.unchecked_transaction()?;
            let changed = tx.execute(
                "UPDATE tag_reviews SET status=?2,updated_at=?3 WHERE id=?1 AND status=?4",
                params![
                    id,
                    if undo { "reverted" } else { "applied" },
                    trace::now_rfc3339(),
                    proposal.status
                ],
            )?;
            if changed != 1 {
                bail!("review changed concurrently; refresh the queue");
            }
            replace_tags(&tx, &row.id, &trace.frontmatter.tags)?;
            upsert_fts(
                &tx,
                &row.id,
                &row.title,
                &trace.body,
                &trace.frontmatter.tags,
            )?;
            self.emit_event(
                &tx,
                "tag_update",
                &row.id,
                &trace::now_rfc3339(),
                json!({"tags": trace.frontmatter.tags, "review_id": id, "undo": undo}),
            )?;
            Self::clear_pending_in_transaction(&tx, pending_key)?;
            tx.commit()?;
            Ok(())
        };
        // Always journal reviewed changes, even with standard durability: queue
        // state and trace file must agree after an interrupted approval.
        self.replace_trace_under_lock(&path, &trace, database)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn setup(tags: &[&str]) -> (tempfile::TempDir, Cortex, String) {
        let dir = tempfile::tempdir().unwrap();
        Cortex::create("test", dir.path()).unwrap();
        let cx = Cortex::open("test", dir.path().join("test")).unwrap();
        let mut t = Trace::new(
            "Tag review fixture",
            "note",
            "",
            tags.iter().map(|s| (*s).into()).collect(),
            "Rust concurrency notes",
        );
        cx.add(&mut t).unwrap();
        (dir, cx, t.frontmatter.id)
    }

    #[test]
    fn hygiene_persists_rejection_and_never_changes_tags() {
        let (_dir, cx, id) = setup(&["Rust Notes"]);
        assert_eq!(cx.review_tag_hygiene().unwrap(), 1);
        assert_eq!(
            cx.get_trace(&id).unwrap().1.frontmatter.tags,
            ["Rust Notes"]
        );
        let p = cx.tag_reviews().unwrap().remove(0);
        assert_eq!(p.after, ["rust-notes"]);
        cx.set_tag_review_status(&p.id, "rejected").unwrap();
        assert_eq!(cx.review_tag_hygiene().unwrap(), 0);
        let reopened = Cortex::open("test", &cx.dir).unwrap();
        assert_eq!(reopened.tag_review(&p.id).unwrap().status, "rejected");
        assert!(reopened.apply_tag_review(&p.id, false).is_err());
    }

    #[test]
    fn apply_and_undo_commit_tags_queue_and_history() {
        let (_dir, cx, id) = setup(&["rust"]);
        let p = cx
            .propose_tag_review(
                &id,
                vec!["rust".into(), "concurrency".into()],
                "Body discusses concurrency",
                "fixture",
            )
            .unwrap()
            .unwrap();
        cx.set_tag_review_status(&p.id, "deferred").unwrap();
        cx.apply_tag_review(&p.id, false).unwrap();
        assert_eq!(cx.tag_review(&p.id).unwrap().status, "applied");
        assert_eq!(cx.get_trace(&id).unwrap().1.frontmatter.tags, p.after);
        let mut indexed = cx.get(&id).unwrap().tags;
        let mut expected = p.after.clone();
        indexed.sort();
        expected.sort();
        assert_eq!(indexed, expected);
        assert!(cx.apply_tag_review(&p.id, false).is_err());
        cx.apply_tag_review(&p.id, true).unwrap();
        assert_eq!(cx.tag_review(&p.id).unwrap().status, "reverted");
        assert_eq!(cx.get_trace(&id).unwrap().1.frontmatter.tags, p.before);
        let events: i64 = cx
            .connection
            .query_row(
                "SELECT COUNT(*) FROM events WHERE trace_id=?1 AND action='tag_update'",
                [&id],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(events, 2);
    }

    #[test]
    fn stale_body_tags_and_visibility_block_apply_and_undo() {
        let (_dir, cx, id) = setup(&["rust"]);
        let p = cx
            .propose_tag_review(&id, vec!["concurrency".into()], "Evidence", "fixture")
            .unwrap()
            .unwrap();
        cx.archive(&id).unwrap();
        assert!(cx.apply_tag_review(&p.id, false).is_err());
        cx.unarchive(&id).unwrap();
        // Visibility round trips do not change content. Use a new snapshot after
        // the archive/unarchive mutations if they update trace metadata.
        let p = cx
            .propose_tag_review(
                &id,
                vec!["rust".into(), "concurrency".into()],
                "Evidence",
                "fixture",
            )
            .unwrap()
            .unwrap();
        cx.apply_tag_review(&p.id, false).unwrap();
        cx.set_tags(&id, vec!["newer-work".into()], false).unwrap();
        assert!(cx.apply_tag_review(&p.id, true).is_err());
        assert_eq!(cx.get(&id).unwrap().tags, ["newer-work"]);
        let p = cx
            .propose_tag_review(&id, vec!["rust".into()], "Evidence", "fixture")
            .unwrap()
            .unwrap();
        cx.append(&id, "new facts", false).unwrap();
        assert!(cx.apply_tag_review(&p.id, false).is_err());
        assert_eq!(cx.tag_review(&p.id).unwrap().status, "pending");
    }

    #[test]
    fn protects_behavioral_tags_and_source_locks() {
        let (_dir, cx, id) = setup(&["rust", "user-preference", "to-future-agents"]);
        assert!(
            cx.propose_tag_review(&id, vec!["rust".into()], "Evidence", "fixture")
                .is_err()
        );
        let (_, mut t) = cx.get_trace(&id).unwrap();
        t.frontmatter.source_locked = true;
        cx.update_trace(&id, &mut t, false).unwrap();
        cx.connection
            .execute("UPDATE traces SET origin='peer-a' WHERE id=?1", [&id])
            .unwrap();
        assert!(
            cx.propose_tag_review(
                &id,
                vec![
                    "rust".into(),
                    "user-preference".into(),
                    "to-future-agents".into(),
                    "concurrency".into()
                ],
                "Evidence",
                "fixture"
            )
            .is_err()
        );
        assert!(validate_tags(&["User.Preference".into()], &["user-preference".into()]).is_err());
        assert!(validate_tags(&[], &["Bad Tag".into()]).is_err());
        assert!(validate_tags(&[], &["rust".into(), "rust".into()]).is_err());
    }

    #[test]
    fn edit_preserves_old_proposal_and_requires_approval() {
        let (_dir, cx, id) = setup(&["rust"]);
        let p = cx
            .propose_tag_review(&id, vec!["concurrency".into()], "Evidence", "fixture")
            .unwrap()
            .unwrap();
        let edited = cx
            .edit_tag_review(&p.id, vec!["rust".into(), "concurrency".into()])
            .unwrap()
            .unwrap();
        assert_eq!(cx.tag_review(&p.id).unwrap().status, "rejected");
        assert_eq!(edited.status, "pending");
        assert_eq!(cx.get(&id).unwrap().tags, ["rust"]);
        assert_eq!(cx.tag_reviews().unwrap().len(), 2);
    }

    #[test]
    fn reordering_edited_tags_preserves_the_actionable_proposal() {
        let (_dir, cx, id) = setup(&["rust"]);
        let proposal = cx
            .propose_tag_review(
                &id,
                vec!["rust".into(), "concurrency".into()],
                "Evidence",
                "fixture",
            )
            .unwrap()
            .unwrap();
        cx.set_tag_review_status(&proposal.id, "deferred").unwrap();
        let edited = cx
            .edit_tag_review(&proposal.id, vec!["concurrency".into(), "rust".into()])
            .unwrap()
            .unwrap();
        assert_eq!(edited.id, proposal.id);
        assert_eq!(cx.tag_review(&proposal.id).unwrap().status, "deferred");
        cx.apply_tag_review(&edited.id, false).unwrap();
        let mut tags = cx.get(&id).unwrap().tags;
        tags.sort();
        assert_eq!(tags, ["concurrency", "rust"]);
    }

    #[test]
    fn purge_between_evidence_read_and_insert_cannot_restore_review_excerpts() {
        use std::{cell::RefCell, sync::mpsc, thread, time::Duration};
        thread_local! {
            static INSERT_BLOCKED: RefCell<Option<mpsc::Sender<()>>> = const { RefCell::new(None) };
        }
        for tombstone in [false, true] {
            let (_dir, cx, id) = setup(&["rust"]);
            let reader = Cortex::open("test", &cx.dir).unwrap();
            let (blocked, wait_blocked) = mpsc::channel();
            cx.connection.execute_batch("BEGIN IMMEDIATE").unwrap();
            let reader_id = id.clone();
            let worker = thread::spawn(move || {
                INSERT_BLOCKED.with(|signal| *signal.borrow_mut() = Some(blocked));
                reader
                    .connection
                    .busy_handler(Some(|_| {
                        INSERT_BLOCKED.with(|signal| {
                            if let Some(sender) = signal.borrow_mut().take() {
                                let _ = sender.send(());
                            }
                        });
                        thread::sleep(Duration::from_millis(1));
                        true
                    }))
                    .unwrap();
                reader.propose_tag_review(
                    &reader_id,
                    vec!["concurrency".into()],
                    "Evidence",
                    "fixture",
                )
            });
            // The public API has read its evidence and is waiting to insert.
            wait_blocked.recv_timeout(Duration::from_secs(5)).unwrap();
            if tombstone {
                cx.connection
                    .execute(
                        "UPDATE traces SET purged_at=?2 WHERE id=?1",
                        params![id, trace::now_rfc3339()],
                    )
                    .unwrap();
            } else {
                cx.connection
                    .execute("DELETE FROM traces WHERE id=?1", [&id])
                    .unwrap();
            }
            cx.connection.execute_batch("COMMIT").unwrap();
            assert!(worker.join().unwrap().unwrap().is_none());
            assert!(cx.tag_reviews().unwrap().is_empty());
        }
    }

    #[test]
    fn purging_a_trace_removes_its_review_excerpts() {
        for tombstone in [false, true] {
            let (_dir, cx, id) = setup(&["rust"]);
            cx.propose_tag_review(&id, vec!["concurrency".into()], "Evidence", "fixture")
                .unwrap();
            if tombstone {
                cx.connection
                    .execute(
                        "UPDATE traces SET purged_at=?2 WHERE id=?1",
                        params![id, trace::now_rfc3339()],
                    )
                    .unwrap();
            } else {
                cx.remove_hard(&id).unwrap();
            }
            assert!(cx.tag_reviews().unwrap().is_empty());
        }
    }

    #[test]
    fn proposal_evidence_is_not_rebased_onto_later_edits() {
        let (_dir, cx, id) = setup(&["rust"]);
        let (row, old) = cx.get_trace(&id).unwrap();
        let evidence = snapshot(&row, &old).unwrap();
        cx.append(&id, "Different evidence", false).unwrap();
        assert!(
            cx.propose_tag_review_checked(
                &id,
                vec!["concurrency".into()],
                "Old evidence",
                "fixture",
                Some((&old.frontmatter.tags, &evidence))
            )
            .is_err()
        );
        assert!(cx.tag_reviews().unwrap().is_empty());
    }
}
