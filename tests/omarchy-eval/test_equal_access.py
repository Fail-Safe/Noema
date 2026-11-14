#!/usr/bin/env python3

from __future__ import annotations

import json
import sys
import unittest
from dataclasses import replace
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from equal_access import build_equal_access_cases, render_equal_access, summarize_equal_access
from omarchy_eval_lib import (
    BenchmarkError,
    CONDITIONS,
    DIRECTIONS,
    SCENARIOS,
    ParsedEvents,
    build_batch_cases,
    build_corpus,
    sanitized_row,
    score_case,
)


def answer_text(case, *, provenance_prefix=None):
    answers = []
    for question in case.questions:
        provenance = list(question.expected_provenance)
        if provenance_prefix:
            provenance = [value.replace("NOEMA-", provenance_prefix + "-", 1) for value in provenance]
        answers.append({
            "question_id": question.question_id,
            "answer": " ".join(question.expected_values),
            "rationale": " ".join(question.expected_rationales),
            "provenance": provenance,
            "abstain": question.should_abstain,
        })
    return json.dumps({"answers": answers})


def scored_row(arm, scenario="same_workspace_exact", score=1.0, batch=1, direction=DIRECTIONS[0]):
    return {
        "condition": arm,
        "batch": batch,
        "track": "deterministic",
        "direction": direction,
        "scenario": scenario,
        "macro_f1": score,
        "exact_accuracy": score,
        "rationale_accuracy": score,
        "provenance_accuracy": score,
        "stale_count": 0,
        "hallucination_count": 0,
        "leakage_count": 0,
        "answer_count": 2,
        "status": "completed",
        "tool_call_count": 0 if arm != "noema" else 1,
        "total_tokens": 120,
        "latency_seconds": 1.0,
    }


def triple(scenario="same_workspace_exact", scores=(0.2, 0.6, 0.9), **kwargs):
    return [scored_row(arm, scenario, score, **kwargs) for arm, score in zip(("native", "markdown", "noema"), scores)]


class EqualAccessCaseTests(unittest.TestCase):
    def test_existing_cases_and_exports_are_unchanged(self):
        original = build_batch_cases(1)
        cases = build_equal_access_cases()
        self.assertEqual(CONDITIONS, ("native", "noema"))
        self.assertEqual(cases[:24], original)
        self.assertEqual(len(cases), 36)
        self.assertEqual(len({case.case_id for case in cases}), 36)
        self.assertEqual({case.track for case in cases}, {"deterministic"})
        self.assertEqual({(case.condition, case.direction, case.scenario) for case in cases}, {
            (arm, direction, scenario)
            for arm in ("native", "markdown", "noema")
            for direction in DIRECTIONS
            for scenario in SCENARIOS
        })

    def test_markdown_matches_noema_literal_records_and_counterbalancing(self):
        for batch in (1, 2):
            for offset in (0, 1):
                cases = build_equal_access_cases(batch, offset)
                for markdown in (case for case in cases if case.condition == "markdown"):
                    with self.subTest(batch=batch, offset=offset, direction=markdown.direction):
                        noema = next(case for case in cases if case.condition == "noema"
                                     and case.direction == markdown.direction and case.scenario == markdown.scenario)
                        native = next(case for case in cases if case.condition == "native"
                                      and case.direction == markdown.direction and case.scenario == markdown.scenario)
                        self.assertEqual(replace(markdown, condition="noema", case_id=noema.case_id), noema)
                        self.assertNotEqual(markdown.corpus_variant, native.corpus_variant)
                        text = render_equal_access(markdown)
                        self.assertIn(f"Recorded by source harness: {markdown.source}.", text)
                        self.assertIn(f"NOEMA-{markdown.source.upper()}-", text)
                        other_source = "opencode" if markdown.source == "codex" else "codex"
                        self.assertNotIn(f"NOEMA-{other_source.upper()}-", text)
                        other_variant = "BETA" if markdown.corpus_variant == "alpha" else "ALPHA"
                        self.assertNotIn(other_variant + "-", text)
                        self.assertNotIn("NATIVE-", text)
                if offset == 0:
                    mirrored = build_equal_access_cases(batch, 1)
                    self.assertTrue(all(a.corpus_variant != b.corpus_variant for a, b in zip(cases, mirrored)))

    def test_boundary_workspaces_receive_full_corpus_not_question_selection(self):
        cases = build_equal_access_cases()
        for scenario in ("sibling_workspace", "outside_work", "scope_distractor_abstention"):
            case = next(case for case in cases if case.condition == "markdown" and case.scenario == scenario)
            text = render_equal_access(case)
            corpus = build_corpus(case.corpus_variant, f"NOEMA-{case.source.upper()}")
            self.assertEqual(text.count("\n## "), 60)
            self.assertEqual(text.count("- Value:"), 60)
            for record in corpus:
                with self.subTest(scenario=scenario, record=record.record_id):
                    self.assertIn(f"## {record.record_id} (", text)
                    self.assertIn(record.value, text)
                    self.assertIn(record.rationale, text)
                    self.assertIn(record.provenance, text)
            for scope in ("global", "project-a", "project-b"):
                self.assertIn(f"Scope: {scope}.", text)
            self.assertIn("## su03-old (obsolete)", text)
            self.assertIn("## su03-current (current)", text)
            self.assertIn("Supersedes: su03-old", text)
            same_assignment = next(candidate for candidate in cases if candidate.condition == "markdown"
                                   and candidate.direction == case.direction and candidate.scenario == "same_workspace_exact")
            self.assertEqual(text, render_equal_access(same_assignment))
            self.assertEqual(text, render_equal_access(replace(case, questions=())))

    def test_markdown_requires_matching_provenance_with_existing_scorer(self):
        for case in (case for case in build_equal_access_cases() if case.condition == "markdown"):
            corpus = build_corpus(case.corpus_variant, f"NOEMA-{case.source.upper()}")
            tokens = {record.value for record in corpus}
            score = score_case(case, answer_text(case), tokens)
            self.assertEqual(score["macro_f1"], 1.0)
            self.assertEqual(score["provenance_accuracy"], 1.0)
            wrong = score_case(case, answer_text(case, provenance_prefix="NATIVE"), tokens)
            self.assertEqual(wrong["provenance_accuracy"], 0.0)
            self.assertLess(wrong["macro_f1"], 1.0)

    def test_other_arms_and_tracks_cannot_render_markdown(self):
        native = build_equal_access_cases()[0]
        with self.assertRaises(BenchmarkError):
            render_equal_access(native)
        markdown = next(case for case in build_equal_access_cases() if case.condition == "markdown")
        with self.assertRaises(BenchmarkError):
            render_equal_access(replace(markdown, track="natural"))
        with self.assertRaises(BenchmarkError):
            render_equal_access(replace(markdown, source="unexpected"))


class EqualAccessSummaryTests(unittest.TestCase):
    def test_known_positive_and_negative_effects_and_residual(self):
        for scores, expected in (((0.2, 0.6, 0.9), (70, 40, 30)), ((0.9, 0.4, 0.2), (-70, -50, -20)),
                                 ((0.5, 0.9, 0.6), (10, 40, -30))):
            with self.subTest(scores=scores):
                summary = summarize_equal_access(triple(scores=scores), 40)
                self.assertTrue(summary["complete"])
                self.assertEqual(summary["matched_triples"], 1)
                decomposition = summary["decomposition"]
                for key, expected_value in zip(("total_pp", "access_pp", "retrieval_pp"), expected):
                    self.assertAlmostEqual(decomposition[key], expected_value)
                self.assertEqual(decomposition["residual_pp"], 0.0)
                self.assertTrue(decomposition["descriptive_only"])
                self.assertEqual(decomposition["bootstrap_95_pp"]["residual"], [0.0, 0.0])
                for key in ("total", "access", "retrieval"):
                    self.assertAlmostEqual(decomposition["bootstrap_95_pp"][key][0], decomposition[key + "_pp"])
                    self.assertAlmostEqual(decomposition["bootstrap_95_pp"][key][1], decomposition[key + "_pp"])

    def test_condition_metrics_preserve_rates_and_weighting(self):
        rows = triple(scores=(0.1, 0.5, 0.9)) + triple(scores=(0.8, 0.7, 0.2), batch=2)
        rows[0].update(stale_count=1, hallucination_count=1, leakage_count=1, total_tokens=80, latency_seconds=2)
        rows[3].update(total_tokens=160, latency_seconds=4)
        summary = summarize_equal_access(rows, 100)
        self.assertAlmostEqual(summary["conditions"]["native"]["macro_f1"], 0.45)
        self.assertAlmostEqual(summary["conditions"]["markdown"]["macro_f1"], 0.6)
        self.assertAlmostEqual(summary["conditions"]["noema"]["macro_f1"], 0.55)
        self.assertAlmostEqual(summary["decomposition"]["total_pp"], 10)
        self.assertAlmostEqual(summary["decomposition"]["access_pp"], 15)
        self.assertAlmostEqual(summary["decomposition"]["retrieval_pp"], -5)
        native = summary["conditions"]["native"]
        self.assertEqual(native["stale_rate"], 0.25)
        self.assertEqual(native["hallucination_rate"], 0.25)
        self.assertEqual(native["scope_leakage_rate"], 0.25)
        self.assertEqual(native["total_tokens_median"], 120)
        self.assertEqual(native["latency_p50_seconds"], 3)

    def test_boundary_update_scoping_and_same_workspace_are_separate(self):
        rows = []
        for scenario in SCENARIOS:
            scores = (0.1, 0.5, 0.9) if scenario in ("sibling_workspace", "outside_work") else (0.9, 0.5, 0.1)
            rows.extend(triple(scenario, scores))
        summary = summarize_equal_access(rows, 20)
        self.assertEqual(summary["matched_triples"], 6)
        subsets = summary["subsets"]
        self.assertEqual(subsets["boundary"]["matched_triples"], 2)
        self.assertAlmostEqual(subsets["boundary"]["decomposition"]["total_pp"], 80)
        self.assertEqual(subsets["same_workspace"]["matched_triples"], 2)
        for name in ("update", "scoping", "same_workspace"):
            self.assertAlmostEqual(subsets[name]["decomposition"]["total_pp"], -80)
        for group in list(subsets.values()) + list(summary["scenarios"].values()):
            self.assertEqual(group["decomposition"]["residual_pp"], 0)
            self.assertEqual(set(group["conditions"]), {"native", "markdown", "noema"})
        self.assertEqual(summary["scenarios"]["outside_work"]["matched_triples"], 1)

    def test_alignment_ignores_case_id_and_input_order_but_not_direction(self):
        rows = triple() + triple(scores=(0.6, 0.4, 0.2), direction=DIRECTIONS[1])
        for index, row in enumerate(rows):
            row["case_id"] = f"opaque-{index}"
            row["execution_order"] = index
        first = summarize_equal_access(rows, 20)
        second = summarize_equal_access(list(reversed(rows)), 20)
        self.assertEqual(first["conditions"], second["conditions"])
        self.assertEqual(first["decomposition"]["total_pp"], second["decomposition"]["total_pp"])
        self.assertEqual(first["matched_triples"], 2)
        with self.assertRaises(BenchmarkError):
            summarize_equal_access(rows[:-1], 20)

    def test_duplicate_incomplete_and_unrecognized_rows_fail_closed(self):
        for rows in (triple()[:2], triple() + [triple()[0]],
                     [dict(row, track="natural") for row in triple()],
                     [dict(row, condition="unexpected") if index == 0 else row for index, row in enumerate(triple())],
                     [dict(row, direction="unexpected") for row in triple()],
                     [dict(row, scenario="unexpected") for row in triple()],
                     [dict(row, batch=0) for row in triple()]):
            with self.subTest(rows=rows), self.assertRaises(BenchmarkError):
                summarize_equal_access(rows, 20)

    def test_failed_turns_retain_scores_weight_and_observation_counts(self):
        rows = triple(scores=(1, 1, 1)) + triple(scores=(1, 0, 1), batch=2)
        rows[4].update(status="failed", total_tokens=None, latency_seconds=None)
        summary = summarize_equal_access(rows, 20)
        self.assertEqual(summary["conditions"]["markdown"]["macro_f1"], 0.5)
        self.assertEqual(summary["conditions"]["markdown"]["failed_turns"], 1)
        self.assertEqual(summary["conditions"]["markdown"]["turns"], 2)
        self.assertEqual(summary["conditions"]["markdown"]["total_tokens_observed_turns"], 1)
        self.assertEqual(summary["conditions"]["markdown"]["latency_observed_turns"], 1)
        self.assertEqual(summary["decomposition"]["access_pp"], -50)
        self.assertEqual(summary["decomposition"]["retrieval_pp"], 50)
        rows[4].update(macro_f1=0.4)
        self.assertEqual(summarize_equal_access(rows, 20)["conditions"]["markdown"]["macro_f1"], 0.7)
        for field in ("macro_f1", "exact_accuracy", "provenance_accuracy", "answer_count"):
            invalid = [dict(row) for row in rows]
            del invalid[4][field]
            with self.subTest(field=field), self.assertRaises(BenchmarkError):
                summarize_equal_access(invalid, 20)
        invalid = [dict(row, rationale_accuracy=None) for row in triple("outside_work")]
        with self.assertRaises(BenchmarkError):
            summarize_equal_access(invalid, 20)

    def test_empty_report_is_explicitly_unavailable(self):
        summary = summarize_equal_access([], 20)
        self.assertFalse(summary["available"])
        self.assertFalse(summary["complete"])
        self.assertEqual(summary["matched_triples"], 0)
        self.assertIsNone(summary["decomposition"]["total_pp"])
        self.assertIsNone(summary["decomposition"]["residual_pp"])
        self.assertTrue(all(not group["available"] for group in summary["subsets"].values()))
        for samples in (0, -1, True):
            with self.assertRaises(BenchmarkError):
                summarize_equal_access([], samples)

    def test_report_does_not_publish_private_fields_or_values(self):
        rows = []
        for case in build_equal_access_cases():
            corpus = build_corpus(case.corpus_variant, f"{('NOEMA' if case.condition != 'native' else 'NATIVE')}-{case.source.upper()}")
            parsed = ParsedEvents(final_text=answer_text(case), total_tokens=100, session_id="PRIVATE-SESSION")
            score = score_case(case, parsed.final_text, {record.value for record in corpus})
            row = sanitized_row(case, parsed, score, status="completed", latency_seconds=1, error_code=None)
            row.update(prompt="PRIVATE-PROMPT", raw_answer="PRIVATE-ANSWER", path="/private/private-run",
                       provenance="PRIVATE-PROVENANCE", case_id="PRIVATE-CASE", error_code="PRIVATE-ERROR")
            rows.append(row)
        encoded = json.dumps(summarize_equal_access(rows, 20), allow_nan=False)
        for private in ("PRIVATE-", "/private/", "NATIVE-CODEX-", "NOEMA-CODEX-", "ALPHA-GP-", "BETA-GP-"):
            self.assertNotIn(private, encoded)


if __name__ == "__main__":
    unittest.main()
