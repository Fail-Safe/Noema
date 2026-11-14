#!/usr/bin/env python3
"""Additive, descriptive equal-access mechanics; not a qualification checkpoint.

Only aggregate scores and operational metrics leave this module's summary API.
Markdown receives the full selected source corpus, including wrong-scope and old
records. This tests scoped answers, not filesystem permissions. Markdown/Noema
use identical literal values and provenance; native keeps the existing equivalent,
counterbalanced alpha/beta vocabulary rather than identical literal tokens.
"""

from __future__ import annotations

import math
from dataclasses import replace
from typing import Any, Mapping, Sequence

from omarchy_eval_lib import (
    BenchmarkError,
    Case,
    DIRECTIONS,
    SCENARIOS,
    build_batch_cases,
    build_corpus,
    render_agents,
    summarize_rows,
)


_ARMS = ("native", "markdown", "noema")
_SUBSETS = {
    "boundary": {"sibling_workspace", "outside_work"},
    "update": {"supersession"},
    "scoping": {"scope_distractor_abstention"},
    "same_workspace": {"same_workspace_exact", "same_workspace_rationale"},
}
_SCORE_FIELDS = ("macro_f1", "exact_accuracy", "rationale_accuracy", "provenance_accuracy")
_SAFETY_FIELDS = ("stale_count", "hallucination_count", "leakage_count", "answer_count")


def build_equal_access_cases(batch: int = 1, corpus_offset: int = 0) -> list[Case]:
    """Keep every deterministic baseline case and add its Noema Markdown sibling."""
    cases = build_batch_cases(batch, corpus_offset=corpus_offset)
    markdown = [
        replace(
            case,
            condition="markdown",
            case_id=case.case_id.replace("-noema-", "-markdown-"),
        )
        for case in cases
        if case.condition == "noema"
    ]
    return cases + markdown


def render_equal_access(case: Case) -> str:
    """Render all 60 records for this Markdown arm, with explicit scope groups.

    Do not select records from the questions or workspace: the selected Cortex
    already holds all scopes, so matching access must expose wrong-scope records
    to Markdown too. Other directions and vocabulary assignments are not bundled.
    """
    if case.condition != "markdown" or case.track != "deterministic":
        raise BenchmarkError("equal-access rendering requires a deterministic markdown case")
    if case.direction not in DIRECTIONS or case.source != case.direction.split("_to_")[0]:
        raise BenchmarkError("equal-access rendering requires a recognized source direction")
    records = build_corpus(case.corpus_variant, f"NOEMA-{case.source.upper()}")
    sections = [
        "# Shared equal-access continuity memory\n\n"
        "The full selected source corpus follows. Scope labels govern answers, not access. "
        "Obsolete records remain visible; use current records for current-value questions.\n"
    ]
    for scope in dict.fromkeys(record.scope for record in records):
        sections.append(render_agents([record for record in records if record.scope == scope], case.source, scope))
    return "\n".join(sections)


def _validate_rows(rows: Sequence[Mapping[str, Any]]) -> None:
    triples: dict[tuple[Any, ...], set[str]] = {}
    for row in rows:
        if row.get("condition") not in _ARMS:
            raise BenchmarkError("equal-access rows contain an unrecognized arm")
        if row.get("track") != "deterministic":
            raise BenchmarkError("equal-access rows require the deterministic track")
        if row.get("direction") not in DIRECTIONS or row.get("scenario") not in SCENARIOS:
            raise BenchmarkError("equal-access rows contain an unrecognized direction or scenario")
        batch = row.get("batch")
        if type(batch) is not int or not 1 <= batch <= 12:
            raise BenchmarkError("equal-access rows require batch in 1..12")
        if row.get("status") not in ("completed", "failed"):
            raise BenchmarkError("equal-access rows require completed or failed status")
        for field in _SCORE_FIELDS:
            value = row.get(field)
            applicable = field != "rationale_accuracy" or row["scenario"] in {
                "same_workspace_rationale", "outside_work", "supersession"
            }
            if field not in row or (applicable and value is None):
                raise BenchmarkError("equal-access rows require scored turns, including failures")
            if value is not None and (
                isinstance(value, bool) or not isinstance(value, (int, float))
                or not math.isfinite(value) or not 0 <= value <= 1
            ):
                raise BenchmarkError("equal-access scores must be finite values in 0..1")
        for field in _SAFETY_FIELDS:
            value = row.get(field)
            if type(value) is not int or value < 0:
                raise BenchmarkError("equal-access rows require nonnegative integer safety counts")
        if row["answer_count"] == 0:
            raise BenchmarkError("equal-access scored turns require an answer denominator")
        key = (batch, row["track"], row["direction"], row["scenario"])
        arms = triples.setdefault(key, set())
        if row["condition"] in arms:
            raise BenchmarkError("equal-access rows contain a duplicate triple arm")
        arms.add(row["condition"])
    if any(arms != set(_ARMS) for arms in triples.values()):
        raise BenchmarkError("equal-access decomposition requires complete matched triples")


def _pair_summary(
    rows: Sequence[Mapping[str, Any]], left: str, right: str, bootstrap_samples: int
) -> dict[str, Any]:
    # Remapping only labels keeps all scoring, aggregation, and paired bootstrap
    # conventions in the established harness; no failed row is filtered out.
    paired_rows = [
        dict(row, condition="native" if row["condition"] == left else "noema")
        for row in rows
        if row["condition"] in (left, right)
    ]
    return summarize_rows(paired_rows, bootstrap_samples)


def _summarize_group(rows: Sequence[Mapping[str, Any]], bootstrap_samples: int) -> dict[str, Any]:
    total = _pair_summary(rows, "native", "noema", bootstrap_samples)
    access = _pair_summary(rows, "native", "markdown", bootstrap_samples)
    retrieval = _pair_summary(rows, "markdown", "noema", bootstrap_samples)
    available = bool(rows)
    effects = {"total": total, "access": access, "retrieval": retrieval}
    decomposition: dict[str, Any] = {
        "metric": "macro_f1",
        "units": "percentage_points",
        "descriptive_only": True,
        "available": available,
        "bootstrap_95_pp": {},
    }
    for name, summary in effects.items():
        paired = summary["paired"]
        difference = paired["noema_minus_native"]
        decomposition[f"{name}_pp"] = difference * 100 if difference is not None else None
        decomposition["bootstrap_95_pp"][name] = [
            paired[key] * 100 if paired[key] is not None else None
            for key in ("bootstrap_95_low", "bootstrap_95_high")
        ]
    decomposition["residual_pp"] = (
        round(decomposition["total_pp"] - decomposition["access_pp"] - decomposition["retrieval_pp"], 12)
        if available else None
    )
    decomposition["bootstrap_95_pp"]["residual"] = [0.0, 0.0] if available else [None, None]
    conditions = {
        "native": total["conditions"]["native"],
        "markdown": access["conditions"]["noema"],
        "noema": total["conditions"]["noema"],
    }
    for arm, metrics in conditions.items():
        arm_rows = [row for row in rows if row["condition"] == arm]
        metrics["total_tokens_observed_turns"] = sum(row.get("total_tokens") is not None for row in arm_rows)
        metrics["latency_observed_turns"] = sum(row.get("latency_seconds") is not None for row in arm_rows)
    return {
        "retrieval_turns": len(rows),
        "matched_triples": total["paired"]["pairs"],
        "available": available,
        "complete": available,
        "conditions": conditions,
        "decomposition": decomposition,
    }


def summarize_equal_access(
    rows: Sequence[Mapping[str, Any]], bootstrap_samples: int
) -> dict[str, Any]:
    """Summarize matched deterministic triples without qualification decisions.

    Nonempty incomplete/duplicate triples, unknown arms/tracks, and unscored
    failed turns raise BenchmarkError rather than silently changing denominators.
    Empty rows return an explicit unavailable decomposition for failure reports.
    Failed turns retain the established harness scores and their original weight;
    status alone neither removes a row nor overrides a scored answer. Accuracy
    fields may be None only for dimensions the harness marks non-applicable.
    """
    if type(bootstrap_samples) is not int or bootstrap_samples < 1:
        raise BenchmarkError("equal-access bootstrap samples must be a positive integer")
    _validate_rows(rows)
    summary = _summarize_group(rows, bootstrap_samples)
    summary["subsets"] = {
        name: _summarize_group([row for row in rows if row["scenario"] in scenarios], bootstrap_samples)
        for name, scenarios in _SUBSETS.items()
    }
    summary["scenarios"] = {
        scenario: _summarize_group([row for row in rows if row["scenario"] == scenario], bootstrap_samples)
        for scenario in SCENARIOS
    }
    summary["descriptive_only"] = True
    summary["notes"] = [
        "Total = Noema - native; access = Markdown - native; retrieval package = Noema - Markdown, in percentage points.",
        "The residual is algebraically zero on the same matched triples, not an identified causal interaction or causal proof.",
        "Paired bootstrap intervals are descriptive and resample matched turn differences; they do not establish causality.",
        "Access and retrieval package remain confounded: retrieval includes delivery, bounding, and context load.",
        "Markdown and Noema share literal vocabulary and provenance; native uses the existing equivalent counterbalanced alpha/beta vocabulary, not identical tokens.",
        "Markdown mirrors all 60 records in the selected Cortex, including both project scopes and obsolete records; scope is an answer-behavior test, not a filesystem permissions test.",
        "Failed turns retain harness scores and equal turn weight without retry or success-only filtering; missing token/latency observations are counted explicitly.",
    ]
    return summary
