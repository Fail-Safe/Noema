#!/usr/bin/env python3
"""Report body text that Obsidian may mistake for inline tags.

This tool is intentionally read-only. It scans active and archived Noema trace
files, skips Markdown code and link destinations, and reports hashtag-shaped
tokens that remain visible to Obsidian's metadata index.
"""

from __future__ import annotations

import argparse
import difflib
import hashlib
import json
import re
import subprocess
import sys
import tempfile
from collections import Counter
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Iterable


FENCE_RE = re.compile(r"^ {0,3}(`{3,}|~{3,})")
CLOSING_FENCE_RE = re.compile(r"^ {0,3}(`{3,}|~{3,})\s*$")
TAG_RE = re.compile(r"(?<![\\\w])#(?P<tag>[\w-]+(?:/[\w-]+)*)")
HEX_COLOR_RE = re.compile(r"(?:[0-9A-Fa-f]{3}|[0-9A-Fa-f]{4}|[0-9A-Fa-f]{6}|[0-9A-Fa-f]{8})")
HEX_LITERAL_RE = re.compile(
    r"(?<![\\\w])#(?:[0-9A-Fa-f]{8}|[0-9A-Fa-f]{6}|[0-9A-Fa-f]{4}|[0-9A-Fa-f]{3})(?![0-9A-Fa-f])"
)
FRONTMATTER_END = "\n---\n"


@dataclass(frozen=True)
class Candidate:
    path: str
    area: str
    trace_id: str
    tier: str
    line: int
    column: int
    token: str
    classification: str
    confidence: str


@dataclass(frozen=True)
class ScanError:
    path: str
    error: str


@dataclass
class Report:
    cortex_dir: str
    scanned_active: int
    scanned_archived: int
    scanned_trash: int
    candidates: list[Candidate]
    errors: list[ScanError]


@dataclass(frozen=True)
class PlanAction:
    strategy: str
    start_line: int
    end_line: int
    candidate_count: int
    additional_edit_count: int
    description: str


@dataclass(frozen=True)
class TracePlan:
    path: str
    tier: str
    actions: list[PlanAction]
    operations: list[dict[str, object]]
    expected_content_hash: str
    expected_result_hash: str
    proposed_source: str
    diff: str


def active_cortex_dir() -> Path:
    try:
        result = subprocess.run(
            ["noema", "cortex", "list"],
            check=True,
            capture_output=True,
            text=True,
        )
    except FileNotFoundError as error:
        raise RuntimeError("noema is not installed; pass --cortex-dir explicitly") from error
    except subprocess.CalledProcessError as error:
        detail = error.stderr.strip() or error.stdout.strip() or f"exit {error.returncode}"
        raise RuntimeError(f"noema cortex list failed: {detail}") from error

    for line in result.stdout.splitlines():
        if not line.rstrip().endswith("*"):
            continue
        fields = line.split("\t", 1)
        if len(fields) != 2:
            continue
        path = fields[1].rstrip()
        path = path[:-1].rstrip()
        if path:
            return Path(path).expanduser()
    raise RuntimeError("no active cortex found; pass --cortex-dir explicitly")


def split_frontmatter(source: str) -> tuple[str, str, int]:
    if not source.startswith("---\n"):
        raise ValueError("missing framed YAML frontmatter")
    end = source.find(FRONTMATTER_END, 4)
    if end < 0:
        raise ValueError("unterminated framed YAML frontmatter")
    frontmatter = source[4:end]
    body_start = end + len(FRONTMATTER_END)
    line_offset = source[:body_start].count("\n")
    body = source[body_start:]
    if body.startswith("\n"):
        body = body[1:]
        line_offset += 1
    return frontmatter, body, line_offset


def frontmatter_scalar(frontmatter: str, key: str) -> str:
    match = re.search(rf"(?m)^{re.escape(key)}:\s*(.*?)\s*$", frontmatter)
    if not match:
        return ""
    value = match.group(1)
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {'"', "'"}:
        value = value[1:-1]
    return value


def mask_inline_code(line: str) -> str:
    masked = list(line)
    index = 0
    while index < len(line):
        if line[index] != "`":
            index += 1
            continue
        run_end = index
        while run_end < len(line) and line[run_end] == "`":
            run_end += 1
        delimiter = line[index:run_end]
        close = line.find(delimiter, run_end)
        if close < 0:
            index = run_end
            continue
        for masked_index in range(index, close + len(delimiter)):
            masked[masked_index] = " "
        index = close + len(delimiter)
    return "".join(masked)


def is_link_fragment(line: str, start: int) -> bool:
    prefix = line[:start]
    if prefix.endswith("](") or prefix.endswith('href="') or prefix.endswith("href='"):
        return True
    segment = re.split(r"\s", prefix)[-1]
    return "://" in segment or segment.startswith("www.")


def classify(line: str, start: int, end: int, tag: str) -> tuple[str, str]:
    if HEX_COLOR_RE.fullmatch(tag):
        return "hex-color", "high"
    before = line[:start]
    after = line[end:]
    if not before.strip() and re.match(r"\s*(?:=|:)", after):
        return "config-comment", "high"
    return "inline-hashtag", "review"


def body_candidates(
    body: str,
    *,
    relative_path: str,
    area: str,
    trace_id: str,
    tier: str,
    line_offset: int,
) -> list[Candidate]:
    candidates: list[Candidate] = []
    fence_character = ""
    fence_length = 0

    for body_line, line in enumerate(body.splitlines(), start=1):
        if fence_character:
            fence = CLOSING_FENCE_RE.match(line)
            if fence:
                delimiter = fence.group(1)
                if delimiter[0] == fence_character and len(delimiter) >= fence_length:
                    fence_character = ""
                    fence_length = 0
            continue

        fence = FENCE_RE.match(line)
        if fence:
            delimiter = fence.group(1)
            fence_character = delimiter[0]
            fence_length = len(delimiter)
            continue
        if line.startswith("    ") or line.startswith("\t"):
            continue
        visible = mask_inline_code(line)
        for match in TAG_RE.finditer(visible):
            if is_link_fragment(line, match.start()):
                continue
            tag = match.group("tag")
            if not any(character.isalpha() for character in tag):
                continue
            classification, confidence = classify(
                line, match.start(), match.end(), tag
            )
            candidates.append(
                Candidate(
                    path=relative_path,
                    area=area,
                    trace_id=trace_id,
                    tier=tier or "unknown",
                    line=line_offset + body_line,
                    column=match.start() + 1,
                    token=f"#{tag}",
                    classification=classification,
                    confidence=confidence,
                )
            )
    return candidates


def trace_files(cortex_dir: Path, include_trash: bool) -> Iterable[tuple[str, Path]]:
    roots = [("active", cortex_dir / "traces"), ("archived", cortex_dir / "archive/traces")]
    if include_trash:
        roots.append(("trash", cortex_dir / "trash/traces"))
    for area, root in roots:
        if not root.is_dir():
            continue
        for path in sorted(root.rglob("*.md")):
            if path.is_file():
                yield area, path


def scan_cortex(cortex_dir: Path, include_trash: bool = False) -> Report:
    cortex_dir = cortex_dir.expanduser().resolve()
    candidates: list[Candidate] = []
    errors: list[ScanError] = []
    counts: Counter[str] = Counter()

    for area, path in trace_files(cortex_dir, include_trash):
        counts[area] += 1
        relative_path = path.relative_to(cortex_dir).as_posix()
        try:
            source = path.read_text(encoding="utf-8")
            frontmatter, body, line_offset = split_frontmatter(source)
        except (OSError, UnicodeError, ValueError) as error:
            errors.append(ScanError(relative_path, str(error)))
            continue
        trace_id = frontmatter_scalar(frontmatter, "id") or path.stem
        tier = frontmatter_scalar(frontmatter, "tier") or "short"
        candidates.extend(
            body_candidates(
                body,
                relative_path=relative_path,
                area=area,
                trace_id=trace_id,
                tier=tier,
                line_offset=line_offset,
            )
        )

    candidates.sort(key=lambda item: (item.path, item.line, item.column))
    errors.sort(key=lambda item: item.path)
    return Report(
        cortex_dir=str(cortex_dir),
        scanned_active=counts["active"],
        scanned_archived=counts["archived"],
        scanned_trash=counts["trash"],
        candidates=candidates,
        errors=errors,
    )


def config_like_line(line: str) -> bool:
    stripped = line.strip()
    return (
        not stripped
        or stripped.startswith("#")
        or re.match(r"[A-Za-z][A-Za-z0-9_-]*\s*=", stripped) is not None
    )


def dense_config_span(
    lines: list[str], body_start: int, candidates: list[Candidate]
) -> tuple[int, int] | None:
    start = body_start
    while start < len(lines) and not lines[start].strip():
        start += 1
    if start >= len(lines) or not config_like_line(lines[start]):
        return None
    end = start
    while end + 1 < len(lines) and config_like_line(lines[end + 1]):
        end += 1
    count = sum(start <= item.line - 1 <= end for item in candidates)
    if count < 5:
        return None
    return start, end


def comment_block_span(lines: list[str], line_index: int) -> tuple[int, int]:
    start = line_index
    while start > 0 and lines[start - 1].lstrip().startswith("#"):
        start -= 1

    end = line_index
    while end + 1 < len(lines):
        if lines[end + 1].lstrip().startswith("#"):
            end += 1
            continue
        if not lines[end + 1].strip():
            lookahead = end + 1
            while lookahead < len(lines) and not lines[lookahead].strip():
                lookahead += 1
            if lookahead < len(lines) and lines[lookahead].lstrip().startswith("#"):
                end = lookahead
                continue
        break
    return start, end


def transcript_user_span(lines: list[str], line_index: int) -> tuple[int, int] | None:
    user_line = None
    for index in range(line_index - 1, max(-1, line_index - 250), -1):
        stripped = lines[index].lstrip()
        if stripped.startswith("**Assistant:**"):
            break
        if stripped.startswith("**User:**"):
            user_line = index
            break
    if user_line is None:
        return None

    assistant_line = None
    for index in range(line_index + 1, min(len(lines), line_index + 250)):
        if lines[index].lstrip().startswith("**Assistant:**"):
            assistant_line = index
            break
    if assistant_line is None:
        return None

    start = user_line + 1
    while start < assistant_line and not lines[start].strip():
        start += 1
    end = assistant_line - 1
    while end >= start and not lines[end].strip():
        end -= 1
    if start <= line_index <= end:
        return start, end
    return None


def merge_spans(spans: list[tuple[int, int]]) -> list[tuple[int, int]]:
    merged: list[tuple[int, int]] = []
    for start, end in sorted(spans):
        if merged and start <= merged[-1][1] + 1:
            merged[-1] = (merged[-1][0], max(merged[-1][1], end))
        else:
            merged.append((start, end))
    return merged


def proposed_trace_plan(cortex_dir: Path, candidates: list[Candidate]) -> TracePlan:
    relative_path = candidates[0].path
    path = cortex_dir / relative_path
    source = path.read_text(encoding="utf-8")
    _, _, line_offset = split_frontmatter(source)
    lines = source.splitlines(keepends=True)
    fence_spans: list[tuple[int, int, str, str]] = []

    dense = dense_config_span(lines, line_offset, candidates)
    if dense:
        fence_spans.append(
            (
                dense[0],
                dense[1],
                "ini",
                "Fence the dense raw configuration block.",
            )
        )

    covered = {
        index
        for start, end, _, _ in fence_spans
        for index in range(start, end + 1)
    }
    comment_lines = [
        item.line - 1
        for item in candidates
        if item.classification == "config-comment" and item.line - 1 not in covered
    ]
    comment_spans = [
        transcript_user_span(lines, line_index)
        or comment_block_span(lines, line_index)
        for line_index in comment_lines
    ]
    for start, end in merge_spans(comment_spans):
        fence_spans.append(
            (
                start,
                end,
                "yaml",
                "Fence the pasted YAML region around the findings.",
            )
        )
        covered.update(range(start, end + 1))

    remaining = [item for item in candidates if item.line - 1 not in covered]
    ambiguous = [item for item in remaining if item.classification != "hex-color"]
    if ambiguous:
        first = ambiguous[0]
        raise RuntimeError(
            f"no deterministic repair for {first.path}:{first.line}:{first.column} {first.token}"
        )
    planned_edits = list(remaining)
    candidate_locations = {(item.line, item.column) for item in remaining}
    hex_lines = {item.line for item in remaining if item.classification == "hex-color"}
    for line_number in sorted(hex_lines):
        line = lines[line_number - 1]
        visible = mask_inline_code(line)
        for match in HEX_LITERAL_RE.finditer(visible):
            location = (line_number, match.start() + 1)
            if location in candidate_locations:
                continue
            planned_edits.append(
                Candidate(
                    path=relative_path,
                    area=candidates[0].area,
                    trace_id=candidates[0].trace_id,
                    tier=candidates[0].tier,
                    line=line_number,
                    column=match.start() + 1,
                    token=match.group(0),
                    classification="formatting-neighbor",
                    confidence="n/a",
                )
            )
    proposed_lines = list(lines)
    by_line: dict[int, list[Candidate]] = {}
    for item in planned_edits:
        by_line.setdefault(item.line - 1, []).append(item)
    for line_index, items in by_line.items():
        line = proposed_lines[line_index]
        for item in sorted(items, key=lambda value: value.column, reverse=True):
            start = item.column - 1
            end = start + len(item.token)
            if line[start:end] != item.token:
                raise RuntimeError(
                    f"candidate location changed for {relative_path}:{item.line}:{item.column}"
                )
            line = f"{line[:start]}`{item.token}`{line[end:]}"
        proposed_lines[line_index] = line

    actions: list[PlanAction] = []
    operations: list[dict[str, object]] = []
    fence_starts = {start: (end, language) for start, end, language, _ in fence_spans}
    fence_ends = {end: language for start, end, language, _ in fence_spans}
    with_fences: list[str] = []
    for line_index, line in enumerate(proposed_lines):
        if line_index in fence_starts:
            if line_index > 0 and proposed_lines[line_index - 1].strip():
                with_fences.append("\n")
            _, language = fence_starts[line_index]
            with_fences.append(f"```{language}\n")
        with_fences.append(line)
        if line_index in fence_ends:
            if not line.endswith("\n"):
                with_fences.append("\n")
            with_fences.append("```\n")
            if line_index + 1 < len(proposed_lines) and proposed_lines[line_index + 1].strip():
                with_fences.append("\n")

    for start, end, language, description in sorted(fence_spans):
        count = sum(start <= item.line - 1 <= end for item in candidates)
        actions.append(
            PlanAction(
                strategy="fenced-block",
                start_line=start + 1,
                end_line=end + 1,
                candidate_count=count,
                additional_edit_count=0,
                description=description,
            )
        )
        operations.append(
            {
                "strategy": "fenced-block",
                "start_line": start + 1 - line_offset,
                "end_line": end + 1 - line_offset,
                "language": language,
            }
        )
    if remaining:
        actions.append(
            PlanAction(
                strategy="inline-code",
                start_line=min(item.line for item in remaining),
                end_line=max(item.line for item in remaining),
                candidate_count=len(remaining),
                additional_edit_count=len(planned_edits) - len(remaining),
                description="Wrap isolated and neighboring hex literals in inline code.",
            )
        )
        for item in sorted(planned_edits, key=lambda value: (value.line, value.column)):
            operations.append(
                {
                    "strategy": "inline-code",
                    "line": item.line - line_offset,
                    "column": item.column,
                    "literal": item.token,
                }
            )

    proposed_source = "".join(with_fences)
    _, proposed_body, proposed_line_offset = split_frontmatter(proposed_source)
    residual = body_candidates(
        proposed_body,
        relative_path=relative_path,
        area=candidates[0].area,
        trace_id=candidates[0].trace_id,
        tier=candidates[0].tier,
        line_offset=proposed_line_offset,
    )
    if residual:
        first = residual[0]
        raise RuntimeError(
            f"proposed plan leaves {len(residual)} candidate(s); first is "
            f"{relative_path}:{first.line}:{first.column} {first.token}"
        )
    diff = "".join(
        difflib.unified_diff(
            source.splitlines(keepends=True),
            proposed_source.splitlines(keepends=True),
            fromfile=relative_path,
            tofile=f"{relative_path} (proposed)",
        )
    )
    return TracePlan(
        path=relative_path,
        tier=candidates[0].tier,
        actions=actions,
        operations=operations,
        expected_content_hash=content_hash(body=split_frontmatter(source)[1]),
        expected_result_hash=content_hash(body=proposed_body),
        proposed_source=proposed_source,
        diff=diff,
    )


def build_plans(report: Report) -> list[TracePlan]:
    cortex_dir = Path(report.cortex_dir)
    grouped: dict[str, list[Candidate]] = {}
    for item in report.candidates:
        grouped.setdefault(item.path, []).append(item)
    return [proposed_trace_plan(cortex_dir, grouped[path]) for path in sorted(grouped)]


def content_hash(*, body: str) -> str:
    return f"sha256:{hashlib.sha256(body.encode('utf-8')).hexdigest()}"


def normalization_payload(report: Report, plans: list[TracePlan] | None = None) -> dict:
    cortex_dir = Path(report.cortex_dir)
    manifest_source = (cortex_dir / "cortex.md").read_text(encoding="utf-8")
    manifest, _, _ = split_frontmatter(manifest_source)
    cortex_id = frontmatter_scalar(manifest, "id")
    if not cortex_id:
        raise RuntimeError("cortex manifest has no id")
    plans = plans or build_plans(report)
    candidates_by_path: dict[str, list[Candidate]] = {}
    for candidate in report.candidates:
        candidates_by_path.setdefault(candidate.path, []).append(candidate)
    return {
        "schema_version": 1,
        "cortex_id": cortex_id,
        "traces": [
            {
                "trace_id": candidates_by_path[plan.path][0].trace_id,
                "relative_path": plan.path,
                "tier": plan.tier,
                "expected_content_hash": plan.expected_content_hash,
                "expected_result_hash": plan.expected_result_hash,
                "operations": plan.operations,
            }
            for plan in plans
        ],
    }


def apply_plan(report: Report, noema: str) -> int:
    if report.errors:
        raise RuntimeError("refusing to apply while scan errors are present")
    plans = build_plans(report)
    payload = normalization_payload(report, plans)
    cortex_source = (Path(report.cortex_dir) / "cortex.md").read_text(encoding="utf-8")
    manifest, _, _ = split_frontmatter(cortex_source)
    cortex_name = frontmatter_scalar(manifest, "name")
    if not cortex_name:
        raise RuntimeError("cortex manifest has no name")
    with tempfile.NamedTemporaryFile(
        mode="w", encoding="utf-8", suffix=".json", prefix="noema-markdown-plan-"
    ) as plan_file:
        json.dump(payload, plan_file, indent=2, sort_keys=True)
        plan_file.flush()
        base = [
            noema,
            "--cortex",
            cortex_name,
            "memory",
            "normalize-markdown",
            plan_file.name,
        ]
        preview = subprocess.run(base, text=True)
        if preview.returncode:
            return preview.returncode
        applied = subprocess.run([*base, "--apply", "--yes"], text=True)
        return applied.returncode


def plan_text(report: Report) -> str:
    plans = build_plans(report)
    lines = [
        "Obsidian body-tag repair plan (read-only)",
        f"Cortex: {report.cortex_dir}",
        f"Proposed changes: {len(report.candidates)} candidates across {len(plans)} trace(s)",
        "No files were changed.",
    ]
    for plan in plans:
        lines.extend(["", f"Plan: {plan.path} [{plan.tier}]"])
        for action in plan.actions:
            location = (
                f"line {action.start_line}"
                if action.start_line == action.end_line
                else f"lines {action.start_line}-{action.end_line}"
            )
            lines.append(
                f"  {action.strategy}: {location}, {action.candidate_count} candidate(s) — "
                f"{action.description}"
            )
            if action.additional_edit_count:
                lines[-1] += f" Includes {action.additional_edit_count} formatting-only neighbor(s)."
        lines.extend(["", plan.diff.rstrip("\n")])
    if report.errors:
        lines.extend(["", f"Scan errors: {len(report.errors)}; no plan was generated for them."])
    return "\n".join(lines)


def report_json(report: Report) -> str:
    classifications = Counter(item.classification for item in report.candidates)
    tiers = Counter(item.tier for item in report.candidates)
    affected_tiers = Counter(
        tier for _, tier in {(item.path, item.tier) for item in report.candidates}
    )
    affected = {item.path for item in report.candidates}
    payload = {
        "schema_version": 1,
        "read_only": True,
        "cortex_dir": report.cortex_dir,
        "summary": {
            "scanned_active": report.scanned_active,
            "scanned_archived": report.scanned_archived,
            "scanned_trash": report.scanned_trash,
            "candidate_count": len(report.candidates),
            "affected_trace_count": len(affected),
            "classifications": dict(sorted(classifications.items())),
            "candidate_occurrences_by_tier": dict(sorted(tiers.items())),
            "affected_traces_by_tier": dict(sorted(affected_tiers.items())),
            "error_count": len(report.errors),
        },
        "candidates": [asdict(item) for item in report.candidates],
        "errors": [asdict(item) for item in report.errors],
    }
    return json.dumps(payload, indent=2, sort_keys=True)


def report_text(report: Report, summary_only: bool = False) -> str:
    classifications = Counter(item.classification for item in report.candidates)
    tiers = Counter(item.tier for item in report.candidates)
    affected_tiers = Counter(
        tier for _, tier in {(item.path, item.tier) for item in report.candidates}
    )
    tokens = Counter(item.token for item in report.candidates)
    affected = {item.path for item in report.candidates}
    lines = [
        "Obsidian body-tag doctor (read-only)",
        f"Cortex: {report.cortex_dir}",
        f"Scanned: {report.scanned_active} active, {report.scanned_archived} archived",
        f"Candidates: {len(report.candidates)} across {len(affected)} trace(s)",
    ]
    if report.scanned_trash:
        lines[2] += f", {report.scanned_trash} trashed"
    if classifications:
        lines.append(
            "Classifications: "
            + ", ".join(f"{name}={count}" for name, count in sorted(classifications.items()))
        )
        lines.append(
            "Candidate occurrences by tier: "
            + ", ".join(f"{name}={count}" for name, count in sorted(tiers.items()))
        )
        lines.append(
            "Affected traces by tier: "
            + ", ".join(f"{name}={count}" for name, count in sorted(affected_tiers.items()))
        )
        lines.append(
            "Top tokens: "
            + ", ".join(f"{token} ({count})" for token, count in tokens.most_common(10))
        )
    lines.append(f"Scan errors: {len(report.errors)}")
    lines.append("No files were changed.")

    if not summary_only and report.candidates:
        lines.append("")
        lines.append("Candidates")
        for item in report.candidates:
            lines.append(
                f"{item.path}:{item.line}:{item.column} "
                f"[{item.tier} {item.confidence}] {item.classification} {item.token}"
            )
    if report.errors:
        lines.append("")
        lines.append("Errors")
        for error in report.errors:
            lines.append(f"{error.path}: {error.error}")
    return "\n".join(lines)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Report body text Obsidian may mistake for inline tags (read-only)."
    )
    parser.add_argument(
        "--cortex-dir",
        type=Path,
        help="Cortex directory; defaults to the active cortex from `noema cortex list`.",
    )
    parser.add_argument("--include-trash", action="store_true")
    parser.add_argument("--json", action="store_true", dest="as_json")
    parser.add_argument("--summary-only", action="store_true")
    parser.add_argument(
        "--plan",
        action="store_true",
        help="Print proposed grouped repairs and unified diffs without writing files.",
    )
    parser.add_argument(
        "--plan-json",
        action="store_true",
        help="Print the machine-readable, hash-pinned normalization plan.",
    )
    parser.add_argument(
        "--apply",
        action="store_true",
        help="Preflight and apply the generated plan through Noema's audited maintenance API.",
    )
    parser.add_argument(
        "--noema",
        default="noema",
        help="Noema executable used by --apply (default: noema).",
    )
    args = parser.parse_args()
    selected_modes = sum((args.plan, args.plan_json, args.apply, args.as_json))
    if selected_modes > 1 or args.summary_only and selected_modes:
        parser.error(
            "--plan, --plan-json, --apply, --json, and --summary-only are mutually exclusive"
        )

    try:
        cortex_dir = args.cortex_dir or active_cortex_dir()
        if not cortex_dir.is_dir():
            raise RuntimeError(f"cortex directory does not exist: {cortex_dir}")
        report = scan_cortex(cortex_dir, include_trash=args.include_trash)
    except RuntimeError as error:
        print(f"error: {error}", file=sys.stderr)
        return 2

    if args.plan:
        print(plan_text(report))
    elif args.plan_json:
        print(json.dumps(normalization_payload(report), indent=2, sort_keys=True))
    elif args.apply:
        try:
            return apply_plan(report, args.noema)
        except (OSError, RuntimeError, ValueError) as error:
            print(f"error: {error}", file=sys.stderr)
            return 2
    elif args.as_json:
        print(report_json(report))
    else:
        print(report_text(report, summary_only=args.summary_only))
    return 1 if report.errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
