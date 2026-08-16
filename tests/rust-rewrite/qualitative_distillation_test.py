#!/usr/bin/env python3
"""Unit tests for qualitative-distillation scoring and blinding."""

from __future__ import annotations

import unittest

from qualitative_distillation import CASES, blind_outputs, score_cluster, score_text


class QualitativeDistillationTest(unittest.TestCase):
    def test_scoring_detects_retention_forbidden_claims_and_novel_numbers(self) -> None:
        case = CASES[0]
        cluster = {
            "outcome": "distilled",
            "ids": ["one"],
            "title": "Orchid 4.2 rollback",
            "tags": ["orchid"],
            "body": "At 22:00, use orchidctl rollback --to 4.1 after 11 minutes. Kubernetes is required.",
        }
        result = score_cluster(
            case, cluster, {"one": "Orchid 4.2 at 22:00; rollback to 4.1."}
        )
        self.assertTrue(result["decision_correct"])
        self.assertIn("orchidctl rollback --to 4.1", result["required_hits"])
        self.assertIn("kubernetes", result["forbidden_hits"])
        self.assertEqual(result["novel_numbers"], ["11"])

    def test_blinding_is_deterministic_and_balanced_across_pairs(self) -> None:
        cases = {
            case.name: {
                "outcome": "distilled" if case.cohesive else "rejected",
                "title": case.name,
                "tags": [],
                "body": "body",
            }
            for case in CASES
        }
        runs = [{"run": 1, "go": {"cases": cases}, "rust": {"cases": cases}}]
        first, first_key = blind_outputs(runs, "seed")
        second, second_key = blind_outputs(runs, "seed")
        self.assertEqual(first, second)
        self.assertEqual(first_key, second_key)
        self.assertEqual(len(first["pairs"]), len(CASES))
        self.assertEqual(set(first_key.values()), {"go", "rust"})

    def test_scoring_separates_source_reference_leaks_from_novel_numbers(self) -> None:
        case = CASES[-1]
        result = score_text(
            case,
            "distilled",
            "Atlas discrepancy, Trace 20260817-atlas-dashboard",
            ["atlas-ambiguity-synthetic-quality-eval"],
            "During 20260817, Atlas showed 256 MiB and 512 MiB; the value is unresolved.",
            "Atlas showed 256 MiB and 512 MiB; the value is unresolved.",
        )
        self.assertEqual(result["novel_numbers"], [])
        self.assertTrue(result["source_reference_leak"])
        self.assertEqual(result["source_id_mentions"], ["20260817-atlas-dashboard"])
        self.assertTrue(result["schema_degraded"])

    def test_required_term_uses_semantic_stem(self) -> None:
        case = CASES[2]
        result = score_text(
            case,
            "distilled",
            "Harbor root cause",
            ["harbor"],
            "The CPU hypothesis faced rejection; connection exhaustion was the cause. "
            "CPU was 34 percent, utilization 100 percent, pool 80 to 120, and p95 840 to 210 ms.",
            "source",
        )
        self.assertIn("reject", result["required_hits"])


if __name__ == "__main__":
    unittest.main()
