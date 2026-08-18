#!/usr/bin/env python3
"""Compare multi-process background-lock contention and crash release."""

from __future__ import annotations

import argparse
import http.client
import json
from pathlib import Path
import subprocess
import tempfile

from federation_ring import Node, environment, free_port, wait_for_port, wait_until
from federation_tls import initialize_node


def start_instance(
    binary: Path,
    name: str,
    rust: bool,
    port: int,
    env: dict[str, str],
    log_path: Path,
) -> subprocess.Popen[str]:
    arguments = [
        str(binary),
        "--cortex",
        name,
        "serve",
        "--transport",
        "http",
        "--host",
        "127.0.0.1",
        "--port",
        str(port),
    ]
    if rust:
        arguments.append("--no-watch")
    with log_path.open("w") as log:
        process = subprocess.Popen(
            arguments,
            env=env,
            text=True,
            stdout=subprocess.DEVNULL,
            stderr=log,
        )
    node = Node(name, binary, rust, port, process)
    wait_for_port(node)
    return process


def stop(process: subprocess.Popen[str], *, crash: bool = False) -> bool:
    if process.poll() is not None:
        return True
    if crash:
        process.kill()
    else:
        process.terminate()
    try:
        process.wait(timeout=5)
        return True
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()
        return False


def initializes(port: int) -> bool:
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=2)
    try:
        connection.request(
            "POST",
            "/mcp",
            body=json.dumps(
                {
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "initialize",
                    "params": {
                        "protocolVersion": "2025-03-26",
                        "capabilities": {},
                        "clientInfo": {"name": "lock-test", "version": "1"},
                    },
                }
            ),
            headers={
                "Content-Type": "application/json",
                "Accept": "application/json, text/event-stream",
            },
        )
        response = connection.getresponse()
        response.read()
        return response.status == 200
    finally:
        connection.close()


def loser_logged(path: Path, rust: bool) -> bool:
    if not path.is_file():
        return False
    marker = "serving MCP only" if rust else "running as MCP-only"
    return marker in path.read_text()


def exercise(binary: Path, rust: bool, root: Path) -> None:
    implementation = "rust" if rust else "go"
    env = environment(root)
    runtime = root / "runtime"
    runtime.mkdir()
    env["XDG_RUNTIME_DIR"] = str(runtime)
    parent = root / "cortexes"
    name = f"{implementation}-lock"
    initialize_node(Node(name, binary, rust, free_port()), env, parent)

    processes: list[subprocess.Popen[str]] = []
    try:
        first_port = free_port()
        first = start_instance(
            binary, name, rust, first_port, env, root / "first.log"
        )
        processes.append(first)

        second_port = free_port()
        second_log = root / "second.log"
        second = start_instance(binary, name, rust, second_port, env, second_log)
        processes.append(second)
        wait_until("MCP-only contention diagnosis", lambda: loser_logged(second_log, rust))
        assert initializes(first_port)
        assert initializes(second_port)

        assert stop(first, crash=True)
        processes.remove(first)

        third_port = free_port()
        third = start_instance(binary, name, rust, third_port, env, root / "third.log")
        processes.append(third)

        fourth_port = free_port()
        fourth_log = root / "fourth.log"
        fourth = start_instance(binary, name, rust, fourth_port, env, fourth_log)
        processes.append(fourth)
        wait_until(
            "contention after crashed-owner replacement",
            lambda: loser_logged(fourth_log, rust),
        )
        assert initializes(second_port)
        assert initializes(third_port)
        assert initializes(fourth_port)
    finally:
        if not all(stop(process) for process in reversed(processes)):
            raise RuntimeError(f"{implementation}: a lock-test server did not stop")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")
    with tempfile.TemporaryDirectory(prefix="noema-go-lock-") as directory:
        exercise(args.go.resolve(), False, Path(directory))
    with tempfile.TemporaryDirectory(prefix="noema-rust-lock-") as directory:
        exercise(args.rust.resolve(), True, Path(directory))
    print("Go/Rust background lock: contention, MCP-only, crash release, reacquire PASS")


if __name__ == "__main__":
    main()
