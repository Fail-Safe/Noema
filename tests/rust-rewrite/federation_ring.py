#!/usr/bin/env python3
"""Exercise a signed three-node Go/Rust federation under background sync."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import tempfile
import time
from typing import Callable


@dataclass
class Node:
    name: str
    binary: Path
    rust: bool
    port: int
    process: subprocess.Popen[str] | None = None


def environment(root: Path) -> dict[str, str]:
    env = os.environ.copy()
    env["HOME"] = str(root / "home")
    env["XDG_CONFIG_HOME"] = str(root / "config")
    return env


def run(
    node: Node,
    env: dict[str, str],
    *arguments: str,
    check: bool = True,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [str(node.binary), "--cortex", node.name, *arguments],
        env=env,
        check=check,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )


def free_port() -> int:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def wait_until(label: str, predicate: Callable[[], bool], timeout: float = 15) -> None:
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            if predicate():
                return
        except (OSError, sqlite3.Error, ValueError) as error:
            last_error = error
        time.sleep(0.1)
    detail = f": {last_error}" if last_error is not None else ""
    raise RuntimeError(f"timed out waiting for {label}{detail}")


def wait_for_port(node: Node) -> None:
    assert node.process is not None
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if node.process.poll() is not None:
            raise RuntimeError(
                f"{node.name} exited early with status {node.process.returncode}"
            )
        try:
            with socket.create_connection(("127.0.0.1", node.port), timeout=0.2):
                return
        except OSError:
            time.sleep(0.05)
    raise RuntimeError(f"{node.name} did not listen on port {node.port}")


def start(node: Node, env: dict[str, str]) -> None:
    arguments = [
        str(node.binary),
        "--cortex",
        node.name,
        "serve",
        "--transport",
        "http",
        "--host",
        "127.0.0.1",
        "--port",
        str(node.port),
    ]
    if node.rust:
        arguments.append("--no-watch")
    log_path = Path(env["HOME"]).parent / f"{node.name}.log"
    with log_path.open("a") as log:
        node.process = subprocess.Popen(
            arguments,
            env=env,
            text=True,
            stdout=subprocess.DEVNULL,
            stderr=log,
        )
    wait_for_port(node)


def stop(node: Node) -> bool:
    if node.process is None or node.process.poll() is not None:
        return True
    node.process.terminate()
    try:
        node.process.wait(timeout=5)
        graceful = True
    except subprocess.TimeoutExpired:
        node.process.kill()
        node.process.wait()
        graceful = False
    node.process = None
    return graceful


def cortex_dir(root: Path, node: Node) -> Path:
    return root / "cortexes" / node.name


def database(root: Path, node: Node) -> Path:
    return cortex_dir(root, node) / "db" / "noema.db"


def configure_policy(path: Path, rust: bool) -> None:
    source = path.read_text()
    if rust:
        source = source.replace("  interval: ''", "  interval: 200ms", 1)
        source = source.replace("  verify: ''", "  verify: enforce", 1)
    else:
        marker = "federation:\n"
        if marker not in source:
            raise RuntimeError("Go manifest has no federation block")
        source = source.replace(
            marker, "federation:\n    interval: 200ms\n    verify: enforce\n", 1
        )
    path.write_text(source)


def add_trace(node: Node, env: dict[str, str], title: str, body: str) -> str:
    result = run(
        node,
        env,
        "add",
        "--title",
        title,
        "--type",
        "fact",
        "--author",
        "test-agent",
        "--tag",
        "ring-test",
        "--body",
        body,
    )
    return result.stdout.strip().rsplit(": ", 1)[-1]


def has_trace(node: Node, env: dict[str, str], trace_id: str, text: str = "") -> bool:
    result = run(node, env, "get", trace_id, check=False)
    return result.returncode == 0 and text in result.stdout


def state(database_path: Path, key: str) -> str:
    with sqlite3.connect(database_path) as connection:
        row = connection.execute(
            "SELECT value FROM federation_state WHERE key=?", (key,)
        ).fetchone()
    return "" if row is None else str(row[0])


def health(database_path: Path, peer: str) -> dict[str, object]:
    raw = state(database_path, f"peer:{peer}:health")
    return {} if not raw else json.loads(raw)


def event_counts(database_path: Path) -> tuple[int, int]:
    with sqlite3.connect(database_path) as connection:
        row = connection.execute(
            "SELECT count(*),count(DISTINCT id) FROM events"
        ).fetchone()
    assert row is not None
    return int(row[0]), int(row[1])


def usage_owner_count(database_path: Path, trace_id: str) -> int:
    with sqlite3.connect(database_path) as connection:
        row = connection.execute(
            "SELECT count(DISTINCT peer_cortex_id) FROM trace_usage WHERE trace_id=?",
            (trace_id,),
        ).fetchone()
    assert row is not None
    return int(row[0])


def divergence_bodies(database_path: Path) -> list[str]:
    with sqlite3.connect(database_path) as connection:
        ids = [
            str(row[0])
            for row in connection.execute(
                "SELECT id FROM traces WHERE type='divergence' AND trashed_at IS NULL"
            )
        ]
        bodies = []
        for trace_id in ids:
            row = connection.execute(
                "SELECT body FROM traces_fts WHERE id=?", (trace_id,)
            ).fetchone()
            if row is not None:
                bodies.append(str(row[0]))
    return bodies


def mcp_call(
    node: Node, env: dict[str, str], tool: str, arguments: dict[str, object]
) -> None:
    messages = [
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2025-03-26",
                "capabilities": {},
                "clientInfo": {"name": "ring-test", "version": "1"},
            },
        },
        {"jsonrpc": "2.0", "method": "notifications/initialized"},
        {
            "jsonrpc": "2.0",
            "id": 2,
            "method": "tools/call",
            "params": {"name": tool, "arguments": arguments},
        },
    ]
    payload = "".join(json.dumps(message) + "\n" for message in messages)
    result = subprocess.run(
        [
            str(node.binary),
            "--cortex",
            node.name,
            "serve",
            "--transport",
            "stdio",
        ],
        env=env,
        input=payload,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    responses = [json.loads(line) for line in result.stdout.splitlines() if line]
    response = next(item for item in responses if item.get("id") == 2)
    if "error" in response or response.get("result", {}).get("isError"):
        raise RuntimeError(f"{node.name} MCP call {tool} failed")


def initialize(root: Path, env: dict[str, str], nodes: list[Node]) -> None:
    parent = root / "cortexes"
    for node in nodes:
        subprocess.run(
            [str(node.binary), "init", "--name", node.name, "--path", str(parent)],
            env=env,
            check=True,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        run(node, env, "keygen")
    for node in nodes:
        for peer in nodes:
            if peer is node:
                continue
            run(
                node,
                env,
                "federation",
                "add-peer",
                peer.name,
                f"http://127.0.0.1:{peer.port}",
            )
        configure_policy(cortex_dir(root, node) / "cortex.md", node.rust)


def exercise(go: Path, rust: Path, root: Path) -> None:
    env = environment(root)
    nodes = [
        Node("peer-a", go, False, free_port()),
        Node("peer-b", rust, True, free_port()),
        Node("peer-c", rust, True, free_port()),
    ]
    initialize(root, env, nodes)
    seeded = {
        node.name: add_trace(
            node, env, f"seed-{node.name}", f"shared ring payload from {node.name}"
        )
        for node in nodes
    }

    try:
        for node in nodes:
            start(node, env)
        for node in nodes:
            for origin, trace_id in seeded.items():
                wait_until(
                    f"{trace_id} on {node.name}",
                    lambda n=node, i=trace_id, o=origin: has_trace(n, env, i, o),
                )
            total, distinct = event_counts(database(root, node))
            assert total >= 3 and total == distinct

        shared_id = seeded["peer-a"]
        for node in nodes:
            mcp_call(node, env, "get_trace", {"id": shared_id, "record_usage": True})
            mcp_call(node, env, "search_traces", {"query": "shared ring payload"})
        for node in nodes:
            wait_until(
                f"three usage owners on {node.name}",
                lambda n=node: usage_owner_count(database(root, n), shared_id) == 3,
            )

        peer_b = nodes[1]
        run(peer_b, env, "federation", "pause-peer", "peer-a")
        time.sleep(0.5)
        paused_cursor = state(database(root, peer_b), "peer:peer-a:last_event")
        paused_id = add_trace(nodes[0], env, "paused-path", "pause and resume payload")
        wait_until(
            "paused event reaching peer-c",
            lambda: has_trace(nodes[2], env, paused_id, "pause and resume payload"),
        )
        time.sleep(0.5)
        assert state(database(root, peer_b), "peer:peer-a:last_event") == paused_cursor
        run(peer_b, env, "federation", "resume-peer", "peer-a")
        wait_until(
            "resumed direct cursor",
            lambda: state(database(root, peer_b), "peer:peer-a:last_event")
            > paused_cursor,
        )

        peer_c = nodes[2]
        assert stop(peer_c)
        outage_id = add_trace(peer_c, env, "outage", "written while server stopped")
        try:
            wait_until(
                "classified peer-c outage",
                lambda: health(database(root, peer_b), "peer-c")
                .get("last_error", {})
                .get("reason")
                == "network_refused",
            )
        except RuntimeError as error:
            log_path = Path(env["HOME"]).parent / f"{peer_b.name}.log"
            log_tail = log_path.read_text()[-2000:]
            raise RuntimeError(
                f"{error}; observed health={health(database(root, peer_b), 'peer-c')}; "
                f"log tail={log_tail}"
            ) from error
        start(peer_c, env)
        for node in nodes:
            wait_until(
                f"outage recovery on {node.name}",
                lambda n=node: has_trace(n, env, outage_id, "written while server stopped"),
            )
        wait_until(
            "peer-c health recovery",
            lambda: health(database(root, peer_b), "peer-c").get(
                "consecutive_failures", 0
            )
            == 0
            and bool(health(database(root, peer_b), "peer-c").get("last_success")),
        )

        assert all(stop(node) for node in nodes)
        run(nodes[1], env, "append", shared_id, "--content", "peer-b concurrent body")
        run(nodes[2], env, "append", shared_id, "--content", "peer-c concurrent body")
        for node in nodes:
            start(node, env)
        for node in nodes:
            wait_until(
                f"concurrent divergence on {node.name}",
                lambda n=node: any(
                    "peer-b concurrent body" in body and "peer-c concurrent body" in body
                    for body in divergence_bodies(database(root, n))
                ),
            )

        peer_a = nodes[0]
        assert stop(peer_a)
        frozen = {
            node.name: state(database(root, node), "peer:peer-a:last_event")
            for node in nodes[1:]
        }
        run(peer_a, env, "keygen", "--force")
        rotated_id = add_trace(peer_a, env, "rotated-key", "must fail closed")
        start(peer_a, env)
        for node in nodes[1:]:
            wait_until(
                f"key rotation rejection on {node.name}",
                lambda n=node: health(database(root, n), "peer-a").get(
                    "last_error", {}
                ).get("reason")
                == "identity_mismatch",
            )
            assert state(database(root, node), "peer:peer-a:last_event") == frozen[node.name]
            assert not has_trace(node, env, rotated_id)
    finally:
        graceful = [stop(node) for node in nodes]
        if not all(graceful):
            raise RuntimeError("one or more servers did not terminate gracefully")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")
    with tempfile.TemporaryDirectory(prefix="noema-rust-ring-") as directory:
        exercise(args.go.resolve(), args.rust.resolve(), Path(directory))
    print(
        "mixed ring: convergence, usage, pause/resume, outage, divergence, "
        "shutdown, key-rotation rejection PASS"
    )


if __name__ == "__main__":
    main()
