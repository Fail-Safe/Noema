#!/usr/bin/env python3
"""Import a fully restored mechanics smoke into an identical fresh run."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Mapping, Sequence

import benchmark
from omarchy_eval_lib import BenchmarkError, load_config, read_json, sha256_file, write_json


LOCKED_STATE_FIELDS = (
    "config_sha256",
    "response_schema_sha256",
    "harness_sha256",
    "command_sha256",
    "source_command_sha256",
    "models",
    "track",
    "retrieval_profile",
    "corpus_cycle",
    "natural_evidence",
)


def validate_smoke_reuse(
    source_state: Mapping[str, Any],
    target_state: Mapping[str, Any],
    smoke_report: Mapping[str, Any],
    restoration_report: Mapping[str, Any],
) -> None:
    if source_state.get("run_id") == target_state.get("run_id"):
        raise BenchmarkError("source and target smoke runs must differ")
    if source_state.get("status") != "restored":
        raise BenchmarkError("source smoke run is not formally restored")
    if target_state.get("status") != "preflight_passed":
        raise BenchmarkError("target run is not at a fresh passed preflight")
    if target_state.get("smoke", {}).get("status") != "not_run":
        raise BenchmarkError("target run already has smoke state")
    if target_state.get("batches") or target_state.get("checkpoints"):
        raise BenchmarkError("target run already contains evidence")
    for field in LOCKED_STATE_FIELDS:
        if source_state.get(field) != target_state.get(field):
            raise BenchmarkError(f"smoke evidence differs on locked field {field}")

    source_smoke = source_state.get("smoke", {})
    if source_smoke.get("status") != "passed":
        raise BenchmarkError("source mechanics smoke did not pass")
    if source_smoke.get("turns") != 4 or source_smoke.get("model_turns") != 8:
        raise BenchmarkError("source mechanics smoke is incomplete")
    if source_smoke.get("failures") or smoke_report.get("failures"):
        raise BenchmarkError("source mechanics smoke contains failures")
    if smoke_report.get("run_id") != source_state.get("run_id"):
        raise BenchmarkError("source smoke report run id does not match")
    if smoke_report.get("status") != "passed":
        raise BenchmarkError("source smoke report did not pass")
    if restoration_report.get("run_id") != source_state.get("run_id"):
        raise BenchmarkError("source restoration report run id does not match")
    for restoration in (source_smoke.get("restoration", {}), restoration_report):
        if not restoration.get("managed_restored"):
            raise BenchmarkError("source smoke did not restore managed paths")
        if not restoration.get("guard_files_unchanged"):
            raise BenchmarkError("source smoke changed a guard file")
        if restoration.get("failures") or restoration.get("guard_drift"):
            raise BenchmarkError("source smoke restoration contains failures or drift")
    if not restoration_report.get("pinned_tools_removed"):
        raise BenchmarkError("source smoke did not remove pinned tools")


def import_smoke(
    config: dict[str, Any], source_run_id: str, target_run_id: str
) -> dict[str, Any]:
    if not benchmark.re_safe_run_id(source_run_id) or not benchmark.re_safe_run_id(target_run_id):
        raise BenchmarkError("run ids contain unsupported characters")
    source = benchmark.Run(config, source_run_id)
    target = benchmark.Run(config, target_run_id)
    source_state = source.load_state()
    target_state = benchmark.require_preflight(target)
    smoke_path = source.report_dir / "smoke.json"
    restoration_path = source.report_dir / "restoration.json"
    if not smoke_path.is_file() or not restoration_path.is_file():
        raise BenchmarkError("source smoke and restoration reports are required")
    smoke_report = read_json(smoke_path)
    restoration_report = read_json(restoration_path)
    validate_smoke_reuse(source_state, target_state, smoke_report, restoration_report)

    evidence = {
        "source_run_id": source_run_id,
        "smoke_report_sha256": sha256_file(smoke_path),
        "restoration_report_sha256": sha256_file(restoration_path),
        "importer_sha256": sha256_file(Path(__file__).resolve()),
        "locked_fields": list(LOCKED_STATE_FIELDS),
        "imported_at": benchmark.utc_now(),
    }
    target_state["smoke"] = {
        "status": "passed",
        "track": target_state["track"],
        "retrieval_profile": target_state["retrieval_profile"],
        "imported": True,
        "model_turns": 0,
        "evidence": evidence,
    }
    target_state["status"] = "smoke_passed"
    target.save_state(target_state)
    target.report_dir.mkdir(parents=True, exist_ok=True)
    public = {
        "run_id": target_run_id,
        "status": "passed",
        "model_turns": 0,
        "smoke_reused": True,
        **evidence,
    }
    write_json(target.report_dir / "smoke-evidence.json", public, 0o644)
    return public


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, default=benchmark.DEFAULT_CONFIG)
    parser.add_argument("--from-run", required=True)
    parser.add_argument("--to-run", required=True)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        result = import_smoke(load_config(args.config), args.from_run, args.to_run)
    except (BenchmarkError, OSError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 2
    print(json.dumps(result, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
