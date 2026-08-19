use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
};

use serde::Serialize;

use crate::cortex::Row;

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum TagIssue {
    Whitespace,
    Period,
    Uppercase,
    NumericOnly,
    RepeatedHyphen,
    EdgeHyphen,
}

impl fmt::Display for TagIssue {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::Whitespace => "whitespace",
            Self::Period => "period",
            Self::Uppercase => "uppercase",
            Self::NumericOnly => "numeric_only",
            Self::RepeatedHyphen => "repeated_hyphen",
            Self::EdgeHyphen => "edge_hyphen",
        })
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct TagDiagnostic {
    pub tag: String,
    pub trace_count: usize,
    pub issues: Vec<TagIssue>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub suggestion: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct TagCollision {
    pub destination: String,
    pub sources: Vec<String>,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct TagDoctorReport {
    pub checked_tags: usize,
    pub findings: Vec<TagDiagnostic>,
    pub collisions: Vec<TagCollision>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct TagChange {
    pub id: String,
    pub before: Vec<String>,
    pub after: Vec<String>,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct TagMutationPlan {
    pub matched_spellings: BTreeMap<String, usize>,
    pub changes: Vec<TagChange>,
    pub blocked_source_locked: Vec<String>,
}

impl TagMutationPlan {
    pub fn matched_assignments(&self) -> usize {
        self.matched_spellings.values().sum()
    }
}

pub fn inspect(tag: &str) -> Vec<TagIssue> {
    let mut issues = Vec::new();
    if tag.chars().any(char::is_whitespace) {
        issues.push(TagIssue::Whitespace);
    }
    if tag.contains('.') {
        issues.push(TagIssue::Period);
    }
    if tag.chars().any(|character| character.is_ascii_uppercase()) {
        issues.push(TagIssue::Uppercase);
    }
    if !tag.is_empty()
        && tag
            .chars()
            .all(|character| character.is_ascii_digit() || matches!(character, '-' | '_'))
    {
        issues.push(TagIssue::NumericOnly);
    }
    if tag.contains("--") {
        issues.push(TagIssue::RepeatedHyphen);
    }
    if tag.starts_with('-') || tag.ends_with('-') {
        issues.push(TagIssue::EdgeHyphen);
    }
    issues
}

pub fn suggested_fix(tag: &str) -> Option<String> {
    let issues = inspect(tag);
    if issues.is_empty() || issues.contains(&TagIssue::NumericOnly) {
        return None;
    }

    let mut output = String::new();
    let mut pending_hyphen = false;
    for character in tag.trim().chars() {
        if character.is_whitespace() || matches!(character, '.' | '-') {
            if !output.is_empty() {
                pending_hyphen = true;
            }
            continue;
        }
        if pending_hyphen {
            output.push('-');
            pending_hyphen = false;
        }
        output.push(character.to_ascii_lowercase());
    }
    while output.ends_with('-') {
        output.pop();
    }
    (!output.is_empty() && output != tag).then_some(output)
}

pub fn doctor(rows: &[Row]) -> TagDoctorReport {
    let mut counts = BTreeMap::<String, usize>::new();
    for row in rows {
        for tag in &row.tags {
            *counts.entry(tag.clone()).or_default() += 1;
        }
    }

    let mut findings = Vec::new();
    let mut destinations = BTreeMap::<String, BTreeSet<String>>::new();
    for (tag, trace_count) in &counts {
        let issues = inspect(tag);
        if issues.is_empty() {
            continue;
        }
        let suggestion = suggested_fix(tag);
        if let Some(destination) = &suggestion {
            destinations
                .entry(destination.clone())
                .or_default()
                .insert(tag.clone());
            if counts.contains_key(destination) {
                destinations
                    .entry(destination.clone())
                    .or_default()
                    .insert(destination.clone());
            }
        }
        findings.push(TagDiagnostic {
            tag: tag.clone(),
            trace_count: *trace_count,
            issues,
            suggestion,
        });
    }

    let collisions = destinations
        .into_iter()
        .filter_map(|(destination, sources)| {
            (sources.len() > 1).then(|| TagCollision {
                destination,
                sources: sources.into_iter().collect(),
            })
        })
        .collect();

    TagDoctorReport {
        checked_tags: counts.len(),
        findings,
        collisions,
    }
}

pub fn rename_plan(
    rows: &[Row],
    cortex_name: &str,
    old: &str,
    new: &str,
    ignore_case: bool,
) -> TagMutationPlan {
    mapping_plan(rows, cortex_name, |tag| {
        matches_tag(tag, old, ignore_case).then(|| new.to_owned())
    })
}

pub fn delete_plan(
    rows: &[Row],
    cortex_name: &str,
    target: &str,
    ignore_case: bool,
) -> TagMutationPlan {
    mapping_plan(rows, cortex_name, |tag| {
        matches_tag(tag, target, ignore_case).then(String::new)
    })
}

pub fn doctor_fix_plan(
    rows: &[Row],
    cortex_name: &str,
    report: &TagDoctorReport,
) -> TagMutationPlan {
    let fixes: BTreeMap<_, _> = report
        .findings
        .iter()
        .filter_map(|finding| {
            finding
                .suggestion
                .as_ref()
                .map(|suggestion| (finding.tag.clone(), suggestion.clone()))
        })
        .collect();
    mapping_plan(rows, cortex_name, |tag| fixes.get(tag).cloned())
}

fn mapping_plan<F>(rows: &[Row], cortex_name: &str, mut replacement: F) -> TagMutationPlan
where
    F: FnMut(&str) -> Option<String>,
{
    let mut plan = TagMutationPlan::default();
    for row in rows {
        let mut matched = false;
        let mut after = Vec::new();
        let mut seen = BTreeSet::new();
        for tag in &row.tags {
            let next = match replacement(tag) {
                Some(value) => {
                    matched = true;
                    *plan.matched_spellings.entry(tag.clone()).or_default() += 1;
                    value
                }
                None => tag.clone(),
            };
            if !next.is_empty() && seen.insert(next.clone()) {
                after.push(next);
            }
        }
        if !matched || after == row.tags {
            continue;
        }
        if row.source_locked && row.origin != cortex_name {
            plan.blocked_source_locked.push(row.id.clone());
            continue;
        }
        plan.changes.push(TagChange {
            id: row.id.clone(),
            before: row.tags.clone(),
            after,
        });
    }
    plan
}

fn matches_tag(candidate: &str, target: &str, ignore_case: bool) -> bool {
    if ignore_case {
        candidate.eq_ignore_ascii_case(target)
    } else {
        candidate == target
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn row(id: &str, tags: &[&str]) -> Row {
        Row {
            id: id.into(),
            tags: tags.iter().map(|tag| (*tag).to_owned()).collect(),
            origin: "local".into(),
            ..Default::default()
        }
    }

    #[test]
    fn doctor_reports_only_deterministic_fixes() {
        let rows = vec![row(
            "one",
            &["Release.Candidate", "2026", "already-valid", "two  words"],
        )];
        let report = doctor(&rows);
        assert_eq!(report.checked_tags, 4);
        assert_eq!(report.findings.len(), 3);
        assert_eq!(
            report
                .findings
                .iter()
                .find(|finding| finding.tag == "Release.Candidate")
                .unwrap()
                .suggestion,
            Some("release-candidate".into())
        );
        assert_eq!(
            report
                .findings
                .iter()
                .find(|finding| finding.tag == "2026")
                .unwrap()
                .suggestion,
            None
        );
        assert_eq!(
            report
                .findings
                .iter()
                .find(|finding| finding.tag == "two  words")
                .unwrap()
                .suggestion,
            Some("two-words".into())
        );
    }

    #[test]
    fn ignore_case_rename_converges_variants_and_deduplicates() {
        let rows = vec![row("one", &["AI", "ai", "other"]), row("two", &["Ai"])];
        let plan = rename_plan(&rows, "local", "ai", "artificial-intelligence", true);
        assert_eq!(plan.changes.len(), 2);
        assert_eq!(plan.matched_spellings.len(), 3);
        assert_eq!(
            plan.changes[0].after,
            vec!["artificial-intelligence", "other"]
        );
    }

    #[test]
    fn exact_delete_leaves_case_variants_untouched() {
        let rows = vec![row("one", &["legacy", "Legacy", "current"])];
        let plan = delete_plan(&rows, "local", "legacy", false);
        assert_eq!(plan.changes[0].after, vec!["Legacy", "current"]);
        assert_eq!(plan.matched_spellings.len(), 1);
    }

    #[test]
    fn source_locked_foreign_rows_are_preflight_blocked() {
        let mut locked = row("one", &["old"]);
        locked.source_locked = true;
        locked.origin = "publisher".into();
        let plan = rename_plan(&[locked], "subscriber", "old", "new", false);
        assert!(plan.changes.is_empty());
        assert_eq!(plan.blocked_source_locked, ["one"]);
    }
}
