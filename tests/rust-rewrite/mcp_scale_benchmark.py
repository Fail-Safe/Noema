#!/usr/bin/env python3
"""Large-corpus and concurrent Go/Rust MCP comparison.

The quick MCP benchmark intentionally creates a fresh corpus for every run.
At 10k-100k traces that setup cost dominates, so this harness creates one
verified corpus per implementation and restarts servers in alternating order
for the measured runs.
"""

from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import nullcontext
import json
import os
from pathlib import Path
import shutil
import sqlite3
import statistics
import subprocess
import tempfile
import time
from typing import Callable

from mcp_benchmark import percentile, process_metrics, request


class MCPClient:
    def __init__(self, binary: Path, home: Path) -> None:
        environment = os.environ.copy()
        environment["HOME"] = str(home)
        self.process = subprocess.Popen(
            [str(binary), "--cortex", "bench", "serve", "--transport", "stdio"],
            env=environment,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            bufsize=1,
        )
        self.sequence = 1
        request(
            self.process,
            {
                "jsonrpc": "2.0",
                "id": self.sequence,
                "method": "initialize",
                "params": {
                    "protocolVersion": "2025-03-26",
                    "capabilities": {},
                    "clientInfo": {"name": "scale-benchmark", "version": "1"},
                },
            },
        )
        self.sequence += 1
        assert self.process.stdin is not None
        self.process.stdin.write(
            '{"jsonrpc":"2.0","method":"notifications/initialized"}\n'
        )
        self.process.stdin.flush()

    def call(self, name: str, arguments: dict[str, object]) -> None:
        request(
            self.process,
            {
                "jsonrpc": "2.0",
                "id": self.sequence,
                "method": "tools/call",
                "params": {"name": name, "arguments": arguments},
            },
        )
        self.sequence += 1

    def search(self, query: str) -> None:
        self.call("search_traces", {"query": query, "mode": "lexical"})

    def create(self, title: str, body: str) -> None:
        self.call(
            "create_trace",
            {
                "title": title,
                "type": "fact",
                "author": "benchmark",
                "tags": "performance",
                "body": body,
            },
        )

    def metrics(self) -> tuple[int, float]:
        return process_metrics(self.process.pid)

    def close(self) -> None:
        if self.process.stdin is not None and not self.process.stdin.closed:
            self.process.stdin.close()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
        finally:
            if self.process.stdout is not None:
                self.process.stdout.close()


def invoke(binary: Path, home: Path, *arguments: str) -> None:
    environment = os.environ.copy()
    environment["HOME"] = str(home)
    subprocess.run(
        [str(binary), *arguments],
        env=environment,
        check=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )


def initialize(binary: Path, home: Path, cortexes: Path) -> None:
    home.mkdir(parents=True)
    cortexes.mkdir(parents=True)
    invoke(binary, home, "init", "--name", "bench", "--path", str(cortexes))


def trace_count(cortexes: Path) -> int:
    databases = list(cortexes.rglob("noema.db"))
    if len(databases) != 1:
        raise RuntimeError(
            f"expected one benchmark database below {cortexes}, found {len(databases)}"
        )
    database = databases[0]
    with sqlite3.connect(database) as connection:
        connection.execute("PRAGMA query_only = ON")
        return int(connection.execute("SELECT COUNT(*) FROM traces").fetchone()[0])


def seed(binary: Path, home: Path, cortexes: Path, traces: int) -> float:
    client = MCPClient(binary, home)
    started = time.perf_counter()
    try:
        for index in range(1, traces + 1):
            client.create(
                f"Scale benchmark trace {index}",
                f"scale alpha beta gamma item {index}",
            )
            if index % 10_000 == 0:
                print(f"seeded {index}/{traces}", flush=True)
    finally:
        client.close()
    elapsed = time.perf_counter() - started
    invoke(binary, home, "--cortex", "bench", "verify")
    count = trace_count(cortexes)
    if count != traces:
        raise RuntimeError(f"seed count {count} does not match expected {traces}")
    return elapsed


def summarize_latencies(
    implementation: str,
    run: int,
    scenario: str,
    latencies: list[float],
    elapsed: float,
    rss_kb: int,
    cpu_seconds: float,
) -> dict[str, object]:
    return {
        "implementation": implementation,
        "run": run,
        "scenario": scenario,
        "operations": len(latencies),
        "operations_per_second": round(len(latencies) / elapsed, 2),
        "sampled_rss_kb": rss_kb,
        "cpu_seconds": round(cpu_seconds, 3),
        "cpu_utilization_percent": round(cpu_seconds / elapsed * 100, 1),
        "latency_ms": {
            "median": round(statistics.median(latencies), 3),
            "p95": round(percentile(latencies, 0.95), 3),
            "max": round(max(latencies), 3),
        },
    }


def serial_search(
    implementation: str,
    run: int,
    client: MCPClient,
    scenario: str,
    query: str,
    requests: int,
    warmups: int,
) -> dict[str, object]:
    for _ in range(warmups):
        client.search(query)
    rss_before, cpu_before = client.metrics()
    latencies = []
    started = time.perf_counter()
    for _ in range(requests):
        before = time.perf_counter_ns()
        client.search(query)
        latencies.append((time.perf_counter_ns() - before) / 1_000_000)
    elapsed = time.perf_counter() - started
    rss_after, cpu_after = client.metrics()
    return summarize_latencies(
        implementation,
        run,
        scenario,
        latencies,
        elapsed,
        max(rss_before, rss_after),
        cpu_after - cpu_before,
    )


def parallel_search(
    implementation: str,
    run: int,
    clients: list[MCPClient],
    scenario: str,
    query: str,
    requests_per_client: int,
) -> dict[str, object]:
    for client in clients:
        client.search(query)
    before = [client.metrics() for client in clients]

    def workload(client: MCPClient) -> list[float]:
        latencies = []
        for _ in range(requests_per_client):
            started = time.perf_counter_ns()
            client.search(query)
            latencies.append((time.perf_counter_ns() - started) / 1_000_000)
        return latencies

    started = time.perf_counter()
    with ThreadPoolExecutor(max_workers=len(clients)) as executor:
        groups = list(executor.map(workload, clients))
    elapsed = time.perf_counter() - started
    after = [client.metrics() for client in clients]
    latencies = [sample for group in groups for sample in group]
    rss_kb = max(sum(value[0] for value in before), sum(value[0] for value in after))
    cpu_seconds = sum(end[1] - start[1] for start, end in zip(before, after))
    return summarize_latencies(
        implementation,
        run,
        scenario,
        latencies,
        elapsed,
        rss_kb,
        cpu_seconds,
    )


def mixed_workload(
    implementation: str,
    run: int,
    clients: list[MCPClient],
    query: str,
    reads_per_client: int,
    writes: int,
    campaign: str,
) -> dict[str, object]:
    readers = clients[:-1]
    writer = clients[-1]
    before = [client.metrics() for client in clients]

    def read_workload(client: MCPClient) -> list[float]:
        latencies = []
        for _ in range(reads_per_client):
            started = time.perf_counter_ns()
            client.search(query)
            latencies.append((time.perf_counter_ns() - started) / 1_000_000)
        return latencies

    def write_workload() -> list[float]:
        latencies = []
        for index in range(1, writes + 1):
            started = time.perf_counter_ns()
            writer.create(
                f"Mixed benchmark {campaign} run {run} trace {index}",
                f"mixed workload delta campaign {campaign} run {run} item {index}",
            )
            latencies.append((time.perf_counter_ns() - started) / 1_000_000)
        return latencies

    started = time.perf_counter()
    with ThreadPoolExecutor(max_workers=len(clients)) as executor:
        futures = [executor.submit(read_workload, client) for client in readers]
        write_future = executor.submit(write_workload)
        read_latencies = [sample for future in futures for sample in future.result()]
        write_latencies = write_future.result()
    elapsed = time.perf_counter() - started
    after = [client.metrics() for client in clients]
    rss_kb = max(sum(value[0] for value in before), sum(value[0] for value in after))
    cpu_seconds = sum(end[1] - start[1] for start, end in zip(before, after))
    read_median = statistics.median(read_latencies) if read_latencies else 0.0
    read_p95 = percentile(read_latencies, 0.95) if read_latencies else 0.0
    write_median = statistics.median(write_latencies) if write_latencies else 0.0
    write_p95 = percentile(write_latencies, 0.95) if write_latencies else 0.0
    return {
        "implementation": implementation,
        "run": run,
        "scenario": "mixed_selective_read_write",
        "operations": len(read_latencies) + len(write_latencies),
        "operations_per_second": round(
            (len(read_latencies) + len(write_latencies)) / elapsed, 2
        ),
        "sampled_rss_kb": rss_kb,
        "cpu_seconds": round(cpu_seconds, 3),
        "cpu_utilization_percent": round(cpu_seconds / elapsed * 100, 1),
        "reads": {
            "count": len(read_latencies),
            "median_ms": round(read_median, 3),
            "p95_ms": round(read_p95, 3),
        },
        "writes": {
            "count": len(write_latencies),
            "median_ms": round(write_median, 3),
            "p95_ms": round(write_p95, 3),
        },
    }


def with_clients(
    binary: Path,
    home: Path,
    count: int,
    operation: Callable[[list[MCPClient]], dict[str, object]],
) -> dict[str, object]:
    clients: list[MCPClient] = []
    try:
        for _ in range(count):
            clients.append(MCPClient(binary, home))
        return operation(clients)
    finally:
        for client in reversed(clients):
            client.close()


def aggregate(results: list[dict[str, object]]) -> dict[str, object]:
    summary: dict[str, object] = {}
    scenarios = sorted({str(result["scenario"]) for result in results})
    for scenario in scenarios:
        summary[scenario] = {}
        for implementation in ("go", "rust"):
            selected = [
                result
                for result in results
                if result["scenario"] == scenario
                and result["implementation"] == implementation
            ]
            summary[scenario][implementation] = {
                "operations_per_second_median": round(
                    statistics.median(
                        float(result["operations_per_second"])
                        for result in selected
                    ),
                    2,
                ),
                "sampled_rss_kb_median": round(
                    statistics.median(
                        int(result["sampled_rss_kb"]) for result in selected
                    )
                ),
            }
            if "latency_ms" in selected[0]:
                summary[scenario][implementation]["latency_ms_median"] = round(
                    statistics.median(
                        float(result["latency_ms"]["median"])
                        for result in selected
                    ),
                    3,
                )
    return summary


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    parser.add_argument("--traces", type=int, required=True)
    parser.add_argument("--runs", type=int, default=5)
    parser.add_argument("--serial-broad", type=int, default=10)
    parser.add_argument("--serial-selective", type=int, default=100)
    parser.add_argument("--clients", type=int, default=4)
    parser.add_argument("--parallel-broad", type=int, default=3)
    parser.add_argument("--parallel-selective", type=int, default=50)
    parser.add_argument("--mixed-reads", type=int, default=50)
    parser.add_argument("--mixed-writes", type=int, default=100)
    corpus = parser.add_mutually_exclusive_group()
    corpus.add_argument(
        "--reuse-root",
        type=Path,
        help="reuse a previously seeded root containing go/ and rust/ trees",
    )
    corpus.add_argument(
        "--clone-cortex",
        type=Path,
        help="clone one verified cortex into independent Go and Rust trees",
    )
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    if args.clients < 2:
        parser.error("--clients must be at least 2")
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")

    binaries = {
        "go": args.go.resolve(),
        "rust": args.rust.resolve(),
    }
    campaign = str(time.time_ns())
    root_context = (
        nullcontext(str(args.reuse_root.resolve()))
        if args.reuse_root
        else tempfile.TemporaryDirectory(prefix="noema-mcp-scale-")
    )
    with root_context as directory:
        root = Path(directory)
        homes: dict[str, Path] = {}
        cortexes: dict[str, Path] = {}
        seed_seconds: dict[str, float] = {}
        initial_counts: dict[str, int] = {}
        for implementation in ("go", "rust"):
            home = root / implementation / "home"
            cortex_parent = root / implementation / "cortexes"
            if args.reuse_root:
                if not home.is_dir() or not cortex_parent.is_dir():
                    raise RuntimeError(
                        f"reused root is missing the {implementation} benchmark tree"
                    )
                invoke(binaries[implementation], home, "--cortex", "bench", "verify")
                initial_counts[implementation] = trace_count(cortex_parent)
                if initial_counts[implementation] < args.traces:
                    raise RuntimeError(
                        f"{implementation} reused count {initial_counts[implementation]} "
                        f"is below requested scale {args.traces}"
                    )
            elif args.clone_cortex:
                initialize(binaries[implementation], home, cortex_parent)
                shutil.copytree(
                    args.clone_cortex.resolve(),
                    cortex_parent / "bench",
                    dirs_exist_ok=True,
                )
                invoke(binaries[implementation], home, "--cortex", "bench", "verify")
                initial_counts[implementation] = trace_count(cortex_parent)
                if initial_counts[implementation] < args.traces:
                    raise RuntimeError(
                        f"{implementation} cloned count {initial_counts[implementation]} "
                        f"is below requested scale {args.traces}"
                    )
            else:
                initialize(binaries[implementation], home, cortex_parent)
                print(f"seeding {implementation} with {args.traces} traces", flush=True)
                seed_seconds[implementation] = round(
                    seed(binaries[implementation], home, cortex_parent, args.traces), 3
                )
                initial_counts[implementation] = args.traces
            homes[implementation] = home
            cortexes[implementation] = cortex_parent

        results: list[dict[str, object]] = []
        selective_query = str(max(1, args.traces // 2))
        expected_counts = initial_counts.copy()
        for run in range(1, args.runs + 1):
            order = ["go", "rust"]
            if run % 2 == 0:
                order.reverse()
            for implementation in order:
                print(f"run {run}/{args.runs}: {implementation}", flush=True)
                binary = binaries[implementation]
                home = homes[implementation]
                results.append(
                    with_clients(
                        binary,
                        home,
                        args.clients,
                        lambda clients: serial_search(
                            implementation,
                            run,
                            clients[0],
                            "serial_broad",
                            "alpha",
                            args.serial_broad,
                            2,
                        ),
                    )
                )
                results.append(
                    with_clients(
                        binary,
                        home,
                        args.clients,
                        lambda clients: serial_search(
                            implementation,
                            run,
                            clients[0],
                            "serial_selective",
                            selective_query,
                            args.serial_selective,
                            5,
                        ),
                    )
                )
                results.append(
                    with_clients(
                        binary,
                        home,
                        args.clients,
                        lambda clients: parallel_search(
                            implementation,
                            run,
                            clients,
                            "parallel_broad",
                            "alpha",
                            args.parallel_broad,
                        ),
                    )
                )
                results.append(
                    with_clients(
                        binary,
                        home,
                        args.clients,
                        lambda clients: parallel_search(
                            implementation,
                            run,
                            clients,
                            "parallel_selective",
                            selective_query,
                            args.parallel_selective,
                        ),
                    )
                )
                results.append(
                    with_clients(
                        binary,
                        home,
                        args.clients,
                        lambda clients: mixed_workload(
                            implementation,
                            run,
                            clients,
                            selective_query,
                            args.mixed_reads,
                            args.mixed_writes,
                            campaign,
                        ),
                    )
                )
                expected_counts[implementation] += args.mixed_writes
                invoke(binary, home, "--cortex", "bench", "verify")
                count = trace_count(cortexes[implementation])
                if count != expected_counts[implementation]:
                    raise RuntimeError(
                        f"{implementation} count {count} does not match "
                        f"expected {expected_counts[implementation]}"
                    )

        output = {
            "configuration": {
                "traces": args.traces,
                "runs": args.runs,
                "clients": args.clients,
                "serial_broad_requests": args.serial_broad,
                "serial_selective_requests": args.serial_selective,
                "parallel_broad_requests_per_client": args.parallel_broad,
                "parallel_selective_requests_per_client": args.parallel_selective,
                "mixed_reads_per_reader": args.mixed_reads,
                "mixed_writes": args.mixed_writes,
                "reused_corpus": bool(args.reuse_root),
                "cloned_corpus": bool(args.clone_cortex),
                "initial_trace_counts": initial_counts,
                "campaign": campaign,
                "rust_durability": os.environ.get("NOEMA_DURABILITY", "standard"),
            },
            "seed_seconds": seed_seconds,
            "runs": results,
            "summary": aggregate(results),
        }
        rendered = json.dumps(output, indent=2)
        if args.output:
            args.output.write_text(rendered + "\n")
        print(rendered)


if __name__ == "__main__":
    main()
