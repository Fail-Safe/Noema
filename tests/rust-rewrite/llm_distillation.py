#!/usr/bin/env python3
"""Compare deterministic Go/Rust model-driven consolidation."""

from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import threading
import time

from federation_ring import (
    Node,
    add_trace,
    cortex_dir,
    database,
    environment,
    free_port,
    start,
    stop,
    wait_until,
)


class FakeModel:
    def __init__(self, mode: str) -> None:
        self.mode = mode
        self.requests: list[dict[str, object]] = []
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), self.handler())
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    @property
    def endpoint(self) -> str:
        host, port = self.server.server_address
        return f"http://{host}:{port}/v1"

    def handler(self) -> type[BaseHTTPRequestHandler]:
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                if self.path == "/v1/models":
                    self.send_json({"data": [{"id": "fixture-model"}]})
                else:
                    self.send_error(404)

            def do_POST(self) -> None:
                if self.path != "/v1/chat/completions":
                    self.send_error(404)
                    return
                length = int(self.headers.get("Content-Length", "0"))
                request = json.loads(self.rfile.read(length))
                owner.requests.append(request)
                if owner.mode == "success":
                    prompt = request["messages"][0]["content"]
                    if "JSON object" in prompt:
                        content = json.dumps(
                            {
                                "cohesive": True,
                                "title": "Shared model result",
                                "tags": ["MCP Server", "model-test"],
                                "body": "The three fixture observations form one durable result.",
                                "confidence": 0.85,
                            }
                        )
                    elif "single word" in prompt:
                        content = "yes"
                    elif "Write one consolidated memory" in prompt:
                        content = (
                            "Title: Shared model result\n"
                            "Tags: MCP Server, model-test\n"
                            "Body: The three fixture observations form one durable result."
                        )
                    elif "Rate how well" in prompt:
                        content = "The key fixture details are preserved.\n8"
                    else:
                        raise RuntimeError(f"unexpected model prompt: {prompt[:120]}")
                elif owner.mode == "rejected":
                    content = json.dumps({"cohesive": False})
                else:
                    content = "not-json"
                self.send_json(
                    {
                        "choices": [
                            {
                                "message": {"role": "assistant", "content": content},
                                "finish_reason": "stop",
                            }
                        ]
                    }
                )

            def send_json(self, value: object) -> None:
                body = json.dumps(value).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, _format: str, *_arguments: object) -> None:
                return

        return Handler

    def __enter__(self) -> FakeModel:
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


def configure(manifest: Path, endpoint: str, profile: str = "frontier") -> None:
    lines = manifest.read_text().splitlines()
    boundaries = [index for index, line in enumerate(lines) if line == "---"]
    if len(boundaries) < 2:
        raise RuntimeError(f"manifest has no closing frontmatter boundary: {manifest}")
    lines[boundaries[1] : boundaries[1]] = [
        "consolidation:",
        "  enabled: true",
        "  cron: 00:00",
        "  window_hours: 24",
        "  llm_enabled: true",
        "  auto_distillation_enabled: true",
        f"  model_tier: {profile}",
        f"  local_llm_endpoint: {endpoint}",
        "  model_name: fixture-model",
        "  graduation:",
        "    enabled: false",
    ]
    manifest.write_text("\n".join(lines) + "\n")


def trace_state(database_path: Path) -> list[tuple[str, str, str]]:
    with sqlite3.connect(database_path) as connection:
        return [
            (str(title), str(trace_type), str(tier))
            for title, trace_type, tier in connection.execute(
                "SELECT title,type,tier FROM traces ORDER BY title"
            )
        ]


def consolidated(database_path: Path) -> list[tuple[str, dict[str, object]]]:
    with sqlite3.connect(database_path) as connection:
        rows = connection.execute(
            "SELECT trace_id,data FROM events WHERE action='consolidate' ORDER BY id"
        ).fetchall()
    return [(str(trace_id), json.loads(str(data))) for trace_id, data in rows]


def lineage(database_path: Path, trace_id: str) -> list[str]:
    with sqlite3.connect(database_path) as connection:
        return [
            str(row[0])
            for row in connection.execute(
                "SELECT derived_from FROM trace_lineage WHERE trace_id=? ORDER BY derived_from",
                (trace_id,),
            )
        ]


def set_vote(database_path: Path, trace_id: str) -> None:
    with sqlite3.connect(database_path) as connection:
        connection.execute("UPDATE traces SET tier_votes=1 WHERE id=?", (trace_id,))


def success_scenario(
    node: Node,
    env: dict[str, str],
    root: Path,
    model: FakeModel,
    profile: str = "frontier",
) -> tuple[list[tuple[str, str, str]], dict[str, object]]:
    parent = root / "cortexes"
    initialize(node, env, parent)
    configure(cortex_dir(root, node) / "cortex.md", model.endpoint, profile)
    source_ids = [
        add_trace(node, env, f"source-{index}", f"fixture observation {index}")
        for index in range(3)
    ]
    now = datetime.now(timezone.utc)
    with sqlite3.connect(database(root, node)) as connection:
        for index, source_id in enumerate(source_ids):
            created = (now - timedelta(seconds=index)).strftime("%Y-%m-%dT%H:%M:%SZ")
            connection.execute(
                "UPDATE traces SET created_at=? WHERE id=?", (created, source_id)
            )
    try:
        start(node, env)
        wait_until(
            f"distillation on {node.name}",
            lambda: len(consolidated(database(root, node))) == 1,
        )
        assert stop(node)
        events = consolidated(database(root, node))
        distilled_id, payload = events[0]
        assert set(lineage(database(root, node), distilled_id)) == set(source_ids)
        expected_payload: dict[str, object] = {
            "source_ids": source_ids,
            "distilled_id": distilled_id,
            "model_name": "fixture-model",
            "model_tier_profile": profile,
        }
        if profile == "frontier":
            expected_payload["cohesion_confidence"] = 0.85
        elif profile == "large":
            expected_payload["cohesion_confidence"] = 0.8
        assert payload == expected_payload, (payload, expected_payload)
        states = trace_state(database(root, node))
        assert ("Shared model result", "observation", "mid") in states
        assert all((f"source-{index}", "fact", "short") in states for index in range(3))
        first_request_count = len(model.requests)
        assert first_request_count == {"small": 2, "large": 3, "frontier": 1}[profile]
        request = model.requests[0]
        assert request["model"] == "fixture-model"
        assert request["stream"] is False
        assert request["chat_template_kwargs"] == {"enable_thinking": False}

        start(node, env)
        time.sleep(0.7)
        assert stop(node)
        assert len(consolidated(database(root, node))) == 1
        assert len(model.requests) == first_request_count
        return states, {
            key: value for key, value in payload.items() if key != "distilled_id"
        }
    finally:
        if not stop(node):
            raise RuntimeError(f"{node.name} did not stop gracefully")


def failure_scenario(
    node: Node,
    env: dict[str, str],
    root: Path,
    endpoint: str,
    expected_requests: int | None,
    model: FakeModel | None = None,
) -> list[tuple[str, str, str]]:
    parent = root / "cortexes"
    initialize(node, env, parent)
    configure(cortex_dir(root, node) / "cortex.md", endpoint)
    hot = add_trace(node, env, "fallback-hot", "fixture hot")
    add_trace(node, env, "fallback-cold-a", "fixture cold a")
    add_trace(node, env, "fallback-cold-b", "fixture cold b")
    set_vote(database(root, node), hot)
    try:
        start(node, env)
        wait_until(
            f"fallback promotion on {node.name}",
            lambda: ("fallback-hot", "fact", "mid")
            in trace_state(database(root, node)),
        )
        assert stop(node)
        states = trace_state(database(root, node))
        assert ("fallback-hot", "fact", "mid") in states
        assert ("fallback-cold-a", "fact", "short") in states
        assert ("fallback-cold-b", "fact", "short") in states
        assert consolidated(database(root, node)) == []
        if expected_requests is not None:
            assert model is not None and len(model.requests) == expected_requests
        log = (root / f"{node.name}.log").read_text()
        assert "fallback" in log or "continuing maintenance" in log
        return states
    finally:
        if not stop(node):
            raise RuntimeError(f"{node.name} did not stop gracefully")


def exercise(go: Path, rust: Path, root: Path) -> None:
    env = environment(root)
    with FakeModel("success") as go_model:
        go_success = success_scenario(
            Node("go-distill", go, False, free_port()), env, root, go_model
        )
    with FakeModel("success") as rust_model:
        rust_success = success_scenario(
            Node("rust-distill", rust, True, free_port()), env, root, rust_model
        )
    assert rust_success == go_success

    for profile in ("small", "large"):
        with FakeModel("success") as go_model:
            go_profile = success_scenario(
                Node(f"go-{profile}", go, False, free_port()),
                env,
                root,
                go_model,
                profile,
            )
        with FakeModel("success") as rust_model:
            rust_profile = success_scenario(
                Node(f"rust-{profile}", rust, True, free_port()),
                env,
                root,
                rust_model,
                profile,
            )
        assert rust_profile == go_profile

    with FakeModel("malformed") as go_model:
        go_malformed = failure_scenario(
            Node("go-malformed", go, False, free_port()),
            env,
            root,
            go_model.endpoint,
            2,
            go_model,
        )
    with FakeModel("malformed") as rust_model:
        rust_malformed = failure_scenario(
            Node("rust-malformed", rust, True, free_port()),
            env,
            root,
            rust_model.endpoint,
            2,
            rust_model,
        )
    assert rust_malformed == go_malformed

    go_offline = failure_scenario(
        Node("go-offline", go, False, free_port()),
        env,
        root,
        f"http://127.0.0.1:{free_port()}/v1",
        None,
    )
    rust_offline = failure_scenario(
        Node("rust-offline", rust, True, free_port()),
        env,
        root,
        f"http://127.0.0.1:{free_port()}/v1",
        None,
    )
    assert rust_offline == go_offline


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")
    with tempfile.TemporaryDirectory(prefix="noema-rust-llm-distillation-") as directory:
        exercise(args.go.resolve(), args.rust.resolve(), Path(directory))
    print(
        "Go/Rust LLM distillation: three profiles, lineage, source exclusion, malformed/offline fallback PASS"
    )


if __name__ == "__main__":
    main()
