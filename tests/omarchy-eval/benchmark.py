#!/usr/bin/env python3
"""Run the preregistered Omarchy cross-harness continuity benchmark."""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import platform
import shlex
import shutil
import stat
import statistics
import subprocess
import sys
import tarfile
import time
from pathlib import Path
from typing import Any, Mapping, Sequence

from omarchy_eval_lib import (
    BenchmarkError,
    Case,
    ParsedEvents,
    atomic_write,
    build_batch_cases,
    build_corpus,
    checkpoint_decision,
    load_config,
    parse_jsonl_events,
    percentile,
    read_json,
    records_by_id,
    render_agents,
    render_capture_prompt,
    render_prompt,
    render_trace_body,
    safe_restore_absent,
    sanitized_row,
    score_case,
    sha256_file,
    snapshot_matches,
    snapshot_path,
    summarize_rows,
    write_json,
    write_tsv,
)


SCRIPT_DIR = Path(__file__).resolve().parent
DEFAULT_CONFIG = SCRIPT_DIR / "benchmark.json"
RESPONSE_SCHEMA = SCRIPT_DIR / "response-schema.json"
PREFETCH_HOOK = SCRIPT_DIR / "prefetch_hook.py"
HARNESS_FILES = (
    Path(__file__).resolve(),
    SCRIPT_DIR / "omarchy_eval_lib.py",
    PREFETCH_HOOK,
    RESPONSE_SCHEMA,
)
RETRIEVAL_PROFILES = ("standard", "prefetch")
TRACKS = ("deterministic", "natural")

GUARD_LABELS = (
    "codex_config",
    "codex_auth",
    "opencode_config",
    "opencode_noema_bootstrap",
    "noema_config",
    "work_agents",
)


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")


def default_run_id() -> str:
    return dt.datetime.now(dt.timezone.utc).strftime("omarchy-%Y%m%dT%H%M%SZ")


def command_path(command: str) -> str:
    if "/" in command or "\\" in command:
        path = Path(command).resolve()
        if not path.is_file() or not os.access(path, os.X_OK):
            raise BenchmarkError(f"command is not executable: {path}")
        return str(path)
    resolved = shutil.which(command)
    if resolved is None:
        raise BenchmarkError(f"command not found on PATH: {command}")
    return resolved


def run_command(
    argv: Sequence[str],
    *,
    cwd: Path,
    env: Mapping[str, str] | None = None,
    timeout: int = 60,
    input_text: str | None = None,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        list(argv),
        cwd=cwd,
        env=dict(env) if env is not None else None,
        input=input_text,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
        check=False,
    )


class Run:
    def __init__(self, config: dict[str, Any], run_id: str):
        self.config = config
        self.run_id = run_id
        self.private_root = Path(config["paths"]["private_root"])
        self.reports_root = Path(config["paths"]["reports_root"])
        self.run_dir = self.private_root / "runs" / run_id
        self.state_path = self.run_dir / "state.json"
        self.raw_dir = self.run_dir / "raw"
        self.runtime_root = self.run_dir / "runtime"
        self.tool_root = self.run_dir / "pinned-tools"
        self.work_parent = Path(config["paths"]["work_parent"])
        self.outside_parent = Path(config["paths"]["outside_parent"])
        self.work_root = self.work_parent / run_id
        self.outside_root = self.outside_parent / run_id
        self.report_dir = self.reports_root / run_id

    def exists(self) -> bool:
        return self.state_path.exists()

    def load_state(self) -> dict[str, Any]:
        if not self.state_path.exists():
            raise BenchmarkError(f"unknown run {self.run_id!r}; run preflight first")
        return read_json(self.state_path)

    def save_state(self, state: dict[str, Any]) -> None:
        state["updated_at"] = utc_now()
        write_json(self.state_path, state)

    def codex_home(self, batch: int, track: str, direction: str, condition: str) -> Path:
        return self.runtime_root / "codex-users" / self.context_key(batch, track, direction, condition) / ".codex"

    def runtime_env(self, case: Case | None = None, *, capture: bool = False) -> dict[str, str]:
        env = dict(os.environ)
        env["XDG_CONFIG_HOME"] = str(self.runtime_root / "xdg-config")
        if capture and case is not None and case.source == "opencode":
            capture_root = self.capture_opencode_root(
                case.batch, case.track, case.direction, case.condition
            )
            env["OPENCODE_CONFIG"] = str(capture_root / "opencode.jsonc")
            env["OPENCODE_CONFIG_DIR"] = str(capture_root / ".opencode")
            env["OPENCODE_DISABLE_PROJECT_CONFIG"] = "1"
        env["XDG_STATE_HOME"] = str(self.runtime_root / "xdg-state")
        env["NO_COLOR"] = "1"
        env["OPENCODE_DISABLE_AUTOUPDATE"] = "1"
        env["OPENCODE_DISABLE_CLAUDE_CODE_PROMPT"] = "1"
        env.pop("NOEMA_CORTEX", None)
        env.pop("NOEMA_MCP_TOOL_PROFILE", None)
        if (
            capture
            and case is not None
            and case.track == "natural"
            and case.condition == "noema"
        ):
            env["NOEMA_MCP_TOOL_PROFILE"] = "continuity-capture"
        if case is not None:
            env["CODEX_HOME"] = str(
                self.codex_home(case.batch, case.track, case.direction, case.condition)
            )
        return env

    def context_key(self, batch: int, track: str, direction: str, condition: str) -> str:
        return f"b{batch:02d}-{track}-{direction}-{condition}"

    def capture_opencode_root(
        self, batch: int, track: str, direction: str, condition: str
    ) -> Path:
        return self.runtime_root / "capture-opencode" / self.context_key(
            batch, track, direction, condition
        )

    def context_paths(self, batch: int, track: str, direction: str, condition: str) -> dict[str, Path]:
        relative = Path(track) / f"batch-{batch:02d}" / direction / condition
        common = self.work_root / relative
        outside = self.outside_root / relative / "outside"
        return {
            "common": common,
            "project-a": common / "project-a",
            "project-b": common / "project-b",
            "outside": outside,
        }

    def cortex_name(self, batch: int, track: str, direction: str) -> str:
        short = self.run_id.lower().replace("_", "-")[-24:]
        direction_short = "c2o" if direction == "codex_to_opencode" else "o2c"
        return f"eval-{short}-b{batch:02d}-{track[0]}-{direction_short}"[:63]


def find_latest_run(config: dict[str, Any]) -> str:
    latest = Path(config["paths"]["private_root"]) / "LATEST"
    if not latest.exists():
        raise BenchmarkError("no latest run; pass --run-id or run preflight")
    return latest.read_text(encoding="utf-8").strip()


def resolve_run(args: argparse.Namespace, config: dict[str, Any], *, allow_new: bool = False) -> Run:
    run_id = args.run_id or (default_run_id() if allow_new else find_latest_run(config))
    if not re_safe_run_id(run_id):
        raise BenchmarkError("run id must contain only ASCII letters, digits, dots, underscores, or hyphens")
    return Run(config, run_id)


def re_safe_run_id(value: str) -> bool:
    return bool(value) and len(value) <= 80 and all(c.isascii() and (c.isalnum() or c in "._-") for c in value)


def nearest_existing_parent(path: Path) -> Path:
    current = path
    while not current.exists() and current != current.parent:
        current = current.parent
    return current


def noema_config_guard() -> Path:
    if sys.platform == "darwin":
        return Path.home() / "Library/Application Support/noema/config.yaml"
    return Path.home() / ".config/noema/config.yaml"


def guard_paths() -> list[Path]:
    return [
        Path.home() / ".codex/config.toml",
        Path.home() / ".codex/auth.json",
        Path.home() / ".config/opencode/opencode.jsonc",
        Path.home() / ".config/opencode/plugins/noema-session-bootstrap.ts",
        noema_config_guard(),
        Path.home() / "Work/AGENTS.md",
    ]


def labeled_guard_snapshots() -> list[dict[str, Any]]:
    snapshots = []
    for label, path in zip(GUARD_LABELS, guard_paths(), strict=True):
        snapshot = snapshot_path(path)
        snapshot["label"] = label
        snapshots.append(snapshot)
    return snapshots


def guard_drift(state: Mapping[str, Any]) -> list[dict[str, Any]]:
    guards = read_json(Path(state["guard_snapshot_file"]))
    drift = []
    for index, snapshot in enumerate(guards):
        if snapshot_matches(snapshot):
            continue
        current = snapshot_path(Path(snapshot["path"]), include_bytes=False)
        drift.append({
            "label": snapshot.get("label", GUARD_LABELS[index]),
            "expected_kind": snapshot.get("kind"),
            "current_kind": current.get("kind"),
            "expected_sha256": snapshot.get("sha256"),
            "current_sha256": current.get("sha256"),
            "expected_mode": snapshot.get("mode"),
            "current_mode": current.get("mode"),
        })
    return drift


def pin_commands(run: Run, configured: Mapping[str, str]) -> tuple[dict[str, str], dict[str, str]]:
    sources = {name: command_path(value) for name, value in configured.items()}
    if "codex" in sources:
        codex_companion = Path(sources["codex"]).resolve().with_name("codex-code-mode-host")
        if not codex_companion.is_file() or not os.access(codex_companion, os.X_OK):
            raise BenchmarkError("Codex code-mode companion executable was not found")
        sources["codex-code-mode-host"] = str(codex_companion)
    run.tool_root.mkdir(parents=True, mode=0o700)
    pinned: dict[str, str] = {}
    for name, source_text in sources.items():
        source = Path(source_text)
        target = run.tool_root / name
        temporary = run.tool_root / f".{name}.tmp"
        shutil.copy2(source, temporary)
        os.chmod(temporary, stat.S_IMODE(source.stat().st_mode))
        if sha256_file(temporary) != sha256_file(source):
            temporary.unlink(missing_ok=True)
            raise BenchmarkError(f"failed to pin {name} executable exactly")
        os.replace(temporary, target)
        pinned[name] = str(target)
    return sources, pinned


def filesystem_canonical_path(path: Path) -> Path:
    resolved = path.resolve()
    current = Path(resolved.anchor)
    for part in resolved.parts[1:]:
        try:
            exact = next((entry for entry in current.iterdir() if entry.name == part), None)
            if exact is not None:
                current = exact
                continue
            folded = next(
                (entry for entry in current.iterdir() if entry.name.casefold() == part.casefold()),
                None,
            )
            current = folded if folded is not None else current / part
        except OSError:
            current /= part
    return current


def scoped_external_permissions(path: Path) -> dict[str, str]:
    requested = f"{path}/**"
    canonical = f"{filesystem_canonical_path(path)}/**"
    return {value: "allow" for value in (requested, canonical)}


def check_ignored(config_path: Path) -> tuple[bool, str]:
    del config_path
    private = SCRIPT_DIR / ".private"
    samples = [private if private.is_symlink() else private / "probe", SCRIPT_DIR / "reports/probe"]
    errors: list[str] = []
    for sample in samples:
        completed = run_command(["git", "check-ignore", "--quiet", str(sample)], cwd=SCRIPT_DIR)
        if completed.returncode != 0:
            errors.append(completed.stderr.strip() or str(sample))
    return not errors, "; ".join(errors)


def probe_command(argv: Sequence[str], cwd: Path, env: Mapping[str, str], timeout: int = 90) -> dict[str, Any]:
    started = time.monotonic()
    try:
        completed = run_command(argv, cwd=cwd, env=env, timeout=timeout)
        return {
            "argv0": Path(argv[0]).name,
            "returncode": completed.returncode,
            "stdout": completed.stdout,
            "stderr": completed.stderr,
            "elapsed_seconds": round(time.monotonic() - started, 6),
        }
    except subprocess.TimeoutExpired as error:
        return {
            "argv0": Path(argv[0]).name,
            "returncode": None,
            "stdout": error.stdout or "",
            "stderr": error.stderr or "",
            "elapsed_seconds": round(time.monotonic() - started, 6),
            "timeout": True,
        }


def load_natural_evidence(
    config: Mapping[str, Any], run_id: str | None, checkpoint: int | None
) -> dict[str, Any]:
    if not run_id or checkpoint is None:
        raise BenchmarkError(
            "natural preflight requires --evidence-run and --evidence-checkpoint"
        )
    if not re_safe_run_id(run_id) or checkpoint < 1:
        raise BenchmarkError("natural evidence run or checkpoint is invalid")
    report_path = (
        Path(config["paths"]["reports_root"])
        / run_id
        / f"checkpoint-{checkpoint:03d}"
        / "summary.json"
    )
    restoration_path = Path(config["paths"]["reports_root"]) / run_id / "restoration.json"
    if not report_path.is_file() or not restoration_path.is_file():
        raise BenchmarkError("natural evidence report and restoration record are required")
    report = read_json(report_path)
    restoration = read_json(restoration_path)
    summary = report.get("summary", {})
    decision = report.get("decision", {})
    paired = summary.get("paired", {})
    noema = summary.get("conditions", {}).get("noema", {})
    eligible = (
        report.get("retrieval_profile") == "prefetch"
        and int(summary.get("retrieval_turns", 0)) >= 192
        and bool(decision.get("natural_track_eligible"))
        and not bool(decision.get("integration_unreliable"))
        and float(paired.get("noema_minus_native", 0.0)) >= 0.10
        and float(paired.get("bootstrap_95_low", 0.0)) > 0.0
        and float(noema.get("stale_rate", 1.0)) <= 0.05
        and float(noema.get("hallucination_rate", 1.0)) <= 0.05
        and float(noema.get("scope_leakage_rate", 1.0)) <= 0.05
        and bool(restoration.get("managed_restored"))
        and not restoration.get("failures")
    )
    if not eligible:
        raise BenchmarkError("the referenced deterministic evidence is not eligible for natural capture")
    return {
        "run_id": run_id,
        "checkpoint": checkpoint,
        "report_sha256": sha256_file(report_path),
        "restoration_sha256": sha256_file(restoration_path),
        "retrieval_turns": int(summary["retrieval_turns"]),
        "noema_minus_native": paired["noema_minus_native"],
        "bootstrap_95_low": paired["bootstrap_95_low"],
        "managed_restored": True,
        "environment_guard_stable": bool(restoration.get("guard_files_unchanged")),
    }


def cmd_preflight(args: argparse.Namespace, config: dict[str, Any]) -> int:
    run = resolve_run(args, config, allow_new=True)
    if run.exists():
        raise BenchmarkError(f"run {run.run_id!r} already exists")
    for target in (run.work_root, run.outside_root, run.runtime_root):
        if target.exists() or target.is_symlink():
            raise BenchmarkError(f"refusing run-id collision at {target}")
    if run.outside_root.is_relative_to(Path.home() / "Work"):
        raise BenchmarkError("outside_parent must be outside ~/Work")
    for parent in (run.private_root, run.reports_root, run.work_parent, run.outside_parent):
        existing = nearest_existing_parent(parent)
        if not os.access(existing, os.W_OK | os.X_OK):
            raise BenchmarkError(f"benchmark parent is not writable: {existing}")
    ignored, ignore_error = check_ignored(Path(config["config_path"]))
    if not ignored:
        raise BenchmarkError(f"private/generated paths are not ignored: {ignore_error or 'git check-ignore failed'}")
    if args.track == "natural" and args.retrieval_profile != "prefetch":
        raise BenchmarkError("the natural capture track requires prefetch retrieval")
    natural_evidence = (
        load_natural_evidence(config, args.evidence_run, args.evidence_checkpoint)
        if args.track == "natural"
        else None
    )
    run.run_dir.mkdir(parents=True, mode=0o700)
    run.private_root.chmod(0o700)
    (run.private_root / "runs").chmod(0o700)
    run.run_dir.chmod(0o700)
    run.raw_dir.mkdir(mode=0o700)
    source_commands, commands = pin_commands(run, config["commands"])
    probe_env = dict(os.environ)
    probe_env["NO_COLOR"] = "1"
    probe_env["OPENCODE_DISABLE_AUTOUPDATE"] = "1"
    probes = {
        "codex_version": probe_command([commands["codex"], "--version"], SCRIPT_DIR, probe_env),
        "opencode_version": probe_command([commands["opencode"], "--version"], SCRIPT_DIR, probe_env),
        "noema_version": probe_command([commands["noema"], "--version"], SCRIPT_DIR, probe_env),
    }
    if not args.skip_model_check:
        probes["codex_models"] = probe_command([commands["codex"], "debug", "models"], SCRIPT_DIR, probe_env)
        probes["opencode_models"] = probe_command([commands["opencode"], "models"], SCRIPT_DIR, probe_env)
    write_json(run.raw_dir / "preflight-probes.json", probes)
    failures: list[str] = []
    for name in ("codex_version", "opencode_version", "noema_version"):
        if probes[name]["returncode"] != 0:
            failures.append(f"{name} failed")
    model_checks: dict[str, bool | None] = {"codex": None, "opencode": None}
    if not args.skip_model_check:
        codex_catalog = probes["codex_models"]["stdout"]
        opencode_catalog = probes["opencode_models"]["stdout"]
        model_checks["codex"] = config["models"]["codex"] in codex_catalog
        model_checks["opencode"] = config["models"]["opencode"] in opencode_catalog
        for client, passed in model_checks.items():
            if not passed:
                failures.append(f"pinned {client} model not found")
    else:
        failures.append("model catalog checks were skipped")
    guards = labeled_guard_snapshots()
    write_json(run.raw_dir / "guard-snapshots.json", guards)
    managed = [snapshot_path(path) for path in (run.work_root, run.outside_root, run.runtime_root)]
    state = {
        "schema_version": 1,
        "run_id": run.run_id,
        "created_at": utc_now(),
        "status": "preflight_failed" if failures else "preflight_passed",
        "config_sha256": sha256_file(Path(config["config_path"])),
        "response_schema_sha256": sha256_file(RESPONSE_SCHEMA),
        "harness_sha256": {path.name: sha256_file(path) for path in HARNESS_FILES},
        "commands": commands,
        "command_sha256": {name: sha256_file(Path(path)) for name, path in commands.items()},
        "source_commands": source_commands,
        "source_command_sha256": {
            name: sha256_file(Path(path)) for name, path in source_commands.items()
        },
        "models": config["models"],
        "track": args.track,
        "retrieval_profile": args.retrieval_profile,
        "corpus_cycle": args.corpus_cycle,
        "natural_evidence": natural_evidence,
        "environment_controls": {
            "commands_pinned": True,
            "opencode_autoupdate_disabled": True,
            "opencode_claude_prompt_disabled": True,
            "opencode_capture_project_discovery_disabled": True,
            "opencode_capture_config_dir_explicit": True,
            "natural_native_capture_targets_preseeded": True,
        },
        "model_checks": model_checks,
        "guard_snapshot_file": str(run.raw_dir / "guard-snapshots.json"),
        "managed_snapshots": managed,
        "parent_kinds": {
            str(run.work_parent): snapshot_path(run.work_parent, include_bytes=False)["kind"],
            str(run.outside_parent): snapshot_path(run.outside_parent, include_bytes=False)["kind"],
        },
        "smoke": {"status": "not_run"},
        "batches": {},
        "checkpoints": {},
        "continuation_authorizations": [],
        "failures": failures,
    }
    run.save_state(state)
    atomic_write(run.private_root / "LATEST", (run.run_id + "\n").encode(), 0o600)
    public = {
        "run_id": run.run_id,
        "status": state["status"],
        "models": config["models"],
        "track": args.track,
        "retrieval_profile": args.retrieval_profile,
        "corpus_cycle": args.corpus_cycle,
        "model_checks": model_checks,
        "commands_available": {name: True for name in commands},
        "commands_pinned": True,
        "natural_evidence": natural_evidence,
        "failures": failures,
        "raw_details_private": True,
    }
    run.report_dir.mkdir(parents=True, exist_ok=True)
    write_json(run.report_dir / "preflight.json", public, 0o644)
    print(json.dumps(public, indent=2))
    return 1 if failures else 0


def require_preflight(run: Run) -> dict[str, Any]:
    state = run.load_state()
    if state["status"] in {"restored", "restored_with_guard_drift"}:
        raise BenchmarkError("run has been restored and is closed")
    if state["status"] == "preflight_failed":
        raise BenchmarkError("preflight did not pass; inspect the private probe record")
    expected_harness = state.get("harness_sha256")
    current_harness = {path.name: sha256_file(path) for path in HARNESS_FILES}
    if expected_harness != current_harness:
        raise BenchmarkError("benchmark harness changed after preflight; restore this run and start a new one")
    if state.get("config_sha256") != sha256_file(Path(run.config["config_path"])):
        raise BenchmarkError("benchmark config changed after preflight; restore this run and start a new one")
    for name, path in state["commands"].items():
        if state.get("command_sha256", {}).get(name) != sha256_file(Path(path)):
            raise BenchmarkError(f"{name} executable changed after preflight; start a new run")
    return state


def track_for(state: Mapping[str, Any]) -> str:
    track = str(state.get("track", "deterministic"))
    if track not in TRACKS:
        raise BenchmarkError(f"unsupported preregistered track: {track}")
    return track


def corpus_cycle_for(state: Mapping[str, Any]) -> int:
    cycle = int(state.get("corpus_cycle", 1))
    if cycle not in (1, 2):
        raise BenchmarkError(f"unsupported preregistered corpus cycle: {cycle}")
    return cycle


def cases_for_run(
    state: Mapping[str, Any], batch: int, track: str
) -> list[Case]:
    return build_batch_cases(batch, track, corpus_cycle_for(state) - 1)


def retrieval_profile_for(state: Mapping[str, Any]) -> str:
    profile = str(state.get("retrieval_profile", "standard"))
    if profile not in RETRIEVAL_PROFILES:
        raise BenchmarkError(f"unsupported preregistered retrieval profile: {profile}")
    return profile


def ensure_runtime(run: Run) -> None:
    (run.runtime_root / "xdg-config").mkdir(parents=True, exist_ok=True, mode=0o700)
    (run.runtime_root / "xdg-state").mkdir(parents=True, exist_ok=True, mode=0o700)
    (run.runtime_root / "cortexes").mkdir(parents=True, exist_ok=True, mode=0o700)


def run_noema(
    run: Run,
    state: Mapping[str, Any],
    args: Sequence[str],
    cwd: Path,
    timeout: int = 120,
    env: Mapping[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    completed = run_command(
        [state["commands"]["noema"], *args],
        cwd=cwd,
        env=env or run.runtime_env(),
        timeout=timeout,
    )
    if completed.returncode != 0:
        raise BenchmarkError(f"Noema command failed ({' '.join(args[:4])}): {completed.stderr.strip()}")
    return completed


def codex_prefetch_hooks() -> dict[str, Any]:
    prompt_command = f"{shlex.quote(sys.executable)} {shlex.quote(str(PREFETCH_HOOK))} codex"
    preference_command = (
        f"{shlex.quote(sys.executable)} {shlex.quote(str(PREFETCH_HOOK))} codex-preferences"
    )
    return {
        "hooks": {
            "SessionStart": [{
                "matcher": "^(startup|resume|clear|compact)$",
                "hooks": [{
                    "type": "command",
                    "command": preference_command,
                    "timeout": 15,
                    "additionalContextLimit": 32000,
                }],
            }],
            "UserPromptSubmit": [{
                "matcher": "",
                "hooks": [{
                    "type": "command",
                    "command": prompt_command,
                    "timeout": 15,
                    "additionalContextLimit": 2500,
                }],
            }],
        },
    }


def prefetch_metrics_path(run: Run, case: Case, *, capture: bool = False) -> Path:
    suffix = "capture" if capture else "retrieve"
    return run.raw_dir / "prefetch-metrics" / f"{case.case_id}-{suffix}.json"


def prepare_context(
    run: Run,
    state: dict[str, Any],
    batch: int,
    track: str,
    direction: str,
    condition: str,
    retrieval_profile: str = "standard",
    preseed_native_targets: bool = True,
) -> dict[str, Path]:
    key = run.context_key(batch, track, direction, condition)
    contexts = state.setdefault("contexts", {})
    paths = run.context_paths(batch, track, direction, condition)
    existing = contexts.get(key, {})
    if existing.get("prepared"):
        if existing.get("retrieval_profile", "standard") != retrieval_profile:
            raise BenchmarkError(f"context {key} was prepared with a different retrieval profile")
        if existing.get("preseed_native_targets", True) != preseed_native_targets:
            raise BenchmarkError(f"context {key} was prepared with a different native target state")
        return paths
    ensure_runtime(run)
    for path in paths.values():
        path.mkdir(parents=True, exist_ok=True)
    (paths["common"] / ".git").mkdir(exist_ok=True)
    (paths["outside"] / ".git").mkdir(exist_ok=True)
    variant = next(
        case.corpus_variant
        for case in cases_for_run(state, batch, track)
        if case.direction == direction and case.condition == condition
    )
    source = direction.split("_to_")[0]
    prefix = f"{condition.upper()}-{source.upper()}"
    corpus = build_corpus(variant, prefix)
    codex_home = run.codex_home(batch, track, direction, condition)
    codex_home.mkdir(parents=True, exist_ok=True, mode=0o700)
    auth_source = Path.home() / ".codex/auth.json"
    auth_link = codex_home / "auth.json"
    if auth_source.exists() and not auth_link.exists():
        auth_link.symlink_to(auth_source)
    native_targets_preseeded = (
        track == "natural" and condition == "native" and preseed_native_targets
    )
    if native_targets_preseeded:
        for target in (
            paths["common"] / "AGENTS.md",
            paths["project-a"] / "AGENTS.md",
            paths["project-b"] / "AGENTS.md",
        ):
            atomic_write(target, b"", 0o644)
    if track == "natural" and source == "opencode" and condition == "native":
        capture_root = run.capture_opencode_root(batch, track, direction, condition)
        capture_root.mkdir(parents=True, exist_ok=True, mode=0o700)
        (capture_root / ".git").mkdir(exist_ok=True)
        write_json(
            capture_root / "opencode.jsonc",
            {
                "permission": {
                    "external_directory": scoped_external_permissions(paths["common"])
                }
            },
            0o600,
        )
    if track == "deterministic" and condition == "native":
        global_records = [record for record in corpus if record.scope == "global"]
        project_a = [record for record in corpus if record.scope == "project-a"]
        project_b = [record for record in corpus if record.scope == "project-b"]
        atomic_write(paths["common"] / "AGENTS.md", render_agents(global_records, source, "global").encode(), 0o644)
        atomic_write(paths["project-a"] / "AGENTS.md", render_agents(project_a, source, "project-a").encode(), 0o644)
        atomic_write(paths["project-b"] / "AGENTS.md", render_agents(project_b, source, "project-b").encode(), 0o644)
    if condition == "noema":
        cortex = run.cortex_name(batch, track, direction)
        if track == "deterministic":
            run_noema(run, state, ["init", "--name", cortex, "--path", str(run.runtime_root / "cortexes")], paths["common"])
            for record in corpus:
                trace_type = {
                    "global_preference": "preference",
                    "decision": "decision",
                    "project_a_fact": "fact",
                    "project_b_fact": "fact",
                    "supersession": "fact",
                }[record.category]
                run_noema(
                    run,
                    state,
                    [
                        "--cortex", cortex, "add", "--title", f"Synthetic {record.record_id}",
                        "--type", trace_type, "--author", source,
                        "--tag", "synthetic-continuity-benchmark", "--tag", record.scope,
                        "--body", render_trace_body(record, source),
                    ],
                    paths["common"],
                )
        else:
            run_noema(run, state, ["init", "--name", cortex, "--path", str(run.runtime_root / "cortexes")], paths["common"])
        if retrieval_profile == "prefetch":
            if track == "natural":
                isolated_home = codex_home.parent
                codex_install_env = run.runtime_env()
                codex_install_env["HOME"] = str(isolated_home)
                run_noema(
                    run,
                    state,
                    ["--cortex", cortex, "integrate", "codex", "install", "--scope", "user"],
                    paths["common"],
                    env=codex_install_env,
                )
                run_noema(
                    run,
                    state,
                    [
                        "--cortex", cortex, "integrate", "codex", "install", "--scope", "user",
                        "--continuity",
                    ],
                    paths["common"],
                    env=codex_install_env,
                )
                capture_root = run.capture_opencode_root(batch, track, direction, condition)
                capture_root.mkdir(parents=True, exist_ok=True, mode=0o700)
                (capture_root / ".git").mkdir(exist_ok=True)
                run_noema(
                    run,
                    state,
                    [
                        "--cortex", cortex, "integrate", "opencode", "install", "--scope", "project",
                    ],
                    capture_root,
                )
                opencode_capture_config = capture_root / "opencode.jsonc"
                capture_config = read_json(opencode_capture_config)
                permission = capture_config.setdefault("permission", {})
                permission["external_directory"] = scoped_external_permissions(paths["common"])
                write_json(opencode_capture_config, capture_config, 0o600)
            write_json(codex_home / "hooks.json", codex_prefetch_hooks(), 0o600)
        else:
            isolated_home = codex_home.parent
            codex_install_env = run.runtime_env()
            codex_install_env["HOME"] = str(isolated_home)
            run_noema(
                run,
                state,
                ["--cortex", cortex, "integrate", "codex", "install", "--scope", "user"],
                paths["common"],
                env=codex_install_env,
            )
            for workspace in (paths["project-a"], paths["project-b"], paths["outside"]):
                run_noema(
                    run,
                    state,
                    ["--cortex", cortex, "integrate", "opencode", "install", "--scope", "project"],
                    workspace,
                )
    contexts[key] = {
        "prepared": True,
        "batch": batch,
        "track": track,
        "direction": direction,
        "condition": condition,
        "retrieval_profile": retrieval_profile,
        "preseed_native_targets": preseed_native_targets,
        "native_targets_preseeded": native_targets_preseeded,
        "corpus_variant": variant,
        "cortex": run.cortex_name(batch, track, direction) if condition == "noema" else None,
    }
    run.save_state(state)
    return paths


def all_tokens_for(case: Case) -> set[str]:
    prefix = f"{case.condition.upper()}-{case.source.upper()}"
    del prefix
    return {record.value for record in build_corpus(case.corpus_variant, "TOKEN-SCAN")}


def case_workspace(run: Run, case: Case) -> Path:
    return run.context_paths(case.batch, case.track, case.direction, case.condition)[case.workspace_role]


def capture_workspace(run: Run, case: Case) -> Path:
    role = {
        "same_workspace_exact": "project-a",
        "same_workspace_rationale": "project-a",
        "sibling_workspace": "project-a",
        "outside_work": "project-a",
        "supersession": "project-a",
        "scope_distractor_abstention": "project-b",
    }[case.scenario]
    return run.context_paths(case.batch, case.track, case.direction, case.condition)[role]


def client_argv(
    run: Run,
    state: Mapping[str, Any],
    case: Case,
    workspace: Path,
    capture: bool = False,
    retrieval_profile: str = "standard",
) -> list[str]:
    client = case.source if capture else case.receiver
    if client == "codex":
        argv = [
            state["commands"]["codex"], "exec", "--ephemeral",
            "--json", "--model", run.config["models"]["codex"],
            "-c", f'model_reasoning_effort="{run.config["models"]["reasoning"]}"',
            "--disable", "plugins", "--disable", "apps",
            "--disable", "remote_plugin", "--disable", "plugin_sharing",
            *(["--approve-for-me"] if capture else ["--sandbox", "workspace-write"]),
            *(["--dangerously-bypass-hook-trust"] if case.condition == "noema" else []),
            "--cd", str(workspace), "--skip-git-repo-check",
        ]
        if case.condition == "noema" and retrieval_profile != "prefetch":
            argv.extend([
                "-c", f"mcp_servers.noema.env.XDG_CONFIG_HOME={json.dumps(str(run.runtime_root / 'xdg-config'))}",
                "-c", f"mcp_servers.noema.env.XDG_STATE_HOME={json.dumps(str(run.runtime_root / 'xdg-state'))}",
            ])
            if retrieval_profile == "fastpath-minimal":
                argv.extend([
                    "-c",
                    'mcp_servers.noema.env.NOEMA_MCP_TOOL_PROFILE="continuity-read"',
                ])
            for tool in (
                "get_instructions",
                "cortex_usage",
                "recall_context",
                "search_traces",
                "list_traces",
                "get_trace",
            ):
                argv.extend([
                    "-c", f'mcp_servers.noema.tools.{tool}.approval_mode="approve"',
                ])
            if capture:
                for tool in ("create_trace", "create_traces", "update_trace"):
                    argv.extend([
                        "-c", f'mcp_servers.noema.tools.{tool}.approval_mode="approve"',
                    ])
            argv.extend(["--add-dir", str(run.runtime_root)])
        if (
            case.condition == "noema"
            and retrieval_profile == "prefetch"
            and case.track == "natural"
        ):
            if capture:
                argv.extend(["--add-dir", str(run.runtime_root)])
            else:
                argv.extend(["--profile", "noema-continuity"])
        if capture and case.condition == "native":
            argv.extend([
                "--add-dir",
                str(run.context_paths(case.batch, case.track, case.direction, case.condition)["common"]),
            ])
        if not capture:
            argv.extend(["--output-schema", str(RESPONSE_SCHEMA)])
        argv.append("-")
        return argv
    return [
        state["commands"]["opencode"], "run", "--format", "json",
        "--model", run.config["models"]["opencode"],
        "--variant", run.config["models"]["reasoning"],
        "--dir", str(workspace), "--title", f"{case.case_id}-{'capture' if capture else 'retrieve'}",
    ]


def normalize_argv(argv: Sequence[str]) -> list[str]:
    return [value for value in argv if value]


def product_capture_argv(
    run: Run,
    state: Mapping[str, Any],
    case: Case,
    client_argv: Sequence[str],
) -> list[str]:
    return [
        state["commands"]["noema"],
        "--cortex",
        run.cortex_name(case.batch, case.track, case.direction),
        "integrate",
        case.source,
        "capture",
        "--scope",
        "user" if case.source == "codex" else "project",
        "--client-binary",
        state["commands"][case.source],
        "--",
        *client_argv[1:],
    ]


def execute_turn(
    run: Run,
    state: dict[str, Any],
    case: Case,
    *,
    capture: bool = False,
    retrieval_profile: str = "standard",
) -> tuple[ParsedEvents, float, int, str | None]:
    workspace = capture_workspace(run, case) if capture else case_workspace(run, case)
    prompt = render_prompt(case, retrieval_profile)
    if capture:
        prefix = f"{case.condition.upper()}-{case.source.upper()}"
        prompt = render_capture_prompt(case, build_corpus(case.corpus_variant, prefix))
        if case.condition == "native":
            paths = run.context_paths(case.batch, case.track, case.direction, case.condition)
            prompt += (
                "\nBenchmark-owned AGENTS.md targets:\n"
                f"- global: {paths['common'] / 'AGENTS.md'}\n"
                f"- project-a: {paths['project-a'] / 'AGENTS.md'}\n"
                f"- project-b: {paths['project-b'] / 'AGENTS.md'}\n"
                "Write each supplied memory to its matching target and do not edit other paths.\n"
            )
    suffix = "capture" if capture else "retrieve"
    prompt_path = run.raw_dir / "prompts" / f"{case.case_id}-{suffix}.txt"
    event_path = run.raw_dir / "events" / f"{case.case_id}-{suffix}.jsonl"
    stderr_path = run.raw_dir / "events" / f"{case.case_id}-{suffix}.stderr.txt"
    started = time.monotonic()
    error_code = None
    try:
        turn_env = run.runtime_env(case, capture=capture)
        if case.condition == "noema" and retrieval_profile == "fastpath-minimal":
            turn_env["NOEMA_MCP_TOOL_PROFILE"] = "continuity-read"
        if case.condition == "noema" and retrieval_profile == "prefetch":
            metrics_path = prefetch_metrics_path(run, case, capture=capture)
            metrics_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            turn_env.update({
                "NOEMA_BIN": state["commands"]["noema"],
                "NOEMA_CORTEX": run.cortex_name(case.batch, case.track, case.direction),
                "NOEMA_PREFETCH_CLIENT": case.source if capture else case.receiver,
                "NOEMA_PREFETCH_METRICS": str(metrics_path),
                "NOEMA_PREFETCH_PYTHON": sys.executable,
                "NOEMA_PREFETCH_SCRIPT": str(PREFETCH_HOOK),
            })
            client = case.source if capture else case.receiver
            if client == "opencode" and not capture:
                prefetched = run_command(
                    [sys.executable, str(PREFETCH_HOOK), "context"],
                    cwd=workspace,
                    env=turn_env,
                    timeout=30,
                    input_text=prompt,
                )
                if prefetched.returncode == 0:
                    prompt = f"{prompt.rstrip()}\n\n{prefetched.stdout.strip()}\n"
                else:
                    error_code = "prefetch_launcher_failed"
        atomic_write(prompt_path, prompt.encode())
        argv = normalize_argv(
            client_argv(
                run,
                state,
                case,
                workspace,
                capture,
                retrieval_profile,
            )
        )
        if (
            capture
            and case.track == "natural"
            and case.condition == "noema"
            and retrieval_profile == "prefetch"
        ):
            argv = product_capture_argv(run, state, case, argv)
        completed = run_command(
            argv,
            cwd=workspace,
            env=turn_env,
            timeout=int(run.config["limits"]["turn_timeout_seconds"]),
            input_text=prompt,
        )
        stdout, stderr, returncode = completed.stdout, completed.stderr, completed.returncode
    except subprocess.TimeoutExpired as error:
        stdout = error.stdout or ""
        stderr = error.stderr or ""
        returncode = 124
        error_code = "timeout"
    latency = time.monotonic() - started
    if isinstance(stdout, bytes):
        stdout = stdout.decode(errors="replace")
    if isinstance(stderr, bytes):
        stderr = stderr.decode(errors="replace")
    atomic_write(event_path, stdout.encode())
    atomic_write(stderr_path, stderr.encode())
    parsed = parse_jsonl_events(stdout, case.receiver if not capture else case.source)
    pricing = run.config.get("pricing_usd_per_million_tokens")
    if parsed.cost_usd is None and isinstance(pricing, dict):
        uncached_tokens = parsed.noncached_input_tokens or 0
        cached_tokens = parsed.cached_input_tokens or 0
        parsed.cost_usd = (
            uncached_tokens * float(pricing["input"])
            + cached_tokens * float(pricing["cached_input"])
            + (parsed.output_tokens or 0) * float(pricing["output"])
        ) / 1_000_000
    if returncode != 0 and error_code is None:
        error_code = "client_nonzero_exit"
    return parsed, latency, returncode, error_code


def capture_records(case: Case) -> list[Any]:
    prefix = f"{case.condition.upper()}-{case.source.upper()}"
    records = build_corpus(case.corpus_variant, prefix)
    relevant_values = {
        value
        for question in case.questions
        for value in (*question.expected_values, *question.forbidden_values)
    }
    return [record for record in records if record.value in relevant_values]


def validate_capture_persistence(run: Run, case: Case, parsed: ParsedEvents) -> str | None:
    if case.condition == "noema":
        if not any(
            marker in name
            for name in parsed.tool_names
            for marker in ("create_trace", "update_trace")
        ):
            return "capture_write_tool_missing"
        return None
    paths = run.context_paths(case.batch, case.track, case.direction, case.condition)
    scope_targets = {
        "global": paths["common"] / "AGENTS.md",
        "project-a": paths["project-a"] / "AGENTS.md",
        "project-b": paths["project-b"] / "AGENTS.md",
    }
    for record in capture_records(case):
        target = scope_targets[record.scope]
        if not target.is_file():
            return "capture_scope_file_missing"
        body = target.read_text(encoding="utf-8")
        if not all(value in body for value in (record.value, record.rationale, record.provenance)):
            return "capture_content_missing"
    return None


def capture_fields(
    parsed: ParsedEvents, latency: float, error_code: str | None
) -> dict[str, Any]:
    return {
        "capture_status": "completed" if error_code is None else "failed",
        "capture_error_code": error_code,
        "capture_latency_seconds": latency,
        "capture_input_tokens": parsed.input_tokens,
        "capture_cached_input_tokens": parsed.cached_input_tokens,
        "capture_noncached_input_tokens": parsed.noncached_input_tokens,
        "capture_output_tokens": parsed.output_tokens,
        "capture_total_tokens": parsed.total_tokens,
        "capture_cost_usd": parsed.cost_usd,
        "capture_tool_call_count": len(parsed.tool_names),
        "capture_tool_duration_ms": parsed.tool_duration_ms,
        "capture_noema_tool_duration_ms": parsed.noema_tool_duration_ms,
        "capture_max_inter_event_gap_ms": parsed.max_inter_event_gap_ms,
        "capture_timed_span_count": parsed.timed_span_count,
    }


def run_capture_case(
    run: Run,
    state: dict[str, Any],
    case: Case,
    *,
    retrieval_profile: str,
) -> tuple[dict[str, Any], str | None]:
    parsed, latency, returncode, error_code = execute_turn(
        run,
        state,
        case,
        capture=True,
        retrieval_profile=retrieval_profile,
    )
    if parsed.parse_errors:
        error_code = "capture_event_parse_error"
    elif returncode != 0:
        error_code = error_code or "capture_client_nonzero_exit"
    elif error_code is None:
        error_code = validate_capture_persistence(run, case, parsed)
    return capture_fields(parsed, latency, error_code), error_code


def run_retrieval_case(
    run: Run,
    state: dict[str, Any],
    case: Case,
    *,
    retrieval_profile: str = "standard",
) -> dict[str, Any]:
    parsed, latency, returncode, error_code = execute_turn(
        run, state, case, retrieval_profile=retrieval_profile
    )
    score = score_case(case, parsed.final_text, all_tokens_for(case))
    if parsed.parse_errors:
        error_code = "event_parse_error"
    if returncode == 0 and not score["valid_response"]:
        error_code = "invalid_response"
    bootstrap = any("get_instructions" in name for name in parsed.tool_names)
    recall = any("recall_context" in name for name in parsed.tool_names)
    if case.condition == "noema" and retrieval_profile == "standard" and not bootstrap:
        error_code = "noema_bootstrap_missing"
    noema_retrieval = any(
        marker in name
        for name in parsed.tool_names
        for marker in ("search_traces", "list_traces", "get_trace")
    )
    if case.condition == "noema" and retrieval_profile == "standard" and not noema_retrieval:
        error_code = "noema_retrieval_tool_missing"
    if case.condition == "noema" and retrieval_profile.startswith("fastpath"):
        recall_count = sum("recall_context" in name for name in parsed.tool_names)
        if recall_count != 1:
            error_code = "fastpath_recall_count"
        elif bootstrap or noema_retrieval:
            error_code = "fastpath_extra_retrieval"
    prefetch_metrics: dict[str, Any] | None = None
    if case.condition == "noema" and retrieval_profile == "prefetch":
        metrics_path = prefetch_metrics_path(run, case)
        if not metrics_path.exists():
            error_code = "prefetch_not_observed"
        else:
            prefetch_metrics = read_json(metrics_path)
            if not prefetch_metrics.get("success"):
                error_code = "prefetch_failed"
            elif parsed.tool_names:
                error_code = "prefetch_tool_call_observed"
    if case.condition == "native" and (bootstrap or recall or noema_retrieval):
        error_code = "native_condition_contaminated"
    status = "completed" if error_code is None else "failed"
    row = sanitized_row(case, parsed, score, status=status, latency_seconds=latency, error_code=error_code)
    row["retrieval_profile"] = retrieval_profile if case.condition == "noema" else "native"
    row["recall_context_observed"] = recall
    row["prefetch_observed"] = prefetch_metrics is not None
    row["prefetch_success"] = prefetch_metrics.get("success") if prefetch_metrics else None
    row["prefetch_engine"] = prefetch_metrics.get("engine") if prefetch_metrics else None
    row["prefetch_latency_ms"] = prefetch_metrics.get("duration_ms") if prefetch_metrics else None
    row["prefetch_hit_count"] = prefetch_metrics.get("hit_count") if prefetch_metrics else None
    row["prefetch_context_chars"] = prefetch_metrics.get("context_chars") if prefetch_metrics else None
    row["prefetch_delivery"] = (
        "codex_user_prompt_hook" if case.receiver == "codex" else "opencode_launcher"
    ) if case.condition == "noema" and retrieval_profile == "prefetch" else None
    write_json(run.raw_dir / "results" / f"{case.case_id}.json", row)
    return row


def restore_managed(run: Run, state: dict[str, Any], *, archive_runtime: bool) -> dict[str, Any]:
    if archive_runtime and run.runtime_root.exists():
        archive = run.raw_dir / f"runtime-archive-{int(time.time())}.tar.gz"
        with tarfile.open(archive, "w:gz") as bundle:
            bundle.add(run.runtime_root, arcname="runtime", recursive=True)
        os.chmod(archive, 0o600)
    allowed = [run.work_parent, run.outside_parent, run.run_dir]
    failures: list[str] = []
    for snapshot in state["managed_snapshots"]:
        if snapshot["kind"] != "absent":
            failures.append(f"unsupported pre-existing managed target: {snapshot['path']}")
            continue
        path = Path(snapshot["path"])
        try:
            safe_restore_absent(path, run.run_id, allowed)
        except FileNotFoundError:
            pass
        except BenchmarkError as error:
            failures.append(str(error))
    for parent_text, original_kind in state.get("parent_kinds", {}).items():
        parent = Path(parent_text)
        if original_kind == "absent" and parent.exists():
            try:
                parent.rmdir()
            except OSError:
                failures.append(f"created parent not empty after restore: {parent}")
    changed_guards = guard_drift(state)
    return {
        "managed_restored": not failures,
        "guard_files_unchanged": not changed_guards,
        "failures": failures,
        "changed_guard_count": len(changed_guards),
        "changed_guards": [item["label"] for item in changed_guards],
        "guard_drift": changed_guards,
    }


def cmd_smoke(args: argparse.Namespace, config: dict[str, Any]) -> int:
    if not args.confirm_live:
        raise BenchmarkError("smoke starts up to eight live model turns; rerun with --confirm-live")
    run = resolve_run(args, config)
    state = require_preflight(run)
    track = track_for(state)
    retrieval_profile = retrieval_profile_for(state)
    if state["smoke"]["status"] == "passed":
        print("smoke already passed")
        return 0
    attempt = int(state.get("smoke", {}).get("attempt", 0)) + 1
    smoke_cases = [
        case for case in cases_for_run(state, 1, track)
        if case.scenario == "same_workspace_exact"
    ]
    rows: list[dict[str, Any]] = []
    model_turns = 0
    try:
        for direction in (case.direction for case in smoke_cases if case.condition == "native"):
            for condition in ("native", "noema"):
                prepare_context(
                    run,
                    state,
                    1,
                    track,
                    direction,
                    condition,
                    retrieval_profile=retrieval_profile,
                )
        for case in smoke_cases:
            smoke_case = Case(**{**case.__dict__, "case_id": f"smoke-a{attempt:02d}-{case.case_id}"})
            captured: dict[str, Any] = {}
            capture_error = None
            if track == "natural":
                model_turns += 1
                captured, capture_error = run_capture_case(
                    run,
                    state,
                    smoke_case,
                    retrieval_profile=retrieval_profile,
                )
            if capture_error:
                parsed = ParsedEvents()
                score = score_case(smoke_case, "", all_tokens_for(smoke_case))
                row = sanitized_row(
                    smoke_case,
                    parsed,
                    score,
                    status="failed",
                    latency_seconds=0.0,
                    error_code=capture_error,
                )
                row["retrieval_profile"] = (
                    retrieval_profile if smoke_case.condition == "noema" else "native"
                )
            else:
                model_turns += 1
                row = run_retrieval_case(
                    run,
                    state,
                    smoke_case,
                    retrieval_profile=retrieval_profile,
                )
            row.update(captured)
            write_json(run.raw_dir / "results" / f"{smoke_case.case_id}.json", row)
            rows.append(row)
            if row["status"] != "completed" or row["error_code"]:
                break
    finally:
        restoration = restore_managed(run, state, archive_runtime=True)
        state["contexts"] = {}
    passed = (
        len(rows) == 4
        and all(row["status"] == "completed" for row in rows)
        and all(not row["error_code"] for row in rows)
        and restoration["managed_restored"]
        and restoration["guard_files_unchanged"]
    )
    previous_smoke = state.get("smoke", {})
    if previous_smoke.get("status") not in {None, "not_run"}:
        state.setdefault("smoke_history", []).append(previous_smoke)
    state["smoke"] = {
        "attempt": attempt,
        "status": "passed" if passed else "failed",
        "completed_at": utc_now(),
        "turns": len(rows),
        "model_turns": model_turns,
        "track": track,
        "retrieval_profile": retrieval_profile,
        "restoration": restoration,
        "failures": [
            {"case_id": row["case_id"], "error_code": row["error_code"]}
            for row in rows if row["status"] != "completed"
        ],
    }
    state["status"] = "smoke_passed" if passed else "smoke_failed"
    run.save_state(state)
    public = {"run_id": run.run_id, **state["smoke"]}
    write_json(run.report_dir / "smoke.json", public, 0o644)
    print(json.dumps(public, indent=2))
    return 0 if passed else 1


NATIVE_CAPTURE_TARGET_PLAN = (
    ("pair-01", "absent", 1, "same_workspace_exact"),
    ("pair-01", "preseeded", 3, "same_workspace_exact"),
    ("pair-02", "preseeded", 5, "same_workspace_rationale"),
    ("pair-02", "absent", 7, "same_workspace_rationale"),
    ("pair-03", "preseeded", 2, "same_workspace_exact"),
    ("pair-03", "absent", 4, "same_workspace_exact"),
    ("pair-04", "absent", 6, "same_workspace_rationale"),
    ("pair-04", "preseeded", 8, "same_workspace_rationale"),
)


def native_capture_target_cases(state: Mapping[str, Any]) -> list[dict[str, Any]]:
    planned: list[dict[str, Any]] = []
    for order, (pair_id, target_state, batch, scenario) in enumerate(
        NATIVE_CAPTURE_TARGET_PLAN, 1
    ):
        case = next(
            candidate
            for candidate in cases_for_run(state, batch, "natural")
            if candidate.direction == "opencode_to_codex"
            and candidate.condition == "native"
            and candidate.scenario == scenario
        )
        experiment_case = Case(
            **{
                **case.__dict__,
                "case_id": (
                    f"native-target-{pair_id}-{target_state}-{case.case_id}"
                ),
            }
        )
        planned.append(
            {
                "pair_id": pair_id,
                "target_state": target_state,
                "order": order,
                "case": experiment_case,
            }
        )
    return planned


def native_capture_targets(run: Run, case: Case) -> list[Path]:
    paths = run.context_paths(case.batch, case.track, case.direction, case.condition)
    scope_targets = {
        "global": paths["common"] / "AGENTS.md",
        "project-a": paths["project-a"] / "AGENTS.md",
        "project-b": paths["project-b"] / "AGENTS.md",
    }
    scopes = {record.scope for record in capture_records(case)}
    return [scope_targets[scope] for scope in sorted(scopes)]


def preseed_native_capture_targets(run: Run, case: Case) -> list[Path]:
    targets = native_capture_targets(run, case)
    for target in targets:
        atomic_write(target, b"", 0o644)
    return targets


def summarize_native_capture_target_rows(rows: Sequence[Mapping[str, Any]]) -> dict[str, Any]:
    arms: dict[str, Any] = {}
    for target_state in ("absent", "preseeded"):
        selected = [row for row in rows if row["target_state"] == target_state]
        completed = [row for row in selected if row["capture_status"] == "completed"]
        latencies = [float(row["capture_latency_seconds"]) for row in selected]
        tokens = [
            int(row["capture_total_tokens"])
            for row in selected
            if row.get("capture_total_tokens") is not None
        ]
        arms[target_state] = {
            "turns": len(selected),
            "completed": len(completed),
            "capture_success_rate": len(completed) / len(selected) if selected else None,
            "latency_median_seconds": statistics.median(latencies) if latencies else None,
            "latency_p95_seconds": percentile(latencies, 0.95) if latencies else None,
            "tokens_median": statistics.median(tokens) if tokens else None,
            "error_codes": sorted(
                row["capture_error_code"]
                for row in selected
                if row.get("capture_error_code")
            ),
        }
    pair_outcomes = {
        "both_completed": 0,
        "only_absent_completed": 0,
        "only_preseeded_completed": 0,
        "neither_completed": 0,
    }
    for pair_id in sorted({str(row["pair_id"]) for row in rows}):
        pair = {str(row["target_state"]): row for row in rows if row["pair_id"] == pair_id}
        absent = pair.get("absent", {}).get("capture_status") == "completed"
        preseeded = pair.get("preseeded", {}).get("capture_status") == "completed"
        if absent and preseeded:
            pair_outcomes["both_completed"] += 1
        elif absent:
            pair_outcomes["only_absent_completed"] += 1
        elif preseeded:
            pair_outcomes["only_preseeded_completed"] += 1
        else:
            pair_outcomes["neither_completed"] += 1
    absent_rate = arms["absent"]["capture_success_rate"]
    preseeded_rate = arms["preseeded"]["capture_success_rate"]
    return {
        "arms": arms,
        "preseeded_minus_absent_success_rate": (
            preseeded_rate - absent_rate
            if preseeded_rate is not None and absent_rate is not None
            else None
        ),
        "paired_outcomes": pair_outcomes,
    }


def cmd_native_capture_target_experiment(
    args: argparse.Namespace, config: dict[str, Any]
) -> int:
    if not args.confirm_live:
        raise BenchmarkError(
            "experiment-native-capture-target starts eight live model turns; "
            "rerun with --confirm-live"
        )
    run = resolve_run(args, config)
    state = require_preflight(run)
    if track_for(state) != "natural" or retrieval_profile_for(state) != "prefetch":
        raise BenchmarkError(
            "the native capture target experiment requires a natural/prefetch preflight"
        )
    experiment_key = "native_capture_target"
    if state.get("experiments", {}).get(experiment_key):
        raise BenchmarkError("this run already contains a native capture target experiment")
    planned = native_capture_target_cases(state)
    rows: list[dict[str, Any]] = []
    try:
        for item in planned:
            case = item["case"]
            print(f"[{item['order']:02d}/08] {case.case_id}", flush=True)
            prepare_context(
                run,
                state,
                case.batch,
                case.track,
                case.direction,
                case.condition,
                retrieval_profile="prefetch",
                preseed_native_targets=False,
            )
            targets = native_capture_targets(run, case)
            if item["target_state"] == "preseeded":
                preseed_native_capture_targets(run, case)
            elif any(target.exists() for target in targets):
                raise BenchmarkError("absent-arm AGENTS.md target was unexpectedly present")
            captured, _capture_error = run_capture_case(
                run,
                state,
                case,
                retrieval_profile="prefetch",
            )
            rows.append(
                {
                    "case_id": case.case_id,
                    "pair_id": item["pair_id"],
                    "order": item["order"],
                    "target_state": item["target_state"],
                    "scenario": case.scenario,
                    "corpus_variant": case.corpus_variant,
                    "required_target_count": len(targets),
                    "materialized_target_count": sum(target.is_file() for target in targets),
                    "nonempty_target_count": sum(
                        target.is_file() and target.stat().st_size > 0 for target in targets
                    ),
                    **captured,
                }
            )
    finally:
        restoration = restore_managed(run, state, archive_runtime=True)
        state["contexts"] = {}

    summary = summarize_native_capture_target_rows(rows)
    execution_complete = len(rows) == len(planned)
    restoration_complete = (
        restoration["managed_restored"] and restoration["guard_files_unchanged"]
    )
    result = {
        "run_id": run.run_id,
        "status": "completed" if execution_complete and restoration_complete else "failed",
        "completed_at": utc_now(),
        "model_turns": len(rows),
        "descriptive_only": True,
        "independent_variable": "required AGENTS.md targets absent or pre-created empty",
        "controls": {
            "source": "opencode",
            "condition": "native",
            "fresh_sessions": True,
            "retrieval_turns": 0,
            "paired_corpora": True,
            "order_balanced": True,
        },
        "summary": summary,
        "failures": [
            {"case_id": row["case_id"], "error_code": row["capture_error_code"]}
            for row in rows
            if row["capture_status"] != "completed"
        ],
        "restoration": restoration,
        "raw_data_private": True,
    }
    state.setdefault("experiments", {})[experiment_key] = result
    state["status"] = (
        "experiment_complete" if result["status"] == "completed" else "experiment_failed"
    )
    run.save_state(state)
    write_json(run.report_dir / "native-capture-target-experiment.json", result, 0o644)
    write_json(run.report_dir / "native-capture-target-turns.json", rows, 0o644)
    print(json.dumps(result, indent=2))
    return 0 if result["status"] == "completed" else 1


def cmd_fastpath_experiment(args: argparse.Namespace, config: dict[str, Any]) -> int:
    if not args.confirm_live:
        raise BenchmarkError(
            "experiment-fastpath starts four live model turns; rerun with --confirm-live"
        )
    run = resolve_run(args, config)
    state = require_preflight(run)
    previous = state.get("experiments", {}).get("fastpath_smoke")
    if previous:
        raise BenchmarkError("this run already contains a fast-path experiment")
    cases = [
        case
        for case in build_batch_cases(1)
        if case.scenario == "same_workspace_exact"
    ]
    retrieval_profile = f"fastpath-{args.catalog}"
    rows: list[dict[str, Any]] = []
    try:
        for direction in (case.direction for case in cases if case.condition == "native"):
            for condition in ("native", "noema"):
                prepare_context(run, state, 1, "deterministic", direction, condition)
        for case in cases:
            experiment_case = Case(
                **{**case.__dict__, "case_id": f"fastpath-{case.case_id}"}
            )
            rows.append(
                run_retrieval_case(
                    run,
                    state,
                    experiment_case,
                    retrieval_profile=retrieval_profile,
                )
            )
    finally:
        restoration = restore_managed(run, state, archive_runtime=True)
        state["contexts"] = {}

    summary = summarize_rows(rows, int(config["limits"]["bootstrap_samples"]))
    native = summary["conditions"]["native"]
    fastpath = summary["conditions"]["noema"]
    gates = {
        "all_turns_completed": len(rows) == 4
        and all(row["status"] == "completed" for row in rows),
        "exact_and_provenance": (fastpath["exact_accuracy"] or 0) >= 0.95
        and (fastpath["provenance_accuracy"] or 0) >= 0.95,
        "no_safety_errors": fastpath["stale_rate"] == 0
        and fastpath["hallucination_rate"] == 0
        and fastpath["scope_leakage_rate"] == 0,
        "one_tool_call": fastpath["tool_calls_mean"] == 1,
        "token_overhead": native["tokens_median"] is not None
        and fastpath["tokens_median"] is not None
        and fastpath["tokens_median"] <= 1.5 * native["tokens_median"],
        "latency_overhead": native["latency_p95_seconds"] is not None
        and fastpath["latency_p95_seconds"] is not None
        and fastpath["latency_p95_seconds"] <= 2 * native["latency_p95_seconds"],
        "restoration": restoration["managed_restored"]
        and restoration["guard_files_unchanged"],
    }
    result = {
        "run_id": run.run_id,
        "status": "passed" if all(gates.values()) else "failed",
        "completed_at": utc_now(),
        "retrieval_turns": len(rows),
        "descriptive_only": True,
        "catalog_profile": args.catalog,
        "conditions": summary["conditions"],
        "gates": gates,
        "failures": [
            {"case_id": row["case_id"], "error_code": row["error_code"]}
            for row in rows
            if row["status"] != "completed"
        ],
        "restoration": restoration,
    }
    state.setdefault("experiments", {})["fastpath_smoke"] = result
    run.save_state(state)
    write_json(run.report_dir / "fastpath-experiment.json", result, 0o644)
    write_json(run.report_dir / "fastpath-turns.json", rows, 0o644)
    print(json.dumps(result, indent=2))
    return 0 if result["status"] == "passed" else 1


def cmd_prefetch_experiment(args: argparse.Namespace, config: dict[str, Any]) -> int:
    scenarios = {
        "smoke": {"same_workspace_exact"},
        "challenge": {"outside_work", "supersession", "scope_distractor_abstention"},
        "coverage": {"same_workspace_rationale", "sibling_workspace", "outside_work"},
    }[args.phase]
    expected_turns = {"smoke": 4, "challenge": 12, "coverage": 12}[args.phase]
    if not args.confirm_live:
        raise BenchmarkError(
            f"experiment-prefetch {args.phase} starts {expected_turns} live model turns; "
            "rerun with --confirm-live"
        )
    run = resolve_run(args, config)
    state = require_preflight(run)
    experiment_key = f"prefetch_{args.phase}"
    if state.get("experiments", {}).get(experiment_key):
        raise BenchmarkError(f"this run already contains a prefetch {args.phase} experiment")
    cases = [case for case in build_batch_cases(1) if case.scenario in scenarios]
    rows: list[dict[str, Any]] = []
    try:
        directions = sorted({case.direction for case in cases})
        for direction in directions:
            for condition in ("native", "noema"):
                prepare_context(
                    run,
                    state,
                    1,
                    "deterministic",
                    direction,
                    condition,
                    retrieval_profile="prefetch",
                )
        for case in cases:
            experiment_case = Case(
                **{**case.__dict__, "case_id": f"prefetch-{args.phase}-{case.case_id}"}
            )
            rows.append(
                run_retrieval_case(
                    run,
                    state,
                    experiment_case,
                    retrieval_profile="prefetch",
                )
            )
    finally:
        restoration = restore_managed(run, state, archive_runtime=True)
        state["contexts"] = {}

    summary = summarize_rows(rows, int(config["limits"]["bootstrap_samples"]))
    native = summary["conditions"]["native"]
    prefetch = summary["conditions"]["noema"]
    prefetch_rows = [row for row in rows if row["condition"] == "noema"]
    hook_latencies = [
        float(row["prefetch_latency_ms"])
        for row in prefetch_rows
        if row.get("prefetch_latency_ms") is not None
    ]
    hook_hits = [
        float(row["prefetch_hit_count"])
        for row in prefetch_rows
        if row.get("prefetch_hit_count") is not None
    ]
    hook_context_sizes = [
        float(row["prefetch_context_chars"])
        for row in prefetch_rows
        if row.get("prefetch_context_chars") is not None
    ]
    gates = {
        "all_turns_completed": len(rows) == expected_turns
        and all(row["status"] == "completed" for row in rows),
        "exact_and_provenance": (prefetch["exact_accuracy"] or 0) >= 0.95
        and (prefetch["provenance_accuracy"] or 0) >= 0.95,
        "no_safety_errors": prefetch["stale_rate"] == 0
        and prefetch["hallucination_rate"] == 0
        and prefetch["scope_leakage_rate"] == 0,
        "prefetch_observed": bool(prefetch_rows)
        and all(row.get("prefetch_success") for row in prefetch_rows),
        "zero_tool_calls": prefetch["tool_calls_mean"] == 0,
        "token_overhead": native["tokens_median"] is not None
        and prefetch["tokens_median"] is not None
        and prefetch["tokens_median"] <= 1.5 * native["tokens_median"],
        "latency_overhead": native["latency_p95_seconds"] is not None
        and prefetch["latency_p95_seconds"] is not None
        and prefetch["latency_p95_seconds"] <= 2 * native["latency_p95_seconds"],
        "restoration": restoration["managed_restored"]
        and restoration["guard_files_unchanged"],
    }
    result = {
        "run_id": run.run_id,
        "phase": args.phase,
        "status": "passed" if all(gates.values()) else "failed",
        "completed_at": utc_now(),
        "retrieval_turns": len(rows),
        "descriptive_only": True,
        "delivery": {
            "codex": "UserPromptSubmit hook",
            "opencode": "launcher-side prefetch",
        },
        "conditions": summary["conditions"],
        "paired": summary["paired"],
        "prefetch_hook": {
            "observed_turns": len(hook_latencies),
            "latency_median_ms": statistics.median(hook_latencies) if hook_latencies else None,
            "latency_p95_ms": percentile(hook_latencies, 0.95) if hook_latencies else None,
            "hit_count_mean": statistics.fmean(hook_hits) if hook_hits else None,
            "context_chars_mean": statistics.fmean(hook_context_sizes) if hook_context_sizes else None,
        },
        "gates": gates,
        "failures": [
            {"case_id": row["case_id"], "error_code": row["error_code"]}
            for row in rows
            if row["status"] != "completed"
        ],
        "restoration": restoration,
    }
    state.setdefault("experiments", {})[experiment_key] = result
    run.save_state(state)
    write_json(run.report_dir / f"prefetch-{args.phase}-experiment.json", result, 0o644)
    write_json(run.report_dir / f"prefetch-{args.phase}-turns.json", rows, 0o644)
    print(json.dumps(result, indent=2))
    return 0 if result["status"] == "passed" else 1


def next_batch_number(state: Mapping[str, Any], track: str) -> int:
    completed = sorted(
        int(number) for number, item in state.get("batches", {}).get(track, {}).items()
        if item.get("status") == "completed"
    )
    return (completed[-1] + 1) if completed else 1


def authorize_checkpoint(state: dict[str, Any], checkpoint: int, provided: int | None) -> None:
    if checkpoint <= 0:
        return
    if any(
        item.get("checkpoint") == checkpoint
        for item in state.get("continuation_authorizations", [])
    ):
        return
    if provided != checkpoint:
        raise BenchmarkError(
            f"checkpoint {checkpoint} is paused; the operator must explicitly continue with "
            f"--continue-after-checkpoint {checkpoint}"
        )
    report = state.get("checkpoints", {}).get(str(checkpoint))
    if not report or report.get("status") != "reported":
        raise BenchmarkError(f"checkpoint {checkpoint} must be analyzed before continuation")
    state.setdefault("continuation_authorizations", []).append(
        {"checkpoint": checkpoint, "authorized_at": utc_now()}
    )


def natural_track_allowed(state: Mapping[str, Any]) -> bool:
    return bool(state.get("natural_evidence"))


def batches_per_checkpoint(track: str) -> int:
    return 1 if track == "natural" else 2


def cmd_run_batch(args: argparse.Namespace, config: dict[str, Any]) -> int:
    if not args.confirm_live:
        raise BenchmarkError("run-batch starts live model turns; rerun with --confirm-live")
    run = resolve_run(args, config)
    state = require_preflight(run)
    retrieval_profile = retrieval_profile_for(state)
    preregistered_track = track_for(state)
    if state["smoke"]["status"] != "passed":
        raise BenchmarkError("the mechanics-only smoke gate must pass before run-batch")
    track = args.track or preregistered_track
    if track != preregistered_track:
        raise BenchmarkError(f"run is preregistered for the {preregistered_track} track")
    if track == "natural" and not natural_track_allowed(state):
        raise BenchmarkError("natural capture requires hash-locked eligible deterministic evidence")
    expected_batch = next_batch_number(state, track)
    batch = args.batch or expected_batch
    if batch != expected_batch:
        raise BenchmarkError(f"batches are sequential; the next {track} batch is {expected_batch}")
    maximum = (
        int(config["limits"].get("natural_max_batches", 2))
        if track == "natural"
        else int(config["limits"]["max_batches"])
    )
    if batch < 1 or batch > maximum:
        raise BenchmarkError(f"batch exceeds the preregistered {track} ceiling of {maximum}")
    interval = batches_per_checkpoint(track)
    if batch > 1 and (batch - 1) % interval == 0:
        checkpoint = (batch - 1) // interval
        authorize_checkpoint(state, checkpoint, args.continue_after_checkpoint)
    batch_key = str(batch)
    batch_state = state.setdefault("batches", {}).setdefault(track, {}).setdefault(
        batch_key,
        {
            "status": "running",
            "started_at": utc_now(),
            "completed_cases": [],
            "retrieval_profile": retrieval_profile,
        },
    )
    if batch_state.get("retrieval_profile", "standard") != retrieval_profile:
        raise BenchmarkError("batch retrieval profile differs from its preregistered run")
    cases = cases_for_run(state, batch, track)
    for direction in ("codex_to_opencode", "opencode_to_codex"):
        for condition in ("native", "noema"):
            prepare_context(
                run,
                state,
                batch,
                track,
                direction,
                condition,
                retrieval_profile=retrieval_profile,
            )
    completed = set(batch_state["completed_cases"])
    for index, case in enumerate(cases, 1):
        result_path = run.raw_dir / "results" / f"{case.case_id}.json"
        if case.case_id in completed and result_path.exists():
            continue
        print(f"[{index:02d}/24] {case.case_id}", flush=True)
        if track == "natural":
            captured, capture_error = run_capture_case(
                run,
                state,
                case,
                retrieval_profile=retrieval_profile,
            )
            if capture_error:
                empty = ParsedEvents()
                score = score_case(case, "", all_tokens_for(case))
                row = sanitized_row(
                    case, empty, score, status="failed", latency_seconds=0.0,
                    error_code=capture_error,
                )
                row["retrieval_profile"] = (
                    retrieval_profile if case.condition == "noema" else "native"
                )
            else:
                row = run_retrieval_case(
                    run,
                    state,
                    case,
                    retrieval_profile=retrieval_profile,
                )
            row.update(captured)
            write_json(result_path, row)
        else:
            row = run_retrieval_case(
                run,
                state,
                case,
                retrieval_profile=retrieval_profile,
            )
        completed.add(case.case_id)
        batch_state["completed_cases"] = sorted(completed)
        batch_state["failed_cases"] = sorted(
            case_id for case_id in completed
            if read_json(run.raw_dir / "results" / f"{case_id}.json")["status"] != "completed"
        )
        run.save_state(state)
    batch_state["status"] = "completed"
    batch_state["completed_at"] = utc_now()
    state["status"] = "checkpoint_pending" if batch % interval == 0 else "batch_complete"
    run.save_state(state)
    public = {
        "run_id": run.run_id,
        "batch": batch,
        "track": track,
        "retrieval_profile": retrieval_profile,
        "retrieval_turns": 24,
        "model_turns": 48 if track == "natural" else 24,
        "failed_turns": len(batch_state.get("failed_cases", [])),
        "checkpoint_required": batch % interval == 0,
    }
    print(json.dumps(public, indent=2))
    return 1 if public["failed_turns"] else 0


def load_checkpoint_rows(
    run: Run, state: Mapping[str, Any], checkpoint: int, track: str
) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for batch in range(1, checkpoint * batches_per_checkpoint(track) + 1):
        for case in cases_for_run(state, batch, track):
            path = run.raw_dir / "results" / f"{case.case_id}.json"
            if not path.exists():
                raise BenchmarkError(f"checkpoint incomplete; missing {case.case_id}")
            rows.append(read_json(path))
    return rows


def summarize_prefetch_metrics(rows: Sequence[Mapping[str, Any]]) -> dict[str, Any] | None:
    prefetch_rows = [
        row
        for row in rows
        if row.get("condition") == "noema" and row.get("retrieval_profile") == "prefetch"
    ]
    if not prefetch_rows:
        return None
    latencies = [
        float(row["prefetch_latency_ms"])
        for row in prefetch_rows
        if row.get("prefetch_latency_ms") is not None
    ]
    hits = [
        float(row["prefetch_hit_count"])
        for row in prefetch_rows
        if row.get("prefetch_hit_count") is not None
    ]
    context_sizes = [
        float(row["prefetch_context_chars"])
        for row in prefetch_rows
        if row.get("prefetch_context_chars") is not None
    ]
    return {
        "observed_turns": len(prefetch_rows),
        "successful_turns": sum(bool(row.get("prefetch_success")) for row in prefetch_rows),
        "latency_median_ms": statistics.median(latencies) if latencies else None,
        "latency_p95_ms": percentile(latencies, 0.95) if latencies else None,
        "hit_count_mean": statistics.fmean(hits) if hits else None,
        "context_chars_mean": statistics.fmean(context_sizes) if context_sizes else None,
    }


def summarize_capture_metrics(rows: Sequence[Mapping[str, Any]]) -> dict[str, Any] | None:
    capture_rows = [row for row in rows if row.get("capture_status")]
    if not capture_rows:
        return None
    conditions: dict[str, Any] = {}
    for condition in ("native", "noema"):
        selected = [row for row in capture_rows if row.get("condition") == condition]
        latencies = [
            float(row["capture_latency_seconds"])
            for row in selected
            if row.get("capture_latency_seconds") is not None
        ]
        tokens = [
            float(row["capture_total_tokens"])
            for row in selected
            if row.get("capture_total_tokens") is not None
        ]
        tool_calls = [float(row.get("capture_tool_call_count", 0)) for row in selected]
        tool_durations = [
            float(row["capture_tool_duration_ms"])
            for row in selected
            if row.get("capture_tool_duration_ms") is not None
        ]
        noema_tool_durations = [
            float(row["capture_noema_tool_duration_ms"])
            for row in selected
            if row.get("capture_noema_tool_duration_ms") is not None
        ]
        event_gaps = [
            float(row["capture_max_inter_event_gap_ms"])
            for row in selected
            if row.get("capture_max_inter_event_gap_ms") is not None
        ]
        conditions[condition] = {
            "turns": len(selected),
            "successful_turns": sum(row.get("capture_status") == "completed" for row in selected),
            "success_rate": (
                sum(row.get("capture_status") == "completed" for row in selected) / len(selected)
                if selected
                else None
            ),
            "tokens_median": statistics.median(tokens) if tokens else None,
            "latency_p50_seconds": statistics.median(latencies) if latencies else None,
            "latency_p95_seconds": percentile(latencies, 0.95) if latencies else None,
            "tool_calls_mean": statistics.fmean(tool_calls) if tool_calls else None,
            "tool_duration_p95_ms": percentile(tool_durations, 0.95) if tool_durations else None,
            "noema_tool_duration_p95_ms": (
                percentile(noema_tool_durations, 0.95) if noema_tool_durations else None
            ),
            "timing_observed_turns": len(event_gaps),
            "max_inter_event_gap_seconds": max(event_gaps) / 1000 if event_gaps else None,
        }
    return {"turns": len(capture_rows), "conditions": conditions}


def natural_checkpoint_decision(
    summary: Mapping[str, Any], capture: Mapping[str, Any]
) -> dict[str, Any]:
    native = summary["conditions"]["native"]
    noema = summary["conditions"]["noema"]
    native_capture = capture["conditions"]["native"]
    noema_capture = capture["conditions"]["noema"]
    harm = (
        noema["failed_turns"] > 0
        or noema["hallucination_rate"] > 0.10
        or noema["scope_leakage_rate"] > 0.10
        or (noema_capture["success_rate"] or 0.0) < 0.90
    )
    plausible = (
        noema["macro_f1"] is not None
        and native["macro_f1"] is not None
        and noema["macro_f1"] >= native["macro_f1"] - 0.03
        and (native_capture["success_rate"] or 0.0) >= 0.90
        and (noema_capture["success_rate"] or 0.0) >= 0.90
    )
    if harm:
        recommendation = "stop_harm_or_instability"
        conclusion = "Natural Noema capture or handoff showed harm or instability."
    elif plausible:
        recommendation = "review_complete"
        conclusion = "The bounded natural capture-and-handoff track completed without a material regression."
    else:
        recommendation = "stop_inconclusive"
        conclusion = "Natural capture-and-handoff evidence was inconclusive."
    return {
        "recommendation": recommendation,
        "conclusion": conclusion,
        "descriptive_only": True,
        "integration_unreliable": harm,
        "natural_track_eligible": False,
    }


def guard_status(run: Run, state: Mapping[str, Any]) -> dict[str, Any]:
    changed = guard_drift(state)
    return {
        "guard_files_unchanged": not changed,
        "changed_guard_count": len(changed),
        "changed_guards": [item["label"] for item in changed],
        "guard_drift": changed,
        "benchmark_runtime_retained": run.runtime_root.exists(),
        "restore_command_required": run.runtime_root.exists() or run.work_root.exists() or run.outside_root.exists(),
    }


def markdown_report(run: Run, checkpoint: int, summary: Mapping[str, Any], decision: Mapping[str, Any], restoration: Mapping[str, Any]) -> str:
    native, noema, paired = summary["conditions"]["native"], summary["conditions"]["noema"], summary["paired"]
    def pct(value: float | None) -> str:
        return "n/a" if value is None else f"{100 * value:.1f}%"
    def num(value: float | None, digits: int = 2) -> str:
        return "n/a" if value is None else f"{value:.{digits}f}"
    prefetch = summary.get("prefetch_metrics")
    track = str(summary.get("track", "deterministic"))
    prefetch_section = ""
    if isinstance(prefetch, Mapping):
        prefetch_section = f"""
## Prefetch delivery

| Measure | Value |
|---|---:|
| Observed successful turns | {prefetch['successful_turns']}/{prefetch['observed_turns']} |
| Median local latency | {num(prefetch['latency_median_ms'])} ms |
| p95 local latency | {num(prefetch['latency_p95_ms'])} ms |
| Mean retrieved hits | {num(prefetch['hit_count_mean'])} |
| Mean context characters | {num(prefetch['context_chars_mean'], 0)} |
"""
    capture = summary.get("capture_metrics")
    capture_section = ""
    if isinstance(capture, Mapping):
        capture_native = capture["conditions"]["native"]
        capture_noema = capture["conditions"]["noema"]
        capture_section = f"""
## Capture phase

| Measure | Native | Noema |
|---|---:|---:|
| Successful capture turns | {capture_native['successful_turns']}/{capture_native['turns']} | {capture_noema['successful_turns']}/{capture_noema['turns']} |
| Median total tokens | {num(capture_native['tokens_median'], 0)} | {num(capture_noema['tokens_median'], 0)} |
| p50 latency (seconds) | {num(capture_native['latency_p50_seconds'])} | {num(capture_noema['latency_p50_seconds'])} |
| p95 latency (seconds) | {num(capture_native['latency_p95_seconds'])} | {num(capture_noema['latency_p95_seconds'])} |
| Mean tool calls | {num(capture_native['tool_calls_mean'])} | {num(capture_noema['tool_calls_mean'])} |
| Capture turns with completed timestamps | {capture_native['timing_observed_turns']}/{capture_native['turns']} | {capture_noema['timing_observed_turns']}/{capture_noema['turns']} |
| p95 completed tool time (ms) | {num(capture_native['tool_duration_p95_ms'])} | {num(capture_noema['tool_duration_p95_ms'])} |
| p95 completed Noema tool time (ms) | {num(capture_native['noema_tool_duration_p95_ms'])} | {num(capture_noema['noema_tool_duration_p95_ms'])} |
| Largest observed client/model gap (seconds) | {num(capture_native['max_inter_event_gap_seconds'])} | {num(capture_noema['max_inter_event_gap_seconds'])} |
"""
    qualification_note = (
        "This natural checkpoint is descriptive and cannot replace the completed deterministic qualification."
        if track == "natural"
        else "No success claim is permitted before 192 retrieval turns. Every completed checkpoint is retained."
    )
    return f"""# Omarchy continuity checkpoint {checkpoint}

This is a sanitized report. Raw prompts, responses, event JSONL, paths, and session IDs remain private.

## Outcome

- Retrieval turns: {summary['retrieval_turns']}
- Preregistered track: `{track}`
- Preregistered retrieval profile: `{summary['retrieval_profile']}`
- Preregistered corpus cycle: `{summary['corpus_cycle']}`
- Recommendation: `{decision['recommendation']}`
- Conclusion: {decision['conclusion']}
- Noema minus native macro F1: {pct(paired['noema_minus_native'])}
- Descriptive paired-bootstrap 95% interval: [{pct(paired['bootstrap_95_low'])}, {pct(paired['bootstrap_95_high'])}]

## Accuracy and safety

| Measure | Native | Noema |
|---|---:|---:|
| Macro F1 | {pct(native['macro_f1'])} | {pct(noema['macro_f1'])} |
| Exact-value accuracy | {pct(native['exact_accuracy'])} | {pct(noema['exact_accuracy'])} |
| Rationale accuracy | {pct(native['rationale_accuracy'])} | {pct(noema['rationale_accuracy'])} |
| Provenance accuracy | {pct(native['provenance_accuracy'])} | {pct(noema['provenance_accuracy'])} |
| Stale-value rate | {pct(native['stale_rate'])} | {pct(noema['stale_rate'])} |
| Hallucination rate | {pct(native['hallucination_rate'])} | {pct(noema['hallucination_rate'])} |
| Scope-leakage rate | {pct(native['scope_leakage_rate'])} | {pct(noema['scope_leakage_rate'])} |

## Efficiency

| Measure | Native | Noema |
|---|---:|---:|
| Median total tokens | {num(native['total_tokens_median'], 0)} | {num(noema['total_tokens_median'], 0)} |
| Median non-cached input tokens | {num(native['noncached_input_tokens_median'], 0)} | {num(noema['noncached_input_tokens_median'], 0)} |
| Median cached input tokens | {num(native['cached_input_tokens_median'], 0)} | {num(noema['cached_input_tokens_median'], 0)} |
| Median output tokens | {num(native['output_tokens_median'], 0)} | {num(noema['output_tokens_median'], 0)} |
| Turns with reported cost | {native['cost_observed_turns']}/{native['turns']} | {noema['cost_observed_turns']}/{noema['turns']} |
| Median reported cost (USD) | {num(native['cost_median_usd'], 4)} | {num(noema['cost_median_usd'], 4)} |
| p50 latency (seconds) | {num(native['latency_p50_seconds'])} | {num(noema['latency_p50_seconds'])} |
| p95 latency (seconds) | {num(native['latency_p95_seconds'])} | {num(noema['latency_p95_seconds'])} |
| Mean tool calls | {num(native['tool_calls_mean'])} | {num(noema['tool_calls_mean'])} |

Cost is `n/a` when a client event stream does not expose cost and no approved pricing table was configured.
{prefetch_section}
{capture_section}

## Subsets

| Subset | Noema macro F1 | Noema advantage |
|---|---:|---:|
| Boundary | {pct(summary['subsets']['boundary']['noema_macro_f1'])} | {pct(summary['subsets']['boundary']['advantage'])} |
| Update | {pct(summary['subsets']['update']['noema_macro_f1'])} | {pct(summary['subsets']['update']['advantage'])} |
| Scoping | {pct(summary['subsets']['scoping']['noema_macro_f1'])} | {pct(summary['subsets']['scoping']['advantage'])} |

## Failures and restoration

- Native failed turns: {native['failed_turns']}
- Noema failed turns: {noema['failed_turns']}
- Guard files unchanged: {str(restoration['guard_files_unchanged']).lower()}
- Changed guards: {', '.join(restoration.get('changed_guards', [])) or 'none'}
- Isolated runtime retained for the next authorized batch: {str(restoration['benchmark_runtime_retained']).lower()}
- Explicit restore still required: {str(restoration['restore_command_required']).lower()}

{qualification_note}
"""


def cmd_analyze(args: argparse.Namespace, config: dict[str, Any]) -> int:
    run = resolve_run(args, config)
    state = require_preflight(run)
    track = track_for(state)
    retrieval_profile = retrieval_profile_for(state)
    checkpoint = args.checkpoint
    maximum_checkpoints = (
        int(config["limits"].get("natural_max_batches", 2))
        if track == "natural"
        else 6
    )
    if checkpoint < 1 or checkpoint > maximum_checkpoints:
        raise BenchmarkError(f"checkpoint must be in 1..{maximum_checkpoints}")
    for batch in range(1, checkpoint * batches_per_checkpoint(track) + 1):
        item = state.get("batches", {}).get(track, {}).get(str(batch))
        if not item or item.get("status") != "completed":
            raise BenchmarkError(f"{track} batch {batch} is not complete")
    checkpoint_dir = run.report_dir / f"checkpoint-{checkpoint:03d}"
    if checkpoint_dir.exists():
        raise BenchmarkError(f"checkpoint {checkpoint} is immutable and has already been generated")
    rows = load_checkpoint_rows(run, state, checkpoint, track)
    for row in rows:
        expected_profile = retrieval_profile if row["condition"] == "noema" else "native"
        if row.get("retrieval_profile") != expected_profile:
            raise BenchmarkError("checkpoint contains a mixed or unexpected retrieval profile")
    summary = summarize_rows(rows, int(config["limits"]["bootstrap_samples"]))
    summary["track"] = track
    summary["retrieval_profile"] = retrieval_profile
    summary["corpus_cycle"] = corpus_cycle_for(state)
    summary["prefetch_metrics"] = summarize_prefetch_metrics(rows)
    summary["capture_metrics"] = summarize_capture_metrics(rows)
    if track == "natural":
        if not isinstance(summary["capture_metrics"], Mapping):
            raise BenchmarkError("natural checkpoint is missing capture metrics")
        decision = natural_checkpoint_decision(summary, summary["capture_metrics"])
    else:
        decision = checkpoint_decision(summary)
    restoration = guard_status(run, state)
    report = {
        "schema_version": 1,
        "run_id": run.run_id,
        "checkpoint": checkpoint,
        "track": track,
        "retrieval_profile": retrieval_profile,
        "corpus_cycle": corpus_cycle_for(state),
        "generated_at": utc_now(),
        "summary": summary,
        "decision": decision,
        "restoration": restoration,
        "raw_data_private": True,
    }
    checkpoint_dir.mkdir(parents=True)
    write_json(checkpoint_dir / "summary.json", report, 0o644)
    write_json(checkpoint_dir / "turns.json", rows, 0o644)
    write_tsv(checkpoint_dir / "turns.tsv", rows)
    atomic_write(
        checkpoint_dir / "report.md",
        markdown_report(run, checkpoint, summary, decision, restoration).encode(),
        0o644,
    )
    state.setdefault("checkpoints", {})[str(checkpoint)] = {
        "status": "reported",
        "reported_at": utc_now(),
        "report_dir": str(checkpoint_dir),
        "recommendation": decision["recommendation"],
        "retrieval_profile": retrieval_profile,
        "natural_track_eligible": decision.get("natural_track_eligible", False),
    }
    state["status"] = "checkpoint_paused"
    run.save_state(state)
    print(json.dumps({"run_id": run.run_id, "checkpoint": checkpoint, **decision}, indent=2))
    return 0


def cmd_restore(args: argparse.Namespace, config: dict[str, Any]) -> int:
    run = resolve_run(args, config)
    state = run.load_state()
    if state["status"] in {"restored", "restored_with_guard_drift"}:
        print(json.dumps(state["restoration"], indent=2))
        return 0 if state["status"] == "restored" else 1
    restoration = restore_managed(run, state, archive_runtime=True)
    try:
        if run.tool_root.exists():
            shutil.rmtree(run.tool_root)
        restoration["pinned_tools_removed"] = True
    except OSError as error:
        restoration["pinned_tools_removed"] = False
        restoration["failures"].append(f"failed to remove run-private pinned tools: {error}")
        restoration["managed_restored"] = False
    restoration["completed_at"] = utc_now()
    state["restoration"] = restoration
    if not restoration["managed_restored"]:
        state["status"] = "restore_failed"
    elif restoration["guard_files_unchanged"]:
        state["status"] = "restored"
    else:
        state["status"] = "restored_with_guard_drift"
    run.save_state(state)
    run.report_dir.mkdir(parents=True, exist_ok=True)
    write_json(run.report_dir / "restoration.json", {"run_id": run.run_id, **restoration}, 0o644)
    print(json.dumps(restoration, indent=2))
    return 0 if state["status"] == "restored" else 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, default=DEFAULT_CONFIG)
    parser.add_argument("--run-id")
    subparsers = parser.add_subparsers(dest="command", required=True)
    preflight = subparsers.add_parser("preflight", help="validate clients, models, paths, and snapshots")
    preflight.add_argument("--skip-model-check", action="store_true", help=argparse.SUPPRESS)
    preflight.add_argument(
        "--retrieval-profile",
        choices=RETRIEVAL_PROFILES,
        default="standard",
        help="lock the run to standard MCP retrieval or bounded prefetch",
    )
    preflight.add_argument(
        "--track",
        choices=TRACKS,
        default="deterministic",
        help="preregister deterministic capability or natural capture-and-handoff",
    )
    preflight.add_argument("--evidence-run")
    preflight.add_argument("--evidence-checkpoint", type=int)
    preflight.add_argument(
        "--corpus-cycle",
        type=int,
        choices=(1, 2),
        default=1,
        help="preregister the lexical assignment cycle; cycle 2 mirrors cycle 1",
    )
    smoke = subparsers.add_parser("smoke", help="run the four-turn mechanics and restoration gate")
    smoke.add_argument("--confirm-live", action="store_true")
    experiment = subparsers.add_parser(
        "experiment-fastpath",
        help="run a separate four-turn native-versus-recall_context smoke",
    )
    experiment.add_argument("--confirm-live", action="store_true")
    experiment.add_argument("--catalog", choices=("full", "minimal"), default="minimal")
    prefetch = subparsers.add_parser(
        "experiment-prefetch",
        help="run one-request native-versus-Noema prompt-prefetch experiments",
    )
    prefetch.add_argument("--confirm-live", action="store_true")
    prefetch.add_argument(
        "--phase",
        choices=("smoke", "challenge", "coverage"),
        default="smoke",
    )
    native_capture_target = subparsers.add_parser(
        "experiment-native-capture-target",
        help="compare absent versus pre-created empty native AGENTS.md capture targets",
    )
    native_capture_target.add_argument("--confirm-live", action="store_true")
    batch = subparsers.add_parser("run-batch", help="run one 24-retrieval-turn balanced batch")
    batch.add_argument("--batch", type=int)
    batch.add_argument("--track", choices=TRACKS)
    batch.add_argument("--continue-after-checkpoint", type=int)
    batch.add_argument("--confirm-live", action="store_true")
    analyze = subparsers.add_parser("analyze", help="generate one immutable two-batch checkpoint")
    analyze.add_argument("--checkpoint", type=int, required=True)
    subparsers.add_parser("restore", help="archive isolated runtime and restore all managed paths")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        config = load_config(args.config)
        return {
            "preflight": cmd_preflight,
            "smoke": cmd_smoke,
            "experiment-fastpath": cmd_fastpath_experiment,
            "experiment-prefetch": cmd_prefetch_experiment,
            "experiment-native-capture-target": cmd_native_capture_target_experiment,
            "run-batch": cmd_run_batch,
            "analyze": cmd_analyze,
            "restore": cmd_restore,
        }[args.command](args, config)
    except (BenchmarkError, OSError, subprocess.SubprocessError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
