#!/usr/bin/env python3
"""Compare steady-state stdio MCP search latency without third-party packages."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import statistics
import subprocess
import tempfile
import time


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


def request(process: subprocess.Popen[str], payload: dict[str, object]) -> dict[str, object]:
    assert process.stdin is not None
    assert process.stdout is not None
    process.stdin.write(json.dumps(payload, separators=(",", ":")) + "\n")
    process.stdin.flush()
    response = process.stdout.readline()
    if not response:
        raise RuntimeError("MCP server closed stdout before responding")
    parsed = json.loads(response)
    if parsed.get("error"):
        raise RuntimeError(f"MCP error: {parsed['error']}")
    result = parsed.get("result")
    if isinstance(result, dict) and result.get("isError"):
        raise RuntimeError(f"MCP tool error: {result}")
    return parsed


def percentile(samples: list[float], value: float) -> float:
    ordered = sorted(samples)
    index = min(len(ordered) - 1, max(0, round((len(ordered) - 1) * value)))
    return ordered[index]


def process_metrics(pid: int) -> tuple[int, float]:
    rss = int(
        subprocess.check_output(
            ["ps", "-o", "rss=", "-p", str(pid)], text=True
        ).strip()
    )
    raw_time = subprocess.check_output(
        ["ps", "-o", "time=", "-p", str(pid)], text=True
    ).strip()
    parts = raw_time.split(":")
    cpu_seconds = 0.0
    for part in parts:
        cpu_seconds = cpu_seconds * 60 + float(part)
    return rss, cpu_seconds


def benchmark(
    implementation: str,
    binary: Path,
    root: Path,
    traces: int,
    requests: int,
    run: int,
) -> dict[str, object]:
    home = root / f"{implementation}-{run}" / "home"
    cortexes = root / f"{implementation}-{run}" / "cortexes"
    home.mkdir(parents=True)
    cortexes.mkdir(parents=True)
    invoke(binary, home, "init", "--name", "bench", "--path", str(cortexes))
    environment = os.environ.copy()
    environment["HOME"] = str(home)
    process = subprocess.Popen(
        [str(binary), "--cortex", "bench", "serve", "--transport", "stdio"],
        env=environment,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        text=True,
        bufsize=1,
    )
    try:
        request(
            process,
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {
                    "protocolVersion": "2025-03-26",
                    "capabilities": {},
                    "clientInfo": {"name": "benchmark", "version": "1"},
                },
            },
        )
        assert process.stdin is not None
        process.stdin.write(
            '{"jsonrpc":"2.0","method":"notifications/initialized"}\n'
        )
        process.stdin.flush()

        sequence = 2
        for index in range(1, traces + 1):
            request(
                process,
                {
                    "jsonrpc": "2.0",
                    "id": sequence,
                    "method": "tools/call",
                    "params": {
                        "name": "create_trace",
                        "arguments": {
                            "title": f"MCP benchmark trace {index}",
                            "type": "fact",
                            "author": "benchmark",
                            "tags": "performance",
                            "body": f"steady state alpha beta gamma item {index}",
                        },
                    },
                },
            )
            sequence += 1

        for _ in range(20):
            request(
                process,
                {
                    "jsonrpc": "2.0",
                    "id": sequence,
                    "method": "tools/call",
                    "params": {
                        "name": "search_traces",
                        "arguments": {"query": "alpha", "mode": "lexical"},
                    },
                },
            )
            sequence += 1

        latency_ms: list[float] = []
        rss_before, cpu_before = process_metrics(process.pid)
        started = time.perf_counter()
        for _ in range(requests):
            before = time.perf_counter_ns()
            request(
                process,
                {
                    "jsonrpc": "2.0",
                    "id": sequence,
                    "method": "tools/call",
                    "params": {
                        "name": "search_traces",
                        "arguments": {"query": "alpha", "mode": "lexical"},
                    },
                },
            )
            latency_ms.append((time.perf_counter_ns() - before) / 1_000_000)
            sequence += 1
        elapsed = time.perf_counter() - started
        rss_after, cpu_after = process_metrics(process.pid)
    finally:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()

    return {
        "implementation": implementation,
        "run": run,
        "corpus_traces": traces,
        "requests": requests,
        "requests_per_second": round(requests / elapsed, 2),
        "max_rss_kb": max(rss_before, rss_after),
        "cpu_seconds": round(cpu_after - cpu_before, 3),
        "cpu_utilization_percent": round((cpu_after - cpu_before) / elapsed * 100, 1),
        "latency_ms": {
            "median": round(statistics.median(latency_ms), 3),
            "p95": round(percentile(latency_ms, 0.95), 3),
            "max": round(max(latency_ms), 3),
        },
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    parser.add_argument("--traces", type=int, default=100)
    parser.add_argument("--requests", type=int, default=250)
    parser.add_argument("--runs", type=int, default=5)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")

    with tempfile.TemporaryDirectory(prefix="noema-mcp-bench-") as directory:
        root = Path(directory)
        results = []
        for run in range(1, args.runs + 1):
            order = [
                ("go", args.go.resolve()),
                ("rust", args.rust.resolve()),
            ]
            if run % 2 == 0:
                order.reverse()
            for implementation, binary in order:
                results.append(
                    benchmark(
                        implementation,
                        binary,
                        root,
                        args.traces,
                        args.requests,
                        run,
                    )
                )

    summary = {}
    for implementation in ("go", "rust"):
        selected = [r for r in results if r["implementation"] == implementation]
        summary[implementation] = {
            "requests_per_second_median": round(
                statistics.median(r["requests_per_second"] for r in selected), 2
            ),
            "latency_ms_median": round(
                statistics.median(r["latency_ms"]["median"] for r in selected), 3
            ),
            "latency_ms_p95_median": round(
                statistics.median(r["latency_ms"]["p95"] for r in selected), 3
            ),
            "max_rss_kb_median": round(
                statistics.median(r["max_rss_kb"] for r in selected)
            ),
            "cpu_utilization_percent_median": round(
                statistics.median(r["cpu_utilization_percent"] for r in selected), 1
            ),
        }
    print(json.dumps({"runs": results, "summary": summary}, indent=2))


if __name__ == "__main__":
    main()
