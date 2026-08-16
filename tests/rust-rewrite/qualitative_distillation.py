#!/usr/bin/env python3
"""Run a repeated, blinded Go/Rust real-model distillation evaluation."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sqlite3
import statistics
import subprocess
import tempfile
import time
from dataclasses import asdict, dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from federation_ring import Node, cortex_dir, database, environment
from real_model_distillation import CountingProxy


@dataclass(frozen=True)
class Source:
    title: str
    body: str


@dataclass(frozen=True)
class Case:
    name: str
    trace_type: str
    days_ago: int
    cohesive: bool
    sources: tuple[Source, ...]
    required: tuple[str, ...] = ()
    forbidden: tuple[str, ...] = ()
    rubric: str = ""


CASES = (
    Case(
        name="orchid-rollback",
        trace_type="decision",
        days_ago=0,
        cohesive=True,
        sources=(
            Source(
                "Orchid release window",
                "Orchid release 4.2 will deploy Tuesday at 22:00 UTC.",
            ),
            Source(
                "Orchid rollback threshold",
                "Rollback if the error rate exceeds 0.5 percent for 10 consecutive minutes.",
            ),
            Source(
                "Orchid rollback command",
                "The approved rollback command is `orchidctl rollback --to 4.1`.",
            ),
            Source(
                "Orchid verification",
                "Before closing the change, verify artifact checksum sha256:7ac9 and retain the audit log.",
            ),
        ),
        required=(
            "orchid",
            "4.2",
            "22:00",
            "0.5",
            "10",
            "orchidctl rollback --to 4.1",
            "sha256:7ac9",
            "audit",
        ),
        forbidden=("kubernetes", "docker"),
        rubric="Preserve the release, rollback threshold and command, checksum, and audit requirement without adding deployment technology.",
    ),
    Case(
        name="unrelated-facts",
        trace_type="fact",
        days_ago=1,
        cohesive=False,
        sources=(
            Source(
                "Bakery oven", "The bakery proofing oven is held at 31 degrees Celsius."
            ),
            Source(
                "Telescope mirror",
                "The observatory telescope has a 2.4 meter primary mirror.",
            ),
            Source(
                "Language class",
                "The evening language class meets in room C17 on Wednesdays.",
            ),
            Source(
                "Bicycle pressure", "The cargo bicycle rear tire is inflated to 58 PSI."
            ),
        ),
        rubric="Reject the bucket; its only relationship is trace type and creation day.",
    ),
    Case(
        name="harbor-root-cause",
        trace_type="observation",
        days_ago=2,
        cohesive=True,
        sources=(
            Source(
                "Harbor initial hypothesis",
                "The first hypothesis blamed database CPU saturation for Harbor latency.",
            ),
            Source(
                "Harbor metrics",
                "Database CPU remained at 34 percent while the connection pool was 100 percent utilized.",
            ),
            Source(
                "Harbor mitigation",
                "Increasing the connection pool from 80 to 120 reduced p95 latency from 840 ms to 210 ms.",
            ),
            Source(
                "Harbor conclusion",
                "Connection-pool exhaustion was the root cause; the CPU-saturation hypothesis was rejected.",
            ),
        ),
        required=(
            "harbor",
            "34",
            "100",
            "80",
            "120",
            "840",
            "210",
            "connection",
            "reject",
        ),
        forbidden=(
            "cpu saturation was the root cause",
            "database saturation was the root cause",
        ),
        rubric="Retain the rejected initial hypothesis, measured evidence, mitigation, and corrected root cause.",
    ),
    Case(
        name="notification-preferences",
        trace_type="preference",
        days_ago=3,
        cohesive=True,
        sources=(
            Source(
                "Quiet hours", "Do not play notification sounds after 21:30 local time."
            ),
            Source(
                "Urgent alerts",
                "During quiet hours, urgent alerts may use the pager channel only.",
            ),
            Source("Weekend digest", "Send the weekend digest at 10:00 on Saturday."),
            Source(
                "Summary channel",
                "Prefer an email summary and never send SMS summaries.",
            ),
        ),
        required=("21:30", "pager", "10:00", "saturday", "email", "sms"),
        forbidden=("sunday", "push notification"),
        rubric="Preserve all channel, timing, and negative-preference constraints.",
    ),
    Case(
        name="superficial-context",
        trace_type="context",
        days_ago=4,
        cohesive=False,
        sources=(
            Source(
                "Concert seating",
                "The concert tickets are for balcony seats B12 and B13.",
            ),
            Source(
                "Backup retention",
                "The reporting database keeps encrypted backups for 35 days.",
            ),
            Source(
                "Hiking equipment",
                "The replacement hiking boots are size 10 with a wide fit.",
            ),
        ),
        rubric="Reject the bucket instead of inventing a broad personal-planning theme.",
    ),
    Case(
        name="account-migration",
        trace_type="note",
        days_ago=5,
        cohesive=True,
        sources=(
            Source(
                "Migration plan",
                "Copy the accounts table into accounts_shadow before validation.",
            ),
            Source(
                "Migration row count",
                "The validation count was 18,442 rows in both source and shadow tables.",
            ),
            Source(
                "Migration mismatch",
                "Three checksum mismatches came from phone-number normalization.",
            ),
            Source(
                "Migration correction",
                "After correcting the phone transform, the checksum mismatch count was zero.",
            ),
            Source(
                "Migration approval",
                "Cutover was approved for 01:15 UTC after the clean validation.",
            ),
        ),
        required=(
            "accounts_shadow",
            "18,442",
            "three",
            "phone",
            "zero",
            "01:15",
            "approved",
        ),
        forbidden=("data loss", "rollback required"),
        rubric="Preserve the chronology from mismatch through correction and final cutover approval.",
    ),
    Case(
        name="mercury-polysemy",
        trace_type="skill",
        days_ago=6,
        cohesive=False,
        sources=(
            Source(
                "Mercury orbit",
                "Remember that planet Mercury completes an orbit in about 88 days.",
            ),
            Source(
                "Mercury sedan",
                "Set the Mercury sedan tire pressure to 33 PSI when cold.",
            ),
            Source(
                "Mercury element",
                "Elemental mercury freezes at approximately -38.83 degrees Celsius.",
            ),
        ),
        rubric="Reject lexical overlap: the planet, vehicle, and chemical element are unrelated.",
    ),
    Case(
        name="atlas-ambiguity",
        trace_type="intent",
        days_ago=7,
        cohesive=True,
        sources=(
            Source(
                "Atlas staging report",
                "One Atlas staging report lists the cache limit as 256 MiB.",
            ),
            Source(
                "Atlas dashboard",
                "The Atlas dashboard displayed a 512 MiB cache limit during the same hour.",
            ),
            Source(
                "Atlas follow-up",
                "No deployment record explains the discrepancy; the authoritative value remains unresolved.",
            ),
        ),
        required=("atlas", "256", "512", "unresolved"),
        forbidden=(
            "authoritative value is 256",
            "authoritative value is 512",
            "resolved at",
        ),
        rubric="Consolidate the shared investigation while preserving both values and the unresolved state.",
    ),
)

NUMBER_PATTERN = re.compile(r"(?<![a-z0-9])[+-]?\d+(?:[.,:]\d+)*(?![a-z0-9-])")
SOURCE_ID_PATTERN = re.compile(r"\b20\d{6}-[a-z0-9-]+")
SOURCE_ID_FRAGMENT_PATTERN = re.compile(r"\b20\d{6}\b")


def initialize(node: Node, env: dict[str, str], parent: Path) -> None:
    subprocess.run(
        [str(node.binary), "init", "--name", node.name, "--path", str(parent)],
        env=env,
        check=True,
        text=True,
        capture_output=True,
    )


def configure(manifest: Path) -> None:
    lines = manifest.read_text().splitlines()
    boundaries = [index for index, line in enumerate(lines) if line == "---"]
    lines[boundaries[1] : boundaries[1]] = [
        "consolidation:",
        "  enabled: true",
        "  llm_enabled: true",
        "  model_tier: small",
        "  local_llm_endpoint: http://127.0.0.1:1/v1",
        "  model_name: overridden",
    ]
    manifest.write_text("\n".join(lines) + "\n")


def seed(
    node: Node, env: dict[str, str], root: Path
) -> tuple[dict[str, str], dict[str, str]]:
    now = datetime.now(timezone.utc)
    trace_cases: dict[str, str] = {}
    source_text: dict[str, str] = {}
    updates: list[tuple[str, str]] = []
    for case in CASES:
        created = (now - timedelta(days=case.days_ago)).strftime("%Y-%m-%dT12:00:00Z")
        for source in case.sources:
            completed = subprocess.run(
                [
                    str(node.binary),
                    "--cortex",
                    node.name,
                    "add",
                    "--title",
                    source.title,
                    "--type",
                    case.trace_type,
                    "--tag",
                    "synthetic-quality-eval",
                    "--tag",
                    case.name,
                    "--body",
                    source.body,
                ],
                env=env,
                check=True,
                text=True,
                capture_output=True,
            )
            trace_id = completed.stdout.strip().rsplit(": ", 1)[-1]
            trace_cases[trace_id] = case.name
            source_text[trace_id] = f"{source.title}\n{source.body}"
            updates.append((created, trace_id))
    with sqlite3.connect(database(root, node)) as connection:
        connection.executemany("UPDATE traces SET created_at=? WHERE id=?", updates)
    return trace_cases, source_text


def run_measured(
    node: Node,
    env: dict[str, str],
    root: Path,
    endpoint: str,
    model: str,
    profile: str,
) -> tuple[dict[str, Any], dict[str, float | None]]:
    output = root / f"{node.name}-quality.json"
    started = time.monotonic()
    process = subprocess.Popen(
        [
            str(node.binary),
            "--cortex",
            node.name,
            "consolidate",
            "--endpoint",
            endpoint,
            "--model",
            model,
            "--model-tier",
            profile,
            "--window",
            "336",
            "--retries",
            "0",
            "--dry-run",
            "--emit-json",
            str(output),
        ],
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    peak_rss_kib = 0
    sampling_available = True
    try:
        while process.poll() is None:
            if sampling_available:
                try:
                    sampled = subprocess.run(
                        ["ps", "-o", "rss=", "-p", str(process.pid)],
                        check=False,
                        text=True,
                        stdout=subprocess.PIPE,
                        stderr=subprocess.DEVNULL,
                    ).stdout.strip()
                    if sampled.isdigit():
                        peak_rss_kib = max(peak_rss_kib, int(sampled))
                except PermissionError:
                    sampling_available = False
            time.sleep(0.05)
    except BaseException:
        process.terminate()
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
        raise
    _stdout, stderr = process.communicate()
    metrics = {
        "real_seconds": time.monotonic() - started,
        "peak_rss_mib": peak_rss_kib / 1024 if sampling_available else None,
    }
    if process.returncode != 0:
        diagnostic = stderr[-4_000:].strip()
        raise RuntimeError(
            f"{node.name} consolidate exited {process.returncode}: {diagnostic}"
        )
    return json.loads(output.read_text()), metrics


def cluster_case(cluster: dict[str, Any], trace_cases: dict[str, str]) -> str:
    names = {trace_cases[trace_id] for trace_id in cluster["ids"]}
    if len(names) != 1:
        raise RuntimeError(f"cluster crossed qualitative cases: {sorted(names)}")
    return names.pop()


def score_text(
    case: Case,
    outcome: str,
    title: str,
    tags: list[str],
    body: str,
    sources: str,
) -> dict[str, Any]:
    distilled = outcome == "distilled"
    rendered = f"{title}\n{body}".lower()
    required_hits = [term for term in case.required if term.lower() in rendered]
    forbidden_hits = [term for term in case.forbidden if term.lower() in rendered]
    sources = sources.lower()
    source_numbers = set(NUMBER_PATTERN.findall(sources))
    output_numbers = set(NUMBER_PATTERN.findall(rendered))
    source_reference_numbers = set(SOURCE_ID_FRAGMENT_PATTERN.findall(rendered))
    source_words = len(re.findall(r"\b\w+\b", sources))
    output_words = len(re.findall(r"\b\w+\b", rendered))
    return {
        "expected_outcome": "distilled" if case.cohesive else "rejected",
        "outcome": outcome,
        "decision_correct": distilled == case.cohesive,
        "required_hits": required_hits,
        "required_total": len(case.required),
        "forbidden_hits": forbidden_hits,
        "novel_numbers": sorted(
            output_numbers - source_numbers - source_reference_numbers
        ),
        "source_id_mentions": sorted(set(SOURCE_ID_PATTERN.findall(rendered))),
        "source_reference_leak": bool(
            SOURCE_ID_PATTERN.search(rendered)
            or SOURCE_ID_FRAGMENT_PATTERN.search(rendered)
        ),
        "evaluation_marker_mention": "synthetic quality eval" in rendered,
        "schema_degraded": distilled
        and (
            not tags
            or "tags:" in title.lower()
            or (
                len(tags) == 1
                and "synthetic-quality-eval" in tags[0]
                and tags[0] != "synthetic-quality-eval"
            )
        ),
        "compression_ratio": round(output_words / source_words, 3)
        if source_words
        else 0,
        "title": title,
        "tags": tags,
        "body": body,
    }


def score_cluster(
    case: Case,
    cluster: dict[str, Any],
    source_text: dict[str, str],
) -> dict[str, Any]:
    sources = "\n".join(source_text[trace_id] for trace_id in cluster["ids"])
    return score_text(
        case,
        cluster["outcome"],
        cluster.get("title", ""),
        cluster.get("tags", []),
        cluster.get("body", ""),
        sources,
    )


def normalize(
    payload: dict[str, Any],
    trace_cases: dict[str, str],
    source_text: dict[str, str],
) -> dict[str, dict[str, Any]]:
    cases_by_name = {case.name: case for case in CASES}
    result: dict[str, dict[str, Any]] = {}
    for cluster in payload["summary"]["cluster_results"]:
        name = cluster_case(cluster, trace_cases)
        if name in result:
            raise RuntimeError(
                f"qualitative case {name} was split into multiple chunks"
            )
        result[name] = score_cluster(cases_by_name[name], cluster, source_text)
    missing = set(cases_by_name) - set(result)
    if missing:
        raise RuntimeError(f"qualitative cases missing from output: {sorted(missing)}")
    return result


def blinded_label(seed: str, pair_id: str) -> bool:
    digest = hashlib.sha256(f"{seed}:{pair_id}".encode()).digest()
    return bool(digest[0] & 1)


def blind_outputs(
    runs: list[dict[str, Any]], seed: str
) -> tuple[dict[str, Any], dict[str, str]]:
    pairs: list[dict[str, Any]] = []
    key: dict[str, str] = {}
    for run in runs:
        for case in CASES:
            pair_id = f"run-{run['run']:02d}:{case.name}"
            go_is_a = blinded_label(seed, pair_id)
            key[pair_id] = "go" if go_is_a else "rust"
            candidates = []
            for label, implementation in (
                ("A", "go" if go_is_a else "rust"),
                ("B", "rust" if go_is_a else "go"),
            ):
                value = run[implementation]["cases"][case.name]
                candidates.append(
                    {
                        "label": label,
                        "outcome": value["outcome"],
                        "title": value["title"],
                        "tags": value["tags"],
                        "body": value["body"],
                    }
                )
            pairs.append(
                {
                    "pair_id": pair_id,
                    "case": case.name,
                    "expected_outcome": "distilled" if case.cohesive else "rejected",
                    "rubric": case.rubric,
                    "sources": [asdict(source) for source in case.sources],
                    "candidates": candidates,
                }
            )
    return {
        "rubric": {"fidelity": 4, "clarity": 3, "calibration": 2, "concision": 1},
        "pairs": pairs,
    }, key


def summarize(runs: list[dict[str, Any]]) -> dict[str, Any]:
    summary: dict[str, Any] = {
        "runs": len(runs),
        "implementations": {},
        "pair_disagreements": 0,
    }
    for implementation in ("go", "rust"):
        values = [run[implementation] for run in runs]
        saved_cases = [
            (case, value["cases"][case.name]) for value in values for case in CASES
        ]
        rescored_cases = []
        for case, saved in saved_cases:
            sources = "\n".join(
                f"{source.title}\n{source.body}" for source in case.sources
            )
            rescored_cases.append(
                score_text(
                    case,
                    saved["outcome"],
                    saved.get("title", ""),
                    saved.get("tags", []),
                    saved.get("body", ""),
                    sources,
                )
            )
        required_total = sum(case["required_total"] for case in rescored_cases)
        rss_values = [
            value["metrics"]["peak_rss_mib"]
            for value in values
            if value["metrics"].get("peak_rss_mib") is not None
        ]
        summary["implementations"][implementation] = {
            "decision_accuracy": f"{sum(case['decision_correct'] for case in rescored_cases)}/{len(rescored_cases)}",
            "required_retention": f"{sum(len(case['required_hits']) for case in rescored_cases)}/{required_total}",
            "forbidden_claims": sum(
                len(case["forbidden_hits"]) for case in rescored_cases
            ),
            "novel_numbers": sum(len(case["novel_numbers"]) for case in rescored_cases),
            "source_id_mentions": sum(
                len(case["source_id_mentions"]) for case in rescored_cases
            ),
            "source_reference_leaks": sum(
                case["source_reference_leak"] for case in rescored_cases
            ),
            "evaluation_marker_mentions": sum(
                case["evaluation_marker_mention"] for case in rescored_cases
            ),
            "schema_degradations": sum(
                case["schema_degraded"] for case in rescored_cases
            ),
            "median_wall_seconds": round(
                statistics.median(
                    value["metrics"].get("real_seconds", 0) for value in values
                ),
                3,
            ),
            "median_peak_rss_mib": round(statistics.median(rss_values), 3)
            if rss_values
            else None,
            "request_count": sum(value["requests"]["count"] for value in values),
            "prompt_tokens": sum(
                value["requests"]["prompt_tokens"] for value in values
            ),
            "request_bytes": sum(
                value["requests"]["request_bytes"] for value in values
            ),
        }
    for run in runs:
        for case in CASES:
            if (
                run["go"]["cases"][case.name]["outcome"]
                != run["rust"]["cases"][case.name]["outcome"]
            ):
                summary["pair_disagreements"] += 1
    return summary


def exercise(args: argparse.Namespace, root: Path) -> dict[str, Any]:
    runs: list[dict[str, Any]] = []
    with CountingProxy(args.endpoint) as proxy:
        for run_number in range(1, args.runs + 1):
            run_root = root / f"run-{run_number:02d}"
            run_root.mkdir()
            env = environment(run_root)
            parent = run_root / "cortexes"
            nodes = {
                "go": Node("go-quality", args.go.resolve(), False, 0),
                "rust": Node("rust-quality", args.rust.resolve(), True, 0),
            }
            mappings: dict[str, tuple[dict[str, str], dict[str, str]]] = {}
            for implementation, node in nodes.items():
                initialize(node, env, parent)
                configure(cortex_dir(run_root, node) / "cortex.md")
                mappings[implementation] = seed(node, env, run_root)
            order = ("go", "rust") if run_number % 2 else ("rust", "go")
            run: dict[str, Any] = {"run": run_number, "order": "-".join(order)}
            for implementation in order:
                node = nodes[implementation]
                request_start = len(proxy.records)
                payload, metrics = run_measured(
                    node, env, run_root, proxy.endpoint, args.model, args.profile
                )
                requests = proxy.records[request_start:]
                trace_cases, source_text = mappings[implementation]
                run[implementation] = {
                    "metrics": metrics,
                    "requests": {
                        "count": len(requests),
                        "statuses": [item["status"] for item in requests],
                        "model_seconds": sum(
                            float(item["seconds"]) for item in requests
                        ),
                        "request_bytes": sum(
                            int(item["request_bytes"]) for item in requests
                        ),
                        "response_bytes": sum(
                            int(item["response_bytes"]) for item in requests
                        ),
                        "prompt_tokens": sum(
                            int(item["prompt_tokens"]) for item in requests
                        ),
                        "completion_tokens": sum(
                            int(item["completion_tokens"]) for item in requests
                        ),
                    },
                    "cases": normalize(payload, trace_cases, source_text),
                }
            runs.append(run)
            print(
                f"completed qualitative run {run_number}/{args.runs} ({run['order']})",
                flush=True,
            )
    blind, key = blind_outputs(runs, args.blind_seed)
    return {"summary": summarize(runs), "runs": runs, "blind": blind, "key": key}


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    parser.add_argument("--endpoint", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument(
        "--profile", default="small", choices=["small", "large", "frontier"]
    )
    parser.add_argument("--runs", type=int, default=4)
    parser.add_argument("--blind-seed", default="noema-quality-v1")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--blind-output", type=Path, required=True)
    parser.add_argument("--key-output", type=Path, required=True)
    args = parser.parse_args()
    if args.runs < 1:
        parser.error("--runs must be positive")
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")
    with tempfile.TemporaryDirectory(prefix="noema-rust-qualitative-") as directory:
        result = exercise(args, Path(directory))
    args.output.write_text(
        json.dumps({"summary": result["summary"], "runs": result["runs"]}, indent=2)
        + "\n"
    )
    args.blind_output.write_text(json.dumps(result["blind"], indent=2) + "\n")
    args.key_output.write_text(json.dumps(result["key"], indent=2) + "\n")
    print(json.dumps(result["summary"], indent=2))


if __name__ == "__main__":
    main()
