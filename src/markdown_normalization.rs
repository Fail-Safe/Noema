use std::collections::HashSet;

use anyhow::{Result, bail};
use serde::{Deserialize, Serialize};

use crate::trace;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct MarkdownNormalizationPlan {
    pub schema_version: u32,
    pub cortex_id: String,
    pub traces: Vec<MarkdownTracePlan>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct MarkdownTracePlan {
    pub trace_id: String,
    pub relative_path: String,
    pub tier: String,
    pub expected_content_hash: String,
    pub expected_result_hash: String,
    pub operations: Vec<MarkdownOperation>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(tag = "strategy", rename_all = "kebab-case")]
pub enum MarkdownOperation {
    InlineCode {
        line: usize,
        column: usize,
        literal: String,
    },
    FencedBlock {
        start_line: usize,
        end_line: usize,
        language: String,
    },
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct Insertion {
    offset: usize,
    text: String,
    sequence: usize,
}

pub fn apply_operations(body: &str, operations: &[MarkdownOperation]) -> Result<String> {
    if operations.is_empty() {
        bail!("markdown normalization plan has no operations");
    }
    let line_ranges = line_ranges(body);
    let mut insertions = Vec::new();
    let mut fenced_lines = HashSet::new();

    for (sequence, operation) in operations.iter().enumerate() {
        match operation {
            MarkdownOperation::InlineCode {
                line,
                column,
                literal,
            } => {
                validate_hex_literal(literal)?;
                let (start, content_end, _) = line_range(&line_ranges, *line)?;
                let source = &body[start..content_end];
                let column_offset = character_offset(source, *column)?;
                let literal_start = start + column_offset;
                let literal_end = literal_start
                    .checked_add(literal.len())
                    .ok_or_else(|| anyhow::anyhow!("inline-code literal range overflow"))?;
                if literal_end > content_end || &body[literal_start..literal_end] != literal {
                    bail!(
                        "inline-code literal mismatch at line {line}, column {column}: expected {literal:?}"
                    );
                }
                insertions.push(Insertion {
                    offset: literal_start,
                    text: "`".into(),
                    sequence: sequence * 2,
                });
                insertions.push(Insertion {
                    offset: literal_end,
                    text: "`".into(),
                    sequence: sequence * 2 + 1,
                });
            }
            MarkdownOperation::FencedBlock {
                start_line,
                end_line,
                language,
            } => {
                if !matches!(language.as_str(), "ini" | "yaml") {
                    bail!("unsupported fenced-block language {language:?}");
                }
                if start_line > end_line {
                    bail!("fenced-block start line must not follow its end line");
                }
                for line in *start_line..=*end_line {
                    if !fenced_lines.insert(line) {
                        bail!("overlapping fenced-block operation at line {line}");
                    }
                }
                let (start, _, _) = line_range(&line_ranges, *start_line)?;
                let (_, content_end, line_end) = line_range(&line_ranges, *end_line)?;
                let previous_nonblank = *start_line > 1
                    && line_range(&line_ranges, start_line - 1).map(
                        |(line_start, previous_content_end, _)| {
                            !body[line_start..previous_content_end].trim().is_empty()
                        },
                    )?;
                let next_nonblank = *end_line < line_ranges.len()
                    && line_range(&line_ranges, end_line + 1).map(
                        |(next_start, next_content_end, _)| {
                            !body[next_start..next_content_end].trim().is_empty()
                        },
                    )?;
                let opener = if previous_nonblank {
                    format!("\n```{language}\n")
                } else {
                    format!("```{language}\n")
                };
                let mut closer = String::new();
                if line_end == content_end {
                    closer.push('\n');
                }
                closer.push_str("```\n");
                if next_nonblank {
                    closer.push('\n');
                }
                insertions.push(Insertion {
                    offset: start,
                    text: opener,
                    sequence: sequence * 2,
                });
                insertions.push(Insertion {
                    offset: line_end,
                    text: closer,
                    sequence: sequence * 2 + 1,
                });
            }
        }
    }

    insertions.sort_by_key(|insertion| (insertion.offset, insertion.sequence));
    for pair in insertions.windows(2) {
        if pair[0].offset == pair[1].offset {
            bail!("markdown normalization operations share an insertion boundary");
        }
    }

    let mut normalized = body.to_owned();
    for insertion in insertions.iter().rev() {
        normalized.insert_str(insertion.offset, &insertion.text);
    }
    if normalized == body {
        bail!("markdown normalization produced no change");
    }

    let mut reconstructed = normalized.clone();
    let mut shifted = Vec::with_capacity(insertions.len());
    let mut added = 0;
    for insertion in &insertions {
        shifted.push((insertion.offset + added, insertion.text.as_str()));
        added += insertion.text.len();
    }
    for (offset, text) in shifted.into_iter().rev() {
        if reconstructed.get(offset..offset + text.len()) != Some(text) {
            bail!("markdown normalization reversibility check failed");
        }
        reconstructed.replace_range(offset..offset + text.len(), "");
    }
    if reconstructed != body {
        bail!("markdown normalization did not preserve the original body");
    }
    Ok(normalized)
}

pub fn validate_trace_plan(plan: &MarkdownTracePlan, body: &str) -> Result<String> {
    if plan.expected_content_hash != trace::content_hash(body) {
        bail!(
            "trace {:?} content hash changed after planning",
            plan.trace_id
        );
    }
    let normalized = apply_operations(body, &plan.operations)?;
    let result_hash = trace::content_hash(&normalized);
    if result_hash != plan.expected_result_hash {
        bail!(
            "trace {:?} result hash does not match the planned result",
            plan.trace_id
        );
    }
    Ok(normalized)
}

fn validate_hex_literal(literal: &str) -> Result<()> {
    let Some(hex) = literal.strip_prefix('#') else {
        bail!("inline-code literal must be a hexadecimal color");
    };
    if !matches!(hex.len(), 3 | 4 | 6 | 8) || !hex.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        bail!("inline-code literal must be a hexadecimal color");
    }
    Ok(())
}

fn character_offset(line: &str, column: usize) -> Result<usize> {
    if column == 0 {
        bail!("inline-code column numbers are one-based");
    }
    line.char_indices()
        .nth(column - 1)
        .map(|(offset, _)| offset)
        .or_else(|| (column == line.chars().count() + 1).then_some(line.len()))
        .ok_or_else(|| anyhow::anyhow!("inline-code column is outside its line"))
}

fn line_ranges(body: &str) -> Vec<(usize, usize, usize)> {
    let mut ranges = Vec::new();
    let mut start = 0;
    for segment in body.split_inclusive('\n') {
        let end = start + segment.len();
        let content_end = segment.strip_suffix('\n').map_or(end, |_| end - 1);
        ranges.push((start, content_end, end));
        start = end;
    }
    if body.is_empty() || body.ends_with('\n') {
        ranges.push((start, start, start));
    }
    ranges
}

fn line_range(ranges: &[(usize, usize, usize)], line: usize) -> Result<(usize, usize, usize)> {
    if line == 0 {
        bail!("markdown normalization line numbers are one-based");
    }
    ranges
        .get(line - 1)
        .copied()
        .ok_or_else(|| anyhow::anyhow!("markdown normalization line {line} is outside the body"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn applies_only_reversible_delimiter_insertions() {
        let body = "Intro\nColor #abcdef and #123456.\nConfig follows\nkey = value\n#disabled = true\nDone\n";
        let operations = vec![
            MarkdownOperation::InlineCode {
                line: 2,
                column: 7,
                literal: "#abcdef".into(),
            },
            MarkdownOperation::InlineCode {
                line: 2,
                column: 19,
                literal: "#123456".into(),
            },
            MarkdownOperation::FencedBlock {
                start_line: 4,
                end_line: 5,
                language: "ini".into(),
            },
        ];

        let normalized = apply_operations(body, &operations).unwrap();

        assert_eq!(
            normalized,
            "Intro\nColor `#abcdef` and `#123456`.\nConfig follows\n\n```ini\nkey = value\n#disabled = true\n```\n\nDone\n"
        );
    }

    #[test]
    fn rejects_arbitrary_inline_text_and_stale_locations() {
        let body = "Color #abcdef\n";
        let arbitrary = [MarkdownOperation::InlineCode {
            line: 1,
            column: 7,
            literal: "#not-a-color".into(),
        }];
        assert!(apply_operations(body, &arbitrary).is_err());

        let stale = [MarkdownOperation::InlineCode {
            line: 1,
            column: 8,
            literal: "#abcdef".into(),
        }];
        assert!(apply_operations(body, &stale).is_err());
    }
}
