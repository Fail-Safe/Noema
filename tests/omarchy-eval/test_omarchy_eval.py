#!/usr/bin/env python3

from __future__ import annotations

import json
import os
import stat
import sys
import tempfile
import unittest
from unittest import mock
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from omarchy_eval_lib import (  # noqa: E402
    CONDITIONS,
    DIRECTIONS,
    SCENARIOS,
    build_batch_cases,
    build_corpus,
    checkpoint_decision,
    corpus_variant_for,
    paired_bootstrap_interval,
    parse_jsonl_events,
    records_by_id,
    render_capture_prompt,
    render_prompt,
    safe_restore_absent,
    score_case,
    sanitized_row,
    snapshot_matches,
    snapshot_path,
    summarize_rows,
)
import benchmark as benchmark_cli  # noqa: E402
import prefetch_hook  # noqa: E402
import reuse_smoke  # noqa: E402


class CorpusTests(unittest.TestCase):
    def test_config_requires_explicit_models_before_launch(self) -> None:
        template = json.loads(benchmark_cli.DEFAULT_CONFIG.read_text())
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "benchmark.json"
            config.write_text(json.dumps(template))
            with self.assertRaisesRegex(benchmark_cli.BenchmarkError, "explicit codex model"):
                benchmark_cli.load_config(config)
            template["models"]["codex"] = "fixture-model"
            config.write_text(json.dumps(template))
            with self.assertRaisesRegex(benchmark_cli.BenchmarkError, "explicit opencode model"):
                benchmark_cli.load_config(config)
            template["models"]["opencode"] = "openai/fixture-model"
            config.write_text(json.dumps(template))
            self.assertEqual(benchmark_cli.load_config(config)["models"], template["models"])

    def test_corpus_has_preregistered_shape(self) -> None:
        corpus = build_corpus("alpha", "NATIVE-CODEX")
        self.assertEqual(len(corpus), 60)
        categories = {}
        for record in corpus:
            categories[record.category] = categories.get(record.category, 0) + 1
        self.assertEqual(categories["global_preference"], 12)
        self.assertEqual(categories["decision"], 12)
        self.assertEqual(categories["project_a_fact"], 12)
        self.assertEqual(categories["project_b_fact"], 12)
        self.assertEqual(categories["supersession"], 12)
        self.assertEqual(sum(not record.current for record in corpus), 6)

    def test_lexical_assignment_swaps_between_batches(self) -> None:
        for direction in DIRECTIONS:
            self.assertNotEqual(
                corpus_variant_for(1, direction, "native"),
                corpus_variant_for(1, direction, "noema"),
            )
            self.assertNotEqual(
                corpus_variant_for(1, direction, "native"),
                corpus_variant_for(2, direction, "native"),
            )

    def test_batch_is_balanced_and_exactly_24_turns(self) -> None:
        cases = build_batch_cases(1)
        self.assertEqual(len(cases), 24)
        observed = {(case.condition, case.direction, case.scenario) for case in cases}
        expected = {
            (condition, direction, scenario)
            for condition in CONDITIONS
            for direction in DIRECTIONS
            for scenario in SCENARIOS
        }
        self.assertEqual(observed, expected)
        self.assertEqual(len({case.case_id for case in cases}), 24)

    def test_second_corpus_cycle_mirrors_assignment_without_changing_batch_id(self) -> None:
        first = build_batch_cases(1)
        mirrored = build_batch_cases(1, corpus_offset=1)
        second = build_batch_cases(2)
        self.assertEqual(
            [case.corpus_variant for case in mirrored],
            [case.corpus_variant for case in second],
        )
        self.assertEqual(
            [case.case_id for case in mirrored],
            [case.case_id for case in first],
        )
        self.assertTrue(all(case.batch == 1 for case in mirrored))


class EventParserTests(unittest.TestCase):
    def test_codex_events_extract_final_tools_and_usage(self) -> None:
        events = "\n".join(
            [
                json.dumps({"type": "thread.started", "thread_id": "private-session"}),
                json.dumps({"type": "item.completed", "item": {"type": "mcp_tool_call", "server": "noema", "name": "get_instructions"}}),
                json.dumps({"type": "item.completed", "item": {"type": "agent_message", "text": '{"answers":[]}'}}),
                json.dumps({"type": "turn.completed", "usage": {"input_tokens": 123, "cached_input_tokens": 20, "output_tokens": 9}}),
            ]
        )
        parsed = parse_jsonl_events(events, "codex")
        self.assertEqual(parsed.final_text, '{"answers":[]}')
        self.assertIn("noema/get_instructions", parsed.tool_names)
        self.assertEqual(parsed.input_tokens, 123)
        self.assertEqual(parsed.cached_input_tokens, 20)
        self.assertEqual(parsed.noncached_input_tokens, 103)
        self.assertEqual(parsed.output_tokens, 9)
        self.assertEqual(parsed.total_tokens, 132)
        self.assertEqual(parsed.session_id, "private-session")

    def test_codex_tool_lifecycle_counts_once(self) -> None:
        events = "\n".join(
            json.dumps(
                {
                    "type": event_type,
                    "item": {"id": "item_7", "type": "mcp_tool_call", "server": "noema", "tool": "get_trace"},
                }
            )
            for event_type in ("item.started", "item.completed")
        )
        parsed = parse_jsonl_events(events, "codex")
        self.assertEqual(parsed.tool_names, ["noema/get_trace"])

    def test_opencode_events_extract_part_text_cost_and_tool(self) -> None:
        events = "\n".join(
            [
                json.dumps({"type": "part", "sessionID": "secret", "part": {"type": "tool", "tool": "get_instructions"}}),
                json.dumps({"type": "part", "part": {"type": "text", "text": '{"answers":[]}', "usage": {"input": 77, "output": 5, "cost": 0.01}}}),
            ]
        )
        parsed = parse_jsonl_events(events, "opencode")
        self.assertEqual(parsed.final_text, '{"answers":[]}')
        self.assertIn("get_instructions", parsed.tool_names)
        self.assertEqual(parsed.input_tokens, 77)
        self.assertEqual(parsed.noncached_input_tokens, 77)
        self.assertEqual(parsed.total_tokens, 82)
        self.assertEqual(parsed.cost_usd, 0.01)

    def test_current_opencode_step_finish_shape(self) -> None:
        events = "\n".join(
            [
                json.dumps({"type": "tool_use", "sessionID": "private", "part": {"id": "prt_123", "type": "tool", "tool": "search_traces", "state": {"status": "completed"}}}),
                json.dumps({"type": "text", "sessionID": "private", "part": {"type": "text", "text": '{"answers":[]}'}}),
                json.dumps({"type": "step_finish", "sessionID": "private", "part": {"type": "step-finish", "cost": 0.012, "tokens": {"total": 284, "input": 200, "output": 30, "reasoning": 4, "cache": {"read": 50, "write": 0}}}}),
            ]
        )
        parsed = parse_jsonl_events(events, "opencode")
        self.assertEqual(parsed.input_tokens, 200)
        self.assertEqual(parsed.cached_input_tokens, 50)
        self.assertEqual(parsed.noncached_input_tokens, 200)
        self.assertEqual(parsed.output_tokens, 30)
        self.assertEqual(parsed.total_tokens, 284)
        self.assertEqual(parsed.cost_usd, 0.012)
        self.assertEqual(parsed.tool_names, ["search_traces"])

    def test_opencode_timing_separates_tool_time_from_inter_event_gap(self) -> None:
        events = "\n".join(
            [
                json.dumps({"type": "tool_use", "part": {"id": "tool-1", "type": "tool", "tool": "noema_get_instructions", "state": {"time": {"start": 1000, "end": 1007}}}}),
                json.dumps({"type": "tool_use", "part": {"id": "tool-2", "type": "tool", "tool": "recall_context", "state": {"time": {"start": 406834, "end": 406837}}}}),
                json.dumps({"type": "tool_use", "part": {"id": "tool-3", "type": "tool", "tool": "create_trace", "state": {"time": {"start": 406900, "end": 406916}}}}),
                json.dumps({"type": "text", "part": {"id": "text-1", "type": "text", "text": '{"answers":[]}', "time": {"start": 406950, "end": 406970}}}),
            ]
        )
        parsed = parse_jsonl_events(events, "opencode")
        self.assertEqual(parsed.tool_duration_ms, 26)
        self.assertEqual(parsed.noema_tool_duration_ms, 26)
        self.assertEqual(parsed.max_inter_event_gap_ms, 405827)
        self.assertEqual(parsed.timed_span_count, 4)

    def test_opencode_usage_sums_multiple_model_steps(self) -> None:
        events = "\n".join(
            json.dumps(
                {
                    "type": "step_finish",
                    "sessionID": "private",
                    "part": {
                        "type": "step-finish",
                        "cost": cost,
                        "tokens": {"input": inp, "output": out, "reasoning": 0, "cache": {"read": cached, "write": 0}},
                    },
                }
            )
            for inp, out, cached, cost in ((100, 10, 20, 0.01), (80, 8, 5, 0.02))
        )
        parsed = parse_jsonl_events(events, "opencode")
        self.assertEqual(parsed.input_tokens, 180)
        self.assertEqual(parsed.cached_input_tokens, 25)
        self.assertEqual(parsed.noncached_input_tokens, 180)
        self.assertEqual(parsed.output_tokens, 18)
        self.assertEqual(parsed.total_tokens, 223)
        self.assertAlmostEqual(parsed.cost_usd or 0, 0.03)

    def test_non_json_line_is_reported(self) -> None:
        parsed = parse_jsonl_events("warning\n{}\n", "codex")
        self.assertEqual(len(parsed.parse_errors), 1)


class ScoringTests(unittest.TestCase):
    def setUp(self) -> None:
        self.case = next(
            case for case in build_batch_cases(1)
            if case.condition == "noema"
            and case.direction == "codex_to_opencode"
            and case.scenario == "supersession"
        )
        self.corpus = build_corpus(self.case.corpus_variant, "NOEMA-CODEX")
        self.tokens = {record.value for record in self.corpus}

    def test_current_value_scores_and_old_value_does_not(self) -> None:
        question = self.case.questions[0]
        answer = {
            "answers": [
                {
                    "question_id": question.question_id,
                    "answer": question.expected_values[0],
                    "rationale": question.expected_rationales[0],
                    "provenance": list(question.expected_provenance),
                    "abstain": False,
                }
            ]
        }
        score = score_case(self.case, json.dumps(answer), self.tokens)
        self.assertEqual(score["macro_f1"], 1.0)
        self.assertEqual(score["stale_count"], 0)
        answer["answers"][0]["answer"] = question.forbidden_values[0]
        stale = score_case(self.case, json.dumps(answer), self.tokens)
        self.assertEqual(stale["stale_count"], 1)
        self.assertEqual(stale["leakage_count"], 0)
        self.assertLess(stale["macro_f1"], 1.0)

    def test_invalid_shape_fails_closed_without_raw_response(self) -> None:
        score = score_case(self.case, '{"answers":[{"question_id":"x"}]}', self.tokens)
        self.assertFalse(score["valid_response"])
        self.assertEqual(score["macro_f1"], 0.0)
        self.assertNotIn("response", score)

    def test_wrong_project_distractor_counts_as_scope_leak(self) -> None:
        case = next(
            candidate for candidate in build_batch_cases(1)
            if candidate.condition == "noema"
            and candidate.direction == "codex_to_opencode"
            and candidate.scenario == "scope_distractor_abstention"
        )
        question = case.questions[0]
        payload = {
            "answers": [
                {
                    "question_id": question.question_id,
                    "answer": question.forbidden_values[0],
                    "rationale": None,
                    "provenance": [],
                    "abstain": False,
                },
                {
                    "question_id": case.questions[1].question_id,
                    "answer": "",
                    "rationale": None,
                    "provenance": [],
                    "abstain": True,
                },
            ]
        }
        score = score_case(case, json.dumps(payload), self.tokens)
        self.assertEqual(score["leakage_count"], 1)


class SnapshotTests(unittest.TestCase):
    def test_snapshot_detects_byte_and_mode_change(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config"
            path.write_bytes(b"before\n")
            os.chmod(path, 0o640)
            snapshot = snapshot_path(path)
            self.assertTrue(snapshot_matches(snapshot))
            path.write_bytes(b"after\n")
            self.assertFalse(snapshot_matches(snapshot))

    def test_directory_snapshot_is_independent_of_embedded_bytes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "tree"
            root.mkdir()
            (root / "one").write_bytes(b"value")
            snapshot = snapshot_path(root)
            self.assertTrue(snapshot_matches(snapshot))

    def test_safe_restore_only_removes_run_scoped_path(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            parent = Path(directory) / "eval"
            target = parent / "run-123"
            target.mkdir(parents=True)
            (target / "owned").write_text("x", encoding="utf-8")
            safe_restore_absent(target, "run-123", [parent])
            self.assertFalse(target.exists())


def synthetic_row(condition: str, scenario: str, score: float, batch: int = 1) -> dict[str, object]:
    return {
        "condition": condition,
        "scenario": scenario,
        "batch": batch,
        "track": "deterministic",
        "direction": "codex_to_opencode",
        "macro_f1": score,
        "exact_accuracy": score,
        "rationale_accuracy": score,
        "provenance_accuracy": score,
        "stale_count": 0,
        "hallucination_count": 0,
        "leakage_count": 0,
        "answer_count": 1,
        "tool_call_count": 0 if condition == "native" else 2,
        "input_tokens": 100,
        "output_tokens": 20,
        "cost_usd": 0.01,
        "latency_seconds": 1.0,
        "status": "completed",
    }


class AnalysisTests(unittest.TestCase):
    def test_cost_median_requires_complete_condition_coverage(self) -> None:
        rows = []
        for condition in CONDITIONS:
            first = synthetic_row(condition, SCENARIOS[0], 1.0)
            second = synthetic_row(condition, SCENARIOS[1], 1.0)
            second["cost_usd"] = None
            rows.extend((first, second))
        summary = summarize_rows(rows, 50)
        for condition in CONDITIONS:
            self.assertEqual(summary["conditions"][condition]["cost_observed_turns"], 1)
            self.assertIsNone(summary["conditions"][condition]["cost_median_usd"])

    def test_bootstrap_interval_is_deterministic(self) -> None:
        first = paired_bootstrap_interval([0.1, 0.2, -0.1], 500)
        second = paired_bootstrap_interval([0.1, 0.2, -0.1], 500)
        self.assertEqual(first, second)

    def test_futility_uses_preregistered_wording(self) -> None:
        rows = []
        for scenario in SCENARIOS:
            rows.append(synthetic_row("native", scenario, 0.80))
            rows.append(synthetic_row("noema", scenario, 0.81))
        summary = summarize_rows(rows, 500)
        decision = checkpoint_decision(summary)
        self.assertEqual(decision["recommendation"], "stop_futility")
        self.assertEqual(decision["conclusion"], "No demonstrated meaningful advantage.")

    def test_no_success_before_192_turns(self) -> None:
        rows = []
        for scenario in SCENARIOS:
            rows.append(synthetic_row("native", scenario, 0.50))
            rows.append(synthetic_row("noema", scenario, 1.00))
        summary = summarize_rows(rows, 500)
        decision = checkpoint_decision(summary)
        self.assertFalse(decision["success_checks"]["minimum_192_turns"])
        self.assertNotEqual(decision["recommendation"], "success_candidate")


class HarnessControlTests(unittest.TestCase):
    def test_smoke_reuse_requires_identical_locked_state_and_clean_restoration(self) -> None:
        locked = {
            field: {"value": field}
            for field in reuse_smoke.LOCKED_STATE_FIELDS
        }
        source = {
            **locked,
            "run_id": "source",
            "status": "restored",
            "smoke": {
                "status": "passed",
                "turns": 4,
                "model_turns": 8,
                "failures": [],
                "restoration": {
                    "managed_restored": True,
                    "guard_files_unchanged": True,
                    "failures": [],
                    "guard_drift": [],
                },
            },
        }
        target = {
            **locked,
            "run_id": "target",
            "status": "preflight_passed",
            "smoke": {"status": "not_run"},
            "batches": {},
            "checkpoints": {},
        }
        smoke = {"run_id": "source", "status": "passed", "failures": []}
        restoration = {
            "run_id": "source",
            "managed_restored": True,
            "guard_files_unchanged": True,
            "failures": [],
            "guard_drift": [],
            "pinned_tools_removed": True,
        }
        reuse_smoke.validate_smoke_reuse(source, target, smoke, restoration)

        mismatched = {**target, "harness_sha256": {"value": "changed"}}
        with self.assertRaisesRegex(reuse_smoke.BenchmarkError, "harness_sha256"):
            reuse_smoke.validate_smoke_reuse(source, mismatched, smoke, restoration)

    def test_retrieval_profile_is_explicitly_preregistered(self) -> None:
        parser = benchmark_cli.build_parser()
        standard = parser.parse_args(["preflight"])
        prefetch = parser.parse_args(
            ["preflight", "--retrieval-profile", "prefetch"]
        )
        self.assertEqual(standard.retrieval_profile, "standard")
        self.assertEqual(prefetch.retrieval_profile, "prefetch")
        natural = parser.parse_args([
            "preflight",
            "--retrieval-profile",
            "prefetch",
            "--track",
            "natural",
            "--evidence-run",
            "source-run",
            "--evidence-checkpoint",
            "4",
        ])
        self.assertEqual(natural.track, "natural")
        self.assertEqual(natural.evidence_checkpoint, 4)
        self.assertEqual(
            benchmark_cli.retrieval_profile_for({"retrieval_profile": "prefetch"}),
            "prefetch",
        )
        with self.assertRaises(benchmark_cli.BenchmarkError):
            benchmark_cli.retrieval_profile_for({"retrieval_profile": "mixed"})

    def test_prefetch_checkpoint_metrics_are_local_and_aggregated(self) -> None:
        rows = [
            {
                "condition": "noema",
                "retrieval_profile": "prefetch",
                "prefetch_success": True,
                "prefetch_latency_ms": latency,
                "prefetch_hit_count": hits,
                "prefetch_context_chars": chars,
            }
            for latency, hits, chars in ((10.0, 1, 900), (20.0, 3, 1300))
        ]
        metrics = benchmark_cli.summarize_prefetch_metrics(rows)
        self.assertIsNotNone(metrics)
        assert metrics is not None
        self.assertEqual(metrics["successful_turns"], 2)
        self.assertEqual(metrics["latency_median_ms"], 15.0)
        self.assertEqual(metrics["hit_count_mean"], 2.0)
        self.assertEqual(metrics["context_chars_mean"], 1100.0)

    def test_prompt_defines_schema_valid_abstention(self) -> None:
        prompt = render_prompt(build_batch_cases(1)[0])
        self.assertIn('answer to the empty string "" (never null)', prompt)

    def test_rationale_scoring_is_explicit_in_question_text(self) -> None:
        for case in build_batch_cases(1):
            for question in case.questions:
                if "rationale" in question.dimensions:
                    self.assertIn("rationale", question.text.lower())

    def test_continuation_authorization_is_explicit_and_persistent(self) -> None:
        state = {
            "checkpoints": {"1": {"status": "reported"}},
            "continuation_authorizations": [],
        }
        with self.assertRaises(benchmark_cli.BenchmarkError):
            benchmark_cli.authorize_checkpoint(state, 1, None)
        benchmark_cli.authorize_checkpoint(state, 1, 1)
        self.assertEqual(state["continuation_authorizations"][0]["checkpoint"], 1)
        benchmark_cli.authorize_checkpoint(state, 1, None)
        self.assertEqual(len(state["continuation_authorizations"]), 1)

    def test_sanitized_row_excludes_private_content(self) -> None:
        case = build_batch_cases(1)[0]
        parsed = benchmark_cli.ParsedEvents(
            final_text="private answer",
            tool_names=["private/tool", "private/tool"],
            session_id="private-session",
            input_tokens=10,
            output_tokens=2,
        )
        score = {
            "valid_response": True,
            "macro_f1": 1.0,
            "exact_accuracy": 1.0,
            "rationale_accuracy": None,
            "provenance_accuracy": 1.0,
            "dimension_scores": {},
            "stale_count": 0,
            "hallucination_count": 0,
            "leakage_count": 0,
            "answer_count": 1,
        }
        row = sanitized_row(case, parsed, score, status="completed", latency_seconds=1.0, error_code=None)
        encoded = json.dumps(row)
        self.assertNotIn("private answer", encoded)
        self.assertNotIn("private-session", encoded)
        self.assertNotIn("private/tool", encoded)

    def test_client_commands_are_fresh_and_pinned(self) -> None:
        config = {
            "paths": {
                "private_root": "/tmp/private",
                "reports_root": "/tmp/reports",
                "work_parent": "/tmp/work",
                "outside_parent": "/tmp/outside",
            },
            "models": {"codex": "fixture-model", "opencode": "openai/fixture-model", "reasoning": "medium"},
        }
        run = benchmark_cli.Run(config, "run-123")
        state = {"commands": {"codex": "/bin/codex", "opencode": "/bin/opencode"}}
        noema_codex = next(
            case for case in build_batch_cases(1)
            if case.condition == "noema" and case.receiver == "codex"
        )
        codex = benchmark_cli.client_argv(run, state, noema_codex, Path("/tmp/work"))
        self.assertIn("--ephemeral", codex)
        self.assertNotIn("--ignore-user-config", codex)
        self.assertIn("--dangerously-bypass-hook-trust", codex)
        self.assertIn("--add-dir", codex)
        self.assertEqual(codex[codex.index("--sandbox") + 1], "workspace-write")
        self.assertIn('model_reasoning_effort="medium"', codex)
        self.assertTrue(any("env.XDG_CONFIG_HOME" in value for value in codex))
        self.assertTrue(any("env.XDG_STATE_HOME" in value for value in codex))
        for tool in (
            "get_instructions",
            "cortex_usage",
            "recall_context",
            "search_traces",
            "list_traces",
            "get_trace",
        ):
            self.assertIn(
                f'mcp_servers.noema.tools.{tool}.approval_mode="approve"',
                codex,
            )
        self.assertFalse(any("create_trace" in value for value in codex))
        self.assertFalse(any("update_trace" in value for value in codex))
        self.assertNotIn("resume", codex)
        minimal_codex = benchmark_cli.client_argv(
            run,
            state,
            noema_codex,
            Path("/tmp/work"),
            retrieval_profile="fastpath-minimal",
        )
        self.assertIn(
            'mcp_servers.noema.env.NOEMA_MCP_TOOL_PROFILE="continuity-read"',
            minimal_codex,
        )
        native_opencode = next(
            case for case in build_batch_cases(1)
            if case.condition == "native" and case.receiver == "opencode"
        )
        opencode = benchmark_cli.client_argv(run, state, native_opencode, Path("/tmp/work"))
        self.assertIn("openai/fixture-model", opencode)
        self.assertIn("medium", opencode)
        self.assertNotIn("--continue", opencode)
        self.assertNotIn("--session", opencode)

        prefetch_codex = benchmark_cli.client_argv(
            run,
            state,
            noema_codex,
            Path("/tmp/work"),
            retrieval_profile="prefetch",
        )
        self.assertIn("--dangerously-bypass-hook-trust", prefetch_codex)
        self.assertNotIn("--add-dir", prefetch_codex)
        self.assertFalse(any("mcp_servers.noema" in value for value in prefetch_codex))

        natural_capture = next(
            case
            for case in build_batch_cases(1, "natural")
            if case.condition == "noema" and case.source == "codex"
        )
        capture_codex = benchmark_cli.client_argv(
            run,
            state,
            natural_capture,
            Path("/tmp/work"),
            capture=True,
            retrieval_profile="prefetch",
        )
        self.assertFalse(any("mcp_servers.noema" in value for value in capture_codex))
        self.assertIn("--add-dir", capture_codex)
        self.assertIn("--approve-for-me", capture_codex)
        self.assertNotIn("--sandbox", capture_codex)
        self.assertNotIn("--profile", capture_codex)
        wrapped_capture = benchmark_cli.product_capture_argv(
            run,
            {"commands": {"noema": "/bin/noema", "codex": "/bin/codex"}},
            natural_capture,
            capture_codex,
        )
        self.assertEqual(wrapped_capture[0], "/bin/noema")
        self.assertIn("capture", wrapped_capture)
        self.assertEqual(
            wrapped_capture[wrapped_capture.index("--client-binary") + 1],
            "/bin/codex",
        )
        self.assertEqual(wrapped_capture[wrapped_capture.index("--") + 1], "exec")

        natural_retrieval = next(
            case
            for case in build_batch_cases(1, "natural")
            if case.condition == "noema" and case.receiver == "codex"
        )
        retrieval_codex = benchmark_cli.client_argv(
            run,
            state,
            natural_retrieval,
            Path("/tmp/work"),
            retrieval_profile="prefetch",
        )
        self.assertEqual(
            retrieval_codex[retrieval_codex.index("--profile") + 1],
            "noema-continuity",
        )
        self.assertFalse(any("create_trace" in value for value in retrieval_codex))

    def test_fastpath_prompt_requires_one_batched_recall(self) -> None:
        case = next(
            case
            for case in build_batch_cases(1)
            if case.condition == "noema"
        )
        prompt = render_prompt(case, "fastpath-minimal")
        self.assertIn("Call recall_context exactly once", prompt)
        self.assertIn("do not call get_instructions", prompt)

    def test_prefetch_prompt_prohibits_tool_calls(self) -> None:
        case = next(
            case
            for case in build_batch_cases(1)
            if case.condition == "noema"
        )
        prompt = render_prompt(case, "prefetch")
        self.assertIn("prefetched Noema memory", prompt)
        self.assertIn("Do not call tools", prompt)

    def test_prefetch_codex_config_is_hook_only(self) -> None:
        hooks = benchmark_cli.codex_prefetch_hooks()
        prompt = hooks["hooks"]["UserPromptSubmit"][0]["hooks"][0]["command"]
        preferences = hooks["hooks"]["SessionStart"][0]["hooks"][0]["command"]
        self.assertIn("prefetch_hook.py", prompt)
        self.assertIn(" codex", prompt)
        self.assertIn("codex-preferences", preferences)
        self.assertEqual(
            hooks["hooks"]["UserPromptSubmit"][0]["hooks"][0]["additionalContextLimit"],
            2500,
        )

    def test_runtime_environment_disables_updates_and_isolates_opencode_capture(self) -> None:
        config = {
            "paths": {
                "private_root": "/tmp/private",
                "reports_root": "/tmp/reports",
                "work_parent": "/tmp/work",
                "outside_parent": "/tmp/outside",
            }
        }
        run = benchmark_cli.Run(config, "run-123")
        case = next(
            candidate
            for candidate in build_batch_cases(1, "natural")
            if candidate.condition == "noema" and candidate.source == "opencode"
        )
        retrieval = run.runtime_env(case)
        capture = run.runtime_env(case, capture=True)
        self.assertEqual(retrieval["OPENCODE_DISABLE_AUTOUPDATE"], "1")
        self.assertEqual(capture["OPENCODE_DISABLE_AUTOUPDATE"], "1")
        self.assertEqual(retrieval["OPENCODE_DISABLE_CLAUDE_CODE_PROMPT"], "1")
        self.assertEqual(capture["OPENCODE_DISABLE_CLAUDE_CODE_PROMPT"], "1")
        self.assertEqual(retrieval["XDG_CONFIG_HOME"], capture["XDG_CONFIG_HOME"])
        self.assertNotIn("OPENCODE_CONFIG", retrieval)
        self.assertNotIn("OPENCODE_CONFIG_DIR", retrieval)
        self.assertNotIn("OPENCODE_DISABLE_PROJECT_CONFIG", retrieval)
        self.assertTrue(capture["OPENCODE_CONFIG"].endswith("opencode.jsonc"))
        self.assertTrue(capture["OPENCODE_CONFIG_DIR"].endswith(".opencode"))
        self.assertEqual(capture["OPENCODE_DISABLE_PROJECT_CONFIG"], "1")
        self.assertEqual(capture["NOEMA_MCP_TOOL_PROFILE"], "continuity-capture")
        self.assertNotIn("NOEMA_MCP_TOOL_PROFILE", retrieval)
        native_case = next(
            candidate
            for candidate in build_batch_cases(1, "natural")
            if candidate.condition == "native" and candidate.source == "opencode"
        )
        self.assertIn("OPENCODE_CONFIG", run.runtime_env(native_case, capture=True))

    def test_run_cases_apply_preregistered_corpus_cycle(self) -> None:
        mirrored = benchmark_cli.cases_for_run(
            {"corpus_cycle": 2}, 1, "natural"
        )
        expected = build_batch_cases(1, "natural", corpus_offset=1)
        self.assertEqual(
            [case.corpus_variant for case in mirrored],
            [case.corpus_variant for case in expected],
        )

    def test_native_capture_target_plan_is_paired_and_balanced(self) -> None:
        planned = benchmark_cli.native_capture_target_cases({"corpus_cycle": 1})
        self.assertEqual(len(planned), 8)
        self.assertEqual(
            [item["target_state"] for item in planned],
            ["absent", "preseeded", "preseeded", "absent", "preseeded", "absent", "absent", "preseeded"],
        )
        for pair_id in {item["pair_id"] for item in planned}:
            pair = [item for item in planned if item["pair_id"] == pair_id]
            self.assertEqual({item["target_state"] for item in pair}, {"absent", "preseeded"})
            self.assertEqual(len({item["case"].scenario for item in pair}), 1)
            self.assertEqual(len({item["case"].corpus_variant for item in pair}), 1)
            self.assertEqual(
                len({item["case"].questions for item in pair}),
                1,
            )
            self.assertTrue(all(item["case"].source == "opencode" for item in pair))
            self.assertTrue(all(item["case"].condition == "native" for item in pair))

    def test_preseed_native_capture_targets_creates_only_required_empty_files(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = {
                "paths": {
                    "private_root": str(root / "private"),
                    "reports_root": str(root / "reports"),
                    "work_parent": str(root / "work"),
                    "outside_parent": str(root / "outside"),
                }
            }
            run = benchmark_cli.Run(config, "run-123")
            case = next(
                item["case"]
                for item in benchmark_cli.native_capture_target_cases({"corpus_cycle": 1})
                if item["case"].scenario == "same_workspace_exact"
            )
            targets = benchmark_cli.preseed_native_capture_targets(run, case)
            self.assertEqual(len(targets), 2)
            self.assertTrue(all(target.is_file() for target in targets))
            self.assertTrue(all(target.read_bytes() == b"" for target in targets))
            project_b = run.context_paths(
                case.batch, case.track, case.direction, case.condition
            )["project-b"] / "AGENTS.md"
            self.assertFalse(project_b.exists())

    def test_natural_native_context_preseeds_complete_empty_hierarchy(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = {
                "paths": {
                    "private_root": str(root / "private"),
                    "reports_root": str(root / "reports"),
                    "work_parent": str(root / "work"),
                    "outside_parent": str(root / "outside"),
                }
            }
            run = benchmark_cli.Run(config, "run-123")
            state = {"corpus_cycle": 1}
            paths = benchmark_cli.prepare_context(
                run,
                state,
                1,
                "natural",
                "codex_to_opencode",
                "native",
                retrieval_profile="prefetch",
            )
            targets = [
                paths["common"] / "AGENTS.md",
                paths["project-a"] / "AGENTS.md",
                paths["project-b"] / "AGENTS.md",
            ]
            self.assertTrue(all(target.is_file() for target in targets))
            self.assertTrue(all(target.read_bytes() == b"" for target in targets))
            context = state["contexts"]["b01-natural-codex_to_opencode-native"]
            self.assertTrue(context["native_targets_preseeded"])

    def test_native_capture_target_summary_keeps_failures_as_results(self) -> None:
        rows = [
            {
                "pair_id": "pair-01",
                "target_state": "absent",
                "capture_status": "failed",
                "capture_error_code": "capture_scope_file_missing",
                "capture_latency_seconds": 10.0,
                "capture_total_tokens": 100,
            },
            {
                "pair_id": "pair-01",
                "target_state": "preseeded",
                "capture_status": "completed",
                "capture_error_code": None,
                "capture_latency_seconds": 8.0,
                "capture_total_tokens": 80,
            },
        ]
        summary = benchmark_cli.summarize_native_capture_target_rows(rows)
        self.assertEqual(summary["arms"]["absent"]["capture_success_rate"], 0.0)
        self.assertEqual(summary["arms"]["preseeded"]["capture_success_rate"], 1.0)
        self.assertEqual(summary["preseeded_minus_absent_success_rate"], 1.0)
        self.assertEqual(summary["paired_outcomes"]["only_preseeded_completed"], 1)

    def test_native_capture_target_command_requires_explicit_live_confirmation(self) -> None:
        parser = benchmark_cli.build_parser()
        args = parser.parse_args(["experiment-native-capture-target"])
        self.assertFalse(args.confirm_live)
        confirmed = parser.parse_args(
            ["experiment-native-capture-target", "--confirm-live"]
        )
        self.assertTrue(confirmed.confirm_live)

    def test_external_permissions_cover_requested_and_canonical_case(self) -> None:
        requested = Path("/Users/example/Work/noema-omarchy-eval/run-123")
        canonical = Path("/Users/example/work/noema-omarchy-eval/run-123")
        with mock.patch.object(
            benchmark_cli, "filesystem_canonical_path", return_value=canonical
        ):
            permissions = benchmark_cli.scoped_external_permissions(requested)
        self.assertEqual(
            permissions,
            {
                f"{requested}/**": "allow",
                f"{canonical}/**": "allow",
            },
        )

    def test_capture_accepts_verified_persistence_without_acknowledgement(self) -> None:
        config = {
            "paths": {
                "private_root": "/tmp/private",
                "reports_root": "/tmp/reports",
                "work_parent": "/tmp/work",
                "outside_parent": "/tmp/outside",
            }
        }
        run = benchmark_cli.Run(config, "run-123")
        case = next(
            candidate
            for candidate in build_batch_cases(1, "natural")
            if candidate.condition == "native"
        )
        parsed = benchmark_cli.ParsedEvents(final_text="")
        with (
            mock.patch.object(
                benchmark_cli,
                "execute_turn",
                return_value=(parsed, 1.25, 0, None),
            ),
            mock.patch.object(
                benchmark_cli, "validate_capture_persistence", return_value=None
            ) as validate,
        ):
            fields, error = benchmark_cli.run_capture_case(
                run, {}, case, retrieval_profile="prefetch"
            )
        self.assertIsNone(error)
        self.assertEqual(fields["capture_status"], "completed")
        validate.assert_called_once_with(run, case, parsed)

    def test_pin_commands_survives_source_replacement(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "source-tool"
            source.write_text("#!/bin/sh\necho before\n", encoding="utf-8")
            source.chmod(0o755)
            config = {
                "paths": {
                    "private_root": str(root / "private"),
                    "reports_root": str(root / "reports"),
                    "work_parent": str(root / "work"),
                    "outside_parent": str(root / "outside"),
                }
            }
            run = benchmark_cli.Run(config, "run-123")
            sources, pinned = benchmark_cli.pin_commands(run, {"client": str(source)})
            source.write_text("#!/bin/sh\necho after\n", encoding="utf-8")
            self.assertEqual(sources["client"], str(source.resolve()))
            self.assertIn("before", Path(pinned["client"]).read_text(encoding="utf-8"))

    def test_pin_commands_includes_codex_code_mode_companion(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            codex = root / "codex"
            companion = root / "codex-code-mode-host"
            for path in (codex, companion):
                path.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
                path.chmod(0o755)
            config = {
                "paths": {
                    "private_root": str(root / "private"),
                    "reports_root": str(root / "reports"),
                    "work_parent": str(root / "work"),
                    "outside_parent": str(root / "outside"),
                }
            }
            run = benchmark_cli.Run(config, "run-123")
            sources, pinned = benchmark_cli.pin_commands(run, {"codex": str(codex)})
            self.assertEqual(sources["codex-code-mode-host"], str(companion.resolve()))
            self.assertTrue(Path(pinned["codex-code-mode-host"]).is_file())

    def test_guard_drift_is_reported_by_label_without_content(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            guard = root / "config.toml"
            guard.write_text("before\n", encoding="utf-8")
            snapshot = snapshot_path(guard)
            snapshot["label"] = "codex_config"
            snapshot_path_file = root / "guards.json"
            snapshot_path_file.write_text(json.dumps([snapshot]), encoding="utf-8")
            guard.write_text("after\n", encoding="utf-8")
            drift = benchmark_cli.guard_drift({"guard_snapshot_file": str(snapshot_path_file)})
            self.assertEqual(drift[0]["label"], "codex_config")
            self.assertNotIn("before", json.dumps(drift))
            self.assertNotIn("after", json.dumps(drift))

    def test_natural_capture_prompt_separates_capture_from_retrieval(self) -> None:
        case = next(
            candidate
            for candidate in build_batch_cases(1, "natural")
            if candidate.condition == "noema"
        )
        records = build_corpus(case.corpus_variant, f"NOEMA-{case.source.upper()}")
        prompt = render_capture_prompt(case, records)
        self.assertIn("each memory as its own trace", prompt)
        self.assertIn("one create_traces call", prompt)
        self.assertIn("without follow-up reads", prompt)

    def test_natural_evidence_requires_qualified_restored_deterministic_report(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            reports = Path(directory) / "reports"
            checkpoint = reports / "source-run" / "checkpoint-004"
            checkpoint.mkdir(parents=True)
            report = {
                "retrieval_profile": "prefetch",
                "summary": {
                    "retrieval_turns": 192,
                    "paired": {
                        "noema_minus_native": 0.25,
                        "bootstrap_95_low": 0.10,
                    },
                    "conditions": {
                        "noema": {
                            "stale_rate": 0.0,
                            "hallucination_rate": 0.0,
                            "scope_leakage_rate": 0.0,
                        }
                    },
                },
                "decision": {
                    "natural_track_eligible": True,
                    "integration_unreliable": False,
                },
            }
            (checkpoint / "summary.json").write_text(json.dumps(report), encoding="utf-8")
            restoration = {
                "managed_restored": True,
                "guard_files_unchanged": False,
                "failures": [],
            }
            (reports / "source-run" / "restoration.json").write_text(
                json.dumps(restoration), encoding="utf-8"
            )
            evidence = benchmark_cli.load_natural_evidence(
                {"paths": {"reports_root": str(reports)}},
                "source-run",
                4,
            )
            self.assertEqual(evidence["retrieval_turns"], 192)
            self.assertFalse(evidence["environment_guard_stable"])
            report["summary"]["retrieval_turns"] = 144
            (checkpoint / "summary.json").write_text(json.dumps(report), encoding="utf-8")
            with self.assertRaises(benchmark_cli.BenchmarkError):
                benchmark_cli.load_natural_evidence(
                    {"paths": {"reports_root": str(reports)}},
                    "source-run",
                    4,
                )

    def test_capture_summary_and_decision_are_descriptive(self) -> None:
        rows = []
        for condition in CONDITIONS:
            row = synthetic_row(condition, SCENARIOS[0], 1.0)
            row.update({
                "track": "natural",
                "capture_status": "completed",
                "capture_latency_seconds": 2.0,
                "capture_total_tokens": 200,
                "capture_tool_call_count": 1,
                "capture_tool_duration_ms": 8,
                "capture_noema_tool_duration_ms": 8 if condition == "noema" else 0,
                "capture_max_inter_event_gap_ms": 1250,
            })
            rows.append(row)
        summary = summarize_rows(rows, 50)
        capture = benchmark_cli.summarize_capture_metrics(rows)
        self.assertIsNotNone(capture)
        assert capture is not None
        self.assertEqual(capture["conditions"]["noema"]["tool_duration_p95_ms"], 8)
        self.assertEqual(capture["conditions"]["noema"]["noema_tool_duration_p95_ms"], 8)
        self.assertEqual(capture["conditions"]["noema"]["timing_observed_turns"], 1)
        self.assertEqual(capture["conditions"]["noema"]["max_inter_event_gap_seconds"], 1.25)
        decision = benchmark_cli.natural_checkpoint_decision(summary, capture)
        self.assertEqual(decision["recommendation"], "review_complete")
        self.assertTrue(decision["descriptive_only"])


class PrefetchHookTests(unittest.TestCase):
    @mock.patch("prefetch_hook.subprocess.run")
    def test_hook_delegates_retrieval_to_native_command(self, run: mock.Mock) -> None:
        run.return_value = mock.Mock(
            returncode=0,
            stdout=json.dumps({
                "context": "[noema-prefetch-v1]\ncontext",
                "included_trace_count": 2,
                "context_chars": 36,
            }),
        )
        with mock.patch.dict(
            os.environ,
            {"NOEMA_BIN": "/bin/noema", "NOEMA_CORTEX": "test"},
        ):
            result = prefetch_hook.invoke_native("private prompt", "raw")
        self.assertEqual(result["included_trace_count"], 2)
        argv = run.call_args.args[0]
        self.assertEqual(argv[:4], ["/bin/noema", "--cortex", "test", "prefetch"])
        self.assertIn("--output", argv)
        self.assertEqual(run.call_args.kwargs["input"], "private prompt")

    def test_metrics_exclude_prompt_query_and_trace_ids(self) -> None:
        metrics = prefetch_hook.metrics_from_result(
            {"included_trace_count": 2, "context_chars": 1200},
            duration_ms=12.3456,
            success=True,
            error_code=None,
        )
        self.assertEqual(metrics["engine"], "native-rust")
        self.assertEqual(metrics["hit_count"], 2)
        self.assertEqual(metrics["duration_ms"], 12.346)
        encoded = json.dumps(metrics)
        self.assertNotIn("prompt", encoded)
        self.assertNotIn("query", encoded)
        self.assertNotIn("trace_id", encoded)

    @mock.patch("prefetch_hook.invoke_native", side_effect=RuntimeError("failed"))
    def test_hook_failure_surfaces_without_inventing_context(self, _invoke: mock.Mock) -> None:
        context, metrics = prefetch_hook.render("private prompt", "raw")
        self.assertFalse(metrics["success"])
        self.assertEqual(metrics["error_code"], "prefetch_unavailable")
        self.assertIn("Do not invent memories", context)


if __name__ == "__main__":
    unittest.main()
