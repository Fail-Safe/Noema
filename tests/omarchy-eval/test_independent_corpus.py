#!/usr/bin/env python3
"""Consumer-visible independent-generator/report mechanics; no model calls."""
from __future__ import annotations

import copy
import json
import statistics
import sys
import unittest
from collections import Counter, defaultdict
from dataclasses import replace
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from independent_corpus import (
    SIZES, build_independent_cases, build_plan, build_smoke_specs,
    records_for_case, summarize_independent,
)
from omarchy_eval_lib import (
    BenchmarkError, DIRECTIONS, SCENARIOS, TOKEN_RE, ParsedEvents,
    render_agents, render_trace_body, sanitized_row, score_case,
)

ARMS = ("native", "markdown", "noema")


def answer_text(case):
    return json.dumps({"answers": [{
        "question_id": q.question_id, "answer": " ".join(q.expected_values),
        "rationale": "Natural explanation: " + " ".join(q.expected_rationales),
        "provenance": list(q.expected_provenance), "abstain": q.should_abstain,
    } for q in case.questions]})


def synthetic_rows(plan, scores=None):
    """Scored-like rows with actual planned IDs/denominators, not live attempts."""
    rows = []
    for spec in plan["corpora"]:
        for case in build_independent_cases(spec):
            value = scores(spec, case) if scores else 1.0
            rationale_applies = case.scenario in {"same_workspace_rationale", "outside_work", "supersession"}
            score = {
                "valid_response": True, "macro_f1": value, "exact_accuracy": value,
                "rationale_accuracy": value if rationale_applies else None,
                "provenance_accuracy": value, "dimension_scores": {}, "stale_count": 0,
                "hallucination_count": 0, "leakage_count": 0, "answer_count": len(case.questions),
            }
            row = sanitized_row(case, ParsedEvents(total_tokens=120), score,
                                status="completed", latency_seconds=1.0, error_code=None)
            row.update(corpus_id=spec["corpus_id"], stratum=spec["stratum"], record_count=spec["record_count"])
            rows.append(row)
    return rows


class IndependentGeneratorTests(unittest.TestCase):
    def test_plan_is_deterministic_stratified_shuffled_and_pilots_disjoint(self):
        plan = build_plan("unit-only-plan-seed")
        self.assertEqual(plan, build_plan("unit-only-plan-seed"))
        self.assertNotEqual(plan, build_plan("different-unit-only-seed"))
        self.assertEqual(plan["expected_turns"], 1080)
        self.assertEqual(len(plan["corpora"]), 30)
        self.assertEqual(Counter(spec["stratum"] for spec in plan["corpora"]), {key: 10 for key in SIZES})
        self.assertEqual(len({spec["seed"] for spec in plan["corpora"]}), 30)
        self.assertNotEqual([spec["corpus_id"] for spec in plan["corpora"]], sorted(spec["corpus_id"] for spec in plan["corpora"]))
        pilots = build_smoke_specs()
        self.assertEqual(pilots, build_smoke_specs())
        self.assertEqual([spec["record_count"] for spec in pilots], [60, 180, 600])
        for field in ("corpus_id", "seed"):
            self.assertTrue({spec[field] for spec in pilots}.isdisjoint(spec[field] for spec in plan["corpora"]))
        for invalid in ("", None, 7):
            with self.assertRaises(BenchmarkError):
                build_plan(invalid)

    def test_full_sizes_unique_tokens_disjoint_directions_and_all_arm_equality(self):
        seen_values = set()
        for spec in build_smoke_specs():
            cases = build_independent_cases(spec)
            self.assertEqual(len(cases), 36)
            self.assertEqual(len({case.case_id for case in cases}), 36)
            self.assertEqual(cases, build_independent_cases(spec))
            groups = defaultdict(list)
            for case in cases:
                groups[(case.direction, case.scenario)].append(case)
            for direction in DIRECTIONS:
                representative = next(case for case in cases if case.direction == direction)
                records = records_for_case(spec, representative)
                values = {record.value for record in records}
                self.assertEqual(len(records), SIZES[spec["stratum"]])
                self.assertEqual(len({record.record_id for record in records}), len(records))
                self.assertEqual(len(values), len(records))
                self.assertTrue(all(TOKEN_RE.fullmatch(value) for value in values))
                self.assertTrue(seen_values.isdisjoint(values))
                seen_values.update(values)
                self.assertEqual(len({record.provenance for record in records}), len(records))
                self.assertEqual(len({record.rationale.split(":", 1)[0] for record in records}), len(records))
            for triple in groups.values():
                self.assertEqual({case.condition for case in triple}, set(ARMS))
                self.assertTrue(all(case.questions == triple[0].questions for case in triple))
                self.assertTrue(all(case.corpus_variant == triple[0].corpus_variant for case in triple))
                corpus = records_for_case(spec, triple[0])
                self.assertTrue(all(records_for_case(spec, case) == corpus for case in triple))
                self.assertTrue(all(case.batch == 1 and case.track == "deterministic" for case in triple))

    def test_variants_and_arm_positions_are_balanced(self):
        plan = build_plan("unit-only-counterbalance")
        for stratum in SIZES:
            for direction in DIRECTIONS:
                variants = Counter()
                for spec in plan["corpora"]:
                    if spec["stratum"] != stratum:
                        continue
                    cases = [case for case in build_independent_cases(spec) if case.direction == direction]
                    variants[cases[0].corpus_variant] += 1
                    self.assertEqual(Counter(cases[offset].condition for offset in range(0, 18, 3)),
                                     {arm: 2 for arm in ARMS})
                self.assertEqual(variants, {"alpha": 5, "beta": 5})

    def test_independent_seeds_change_content_targets_order_chains_and_distractors(self):
        base = build_smoke_specs()[0]
        signatures = defaultdict(set)
        target_entities = set()
        for index in range(12):
            spec = dict(base, seed=f"unit-only-independent-draw-{index}")
            cases = build_independent_cases(spec)
            case = next(case for case in cases if case.direction == DIRECTIONS[0])
            records = records_for_case(spec, case)
            # Strip tokens entirely: independence must change the actual corpus/task structure.
            signatures["contents"].add(tuple((r.record_id, r.category, r.rationale.split(": ", 1)[1], r.current, r.supersedes)
                                               for r in records))
            signatures["order"].add(tuple((c.direction, c.scenario, c.condition) for c in cases))
            signatures["chains"].add(tuple(sorted(Counter(r.record_id.split(" / ")[0] for r in records
                                                          if r.category == "supersession").values())))
            signatures["distractors"].add(tuple(sorted(r.record_id for r in records if r.scope == "project-b")))
            signatures["tasks"].add(tuple(q.text for c in cases if c.condition == "native" for q in c.questions))
            by_value = {r.value: r for r in records}
            for current_case in cases:
                if current_case.direction == case.direction:
                    for q in current_case.questions:
                        target_entities.update(by_value[v].record_id.split(" / ")[0] for v in q.expected_values)
        for name, values in signatures.items():
            with self.subTest(name=name):
                self.assertGreater(len(values), 1)
        self.assertGreater(len(target_entities), 30)

    def test_chain_pointers_all_targets_and_rendered_visibility(self):
        for spec in build_smoke_specs():
            cases = build_independent_cases(spec)
            for direction in DIRECTIONS:
                selected = [case for case in cases if case.direction == direction and case.condition == "native"]
                records = records_for_case(spec, selected[0])
                by_id = {record.record_id: record for record in records}
                by_value = {record.value: record for record in records}
                # These are the native hierarchy's existing visible scopes, unchanged.
                native_scopes = {"project-a": {"global", "project-a"},
                                 "project-b": {"global", "project-b"}, "outside": set()}
                native_docs = {scope: render_agents([r for r in records if r.scope == scope], "codex", scope)
                               for scope in ("global", "project-a", "project-b")}
                markdown = "\n".join(native_docs.values())
                traces = "\n".join(render_trace_body(r, "codex") for r in records)
                self.assertLess(len(markdown.encode("utf-8")), 1048576)
                for record in records:
                    self.assertIn(record.value, markdown)
                    self.assertIn(record.value, traces)
                    self.assertIn(record.provenance, markdown)
                    self.assertIn(record.record_id, traces)
                    self.assertEqual(record.value in native_docs[record.scope], True)
                for case in selected:
                    for q in case.questions:
                        for value in (*q.expected_values, *q.forbidden_values):
                            self.assertIn(value, by_value)
                            self.assertNotIn(value, q.text)
                        for value in q.expected_values:
                            target = by_value[value]
                            entity, topic, _, _ = target.record_id.split(" / ")
                            self.assertIn(entity, q.text)
                            self.assertIn(topic, q.text)
                            self.assertNotIn(target.record_id, q.text)
                            visible = target.scope in native_scopes[case.workspace_role]
                            if case.scenario.startswith("same_workspace") or case.scenario in {"supersession", "scope_distractor_abstention"}:
                                self.assertTrue(visible)
                            if case.scenario == "outside_work" or (case.scenario == "sibling_workspace" and target.scope == "project-a"):
                                self.assertFalse(visible)
                        for rationale in q.expected_rationales:
                            self.assertTrue(any(rationale in record.rationale for record in records))
                        for provenance in q.expected_provenance:
                            self.assertTrue(any(provenance == record.provenance for record in records))
                        if q.should_abstain:
                            self.assertFalse(q.expected_values)
                    score = score_case(case, answer_text(case), set(by_value))
                    self.assertEqual(score["macro_f1"], 1.0)
                    self.assertEqual((score["stale_count"], score["hallucination_count"], score["leakage_count"]), (0, 0, 0))
                supersession = next(case for case in selected if case.scenario == "supersession")
                question = supersession.questions[0]
                current = by_value[question.expected_values[0]]
                self.assertTrue(current.current)
                chain = [current]
                while chain[-1].supersedes:
                    predecessor = by_id[chain[-1].supersedes]
                    self.assertFalse(predecessor.current)
                    self.assertIn("-OLD-", predecessor.value)
                    self.assertEqual(predecessor.record_id.split(" / ")[:2], current.record_id.split(" / ")[:2])
                    chain.append(predecessor)
                self.assertIn(len(chain), (3, 4, 5))
                self.assertEqual(set(question.forbidden_values), {r.value for r in chain[1:]})
                scoped = next(case for case in selected if case.scenario == "scope_distractor_abstention").questions[0]
                right, wrong = by_value[scoped.expected_values[0]], by_value[scoped.forbidden_values[0]]
                self.assertEqual((right.scope, wrong.scope), ("project-b", "project-a"))
                self.assertEqual(right.record_id.split(" / ")[:2], wrong.record_id.split(" / ")[:2])

    def test_wrong_scope_and_stale_answers_use_existing_safety_scorer(self):
        spec = build_smoke_specs()[0]
        for scenario, safety in (("supersession", "stale_count"), ("scope_distractor_abstention", "leakage_count")):
            case = next(c for c in build_independent_cases(spec) if c.scenario == scenario)
            payload = json.loads(answer_text(case))
            payload["answers"][0]["answer"] = case.questions[0].forbidden_values[0]
            score = score_case(case, json.dumps(payload), set())
            self.assertGreater(score[safety], 0)
            self.assertLess(score["exact_accuracy"], 1.0)

    def test_invalid_specs_and_cross_corpus_cases_fail_closed(self):
        spec = build_smoke_specs()[0]
        case = build_independent_cases(spec)[0]
        for invalid in (dict(spec, record_count=61), dict(spec, stratum="huge"), dict(spec, stratum=[]),
                        dict(spec, corpus_id="../outside"), dict(spec, seed="")):
            with self.assertRaises(BenchmarkError):
                build_independent_cases(invalid)
        for wrong in (replace(case, condition="unknown"), replace(case, corpus_variant="wrong"),
                      replace(case, case_id="corpus-999-other"),
                      build_independent_cases(dict(spec, seed="different-unit-seed"))[0]):
            with self.assertRaises(BenchmarkError):
                records_for_case(spec, wrong)


class IndependentSummaryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.plan = build_plan("unit-only-summary-plan")
        cls.rows = synthetic_rows(cls.plan)

    def summarize(self, rows=None, plan=None, samples=20):
        return summarize_independent(self.rows if rows is None else rows,
                                     self.plan if plan is None else plan, samples)

    def test_complete_report_all_groups_ceiling_caveat_and_sanitization(self):
        rows = [dict(row, question="PRIVATE-QUESTION", rationale="PRIVATE-RATIONALE", provenance="PRIVATE-SOURCE",
                     path="/private/output", raw_answer="PRIVATE-ANSWER", error_code="PRIVATE-ERROR", seed="PRIVATE-SEED")
                for row in self.rows]
        summary = self.summarize(rows)
        self.assertTrue(summary["available"])
        self.assertTrue(summary["headline_available"])
        self.assertEqual(summary["cluster_count"], 30)
        self.assertEqual(summary["clusters_by_stratum"], {key: 10 for key in SIZES})
        self.assertEqual(summary["matched_triples"], 360)
        self.assertEqual(summary["coverage"]["complete_corpora"], 30)
        self.assertEqual(summary["coverage"]["missing_turns"], 0)
        for metrics in summary["conditions"].values():
            self.assertEqual(metrics["coverage"]["planned_turns"], 360)
            self.assertEqual(metrics["coverage"]["observed_turns"], 360)
            self.assertEqual(metrics["coverage"]["missing_turns"], 0)
        self.assertEqual(set(summary["subsets"]), {"boundary", "update", "scoping", "same_workspace"})
        self.assertEqual(set(summary["scenarios"]), set(SCENARIOS))
        self.assertEqual(set(summary["directions"]), set(DIRECTIONS))
        self.assertEqual(set(summary["strata"]), set(SIZES))
        self.assertEqual(len(summary["per_corpus"]), 30)
        groups = [summary, *summary["subsets"].values(), *summary["scenarios"].values(),
                  *summary["directions"].values(), *summary["strata"].values()]
        for group in groups:
            self.assertTrue(group["available"])
            self.assertEqual(group["decomposition"]["residual_pp"], 0.0)
            for arm in ARMS:
                metrics = group["conditions"][arm]
                self.assertEqual(metrics["macro_f1"], 1.0)
                self.assertEqual(metrics["bootstrap_95"]["macro_f1"], [1.0, 1.0])
                self.assertEqual(metrics["bootstrap_95"]["stale_rate"], [0.0, 0.0])
        for corpus in summary["per_corpus"].values():
            self.assertEqual(corpus["cluster_count"], 1)
            self.assertEqual(corpus["conditions"]["native"]["bootstrap_95"]["macro_f1"], [None, None])
        uncertainty = summary["uncertainty"]
        self.assertTrue(uncertainty["conditional_on_fixed_generator"])
        self.assertFalse(uncertainty["population_generalization_established"])
        self.assertFalse(uncertainty["zero_observed_errors_proves_zero_population_rate"])
        self.assertFalse(uncertainty["zero_width_interval_proves_no_population_uncertainty"])
        encoded = json.dumps(summary, allow_nan=False)
        for private in ("PRIVATE-", "/private/", '"seed"', '"case_id"', '"questions"', '"provenance"', "ALPHA-GP-", "BETA-GP-"):
            self.assertNotIn(private, encoded)
        for spec in self.plan["corpora"]:
            self.assertNotIn(spec["seed"], encoded)

    def test_known_cluster_weighted_effects_equal_size_stratum_weight(self):
        bases = {"small": 0.1, "medium": 0.4, "large": 0.7}
        def scores(spec, case):
            return bases[spec["stratum"]] + {"native": 0, "markdown": .1, "noema": .2}[case.condition]
        summary = self.summarize(synthetic_rows(self.plan, scores))
        self.assertAlmostEqual(summary["conditions"]["native"]["macro_f1"], .4)
        self.assertAlmostEqual(summary["conditions"]["markdown"]["macro_f1"], .5)
        self.assertAlmostEqual(summary["conditions"]["noema"]["macro_f1"], .6)
        self.assertAlmostEqual(summary["decomposition"]["total_pp"], 20)
        self.assertAlmostEqual(summary["decomposition"]["access_pp"], 10)
        self.assertAlmostEqual(summary["decomposition"]["retrieval_pp"], 10)
        for low, high in (summary["conditions"][arm]["bootstrap_95"]["macro_f1"] for arm in ARMS):
            self.assertAlmostEqual(low, high)
        # One stratum-constant sample has no empirical between-corpus uncertainty
        # within a stratum. A turn-level bootstrap would incorrectly vary it.
        self.assertAlmostEqual(summary["conditions"]["native"]["bootstrap_95"]["macro_f1"][0], .4)

    def test_intervals_resample_entire_corpus_not_scenario_or_direction(self):
        def scores(spec, case):
            ordinal = int(spec["corpus_id"].rsplit("-", 1)[1])
            corpus_effect = (ordinal % 10) / 20
            direction_effect = .05 if case.direction == DIRECTIONS[0] else -.05
            scenario_effect = .02 if case.scenario == SCENARIOS[0] else -.004
            return .2 if case.condition != "noema" else .2 + corpus_effect + direction_effect + scenario_effect
        rows = synthetic_rows(self.plan, scores)
        summary = self.summarize(rows, samples=100)
        overall_interval = summary["decomposition"]["bootstrap_95_pp"]["total"]
        # Direction shifts translate precisely; retaining the same corpus draws
        # forbids either direction or scenario being independently resampled.
        for bound, shifted in zip(overall_interval, summary["directions"][DIRECTIONS[0]]["decomposition"]["bootstrap_95_pp"]["total"]):
            self.assertAlmostEqual(shifted - bound, 5)
        for bound, shifted in zip(overall_interval, summary["scenarios"][SCENARIOS[0]]["decomposition"]["bootstrap_95_pp"]["total"]):
            self.assertAlmostEqual(shifted - bound, 2)
        self.assertLess(*overall_interval)
        reversed_summary = self.summarize(list(reversed(rows)), samples=100)
        self.assertEqual(overall_interval, reversed_summary["decomposition"]["bootstrap_95_pp"]["total"])
        means = [corpus["decomposition"]["total_pp"] for corpus in summary["per_corpus"].values()]
        self.assertAlmostEqual(summary["decomposition"]["total_pp"], statistics.fmean(means))

    def test_subsets_directions_strata_and_corpus_have_planned_denominators(self):
        summary = self.summarize()
        for subset, count in (("boundary", 360), ("same_workspace", 360), ("update", 180), ("scoping", 180)):
            self.assertEqual(summary["subsets"][subset]["retrieval_turns"], count)
        for group in summary["scenarios"].values():
            self.assertEqual(group["retrieval_turns"], 180)
        for group in summary["directions"].values():
            self.assertEqual(group["retrieval_turns"], 540)
        for group in summary["strata"].values():
            self.assertEqual(group["cluster_count"], 10)
            self.assertEqual(group["retrieval_turns"], 360)
        for group in summary["per_corpus"].values():
            self.assertEqual(group["retrieval_turns"], 36)
            self.assertEqual(group["matched_triples"], 12)

    def test_incomplete_is_unavailable_but_retains_progress_and_failures(self):
        rows = [dict(row) for row in self.rows[:-1]]
        rows[0].update(status="failed", macro_f1=.3, total_tokens=None, latency_seconds=None)
        summary = self.summarize(rows)
        self.assertFalse(summary["available"])
        self.assertFalse(summary["headline_available"])
        self.assertFalse(summary["complete"])
        self.assertEqual(summary["coverage"]["observed_turns"], 1079)
        self.assertEqual(summary["coverage"]["missing_turns"], 1)
        self.assertEqual(summary["coverage"]["failed_turns"], 1)
        self.assertEqual(summary["coverage"]["complete_corpora"], 29)
        self.assertIsNone(summary["decomposition"]["total_pp"])
        self.assertTrue(all(not group["available"] for group in summary["strata"].values()))
        self.assertEqual(sum(group["conditions"][arm]["failed_turns"] for group in summary["per_corpus"].values() for arm in ARMS), 1)
        empty = self.summarize([])
        self.assertEqual(empty["coverage"]["missing_turns"], 1080)
        self.assertFalse(empty["available"])
        self.assertIsNone(empty["conditions"]["native"]["macro_f1"])

    def test_failed_scored_rows_keep_scores_and_measurement_missingness(self):
        rows = [dict(row) for row in self.rows]
        native_index = next(i for i, row in enumerate(rows) if row["condition"] == "native")
        rows[native_index].update(status="failed", macro_f1=.25, exact_accuracy=.5,
                                  stale_count=1, hallucination_count=1, leakage_count=1,
                                  total_tokens=None, latency_seconds=None)
        summary = self.summarize(rows)
        native = summary["conditions"]["native"]
        self.assertTrue(summary["available"])
        self.assertEqual(native["failed_turns"], 1)
        self.assertEqual(native["failed_corpora"], 1)
        self.assertEqual(summary["failed_cluster_count"], 1)
        self.assertEqual(native["stale_corpora"], 1)
        self.assertEqual(native["hallucination_corpora"], 1)
        self.assertEqual(native["leakage_corpora"], 1)
        self.assertEqual(native["turns"], 360)
        self.assertAlmostEqual(native["macro_f1"], (359 + .25) / 360)
        self.assertEqual(native["total_tokens_observed_turns"], 359)
        self.assertEqual(native["latency_observed_turns"], 359)
        self.assertAlmostEqual(native["stale_rate"], 1 / native["answer_count"])
        self.assertAlmostEqual(summary["decomposition"]["total_pp"], .75 / 360 * 100)

    def test_duplicate_unknown_id_metadata_plan_and_score_mismatches_rejected(self):
        variants = [self.rows + [self.rows[0]]]
        for field, value in (("case_id", "unknown"), ("corpus_id", "corpus-999"), ("condition", "other"),
                             ("direction", "other"), ("scenario", "other"), ("stratum", "other"),
                             ("record_count", 61), ("batch", 2), ("track", "natural"), ("receiver", "other"),
                             ("corpus_variant", "wrong"), ("status", "pending"), ("macro_f1", None),
                             ("macro_f1", float("nan")), ("exact_accuracy", True), ("provenance_accuracy", 1.1),
                             ("answer_count", 99), ("stale_count", -1), ("leakage_count", 99),
                             ("total_tokens", float("inf")), ("latency_seconds", -1)):
            variants.append([dict(self.rows[0], **{field: value}), *self.rows[1:]])
        missing_score = dict(self.rows[0])
        del missing_score["exact_accuracy"]
        variants.append([missing_score, *self.rows[1:]])
        for rows in variants:
            with self.subTest(first=rows[0]), self.assertRaises(BenchmarkError):
                self.summarize(rows)
        for change in ("expected_turns", "duplicate", "size", "seed", "strata", "unknown", "schema"):
            plan = copy.deepcopy(self.plan)
            if change == "expected_turns":
                plan["expected_turns"] = 36
            elif change == "duplicate":
                plan["corpora"][1] = dict(plan["corpora"][0])
            elif change == "size":
                plan["corpora"][0]["record_count"] += 1
            elif change == "seed":
                plan["corpora"][0]["seed"] = "different-private-plan-seed"
            elif change == "strata":
                for spec in plan["corpora"]:
                    spec.update(stratum="small", record_count=60)
            elif change == "unknown":
                plan["corpora"][0]["corpus_id"] = "corpus-999"
            else:
                plan["schema_version"] = 2
            with self.subTest(plan_change=change), self.assertRaises(BenchmarkError):
                self.summarize(plan=plan)
        for samples in (0, -1, True, 1.5):
            with self.assertRaises(BenchmarkError):
                self.summarize(samples=samples)


if __name__ == "__main__":
    unittest.main()
