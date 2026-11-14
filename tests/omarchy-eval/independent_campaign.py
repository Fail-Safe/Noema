#!/usr/bin/env python3
"""Frozen, descriptive independent-corpus equal-access campaign (stdlib only)."""
from __future__ import annotations

import argparse
import contextlib
import fcntl
import hashlib
import io
import json
from pathlib import Path
import secrets
import sqlite3
import sys
from typing import Any, Mapping, Sequence

import benchmark as base
from independent_corpus import (
    build_independent_cases, build_plan, build_smoke_specs, records_for_case,
    summarize_independent,
)
from omarchy_eval_lib import (
    BenchmarkError, Case, ParsedEvents, atomic_write, load_config, read_json,
    render_agents, render_trace_body, sanitized_row, score_case, sha256_file,
    snapshot_path, write_json,
)

SCRIPT_DIR = Path(__file__).resolve().parent
PROTOCOL = SCRIPT_DIR / "independent-protocol.md"
OWN_FILES = (Path(__file__).resolve(), SCRIPT_DIR / "independent_corpus.py", PROTOCOL)
BYTE_CAP = 1048576
EXPERIMENT = "independent_campaign"
CLOSED = {"restored", "restored_with_guard_drift", "restore_failed"}


class CorpusRun(base.Run):
    """Private unit roots and state, never the parent manifest or another corpus."""
    def __init__(self, parent: base.Run, spec: Mapping[str, Any]):
        super().__init__(parent.config, parent.run_id)
        corpus_id = str(spec["corpus_id"])
        if not base.re_safe_run_id(corpus_id) or corpus_id in {".", ".."}:
            raise BenchmarkError("unsafe corpus identifier")
        self.corpus_id = corpus_id
        self.work_root = parent.work_root / corpus_id
        self.outside_root = parent.outside_root / corpus_id
        self.runtime_root = parent.runtime_root / corpus_id
        self.raw_dir = parent.raw_dir / "corpora" / corpus_id
        self.state_path = parent.run_dir / "corpus-states" / f"{corpus_id}.json"

    def cortex_name(self, batch: int, track: str, direction: str) -> str:
        identity = f"{self.run_id}:{self.corpus_id}:{batch}:{track}:{direction}"
        return "independent-" + hashlib.sha256(identity.encode()).hexdigest()[:40]

    def runtime_env(self, case: Case | None = None, *, capture: bool = False) -> dict[str, str]:
        env = super().runtime_env(case, capture=capture)
        if case is not None and case.condition != "markdown":
            client_root = self.runtime_root / "opencode-users" / self.context_key(
                case.batch, case.track, case.direction, case.condition
            )
            env.pop("OPENCODE_CONFIG_CONTENT", None)
            env["OPENCODE_CONFIG"] = str(client_root / "opencode.json")
            env["OPENCODE_CONFIG_DIR"] = str(client_root / ".opencode")
        return env


def own_hashes() -> dict[str, str]:
    return {path.name: sha256_file(path) for path in OWN_FILES}


def plan_path(run: base.Run) -> Path:
    return run.run_dir / "independent-plan.json"


def smoke_path(run: base.Run) -> Path:
    return run.run_dir / "independent-pilots.json"


def experiment(state: dict[str, Any]) -> dict[str, Any]:
    return state["experiments"][EXPERIMENT]


def assert_namespace(state: Mapping[str, Any]) -> None:
    if state.get("experiment_kind") != "equal_access" or set(state.get("experiments", {})) != {EXPERIMENT}:
        raise BenchmarkError("not an independent campaign; historical evidence cannot be imported")
    if (state.get("batches") or state.get("checkpoints") or state.get("continuation_authorizations")
            or state.get("natural_evidence") or state.get("smoke", {}).get("status") != "not_run"
            or state.get("smoke_history") or state.get("contexts")):
        raise BenchmarkError("independent campaign contains mixed historical evidence")


def require_campaign(run: base.Run, *, execution: bool = True) -> tuple[dict[str, Any], dict[str, Any]]:
    state = run.load_state()
    assert_namespace(state)
    if execution:
        state = base.require_preflight(run, allow_equal_access=True)
        own = experiment(state)
        if state.get("status") in CLOSED:
            raise BenchmarkError("campaign is closed")
        if state.get("models") != run.config["models"] or run.config["models"].get("reasoning") != "medium":
            raise BenchmarkError("campaign model or medium reasoning pin changed")
        if state.get("model_checks") != {"codex": True, "opencode": True}:
            raise BenchmarkError("normal model catalog checks are required")
        if base.guard_drift(state):
            raise BenchmarkError("guard hashes changed; restore before continuing")
    own = experiment(state)
    if own.get("implementation_sha256") != own_hashes():
        raise BenchmarkError("independent driver, generator, or protocol changed after preflight")
    if not execution:
        if state.get("harness_sha256") != {path.name: sha256_file(path) for path in base.HARNESS_FILES}:
            raise BenchmarkError("base harness changed after preflight; frozen analysis refused")
        if state.get("config_sha256") != sha256_file(Path(run.config["config_path"])):
            raise BenchmarkError("config changed after preflight; frozen analysis refused")
        if state.get("models") != run.config["models"]:
            raise BenchmarkError("model pin changed after preflight; frozen analysis refused")
    for path, key in ((plan_path(run), "plan_sha256"), (smoke_path(run), "pilot_sha256")):
        if not path.is_file() or sha256_file(path) != own.get(key):
            raise BenchmarkError("private frozen plan changed after preflight")
    plan = read_json(plan_path(run))
    return state, plan


@contextlib.contextmanager
def command_lock(run: base.Run):
    if not run.exists():
        raise BenchmarkError("run preflight first")
    path = run.run_dir / "independent.lock"
    with path.open("a", encoding="utf-8") as handle:
        path.chmod(0o600)
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise BenchmarkError("another independent campaign command is active") from error
        try:
            yield
        finally:
            fcntl.flock(handle, fcntl.LOCK_UN)


def cmd_preflight(args: argparse.Namespace, config: dict[str, Any]) -> int:
    if config["models"].get("reasoning") != "medium":
        raise BenchmarkError("independent campaign requires fixed medium reasoning")
    hashes = own_hashes()
    run = base.Run(config, args.run_id)
    if run.run_dir.exists() or run.report_dir.exists():
        raise BenchmarkError("independent preflight requires new private and report roots")
    # Base preflight performs real catalogs, executable pinning and guard snapshots.
    base_args = argparse.Namespace(run_id=args.run_id, track="deterministic", retrieval_profile="prefetch",
                                   corpus_cycle=1, evidence_run=None, evidence_checkpoint=None, skip_model_check=False)
    result = base.cmd_preflight(base_args, config)
    state = run.load_state()
    state["experiment_kind"] = "equal_access"  # Existing ordinary command gates refuse this run.
    state["experiments"] = {EXPERIMENT: {
        "implementation_sha256": hashes, "smoke": {"status": "not_run"},
        "run": {"status": "not_run"}, "descriptive_only": True,
    }}
    # These are created only here, never regenerated on resume, smoke or analyze.
    write_json(plan_path(run), build_plan(secrets.token_hex(32)))
    write_json(smoke_path(run), build_smoke_specs())
    own = experiment(state)
    own["plan_sha256"] = sha256_file(plan_path(run))
    own["pilot_sha256"] = sha256_file(smoke_path(run))
    own["frozen_at"] = base.utc_now()
    run.save_state(state)
    print(json.dumps({"run_id": run.run_id, "event": "independent_preflight",
                      "status": state["status"], "expected_turns": 1080,
                      "implementation_sha256": hashes, "plan_sha256": own["plan_sha256"]}), flush=True)
    return result


def fresh_unit_state(parent_state: Mapping[str, Any], unit: CorpusRun) -> dict[str, Any]:
    return {
        "schema_version": 1, "run_id": unit.run_id, "corpus_id": unit.corpus_id,
        "commands": parent_state["commands"], "models": parent_state["models"],
        "guard_snapshot_file": parent_state["guard_snapshot_file"], "contexts": {},
        "managed_snapshots": [snapshot_path(path) for path in (unit.work_root, unit.outside_root, unit.runtime_root)],
        "parent_kinds": {}, "attempts": {},
    }


def prepare_context(unit: CorpusRun, state: dict[str, Any], spec: Mapping[str, Any], case: Case) -> dict[str, Path]:
    key = unit.context_key(case.batch, case.track, case.direction, case.condition)
    paths = unit.context_paths(case.batch, case.track, case.direction, case.condition)
    if state.setdefault("contexts", {}).get(key, {}).get("prepared"):
        return paths
    corpus = records_for_case(spec, case)
    if len(corpus) != spec["record_count"]:
        raise BenchmarkError("generator record count mismatch")
    base.ensure_runtime(unit)
    for path in paths.values():
        path.mkdir(parents=True, exist_ok=True, mode=0o700)
    for role in ("common", "outside"):
        (paths[role] / ".git").mkdir(exist_ok=True)
    home = unit.codex_home(case.batch, case.track, case.direction, case.condition)
    home.mkdir(parents=True, exist_ok=True, mode=0o700)
    auth = Path.home() / ".codex/auth.json"
    if auth.exists() and not (home / "auth.json").exists():
        (home / "auth.json").symlink_to(auth)
    atomic_write(home / "config.toml", f"project_doc_max_bytes = {BYTE_CAP}\n".encode())
    if case.condition != "markdown":
        client_root = unit.runtime_root / "opencode-users" / key
        (client_root / ".opencode").mkdir(parents=True, exist_ok=True, mode=0o700)
        write_json(client_root / "opencode.json", {"mcp": {}, "plugin": []})
    documents: dict[str, bytes] = {}
    if case.condition == "native":
        for role, scope in (("common", "global"), ("project-a", "project-a"), ("project-b", "project-b")):
            documents[role] = render_agents([record for record in corpus if record.scope == scope], case.source, scope).encode()
    elif case.condition == "markdown":
        parts = ["# Full shared synthetic memory\n\nScope labels govern answers, not access.\n"]
        for scope in ("global", "project-a", "project-b"):
            parts.append(render_agents([record for record in corpus if record.scope == scope], case.source, scope))
        memory = "\n".join(parts).encode()
        documents = {role: memory for role in ("project-a", "project-b", "outside")}
        client_root = unit.markdown_client_root(case)
        for relative in ("xdg-config", "xdg-state", ".opencode"):
            (client_root / relative).mkdir(parents=True, exist_ok=True, mode=0o700)
        write_json(client_root / "opencode.json", {"mcp": {}, "plugin": []})
    elif case.condition == "noema":
        cortex = unit.cortex_name(case.batch, case.track, case.direction)
        base.run_noema(unit, state, ["init", "--name", cortex, "--path", str(unit.runtime_root / "cortexes")], paths["common"])
        types = {"global_preference": "preference", "decision": "decision", "project_a_fact": "fact",
                 "project_b_fact": "fact", "supersession": "fact"}
        for record in corpus:
            base.run_noema(unit, state, ["--cortex", cortex, "add", "--title", f"Synthetic {record.record_id}",
                           "--type", types[record.category], "--author", case.source,
                           "--tag", "synthetic-continuity-benchmark", "--tag", record.scope,
                           "--body", render_trace_body(record, case.source)], paths["common"])
        write_json(home / "hooks.json", base.codex_prefetch_hooks())
    else:
        raise BenchmarkError("unknown independent arm")
    for role, memory in documents.items():
        if len(memory) > BYTE_CAP:
            raise BenchmarkError("context_document_exceeds_fixed_byte_cap")
        atomic_write(paths[role] / "AGENTS.md", memory, 0o644)
    metadata = {
        "prepared": True, "condition": case.condition, "direction": case.direction,
        "corpus_variant": case.corpus_variant, "record_count": len(corpus),
        "retrieval_profile": "prefetch" if case.condition == "noema" else case.condition,
        "cortex": unit.cortex_name(case.batch, case.track, case.direction) if case.condition == "noema" else None,
        "project_doc_max_bytes": BYTE_CAP, "fixed_codex_byte_cap_raised_all_arms": True,
        "documents": {role: {"bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()}
                      for role, data in documents.items()},
        "corpus_sha256": hashlib.sha256("\n".join(render_trace_body(record, case.source) for record in corpus).encode()).hexdigest(),
        "delivery": "full_workspace_agents" if case.condition == "markdown" else case.condition,
    }
    state["contexts"][key] = metadata
    unit.save_state(state)
    return paths


def verify_prefetch_control(
    unit: CorpusRun, state: dict[str, Any], spec: Mapping[str, Any], case: Case
) -> dict[str, Any]:
    records = records_for_case(spec, case)
    target = next(record for record in records if record.value == case.questions[0].expected_values[0])
    cortex = unit.cortex_name(case.batch, case.track, case.direction)
    database = unit.runtime_root / "cortexes" / cortex / "db" / "noema.db"
    # Seed CLI writers have exited; inspect the checkpoint without creating WAL sidecars.
    with contextlib.closing(sqlite3.connect(database.as_uri() + "?mode=ro&immutable=1", uri=True)) as connection:
        observed = connection.execute("SELECT COUNT(*) FROM traces").fetchone()[0]
    control = {
        "success": False, "expected_records": spec["record_count"],
        "observed_records": observed, "hit_count": 0, "context_chars": 0,
    }
    if observed == spec["record_count"]:
        result = base.run_command(
            [state["commands"]["noema"], "--cortex", cortex, "prefetch", "--input", "raw",
             "--output", "json", "--max-results", "8", "--max-preferences", "4", "--max-chars", "8000"],
            cwd=unit.context_paths(case.batch, case.track, case.direction, case.condition)["common"],
            env=unit.runtime_env(), timeout=30, input_text=target.record_id,
        )
        if result.returncode == 0:
            payload = json.loads(result.stdout)
            context = payload.get("context", "")
            control.update(
                hit_count=payload.get("included_trace_count", 0),
                context_chars=len(context),
                success=bool(payload.get("included_trace_count", 0) and target.value in context),
            )
    state.setdefault("positive_controls", {})[case.direction] = control
    unit.save_state(state)
    if not control["success"]:
        raise BenchmarkError("seeded direct-retrieval positive control failed")
    return control


def restore_safely(run: base.Run, state: dict[str, Any]) -> dict[str, Any]:
    try:
        return base.restore_managed(run, state, archive_runtime=True)
    except (Exception, KeyboardInterrupt) as error:
        try:
            result = base.restore_managed(run, state, archive_runtime=False)
            result["archive_error_type"] = type(error).__name__
            result["managed_restored"] = False  # Archive preservation is a required mechanics gate.
            result["failures"].append("runtime_archive_failed")
            return result
        except (Exception, KeyboardInterrupt) as restore_error:
            return {"managed_restored": False, "guard_files_unchanged": False,
                    "failures": ["restoration_error"], "guard_drift": [],
                    "error_type": type(restore_error).__name__}


def clean_restoration(result: Mapping[str, Any]) -> bool:
    return bool(result.get("managed_restored") and result.get("guard_files_unchanged")
                and not result.get("failures") and not result.get("guard_drift"))


def mechanics_ok(row: Mapping[str, Any]) -> bool:
    return row.get("status") == "completed" and not row.get("error_code") and bool(row.get("valid_response"))


def failed_row(case: Case, code: str) -> dict[str, Any]:
    return sanitized_row(case, ParsedEvents(), score_case(case, "", set()), status="failed",
                         latency_seconds=0.0, error_code=code)


def tag_row(row: dict[str, Any], spec: Mapping[str, Any], order: int) -> dict[str, Any]:
    row.update({key: spec[key] for key in ("corpus_id", "stratum", "record_count")})
    row["execution_order"] = order
    return row


def case_rows(unit: CorpusRun, cases: Sequence[Case], spec: Mapping[str, Any]) -> list[dict[str, Any]]:
    rows = []
    for order, case in enumerate(cases, 1):
        path = unit.raw_dir / "results" / f"{case.case_id}.json"
        if path.exists():
            row = read_json(path)
            if any(row.get(key) != value for key, value in {
                "case_id": case.case_id, "condition": case.condition, "direction": case.direction,
                "scenario": case.scenario, "corpus_variant": case.corpus_variant,
            }.items()):
                raise BenchmarkError("durable row does not match frozen case")
            rows.append(tag_row(row, spec, order))
    return rows


def run_corpus(parent: base.Run, parent_state: dict[str, Any], spec: Mapping[str, Any], phase: str) -> dict[str, Any]:
    unit = CorpusRun(parent, spec)
    cases = build_independent_cases(spec)
    if phase == "smoke":
        cases = [case for case in cases if case.scenario == "same_workspace_exact"]
    state = unit.load_state() if unit.exists() else fresh_unit_state(parent_state, unit)
    unit.raw_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    unit.save_state(state)
    rows = case_rows(unit, cases, spec)
    error_type = None
    try:
        # A durable started intent without a result means an uncertain model call. Never repeat it.
        known = {row["case_id"] for row in rows}
        for order, case in enumerate(cases, 1):
            if case.case_id not in known and state.get("attempts", {}).get(case.case_id):
                row = tag_row(failed_row(case, "interrupted_attempt_not_retried"), spec, order)
                write_json(unit.raw_dir / "results" / f"{case.case_id}.json", row)
                rows.append(row)
        if any(not mechanics_ok(row) for row in rows):
            return_value = "stopped"
        else:
            return_value = "completed"
            known = {row["case_id"] for row in rows}
            for order, case in enumerate(cases, 1):
                if case.case_id in known:
                    continue
                state.setdefault("attempts", {})[case.case_id] = {"status": "started", "started_at": base.utc_now()}
                unit.save_state(state)
                try:
                    if base.guard_drift(parent_state):
                        raise BenchmarkError("guard drift during independent turn")
                    prepare_context(unit, state, spec, case)
                    if phase == "smoke" and case.condition == "noema":
                        verify_prefetch_control(unit, state, spec, case)
                    row = base.run_retrieval_case(unit, state, case, retrieval_profile="prefetch")
                except (Exception, KeyboardInterrupt) as error:
                    error_type = type(error).__name__
                    durable = unit.raw_dir / "results" / f"{case.case_id}.json"
                    if durable.exists():
                        row = read_json(durable)  # Never replace a score already committed by the base runner.
                    else:
                        row = failed_row(case, "context_limit_exceeded" if "context_document_exceeds_fixed_byte_cap" in str(error)
                                         else "execution_exception")
                    row["execution_error_type"] = error_type
                stderr_path = unit.raw_dir / "events" / f"{case.case_id}-retrieve.stderr.txt"
                stderr = stderr_path.read_text(errors="replace").lower() if stderr_path.exists() else ""
                event_path = unit.raw_dir / "events" / f"{case.case_id}-retrieve.jsonl"
                events = event_path.read_text(errors="replace").lower() if not mechanics_ok(row) and event_path.exists() else ""
                diagnostics = stderr + "\n" + events
                row["context_limit_error"] = row.get("error_code") == "context_limit_exceeded" or any(
                    marker in diagnostics for marker in (
                        "context_length_exceeded", "context window", "maximum context", "context limit", "project_doc_max_bytes",
                    )
                ) or ("truncat" in diagnostics and ("agents.md" in diagnostics or "project_doc" in diagnostics))
                if row["context_limit_error"]:
                    row["status"], row["error_code"] = "failed", "context_limit_exceeded"
                tag_row(row, spec, order)
                key = unit.context_key(case.batch, case.track, case.direction, case.condition)
                row["condition_metadata"] = state.get("contexts", {}).get(key, {})
                write_json(unit.raw_dir / "results" / f"{case.case_id}.json", row)
                rows.append(row)
                state["attempts"][case.case_id]["status"] = "recorded"
                unit.save_state(state)
                if error_type is not None or not mechanics_ok(row):
                    return_value = "stopped"
                    break
    finally:
        restoration = restore_safely(unit, state)
        contexts = state.get("contexts", {})
        state["contexts"] = {}
        state["restoration"] = restoration
        unit.save_state(state)
    result = {"corpus_id": spec["corpus_id"], "stratum": spec["stratum"], "record_count": spec["record_count"],
              "status": return_value if clean_restoration(restoration) else "stopped",
              "turns": len(rows), "expected_turns": len(cases), "rows": rows,
              "restoration": restoration, "execution_error_type": error_type,
              "condition_metadata": list(contexts.values()),
              "positive_controls": list(state.get("positive_controls", {}).values())}
    write_json(unit.raw_dir / "completed.json", result)
    return result


def collect_rows(run: base.Run, specs: Sequence[Mapping[str, Any]], phase: str) -> list[dict[str, Any]]:
    rows = []
    for spec in specs:
        cases = build_independent_cases(spec)
        if phase == "smoke":
            cases = [case for case in cases if case.scenario == "same_workspace_exact"]
        rows.extend(case_rows(CorpusRun(run, spec), cases, spec))
    return rows


def public_restoration(result: Mapping[str, Any]) -> dict[str, Any]:
    # Paths and exception strings stay in the private manifest.
    return {"managed_restored": bool(result.get("managed_restored")),
            "guard_files_unchanged": bool(result.get("guard_files_unchanged")),
            "failure_count": len(result.get("failures", [])),
            "changed_guard_count": len(result.get("guard_drift", [])),
            "archive_error_type": result.get("archive_error_type")}


def public_rows(rows: Sequence[Mapping[str, Any]]) -> list[dict[str, Any]]:
    """Keep scores intact while publishing only the delivery metadata allowlist."""
    allowed = {
        "condition", "direction", "corpus_variant", "record_count", "retrieval_profile",
        "project_doc_max_bytes", "fixed_codex_byte_cap_raised_all_arms", "documents",
        "corpus_sha256", "delivery",
    }
    result = []
    for row in rows:
        public = dict(row)
        if "condition_metadata" in public:
            metadata = row["condition_metadata"]
            public["condition_metadata"] = {key: value for key, value in metadata.items() if key in allowed}
        result.append(public)
    return result


def publish(run: base.Run, state: dict[str, Any], plan: Mapping[str, Any]) -> dict[str, Any]:
    own = experiment(state)
    rows = public_rows(collect_rows(run, plan["corpora"], "run"))
    summary = summarize_independent(rows, plan, int(run.config["limits"]["bootstrap_samples"]))
    result = {"run_id": run.run_id, "experiment": EXPERIMENT, "status": own["run"]["status"],
              "descriptive_only": True, "qualification_evidence": False, "superiority_candidate": False,
              "retrieval_turns": len(rows), "expected_retrieval_turns": plan["expected_turns"],
              "summary": summary, "context_limit_failure_count": sum(bool(row.get("context_limit_error")) for row in rows),
              "failed_turns": sum(not mechanics_ok(row) for row in rows),
              "restoration": public_restoration(own["run"].get("restoration", {})),
              "corpora": [{key: value for key, value in entry.items() if key in {
                  "corpus_id", "stratum", "record_count", "status", "turns", "expected_turns",
              }} for entry in own["run"].get("corpora", {}).values()]}
    write_json(run.report_dir / "independent-campaign.json", result, 0o644)
    write_json(run.report_dir / "independent-turns.json", rows, 0o644)
    return result


def cmd_phase(args: argparse.Namespace, config: dict[str, Any], phase: str) -> int:
    if not args.confirm_live:
        raise BenchmarkError(f"{phase} starts live model turns; supply --confirm-live")
    run = base.Run(config, args.run_id)
    with command_lock(run):
        state, plan = require_campaign(run)
        own = experiment(state)
        previous = own[phase]
        if previous["status"] in {"passed", "completed", "stopped", "failed"}:
            raise BenchmarkError(f"{phase} is terminal; scored failures are never rerun")
        if phase == "run":
            smoke = own["smoke"]
            if smoke.get("status") != "passed" or smoke.get("turns") != 18 or not clean_restoration(smoke.get("restoration", {})):
                raise BenchmarkError("campaign requires this run's passed 18-turn pilot with seeded direct-retrieval controls")
        elif own["run"].get("status") != "not_run":
            raise BenchmarkError("cannot smoke after campaign starts")
        specs = read_json(smoke_path(run)) if phase == "smoke" else plan["corpora"]
        record = own[phase]
        record.update({"status": "running", "started_at": record.get("started_at", base.utc_now())})
        record.setdefault("corpora", {})
        state["status"] = f"independent_{phase}_running"
        run.save_state(state)
        execution_error = None
        try:
            for index, spec in enumerate(specs, 1):
                saved = record["corpora"].get(spec["corpus_id"])
                if saved and saved.get("status") == "completed":
                    continue
                if saved and saved.get("status") == "stopped":
                    break
                if base.guard_drift(state):
                    raise BenchmarkError("guard drift during independent campaign")
                result = run_corpus(run, state, spec, phase)
                record["corpora"][spec["corpus_id"]] = {key: value for key, value in result.items() if key != "rows"}
                run.save_state(state)
                print(json.dumps({"event": "corpus_complete", "phase": phase, "corpus_index": index,
                                  "corpora_expected": len(specs), "corpus_id": spec["corpus_id"],
                                  "stratum": spec["stratum"], "turns": result["turns"],
                                  "status": result["status"]}), flush=True)
                if result["status"] != "completed":
                    break
        except (Exception, KeyboardInterrupt) as error:
            execution_error = type(error).__name__
        finally:
            restoration = restore_safely(run, state)
            rows = collect_rows(run, specs, phase)
            expected = 18 if phase == "smoke" else 1080
            passed = (execution_error is None and len(rows) == expected and all(mechanics_ok(row) for row in rows)
                      and len(record["corpora"]) == len(specs)
                      and all(entry["status"] == "completed" for entry in record["corpora"].values())
                      and clean_restoration(restoration))
            controls = [
                control for entry in record["corpora"].values()
                for control in entry.get("positive_controls", [])
            ]
            if phase == "smoke":
                passed = passed and len(controls) == 6 and all(control["success"] for control in controls)
            record.update({"status": ("passed" if phase == "smoke" else "completed") if passed else "stopped",
                           "turns": len(rows), "expected_turns": expected, "restoration": restoration,
                           "execution_error_type": execution_error, "completed_at": base.utc_now()})
            state["status"] = f"independent_{phase}_{record['status']}"
            run.save_state(state)
            if phase == "smoke":
                result = {"run_id": run.run_id, "phase": phase, "status": record["status"],
                          "retrieval_turns": len(rows), "expected_retrieval_turns": 18,
                          "restoration": public_restoration(restoration),
                          "admission_rule": "mechanics_and_seeded_direct_retrieval_control",
                          "positive_controls": controls,
                          "exact_provenance_safety_perfect": all(
                              row["exact_accuracy"] == 1.0 and row["provenance_accuracy"] == 1.0
                              and not any(row[key] for key in ("stale_count", "hallucination_count", "leakage_count"))
                              for row in rows
                          ) and len(rows) == 18,
                          "raw_data_private": True}
                write_json(run.report_dir / "independent-smoke.json", result, 0o644)
                write_json(run.report_dir / "independent-smoke-turns.json", public_rows(rows), 0o644)
            else:
                result = publish(run, state, plan)
        print(json.dumps({key: value for key, value in result.items() if key not in {"summary", "corpora"}}), flush=True)
        return 0 if passed else 1


def cmd_analyze(args: argparse.Namespace, config: dict[str, Any]) -> int:
    run = base.Run(config, args.run_id)
    with command_lock(run):
        # Analysis and restore remain available after drift/closure; plans must still match.
        state, plan = require_campaign(run, execution=False)
        result = publish(run, state, plan)
        print(json.dumps(result, indent=2), flush=True)
    return 0


def cmd_restore(args: argparse.Namespace, config: dict[str, Any]) -> int:
    run = base.Run(config, args.run_id)
    with command_lock(run):
        state = run.load_state()
        assert_namespace(state)
        # Base restore is intentionally independent of code/config/plan hash locks.
        with contextlib.redirect_stdout(io.StringIO()):
            code = base.cmd_restore(args, config)
        restored = run.load_state()
        public = {"run_id": run.run_id, "status": restored["status"],
                  **public_restoration(restored["restoration"]),
                  "pinned_tools_removed": restored["restoration"].get("pinned_tools_removed", False)}
        write_json(run.report_dir / "restoration.json", public, 0o644)
        print(json.dumps(public), flush=True)
        return code


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, default=base.DEFAULT_CONFIG)
    parser.add_argument("--run-id", required=True)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("preflight")
    for name in ("smoke", "run"):
        commands.add_parser(name).add_argument("--confirm-live", action="store_true")
    commands.add_parser("analyze")
    commands.add_parser("restore")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if not base.re_safe_run_id(args.run_id) or args.run_id in {".", ".."}:
        parser.error("unsafe run id")
    try:
        config = load_config(args.config.resolve())
        if args.command == "preflight":
            return cmd_preflight(args, config)
        if args.command in {"smoke", "run"}:
            return cmd_phase(args, config, args.command)
        return cmd_analyze(args, config) if args.command == "analyze" else cmd_restore(args, config)
    except (BenchmarkError, OSError, ValueError) as error:
        # Raw subprocess/exception details belong to private evidence, not console output.
        print(json.dumps({"status": "refused", "error_type": type(error).__name__}), file=sys.stderr, flush=True)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
