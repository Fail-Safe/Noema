#!/usr/bin/env python3
"""Isolated equal-access delivery, lifecycle, and restoration mechanics."""

from __future__ import annotations

import contextlib
import copy
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))

import benchmark
from equal_access import build_equal_access_cases, render_equal_access
from omarchy_eval_lib import (
    BenchmarkError,
    ParsedEvents,
    build_corpus,
    read_json,
    sha256_file,
    snapshot_path,
    write_json,
)


def answer_payload(case, *, correct=True):
    return json.dumps({"answers": [
        {
            "question_id": question.question_id,
            "answer": " ".join(question.expected_values) if correct else "",
            "rationale": " ".join(question.expected_rationales) or None,
            "provenance": list(question.expected_provenance) if correct else [],
            "abstain": question.should_abstain if correct else False,
        }
        for question in case.questions
    ]})


class EqualAccessRuntimeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.home = self.root / "home"
        self.home.mkdir()
        home_patch = mock.patch.object(Path, "home", return_value=self.home)
        home_patch.start()
        self.addCleanup(home_patch.stop)
        command = self.root / "fixture-client"
        command.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        command.chmod(0o700)
        self.config = {
            "config_path": str(self.root / "config.json"),
            "paths": {
                "private_root": str(self.root / "private"),
                "reports_root": str(self.root / "reports"),
                "work_parent": str(self.root / "work"),
                "outside_parent": str(self.root / "outside"),
            },
            "models": {"codex": "fixture-model", "opencode": "fixture/fixture-model", "reasoning": "medium"},
            "limits": {"turn_timeout_seconds": 10, "bootstrap_samples": 20},
        }
        write_json(Path(self.config["config_path"]), self.config)
        self.run = benchmark.Run(self.config, "equal-access-fixture")
        self.run.raw_dir.mkdir(parents=True)
        self.run.report_dir.mkdir(parents=True)
        write_json(self.run.report_dir / "preflight.json", {"status": "preflight_passed"})
        self.guard = self.root / "guard"
        self.guard.write_text("original guarded bytes", encoding="utf-8")
        guard = snapshot_path(self.guard)
        guard["label"] = "fixture_guard"
        guard_file = self.run.raw_dir / "guard-snapshots.json"
        write_json(guard_file, [guard])
        commands = {name: str(command) for name in ("codex", "opencode", "noema")}
        self.state = {
            "run_id": self.run.run_id,
            "status": "preflight_passed",
            "track": "deterministic", "retrieval_profile": "prefetch", "corpus_cycle": 1,
            "config_sha256": sha256_file(Path(self.config["config_path"])),
            "harness_sha256": {path.name: sha256_file(path) for path in benchmark.HARNESS_FILES},
            "commands": commands,
            "command_sha256": {name: sha256_file(Path(path)) for name, path in commands.items()},
            "guard_snapshot_file": str(guard_file),
            "managed_snapshots": [snapshot_path(path) for path in (
                self.run.work_root, self.run.outside_root, self.run.runtime_root
            )],
            "parent_kinds": {
                str(self.run.work_parent): "absent", str(self.run.outside_parent): "absent",
            },
            "smoke": {"status": "not_run"},
            "batches": {}, "checkpoints": {}, "continuation_authorizations": [],
        }
        self.run.save_state(self.state)
        self.executed = []

    def args(self, phase="smoke", *, confirm=True, explicit_id=True):
        argv = ["--run-id", self.run.run_id] if explicit_id else []
        argv += ["experiment-equal-access", "--phase", phase]
        if confirm:
            argv.append("--confirm-live")
        return benchmark.build_parser().parse_args(argv)

    def phase(self, phase="smoke", *, correct=True, fail_at=None, tools=None):
        def execute(run, state, case, **kwargs):
            self.executed.append(case)
            reserved = run.load_state()
            self.assertEqual(reserved["experiment_kind"], "equal_access")
            self.assertEqual(reserved["experiments"]["equal_access"][phase]["status"], "running")
            self.assertTrue(benchmark.case_workspace(run, case).is_dir())
            if fail_at == len(self.executed):
                raise RuntimeError("private exception text must not be published")
            if case.condition == "noema":
                write_json(benchmark.prefetch_metrics_path(run, case), {
                    "success": True, "engine": "fixture", "duration_ms": 2,
                    "hit_count": 4, "context_chars": 80,
                })
            if case.condition == "markdown":
                paths = run.context_paths(case.batch, case.track, case.direction, "markdown")
                self.assertEqual((paths[case.workspace_role] / "AGENTS.md").read_text(), render_equal_access(case))
            return ParsedEvents(
                final_text=answer_payload(case, correct=correct),
                tool_names=list(tools or []), input_tokens=10, output_tokens=5,
            ), 0.01, 0, None

        with mock.patch.object(benchmark, "execute_turn", side_effect=execute), mock.patch.object(
            benchmark, "run_noema", return_value=subprocess.CompletedProcess([], 0, "", "")
        ), contextlib.redirect_stdout(io.StringIO()):
            return benchmark.cmd_equal_access_experiment(self.args(phase), self.config)

    def assert_restored(self):
        for path in (self.run.work_root, self.run.outside_root, self.run.runtime_root):
            self.assertFalse(path.exists(), path)
        self.assertEqual(self.guard.read_text(), "original guarded bytes")
        state = self.run.load_state()
        self.assertEqual(state["contexts"], {})

    def test_markdown_full_corpus_in_sibling_and_outside_without_integrations(self):
        cases = [case for case in build_equal_access_cases() if case.condition == "markdown"]
        case = cases[0]
        with mock.patch.object(benchmark, "run_noema") as noema:
            paths = benchmark.prepare_equal_access_markdown(self.run, self.state, case)
        noema.assert_not_called()
        expected = render_equal_access(case)
        records = build_corpus(case.corpus_variant, f"NOEMA-{case.source.upper()}")
        self.assertEqual(len(records), 60)
        for role in ("project-a", "project-b", "outside"):
            memory = (paths[role] / "AGENTS.md").read_text()
            self.assertEqual(memory, expected)
            for record in records:
                self.assertIn(record.value, memory)
                self.assertIn(record.provenance, memory)
            self.assertFalse((paths[role] / ".noema").exists())
            self.assertFalse((paths[role] / "opencode.jsonc").exists())
        self.assertFalse((paths["common"] / "AGENTS.md").exists())
        self.assertFalse(self.run.context_paths(1, "deterministic", case.direction, "native")["common"].exists())
        other_direction = next(other.direction for other in cases if other.direction != case.direction)
        self.assertFalse(self.run.context_paths(1, "deterministic", other_direction, "markdown")["common"].exists())
        home = self.run.codex_home(case.batch, case.track, case.direction, "markdown")
        self.assertEqual(list(home.iterdir()), [])
        client_root = self.run.markdown_client_root(case)
        self.assertEqual(read_json(client_root / "opencode.json"), {"mcp": {}, "plugin": []})
        self.assertEqual(list((client_root / ".opencode").iterdir()), [])
        self.assertFalse((self.run.runtime_root / "cortexes").exists())
        self.assertFalse((home / "hooks.json").exists())
        self.assertFalse((home / "config.toml").exists())


    def test_markdown_environment_cannot_inherit_noema_integrations(self):
        case = next(case for case in build_equal_access_cases()
                    if case.condition == "markdown" and case.receiver == "codex")
        benchmark.prepare_equal_access_markdown(self.run, self.state, case)
        inherited = {
            "NOEMA_BIN": "/not/allowed", "NOEMA_PREFETCH_SCRIPT": "/not/allowed",
            "OPENCODE_CONFIG_CONTENT": '{"mcp":{"bad":{}}}',
            "OPENCODE_CONFIG": "/not/allowed", "OPENCODE_CONFIG_DIR": "/not/allowed",
        }
        with mock.patch.dict(os.environ, inherited):
            env = self.run.runtime_env(case)
            self.assertFalse(any(name.startswith("NOEMA_") for name in env))
            self.assertNotIn("OPENCODE_CONFIG_CONTENT", env)
            self.assertTrue(Path(env["XDG_CONFIG_HOME"]).is_relative_to(self.run.markdown_client_root(case)))
            self.assertEqual(read_json(Path(env["OPENCODE_CONFIG"])), {"mcp": {}, "plugin": []})
            with mock.patch.object(benchmark, "run_command", return_value=subprocess.CompletedProcess([], 0, "", "")) as process:
                benchmark.execute_turn(self.run, self.state, case, retrieval_profile="prefetch")
        self.assertEqual(process.call_count, 1)
        command = process.call_args.args[0]
        self.assertNotIn("--dangerously-bypass-hook-trust", command)
        self.assertFalse(any("mcp_servers" in part for part in command))
        self.assertFalse(benchmark.prefetch_metrics_path(self.run, case).exists())

    def test_markdown_noema_tool_contamination_fails_closed(self):
        case = next(case for case in build_equal_access_cases() if case.condition == "markdown")
        for tools, expected in ((["noema/create_trace"], "markdown_condition_contaminated"),
                                (["read_file"], "markdown_tool_call_observed")):
            with self.subTest(tools=tools), mock.patch.object(
                benchmark, "execute_turn", return_value=(ParsedEvents(
                    final_text=answer_payload(case), tool_names=tools,
                ), 0.01, 0, None)
            ):
                row = benchmark.run_retrieval_case(self.run, self.state, case, retrieval_profile="prefetch")
                self.assertEqual(row["status"], "failed")
                self.assertEqual(row["error_code"], expected)

    def test_confirm_gate_and_explicit_id_do_not_reserve_or_launch(self):
        with mock.patch.object(benchmark, "execute_turn") as execute:
            with self.assertRaisesRegex(BenchmarkError, "confirm-live"):
                benchmark.cmd_equal_access_experiment(self.args(confirm=False), self.config)
            with self.assertRaisesRegex(BenchmarkError, "explicit --run-id"):
                benchmark.cmd_equal_access_experiment(self.args(explicit_id=False), self.config)
        execute.assert_not_called()
        self.assertNotIn("experiment_kind", self.run.load_state())
        self.assertFalse(self.run.runtime_root.exists())

    def test_freshness_refuses_historical_state(self):
        variants = (
            {"track": "natural"}, {"retrieval_profile": "standard"},
            {"batches": {"deterministic": {"1": {"status": "completed"}}}},
            {"checkpoints": {"1": {"status": "reported"}}},
            {"experiments": {"prefetch_smoke": {"status": "passed"}}},
            {"smoke": {"status": "passed"}},
            {"smoke_history": [{"status": "failed"}]},
            {"contexts": {"old": {"prepared": True}}},
        )
        with mock.patch.object(benchmark, "execute_turn") as execute:
            for variant in variants:
                with self.subTest(variant=variant):
                    state = {**copy.deepcopy(self.state), **variant}
                    self.run.save_state(state)
                    with self.assertRaises(BenchmarkError):
                        benchmark.cmd_equal_access_experiment(self.args(), self.config)
                    self.assertNotIn("experiment_kind", self.run.load_state())
        execute.assert_not_called()

    def test_freshness_refuses_historical_raw_or_report_data(self):
        for path in (self.run.raw_dir / "old-smoke.json", self.run.report_dir / "smoke.json"):
            with self.subTest(path=path):
                path.write_text("{}")
                with self.assertRaisesRegex(BenchmarkError, "historical"):
                    benchmark.cmd_equal_access_experiment(self.args(), self.config)
                self.assertNotIn("experiment_kind", self.run.load_state())
                path.unlink()

    def test_smoke_restores_and_does_not_create_qualification_evidence(self):
        self.assertEqual(self.phase(), 0)
        self.assertEqual(len(self.executed), 6)
        self.assertEqual({case.scenario for case in self.executed}, {"same_workspace_exact"})
        self.assert_restored()
        state = self.run.load_state()
        self.assertEqual(state["smoke"], {"status": "not_run"})
        self.assertEqual(state["batches"], {})
        self.assertEqual(state["checkpoints"], {})
        result = read_json(self.run.report_dir / "equal-access-smoke-experiment.json")
        self.assertEqual(result["status"], "passed")
        self.assertFalse(result["qualification_evidence"])
        self.assertFalse(result["superiority_candidate"])
        self.assertEqual(result["prefetch_metrics"]["successful_turns"], 2)
        rows = read_json(self.run.report_dir / "equal-access-smoke-turns.json")
        self.assertEqual([row["execution_order"] for row in rows], list(range(1, 7)))
        public = json.dumps({"result": result, "rows": rows})
        for case in self.executed:
            for question in case.questions:
                for token in question.expected_values:
                    self.assertNotIn(token, public)
        with self.assertRaises(BenchmarkError):
            benchmark.require_preflight(self.run)
        self.assertEqual(benchmark.require_preflight(self.run, allow_equal_access=True)["experiment_kind"], "equal_access")

    def test_reserved_run_refuses_all_ordinary_paths_but_can_restore(self):
        benchmark.reserve_equal_access_phase(self.run, self.state, "smoke")
        commands = (
            ("smoke", benchmark.cmd_smoke, ["--confirm-live"]),
            ("run-batch", benchmark.cmd_run_batch, ["--confirm-live"]),
            ("experiment-fastpath", benchmark.cmd_fastpath_experiment, ["--confirm-live"]),
            ("experiment-prefetch", benchmark.cmd_prefetch_experiment, ["--confirm-live"]),
            ("experiment-native-capture-target", benchmark.cmd_native_capture_target_experiment, ["--confirm-live"]),
            ("analyze", benchmark.cmd_analyze, ["--checkpoint", "1"]),
        )
        for command, handler, options in commands:
            args = benchmark.build_parser().parse_args(["--run-id", self.run.run_id, command, *options])
            with self.subTest(command=command), self.assertRaisesRegex(BenchmarkError, "descriptive-only"):
                handler(args, self.config)
        args = benchmark.build_parser().parse_args(["--run-id", self.run.run_id, "restore"])
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(benchmark.cmd_restore(args, self.config), 0)
        self.assertEqual(self.run.load_state()["status"], "restored")

    def test_campaign_requires_own_smoke_and_current_guard_hashes(self):
        with self.assertRaisesRegex(BenchmarkError, "passed equal-access smoke"):
            benchmark.cmd_equal_access_experiment(self.args("campaign"), self.config)
        self.assertEqual(self.phase(), 0)
        self.guard.write_text("changed after smoke")
        with mock.patch.object(benchmark, "execute_turn") as execute:
            with self.assertRaisesRegex(BenchmarkError, "guard hashes changed"):
                benchmark.cmd_equal_access_experiment(self.args("campaign"), self.config)
        execute.assert_not_called()
        self.assertNotIn("campaign", self.run.load_state()["experiments"]["equal_access"])

    def test_campaign_is_36_descriptive_turns_without_accuracy_threshold(self):
        self.assertEqual(self.phase(), 0)
        self.executed.clear()
        self.assertEqual(self.phase("campaign", correct=False), 0)
        self.assertEqual(len(self.executed), 36)
        self.assertEqual(len({case.scenario for case in self.executed}), 6)
        self.assertEqual(len({case.direction for case in self.executed}), 2)
        self.assert_restored()
        report = read_json(self.run.report_dir / "equal-access-campaign-experiment.json")
        self.assertEqual(report["status"], "completed")
        self.assertEqual(report["summary"]["matched_triples"], 12)
        self.assertNotIn("exact_and_provenance", report["gates"])
        self.assertNotIn("no_safety_errors", report["gates"])
        self.assertTrue(report["descriptive_only"])
        self.assertFalse(report["qualification_evidence"])
        with self.assertRaises(BenchmarkError):
            benchmark.cmd_equal_access_experiment(self.args("campaign"), self.config)

    def test_failure_restores_paths_and_persists_reserved_failed_state(self):
        self.assertEqual(self.phase(fail_at=1), 1)
        self.assertEqual(len(self.executed), 1)
        self.assert_restored()
        state = self.run.load_state()
        self.assertEqual(state["status"], "equal_access_smoke_failed")
        self.assertEqual(state["experiment_kind"], "equal_access")
        result = read_json(self.run.report_dir / "equal-access-smoke-experiment.json")
        self.assertEqual(result["execution_error_type"], "RuntimeError")
        self.assertEqual(result["status"], "failed")
        self.assertNotIn("private exception", json.dumps(result))
        with self.assertRaises(BenchmarkError):
            benchmark.cmd_equal_access_experiment(self.args("campaign"), self.config)
        with self.assertRaises(BenchmarkError):
            benchmark.cmd_equal_access_experiment(self.args(), self.config)

    def test_interruption_after_complete_triple_cannot_publish_partial_headline(self):
        self.assertEqual(self.phase(fail_at=4), 1)
        self.assert_restored()
        report = read_json(self.run.report_dir / "equal-access-smoke-experiment.json")
        self.assertEqual(report["retrieval_turns"], 3)
        self.assertFalse(report["summary"]["available"])
        self.assertFalse(report["summary"]["complete"])
        self.assertFalse(report["gates"]["matched_summary"])

    def test_first_mechanics_failure_stops_and_restores(self):
        self.assertEqual(self.phase(tools=["noema/get_trace"]), 1)
        self.assertEqual(len(self.executed), 1)
        self.assert_restored()
        report = read_json(self.run.report_dir / "equal-access-smoke-experiment.json")
        self.assertFalse(report["gates"]["all_turns_completed"])
        self.assertEqual(report["failures"][0]["error_code"], "native_condition_contaminated")

    def test_campaign_refuses_smoke_with_incomplete_restoration(self):
        self.assertEqual(self.phase(), 0)
        state = self.run.load_state()
        state["experiments"]["equal_access"]["smoke"]["restoration"]["managed_restored"] = False
        self.run.save_state(state)
        with mock.patch.object(benchmark, "execute_turn") as execute:
            with self.assertRaisesRegex(BenchmarkError, "clean restoration"):
                benchmark.cmd_equal_access_experiment(self.args("campaign"), self.config)
        execute.assert_not_called()

    def test_freshness_refuses_live_managed_paths(self):
        self.run.work_root.mkdir(parents=True)
        with self.assertRaisesRegex(BenchmarkError, "originally absent managed paths"):
            benchmark.cmd_equal_access_experiment(self.args(), self.config)
        self.assertNotIn("experiment_kind", self.run.load_state())

    def test_archive_error_still_restores_and_persists_failure(self):
        restore = benchmark.restore_managed

        def archive_failure(run, state, *, archive_runtime):
            if archive_runtime:
                raise OSError("fixture archive failure")
            return restore(run, state, archive_runtime=False)

        with mock.patch.object(benchmark, "restore_managed", side_effect=archive_failure):
            self.assertEqual(self.phase(), 1)
        self.assert_restored()
        result = read_json(self.run.report_dir / "equal-access-smoke-experiment.json")
        self.assertEqual(result["status"], "failed")
        self.assertEqual(result["restoration"]["archive_error_type"], "OSError")
        self.assertTrue(result["restoration"]["managed_restored"])

    def test_smoke_first_wrong_exact_answer_stops_without_reusing_checkpoint_smoke(self):
        self.assertEqual(self.phase(correct=False), 1)
        self.assertEqual(len(self.executed), 1)
        self.assert_restored()
        result = read_json(self.run.report_dir / "equal-access-smoke-experiment.json")
        self.assertFalse(result["gates"]["exact_and_provenance"])
        self.assertFalse(result["summary"]["available"])
        self.assertEqual(self.run.load_state()["smoke"]["status"], "not_run")


if __name__ == "__main__":
    unittest.main()
