#!/usr/bin/env python3
"""Run equivalent bounded federation workloads and sample server resources."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import sqlite3
import statistics
import subprocess
import tempfile
import time

from federation_ring import (
    Node,
    add_trace,
    database,
    environment,
    event_counts,
    free_port,
    initialize,
    start,
    stop,
)


def trace_count(database_path: Path, trace_ids: list[str]) -> int:
    placeholders = ",".join("?" for _ in trace_ids)
    with sqlite3.connect(database_path) as connection:
        row = connection.execute(
            f"SELECT count(*) FROM traces WHERE id IN ({placeholders})", trace_ids
        ).fetchone()
    assert row is not None
    return int(row[0])


def sample(nodes: list[Node]) -> tuple[int, float]:
    pids = [str(node.process.pid) for node in nodes if node.process is not None]
    result = subprocess.run(
        ["ps", "-o", "rss=", "-o", "%cpu=", "-p", ",".join(pids)],
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    rss_kib = 0
    cpu_percent = 0.0
    for line in result.stdout.splitlines():
        fields = line.split()
        if len(fields) == 2:
            rss_kib += int(fields[0])
            cpu_percent += float(fields[1])
    return rss_kib, cpu_percent


def run_cluster(
    implementation: str,
    binary: Path,
    rust: bool,
    duration: float,
    mutations: int,
    root: Path,
) -> dict[str, object]:
    env = environment(root)
    nodes = [
        Node("peer-a", binary, rust, free_port()),
        Node("peer-b", binary, rust, free_port()),
        Node("peer-c", binary, rust, free_port()),
    ]
    initialize(root, env, nodes)
    rss_samples: list[int] = []
    cpu_samples: list[float] = []
    trace_ids: list[str] = []
    restarted = False
    started_at = time.monotonic()

    try:
        for node in nodes:
            start(node, env)
        rss, cpu = sample(nodes)
        rss_samples.append(rss)
        cpu_samples.append(cpu)

        for index in range(mutations):
            scheduled = started_at + duration * index / max(mutations, 1)
            while time.monotonic() < scheduled:
                time.sleep(min(0.05, scheduled - time.monotonic()))
            if not restarted and index >= mutations // 2:
                if not stop(nodes[2]):
                    raise RuntimeError("restart target did not stop gracefully")
                time.sleep(0.5)
                start(nodes[2], env)
                restarted = True
            node = nodes[index % len(nodes)]
            trace_ids.append(
                add_trace(
                    node,
                    env,
                    f"soak-{index}",
                    f"bounded federation payload {index}",
                )
            )
            rss, cpu = sample(nodes)
            rss_samples.append(rss)
            cpu_samples.append(cpu)

        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            rss, cpu = sample(nodes)
            rss_samples.append(rss)
            cpu_samples.append(cpu)
            if all(
                trace_count(database(root, node), trace_ids) == len(trace_ids)
                for node in nodes
            ):
                break
            time.sleep(0.1)
        else:
            raise RuntimeError(f"{implementation} cluster did not converge")

        counts = [event_counts(database(root, node)) for node in nodes]
        if any(total != distinct for total, distinct in counts):
            raise RuntimeError(f"{implementation} cluster stored duplicate event IDs")
        if len({total for total, _ in counts}) != 1:
            raise RuntimeError(f"{implementation} cluster event counts differ: {counts}")
    finally:
        graceful = [stop(node) for node in nodes]

    return {
        "implementation": implementation,
        "mutations": len(trace_ids),
        "wall_seconds": round(time.monotonic() - started_at, 3),
        "samples": len(rss_samples),
        "peak_cluster_rss_mib": round(max(rss_samples) / 1024, 3),
        "median_cluster_rss_mib": round(statistics.median(rss_samples) / 1024, 3),
        "mean_cluster_cpu_percent": round(statistics.mean(cpu_samples), 3),
        "peak_cluster_cpu_percent": round(max(cpu_samples), 3),
        "event_rows_per_node": counts[0][0],
        "restart_recovered": restarted,
        "graceful_shutdown": all(graceful),
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    parser.add_argument("--duration", type=float, default=8.0)
    parser.add_argument("--mutations", type=int, default=80)
    args = parser.parse_args()
    if args.duration <= 0 or args.mutations <= 0:
        parser.error("duration and mutations must be positive")
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")

    results = []
    with tempfile.TemporaryDirectory(prefix="noema-go-soak-") as directory:
        results.append(
            run_cluster(
                "go",
                args.go.resolve(),
                False,
                args.duration,
                args.mutations,
                Path(directory),
            )
        )
    with tempfile.TemporaryDirectory(prefix="noema-rust-soak-") as directory:
        results.append(
            run_cluster(
                "rust",
                args.rust.resolve(),
                True,
                args.duration,
                args.mutations,
                Path(directory),
            )
        )

    go_result, rust_result = results
    report = {
        "schema_version": 1,
        "duration_seconds_per_cluster": args.duration,
        "target_mutations_per_cluster": args.mutations,
        "results": results,
        "rust_to_go_peak_rss_ratio": round(
            rust_result["peak_cluster_rss_mib"] / go_result["peak_cluster_rss_mib"],
            3,
        ),
        "rust_to_go_mean_cpu_ratio": round(
            rust_result["mean_cluster_cpu_percent"]
            / max(go_result["mean_cluster_cpu_percent"], 0.001),
            3,
        ),
    }
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
