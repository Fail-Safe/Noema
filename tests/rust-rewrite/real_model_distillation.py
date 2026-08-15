#!/usr/bin/env python3
"""Compare Go/Rust consolidation quality and resources on synthetic traces."""

from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import re
import sqlite3
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

from federation_ring import Node, cortex_dir, database, environment


CORPUS = [
    (
        "decision",
        "Lantern identity protocol",
        "Project Lantern will use OIDC authorization code flow with PKCE. Access tokens expire after 15 minutes.",
    ),
    (
        "decision",
        "Lantern signing policy",
        "Project Lantern service tokens use Ed25519 signatures. Signing keys rotate every 90 days.",
    ),
    (
        "decision",
        "Lantern session boundary",
        "Project Lantern refresh tokens stay server-side and are revoked when a device is removed.",
    ),
    (
        "fact",
        "Greenhouse irrigation",
        "The greenhouse basil bed receives four liters of water at 06:30 each morning.",
    ),
    (
        "fact",
        "Book club schedule",
        "The book club discusses The Left Hand of Darkness on the second Thursday in October.",
    ),
    (
        "fact",
        "Satellite calibration",
        "The synthetic satellite sensor requires a 12.5 millivolt calibration offset.",
    ),
    (
        "observation",
        "Harbor service baseline",
        "Synthetic Harbor availability was 99.95 percent with p95 latency of 230 milliseconds.",
    ),
    (
        "observation",
        "Harbor retry incident",
        "Synthetic Harbor had a retry storm when queue depth reached 42; limiting retries to 7 restored stability.",
    ),
    (
        "observation",
        "Harbor maintenance result",
        "Synthetic Harbor cache maintenance at 02:00 reduced p95 latency without lowering availability.",
    ),
]

EXPECTED = {
    "decision": {
        "cohesive": True,
        "terms": ["project lantern", "oidc", "pkce", "15", "ed25519", "90", "server-side", "device"],
    },
    "fact": {"cohesive": False, "terms": []},
    "observation": {
        "cohesive": True,
        "terms": ["harbor", "99.95", "230", "retry storm", "42", "7", "02:00", "latency"],
    },
}


class CountingProxy:
    def __init__(self, upstream: str) -> None:
        self.upstream = upstream.rstrip("/")
        self.records: list[dict[str, object]] = []
        self.lock = threading.Lock()
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), self.handler())
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    @property
    def endpoint(self) -> str:
        host, port = self.server.server_address
        return f"http://{host}:{port}/v1"

    def handler(self) -> type[BaseHTTPRequestHandler]:
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self) -> None:
                started = time.monotonic()
                size = int(self.headers.get("Content-Length", "0"))
                body = self.rfile.read(size)
                suffix = self.path.removeprefix("/v1")
                request = urllib.request.Request(
                    owner.upstream + suffix,
                    data=body,
                    headers={"Content-Type": "application/json"},
                    method="POST",
                )
                status = 502
                response_body = b""
                response_type = "application/json"
                try:
                    with urllib.request.urlopen(request, timeout=330) as response:
                        status = response.status
                        response_body = response.read()
                        response_type = response.headers.get(
                            "Content-Type", "application/json"
                        )
                except urllib.error.HTTPError as error:
                    status = error.code
                    response_body = error.read()
                usage: dict[str, object] = {}
                try:
                    parsed = json.loads(response_body)
                    if isinstance(parsed.get("usage"), dict):
                        usage = parsed["usage"]
                except (json.JSONDecodeError, AttributeError):
                    pass
                elapsed = time.monotonic() - started
                with owner.lock:
                    owner.records.append(
                        {
                            "path": suffix,
                            "status": status,
                            "seconds": elapsed,
                            "request_bytes": len(body),
                            "response_bytes": len(response_body),
                            "prompt_tokens": usage.get("prompt_tokens", 0),
                            "completion_tokens": usage.get("completion_tokens", 0),
                        }
                    )
                self.send_response(status)
                self.send_header("Content-Type", response_type)
                self.send_header("Content-Length", str(len(response_body)))
                self.end_headers()
                self.wfile.write(response_body)

            def log_message(self, _format: str, *_arguments: object) -> None:
                return

        return Handler

    def __enter__(self) -> CountingProxy:
        self.thread.start()
        return self

    def __exit__(self, *_args: object) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)


def initialize(node: Node, env: dict[str, str], parent: Path) -> None:
    subprocess.run(
        [str(node.binary), "init", "--name", node.name, "--path", str(parent)],
        env=env,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
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


def seed(node: Node, env: dict[str, str], root: Path) -> None:
    created = datetime.now(timezone.utc)
    rows: list[tuple[str, str]] = []
    for index, (trace_type, title, body) in enumerate(CORPUS):
        completed = subprocess.run(
            [
                str(node.binary),
                "--cortex",
                node.name,
                "add",
                "--title",
                title,
                "--type",
                trace_type,
                "--tag",
                "synthetic-model-eval",
                "--body",
                body,
            ],
            env=env,
            check=True,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        trace_id = completed.stdout.strip().rsplit(": ", 1)[-1]
        timestamp = (created - timedelta(seconds=index)).strftime("%Y-%m-%dT%H:%M:%SZ")
        rows.append((timestamp, trace_id))
    with sqlite3.connect(database(root, node)) as connection:
        connection.executemany(
            "UPDATE traces SET created_at=? WHERE id=?",
            rows,
        )


def parse_metrics(stderr: str) -> dict[str, float]:
    metrics: dict[str, float] = {}
    for key in ("real", "user", "sys"):
        match = re.search(rf"^{key} ([0-9.]+)$", stderr, re.MULTILINE)
        if match:
            metrics[f"{key}_seconds"] = float(match.group(1))
    match = re.search(r"^\s*([0-9]+)\s+maximum resident set size$", stderr, re.MULTILINE)
    if match:
        metrics["peak_rss_mib"] = int(match.group(1)) / (1024 * 1024)
    return metrics


def run_measured(
    node: Node,
    env: dict[str, str],
    root: Path,
    endpoint: str,
    model: str,
    profile: str,
) -> tuple[dict[str, object], dict[str, float]]:
    output = root / f"{node.name}-real-model.json"
    completed = subprocess.run(
        [
            "/usr/bin/time",
            "-lp",
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
            "24",
            "--retries",
            "0",
            "--dry-run",
            "--emit-json",
            str(output),
        ],
        env=env,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    payload = json.loads(output.read_text())
    return payload, parse_metrics(completed.stderr)


def quality(payload: dict[str, object]) -> dict[str, object]:
    clusters = payload["summary"]["cluster_results"]
    results: dict[str, object] = {}
    correct = 0
    retained = 0
    possible = 0
    for cluster in clusters:
        trace_type = cluster["bucket"].split("|", 1)[0]
        expectation = EXPECTED[trace_type]
        cohesive = cluster["outcome"] == "distilled"
        if cohesive == expectation["cohesive"]:
            correct += 1
        text = f"{cluster.get('title', '')}\n{cluster.get('body', '')}".lower()
        hits = [term for term in expectation["terms"] if term in text]
        retained += len(hits)
        possible += len(expectation["terms"])
        results[trace_type] = {
            "outcome": cluster["outcome"],
            "title": cluster.get("title", ""),
            "confidence": cluster.get("confidence", 0),
            "retained_terms": hits,
            "expected_terms": expectation["terms"],
        }
    return {
        "bucket_accuracy": f"{correct}/{len(EXPECTED)}",
        "term_retention": f"{retained}/{possible}",
        "clusters": results,
    }


def exercise(args: argparse.Namespace, root: Path) -> dict[str, object]:
    env = environment(root)
    parent = root / "cortexes"
    nodes = {
        "go": Node("go-real-model", args.go.resolve(), False, 0),
        "rust": Node("rust-real-model", args.rust.resolve(), True, 0),
    }
    for node in nodes.values():
        initialize(node, env, parent)
        configure(cortex_dir(root, node) / "cortex.md")
        seed(node, env, root)

    report: dict[str, object] = {}
    with CountingProxy(args.endpoint) as proxy:
        for label in args.order.split("-"):
            node = nodes[label]
            request_start = len(proxy.records)
            payload, metrics = run_measured(
                node, env, root, proxy.endpoint, args.model, args.profile
            )
            requests = proxy.records[request_start:]
            report[label] = {
                "metrics": metrics,
                "requests": {
                    "count": len(requests),
                    "model_seconds": sum(float(item["seconds"]) for item in requests),
                    "statuses": [item["status"] for item in requests],
                    "request_bytes": sum(int(item["request_bytes"]) for item in requests),
                    "response_bytes": sum(int(item["response_bytes"]) for item in requests),
                    "prompt_tokens": sum(int(item["prompt_tokens"]) for item in requests),
                    "completion_tokens": sum(
                        int(item["completion_tokens"]) for item in requests
                    ),
                },
                "quality": quality(payload),
            }
    return report


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    parser.add_argument("--endpoint", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--profile", default="small")
    parser.add_argument("--order", choices=["go-rust", "rust-go"], default="go-rust")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")
    with tempfile.TemporaryDirectory(prefix="noema-rust-real-model-") as directory:
        report = exercise(args, Path(directory))
    rendered = json.dumps(report, indent=2)
    if args.output:
        args.output.write_text(rendered + "\n")
    print(rendered)


if __name__ == "__main__":
    main()
