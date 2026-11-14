#!/usr/bin/env python3
"""Offline lifecycle and isolation tests; all client/Noema execution is mocked."""
from __future__ import annotations

import argparse
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import subprocess
import sqlite3
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import benchmark as base
import independent_campaign as campaign
from independent_corpus import build_independent_cases, build_plan, build_smoke_specs, records_for_case
from omarchy_eval_lib import BenchmarkError, ParsedEvents, read_json, sha256_file, snapshot_path, write_json


def answer_payload(case, correct=True):
    return json.dumps({"answers": [{
        "question_id": q.question_id,
        "answer": " ".join(q.expected_values) if correct else "",
        "rationale": " ".join(q.expected_rationales) if correct else None,
        "provenance": list(q.expected_provenance) if correct else [],
        "abstain": q.should_abstain if correct else False,
    } for q in case.questions]})


class IndependentCampaignTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.home = self.root / "home"
        (self.home / ".codex").mkdir(parents=True)
        (self.home / ".codex/auth.json").write_text("synthetic-auth", encoding="utf-8")
        patch = mock.patch.object(Path, "home", return_value=self.home)
        patch.start()
        self.addCleanup(patch.stop)
        command = self.root / "fixture-client"
        command.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        command.chmod(0o700)
        self.config = {
            "config_path": str(self.root / "config.json"),
            "paths": {key: str(self.root / value) for key, value in {
                "private_root": "private", "reports_root": "reports",
                "work_parent": "work", "outside_parent": "outside",
            }.items()},
            "commands": {key: str(command) for key in ("codex", "opencode", "noema")},
            "models": {"codex": "fixture-model", "opencode": "fixture/fixture-model", "reasoning": "medium"},
            "limits": {"turn_timeout_seconds": 10, "bootstrap_samples": 20},
        }
        write_json(Path(self.config["config_path"]), self.config)
        self.run = base.Run(self.config, "independent-fixture")
        self.run.raw_dir.mkdir(parents=True)
        self.run.report_dir.mkdir(parents=True)
        self.guard = self.root / "guard"
        self.guard.write_text("original guard bytes", encoding="utf-8")
        guard = snapshot_path(self.guard)
        guard["label"] = "fixture_guard"
        write_json(self.run.raw_dir / "guard-snapshots.json", [guard])
        self.plan = build_plan("offline-fixture-seed")
        self.pilots = build_smoke_specs()
        write_json(campaign.plan_path(self.run), self.plan)
        write_json(campaign.smoke_path(self.run), self.pilots)
        self.state = self.base_state(self.run)
        self.state.update({"experiment_kind": "equal_access", "experiments": {campaign.EXPERIMENT: {
            "implementation_sha256": campaign.own_hashes(),
            "plan_sha256": sha256_file(campaign.plan_path(self.run)),
            "pilot_sha256": sha256_file(campaign.smoke_path(self.run)),
            "smoke": {"status": "not_run"}, "run": {"status": "not_run"},
        }}})
        self.run.save_state(self.state)
        self.executed = []

    def base_state(self, run):
        return {
            "run_id": run.run_id, "status": "preflight_passed", "track": "deterministic",
            "retrieval_profile": "prefetch", "corpus_cycle": 1,
            "config_sha256": sha256_file(Path(self.config["config_path"])),
            "harness_sha256": {p.name: sha256_file(p) for p in base.HARNESS_FILES},
            "commands": self.config["commands"],
            "command_sha256": {k: sha256_file(Path(v)) for k, v in self.config["commands"].items()},
            "models": copy.deepcopy(self.config["models"]), "model_checks": {"codex": True, "opencode": True},
            "guard_snapshot_file": str(self.run.raw_dir / "guard-snapshots.json"),
            "managed_snapshots": [snapshot_path(p) for p in (run.work_root, run.outside_root, run.runtime_root)],
            "parent_kinds": {}, "smoke": {"status": "not_run"},
            "batches": {}, "checkpoints": {}, "continuation_authorizations": [],
        }

    def args(self, command, run=None, confirm=True):
        argv = ["--config", self.config["config_path"], "--run-id", (run or self.run).run_id, command]
        if command in {"smoke", "run"} and confirm:
            argv.append("--confirm-live")
        return campaign.build_parser().parse_args(argv)

    def fake_prepare(self, unit, state, spec, case):
        paths = unit.context_paths(case.batch, case.track, case.direction, case.condition)
        for path in paths.values():
            path.mkdir(parents=True, exist_ok=True)
        key = unit.context_key(case.batch, case.track, case.direction, case.condition)
        state.setdefault("contexts", {})[key] = {"prepared": True, "record_count": spec["record_count"],
                                                "project_doc_max_bytes": campaign.BYTE_CAP,
                                                "cortex": "private-cortex-sentinel", "private_path": str(self.root)}
        unit.save_state(state)
        return paths

    def execute(self, unit, state, case, **kwargs):
        self.executed.append(case)
        if case.condition == "noema":
            write_json(base.prefetch_metrics_path(unit, case), {"success": True, "engine": "fixture",
                       "duration_ms": 1, "hit_count": 2, "context_chars": 50})
        return ParsedEvents(final_text=answer_payload(case), input_tokens=10, output_tokens=5), 0.01, 0, None

    def positive_control(self, unit, state, spec, case):
        control = {"success": True, "expected_records": spec["record_count"],
                   "observed_records": spec["record_count"], "hit_count": 1, "context_chars": 10}
        state.setdefault("positive_controls", {})[case.direction] = control
        unit.save_state(state)
        return control

    def phase(self, phase, execute=None, control=None):
        with mock.patch.object(campaign, "prepare_context", side_effect=self.fake_prepare), mock.patch.object(
            base, "execute_turn", side_effect=execute or self.execute
        ), mock.patch.object(
            campaign, "verify_prefetch_control", side_effect=control or self.positive_control
        ), contextlib.redirect_stdout(io.StringIO()):
            return campaign.cmd_phase(self.args(phase), self.config, phase)

    def pass_smoke(self):
        self.assertEqual(self.phase("smoke"), 0)
        self.assertEqual(len(self.executed), 18)
        self.executed.clear()

    def assert_roots_restored(self):
        for path in (self.run.work_root, self.run.outside_root, self.run.runtime_root):
            self.assertFalse(path.exists(), path)
        self.assertEqual(self.guard.read_text(), "original guard bytes")

    def test_preflight_freezes_fresh_private_plan_and_normal_model_checks(self):
        new_run = base.Run(self.config, "new-independent-fixture")
        def preflight(args, config):
            self.assertFalse(args.skip_model_check)
            self.assertEqual((args.track, args.retrieval_profile), ("deterministic", "prefetch"))
            new_run.raw_dir.mkdir(parents=True)
            new_run.report_dir.mkdir(parents=True)
            new_run.save_state(self.base_state(new_run))
            return 0
        with mock.patch.object(base, "cmd_preflight", side_effect=preflight), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(campaign.cmd_preflight(self.args("preflight", new_run), self.config), 0)
        state, plan = campaign.require_campaign(new_run)
        self.assertEqual(plan["expected_turns"], 1080)
        self.assertEqual(len(plan["corpora"]), 30)
        self.assertEqual(state["experiment_kind"], "equal_access")
        for path in (campaign.plan_path(new_run), campaign.smoke_path(new_run)):
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        with self.assertRaises(BenchmarkError):
            campaign.cmd_preflight(self.args("preflight", new_run), self.config)
        with self.assertRaises(BenchmarkError):
            base.require_preflight(new_run)

    def test_hash_model_guard_and_plan_drift_refuse_before_execution(self):
        changed = dict(campaign.own_hashes(), **{"independent_campaign.py": "changed"})
        with mock.patch.object(campaign, "own_hashes", return_value=changed), self.assertRaises(BenchmarkError):
            campaign.require_campaign(self.run)
        state = self.run.load_state()
        state["models"]["reasoning"] = "high"
        self.run.save_state(state)
        with self.assertRaises(BenchmarkError):
            campaign.require_campaign(self.run)
        self.run.save_state(self.state)
        self.guard.write_text("drift", encoding="utf-8")
        with self.assertRaises(BenchmarkError):
            campaign.require_campaign(self.run)
        self.guard.write_text("original guard bytes", encoding="utf-8")
        write_json(campaign.plan_path(self.run), {"tampered": True})
        with self.assertRaises(BenchmarkError):
            campaign.require_campaign(self.run)

    def test_historical_evidence_never_supplies_smoke_or_campaign(self):
        for update in ({"batches": {"1": {}}}, {"smoke": {"status": "passed"}},
                       {"experiments": {"equal_access": {}}}, {"experiment_kind": "ordinary"}):
            with self.subTest(update=update):
                state = copy.deepcopy(self.state)
                state.update(update)
                self.run.save_state(state)
                with self.assertRaises(BenchmarkError):
                    campaign.require_campaign(self.run)
        self.run.save_state(self.state)
        with self.assertRaises(BenchmarkError):
            self.phase("run")
        self.assertFalse(self.executed)
        with self.assertRaises(BenchmarkError):
            campaign.cmd_phase(self.args("smoke", confirm=False), self.config, "smoke")

    def test_corpus_roots_state_cortex_and_direction_isolation(self):
        units = [campaign.CorpusRun(self.run, spec) for spec in self.plan["corpora"][:2]]
        for attribute in ("work_root", "outside_root", "runtime_root", "raw_dir", "state_path"):
            self.assertNotEqual(getattr(units[0], attribute), getattr(units[1], attribute))
        names = {u.cortex_name(1, "deterministic", d) for u in units
                 for d in ("codex_to_opencode", "opencode_to_codex")}
        self.assertEqual(len(names), 4)
        self.assertTrue(all(len(name) <= 63 for name in names))
        before = self.run.state_path.read_bytes()
        units[0].save_state(campaign.fresh_unit_state(self.state, units[0]))
        self.assertEqual(before, self.run.state_path.read_bytes())
        self.assertEqual(units[0].load_state()["parent_kinds"], {})
        for corpus_id in ("../escape", "..", "/escape"):
            with self.assertRaises(BenchmarkError):
                campaign.CorpusRun(self.run, {"corpus_id": corpus_id})

    def test_full_large_markdown_contains_every_record_without_noema_or_mcp(self):
        spec = next(s for s in self.plan["corpora"] if s["record_count"] == 600)
        case = next(c for c in build_independent_cases(spec) if c.condition == "markdown")
        unit = campaign.CorpusRun(self.run, spec)
        state = campaign.fresh_unit_state(self.state, unit)
        with mock.patch.object(base, "run_noema") as noema:
            paths = campaign.prepare_context(unit, state, spec, case)
        noema.assert_not_called()
        corpus = records_for_case(spec, case)
        for role in ("project-a", "project-b", "outside"):
            text = (paths[role] / "AGENTS.md").read_text()
            for record in corpus:
                self.assertIn(record.value, text)
                self.assertIn(record.provenance, text)
            self.assertFalse((paths[role] / ".noema").exists())
        self.assertFalse((paths["common"] / "AGENTS.md").exists())
        other = "opencode_to_codex" if case.direction == "codex_to_opencode" else "codex_to_opencode"
        self.assertFalse(unit.context_paths(1, "deterministic", other, "markdown")["common"].exists())
        with mock.patch.dict(os.environ, {"NOEMA_CORTEX": "ambient", "NOEMA_BIN": "ambient",
                                         "OPENCODE_CONFIG_CONTENT": "ambient"}):
            env = unit.runtime_env(case)
        self.assertFalse(any(key.startswith("NOEMA_") for key in env))
        self.assertNotIn("OPENCODE_CONFIG_CONTENT", env)
        self.assertEqual(read_json(Path(env["OPENCODE_CONFIG"])), {"mcp": {}, "plugin": []})
        metadata = next(iter(state["contexts"].values()))
        self.assertEqual(metadata["record_count"], 600)
        self.assertEqual(metadata["documents"]["project-a"]["bytes"], len(text.encode()))
        self.assertLess(len(text.encode()), campaign.BYTE_CAP)

    def test_all_arms_fixed_cap_and_isolated_client_homes(self):
        spec = self.pilots[0]
        unit = campaign.CorpusRun(self.run, spec)
        state = campaign.fresh_unit_state(self.state, unit)
        cases = build_independent_cases(spec)
        direction = cases[0].direction
        with mock.patch.object(base, "run_noema", return_value=subprocess.CompletedProcess([], 0, "", "")):
            for arm in ("native", "markdown", "noema"):
                case = next(c for c in cases if c.condition == arm and c.direction == direction)
                campaign.prepare_context(unit, state, spec, case)
                home = unit.codex_home(case.batch, case.track, direction, arm)
                self.assertEqual((home / "config.toml").read_text(), f"project_doc_max_bytes = {campaign.BYTE_CAP}\n")
                self.assertTrue((home / "auth.json").is_symlink())
                self.assertFalse((home / "config.toml").read_text().find("mcp_servers") >= 0)
                self.assertEqual((home / "hooks.json").exists(), arm == "noema")
                self.assertEqual(read_json(Path(unit.runtime_env(case)["OPENCODE_CONFIG"])), {"mcp": {}, "plugin": []})

    def test_smoke_uses_only_disjoint_pilot_shards_and_restores(self):
        self.pass_smoke()
        state = self.run.load_state()
        smoke = campaign.experiment(state)["smoke"]
        self.assertEqual(smoke["status"], "passed")
        self.assertEqual(smoke["turns"], 18)
        self.assertEqual({entry["record_count"] for entry in smoke["corpora"].values()}, {60, 180, 600})
        self.assertTrue(set(smoke["corpora"]).isdisjoint(s["corpus_id"] for s in self.plan["corpora"]))
        self.assertEqual(campaign.collect_rows(self.run, self.plan["corpora"], "run"), [])
        self.assert_roots_restored()
        with self.assertRaises(BenchmarkError):
            self.phase("smoke")

    def test_natural_accuracy_errors_are_outcomes_not_smoke_admission(self):
        def wrong(unit, state, case, **kwargs):
            parsed, latency, code, error = self.execute(unit, state, case, **kwargs)
            parsed.final_text = answer_payload(case, correct=False)
            return parsed, latency, code, error
        self.assertEqual(self.phase("smoke", wrong), 0)
        self.assertEqual(len(self.executed), 18)
        report = read_json(self.run.report_dir / "independent-smoke.json")
        self.assertEqual(report["status"], "passed")
        self.assertFalse(report["exact_provenance_safety_perfect"])
        self.assertEqual(len(report["positive_controls"]), 6)
        self.assert_roots_restored()

    def test_failed_direct_control_stops_and_cannot_gate_campaign(self):
        def fail_control(*args):
            raise BenchmarkError("positive control failed")
        self.assertEqual(self.phase("smoke", control=fail_control), 1)
        self.assertTrue(all(case.condition != "noema" for case in self.executed))
        with self.assertRaises(BenchmarkError):
            self.phase("run")
        self.assert_roots_restored()

    def test_direct_control_requires_readonly_seed_snapshot_and_exact_record(self):
        spec = self.pilots[0]
        case = next(case for case in build_independent_cases(spec)
                    if case.condition == "noema" and case.scenario == "same_workspace_exact")
        unit = campaign.CorpusRun(self.run, spec)
        state = campaign.fresh_unit_state(self.state, unit)
        database = unit.runtime_root / "cortexes" / unit.cortex_name(
            case.batch, case.track, case.direction
        ) / "db/noema.db"
        database.parent.mkdir(parents=True)
        with contextlib.closing(sqlite3.connect(database)) as connection, connection:
            connection.execute("PRAGMA journal_mode=WAL")
            connection.execute("CREATE TABLE traces (id INTEGER PRIMARY KEY)")
            connection.executemany("INSERT INTO traces DEFAULT VALUES", [()] * 59)
        self.addCleanup(database.parent.chmod, 0o700)
        database.parent.chmod(0o500)
        with mock.patch.object(base, "run_command") as command:
            with self.assertRaises(BenchmarkError):
                campaign.verify_prefetch_control(unit, state, spec, case)
        command.assert_not_called()
        self.assertEqual(state["positive_controls"][case.direction]["observed_records"], 59)
        database.parent.chmod(0o700)
        with contextlib.closing(sqlite3.connect(database)) as connection, connection:
            connection.execute("INSERT INTO traces DEFAULT VALUES")
        database.parent.chmod(0o500)
        for payload in ({"included_trace_count": 0, "context": ""},
                        {"included_trace_count": 3, "context": "wrong record"}):
            with self.subTest(payload=payload), mock.patch.object(
                base, "run_command", return_value=subprocess.CompletedProcess([], 0, json.dumps(payload), "")
            ), self.assertRaises(BenchmarkError):
                campaign.verify_prefetch_control(unit, state, spec, case)
            self.assertFalse(state["positive_controls"][case.direction]["success"])
        target = case.questions[0].expected_values[0]
        with mock.patch.object(base, "run_command", return_value=subprocess.CompletedProcess(
            [], 0, json.dumps({"included_trace_count": 1, "context": target}), ""
        )):
            control = campaign.verify_prefetch_control(unit, state, spec, case)
        self.assertTrue(control["success"])
        self.assertEqual(control["observed_records"], 60)


    def test_full_campaign_preserves_imperfect_scoreboard_without_accuracy_filtering(self):
        self.pass_smoke()
        def wrong_markdown(unit, state, case, **kwargs):
            parsed, latency, code, error = self.execute(unit, state, case, **kwargs)
            if case.condition == "markdown":
                parsed.final_text = answer_payload(case, correct=False)
            return parsed, latency, code, error
        self.assertEqual(self.phase("run", wrong_markdown), 0)
        self.assertEqual(len(self.executed), 1080)
        report = read_json(self.run.report_dir / "independent-campaign.json")
        rows = read_json(self.run.report_dir / "independent-turns.json")
        markdown = [r for r in rows if r["condition"] == "markdown"]
        self.assertEqual(len(markdown), 360)
        self.assertTrue(any(r["macro_f1"] < 1 for r in markdown))
        self.assertEqual(report["failed_turns"], 0)
        self.assertEqual(report["status"], "completed")
        self.assertFalse(report["superiority_candidate"])
        public = json.dumps(report)
        for secret in (str(self.root), "offline-fixture-seed", "expected_values", "expected_provenance"):
            self.assertNotIn(secret, public)
        self.assert_roots_restored()
        with self.assertRaises(BenchmarkError):
            self.phase("run")

    def test_mechanics_failure_retained_and_analyzable_without_rerun(self):
        self.pass_smoke()
        def failed(unit, state, case, **kwargs):
            parsed, latency, _, _ = self.execute(unit, state, case, **kwargs)
            return parsed, latency, 1, "client_nonzero_exit"
        self.assertEqual(self.phase("run", failed), 1)
        self.assertEqual(len(self.executed), 1)
        rows = read_json(self.run.report_dir / "independent-turns.json")
        self.assertEqual(rows[0]["error_code"], "client_nonzero_exit")
        self.assertEqual(rows[0]["macro_f1"], 1.0)  # Do not rewrite established failed-row scoring.
        report = read_json(self.run.report_dir / "independent-campaign.json")
        self.assertFalse(report["summary"]["available"])
        with self.assertRaises(BenchmarkError):
            self.phase("run")
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(campaign.cmd_analyze(self.args("analyze"), self.config), 0)
        self.assert_roots_restored()

    def test_resume_skips_committed_rows_and_marks_uncertain_attempt_without_retry(self):
        spec = self.pilots[0]
        unit = campaign.CorpusRun(self.run, spec)
        cases = [c for c in build_independent_cases(spec) if c.scenario == "same_workspace_exact"]
        state = campaign.fresh_unit_state(self.state, unit)
        row = campaign.failed_row(cases[0], "persisted_failure")
        campaign.tag_row(row, spec, 1)
        write_json(unit.raw_dir / "results" / f"{cases[0].case_id}.json", row)
        state["attempts"][cases[0].case_id] = {"status": "recorded"}
        unit.save_state(state)
        with mock.patch.object(base, "run_retrieval_case") as execute:
            result = campaign.run_corpus(self.run, self.state, spec, "smoke")
        execute.assert_not_called()
        self.assertEqual(result["rows"][0]["error_code"], "persisted_failure")
        spec = self.pilots[1]
        unit = campaign.CorpusRun(self.run, spec)
        case = next(c for c in build_independent_cases(spec) if c.scenario == "same_workspace_exact")
        state = campaign.fresh_unit_state(self.state, unit)
        state["attempts"][case.case_id] = {"status": "started"}
        unit.save_state(state)
        with mock.patch.object(base, "run_retrieval_case") as execute:
            result = campaign.run_corpus(self.run, self.state, spec, "smoke")
        execute.assert_not_called()
        self.assertEqual(result["rows"][0]["error_code"], "interrupted_attempt_not_retried")

    def test_resume_only_genuinely_missing_successful_cases(self):
        spec = self.pilots[0]
        unit = campaign.CorpusRun(self.run, spec)
        cases = [c for c in build_independent_cases(spec) if c.scenario == "same_workspace_exact"]
        state = campaign.fresh_unit_state(self.state, unit)
        unit.save_state(state)
        self.fake_prepare(unit, state, spec, cases[0])
        with mock.patch.object(base, "execute_turn", side_effect=self.execute):
            row = base.run_retrieval_case(unit, state, cases[0], retrieval_profile="prefetch")
        state["attempts"][cases[0].case_id] = {"status": "recorded"}
        unit.save_state(state)
        self.executed.clear()
        with mock.patch.object(campaign, "prepare_context", side_effect=self.fake_prepare), mock.patch.object(
            base, "execute_turn", side_effect=self.execute
        ), mock.patch.object(campaign, "verify_prefetch_control", side_effect=self.positive_control):
            result = campaign.run_corpus(self.run, self.state, spec, "smoke")
        self.assertEqual(result["status"], "completed")
        self.assertEqual(len(self.executed), 5)
        self.assertNotIn(cases[0].case_id, [c.case_id for c in self.executed])
        self.assertEqual(result["rows"][0]["macro_f1"], row["macro_f1"])

    def test_restore_failure_and_archive_failure_are_reported_not_hidden(self):
        fail = {"managed_restored": False, "guard_files_unchanged": True,
                "failures": ["fixture failure"], "guard_drift": []}
        with mock.patch.object(base, "restore_managed", return_value=fail):
            self.assertEqual(self.phase("smoke"), 1)
        result = campaign.experiment(self.run.load_state())["smoke"]
        self.assertEqual(result["status"], "stopped")
        self.assertFalse(result["restoration"]["managed_restored"])
        good = {"managed_restored": True, "guard_files_unchanged": True, "failures": [], "guard_drift": []}
        with mock.patch.object(base, "restore_managed", side_effect=[OSError("private path"), good]):
            restored = campaign.restore_safely(self.run, self.state)
        self.assertFalse(restored["managed_restored"])
        self.assertEqual(restored["archive_error_type"], "OSError")
        self.assertEqual(restored["failures"], ["runtime_archive_failed"])

    def test_committed_base_score_survives_exception_after_write(self):
        spec = self.pilots[0]
        real_retrieval = base.run_retrieval_case
        def interrupted(unit, state, case, **kwargs):
            real_retrieval(unit, state, case, **kwargs)
            raise KeyboardInterrupt()
        with mock.patch.object(campaign, "prepare_context", side_effect=self.fake_prepare), mock.patch.object(
            base, "execute_turn", side_effect=self.execute
        ), mock.patch.object(base, "run_retrieval_case", side_effect=interrupted), mock.patch.object(
            campaign, "verify_prefetch_control", side_effect=self.positive_control
        ):
            result = campaign.run_corpus(self.run, self.state, spec, "smoke")
        self.assertEqual(result["status"], "stopped")
        self.assertEqual(result["turns"], 1)
        self.assertEqual(result["rows"][0]["macro_f1"], 1.0)
        self.assertEqual(result["rows"][0]["status"], "completed")

    def test_context_limit_error_is_explicit_and_preserves_original_score(self):
        spec = self.pilots[0]
        def limited(unit, state, case, **kwargs):
            parsed, latency, _, _ = self.execute(unit, state, case, **kwargs)
            path = unit.raw_dir / "events" / f"{case.case_id}-retrieve.stderr.txt"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("context_length_exceeded", encoding="utf-8")
            return parsed, latency, 1, "client_nonzero_exit"
        with mock.patch.object(campaign, "prepare_context", side_effect=self.fake_prepare), mock.patch.object(
            base, "execute_turn", side_effect=limited
        ), mock.patch.object(campaign, "verify_prefetch_control", side_effect=self.positive_control):
            result = campaign.run_corpus(self.run, self.state, spec, "smoke")
        self.assertEqual(result["status"], "stopped")
        self.assertEqual(result["rows"][0]["error_code"], "context_limit_exceeded")
        self.assertEqual(result["rows"][0]["macro_f1"], 1.0)

    def test_explicit_restore_works_after_hash_drift_closes_tools_and_analysis_survives(self):
        self.pass_smoke()
        self.run.tool_root.mkdir()
        (self.run.tool_root / "fixture").write_text("pinned", encoding="utf-8")
        with mock.patch.object(campaign, "own_hashes", return_value={}), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(campaign.cmd_restore(self.args("restore"), self.config), 0)
            with self.assertRaises(BenchmarkError):
                campaign.cmd_analyze(self.args("analyze"), self.config)
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(campaign.cmd_analyze(self.args("analyze"), self.config), 0)
        self.assertFalse(self.run.tool_root.exists())
        self.assertEqual(self.run.load_state()["status"], "restored")
        self.assertNotIn(str(self.root), (self.run.report_dir / "restoration.json").read_text())
        with self.assertRaises(BenchmarkError):
            campaign.require_campaign(self.run)

    def test_public_pilot_and_campaign_rows_allowlist_metadata_without_cortex_paths(self):
        self.pass_smoke()
        public = (self.run.report_dir / "independent-smoke-turns.json").read_text()
        self.assertNotIn("private-cortex-sentinel", public)
        self.assertNotIn(str(self.root), public)
        private_rows = campaign.collect_rows(self.run, self.pilots, "smoke")
        self.assertIn("private-cortex-sentinel", json.dumps(private_rows))
        spec = self.plan["corpora"][0]
        unit = campaign.CorpusRun(self.run, spec)
        case = build_independent_cases(spec)[0]
        row = campaign.tag_row(campaign.failed_row(case, "fixture_failure"), spec, 1)
        row["condition_metadata"] = {
            "cortex": "private-cortex-sentinel", "private_path": str(self.root),
            "project_doc_max_bytes": campaign.BYTE_CAP,
            "documents": {"project-a": {"bytes": 100, "sha256": "fixture-digest"}},
        }
        write_json(unit.raw_dir / "results" / f"{case.case_id}.json", row)
        with contextlib.redirect_stdout(io.StringIO()):
            campaign.cmd_analyze(self.args("analyze"), self.config)
        public_rows = read_json(self.run.report_dir / "independent-turns.json")
        self.assertNotIn("private-cortex-sentinel", json.dumps(public_rows))
        self.assertNotIn(str(self.root), json.dumps(public_rows))
        self.assertEqual(public_rows[0]["macro_f1"], row["macro_f1"])
        self.assertEqual(public_rows[0]["condition_metadata"]["documents"], row["condition_metadata"]["documents"])

    def test_analysis_refuses_core_and_config_hash_drift_without_overwriting_report(self):
        report = self.run.report_dir / "independent-campaign.json"
        report.write_text("historical report sentinel", encoding="utf-8")
        state = self.run.load_state()
        state["harness_sha256"]["benchmark.py"] = "changed"
        self.run.save_state(state)
        with self.assertRaises(BenchmarkError):
            campaign.cmd_analyze(self.args("analyze"), self.config)
        self.assertEqual(report.read_text(), "historical report sentinel")
        self.run.save_state(self.state)
        config_path = Path(self.config["config_path"])
        config_path.write_text(config_path.read_text() + "\n", encoding="utf-8")
        with self.assertRaises(BenchmarkError):
            campaign.cmd_analyze(self.args("analyze"), self.config)
        self.assertEqual(report.read_text(), "historical report sentinel")

    def test_guard_drift_between_turns_stops_and_retains_guard_evidence(self):
        def drift(unit, state, case, **kwargs):
            result = self.execute(unit, state, case, **kwargs)
            self.guard.write_text("external drift", encoding="utf-8")
            return result
        self.assertEqual(self.phase("smoke", drift), 1)
        self.assertEqual(len(self.executed), 1)
        smoke = campaign.experiment(self.run.load_state())["smoke"]
        self.assertFalse(smoke["restoration"]["guard_files_unchanged"])
        self.assertEqual(self.guard.read_text(), "external drift")
        for path in (self.run.work_root, self.run.outside_root, self.run.runtime_root):
            self.assertFalse(path.exists())

    def test_progress_is_flushed_sanitized_per_corpus_metadata(self):
        stream = io.StringIO()
        with mock.patch.object(campaign, "prepare_context", side_effect=self.fake_prepare), mock.patch.object(
            base, "execute_turn", side_effect=self.execute
        ), mock.patch.object(
            campaign, "verify_prefetch_control", side_effect=self.positive_control
        ), contextlib.redirect_stdout(stream):
            campaign.cmd_phase(self.args("smoke"), self.config, "smoke")
        lines = [json.loads(line) for line in stream.getvalue().splitlines()]
        self.assertEqual(sum(line.get("event") == "corpus_complete" for line in lines), 3)
        self.assertNotIn("private-cortex-sentinel", stream.getvalue())
        self.assertNotIn(str(self.root), stream.getvalue())


if __name__ == "__main__":
    unittest.main()
