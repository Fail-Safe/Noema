#!/usr/bin/env python3
"""Exercise signed federation across the Go and Rust implementations."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import tempfile
import time


def environment(root: Path) -> dict[str, str]:
    env = os.environ.copy()
    env["HOME"] = str(root / "home")
    env["XDG_CONFIG_HOME"] = str(root / "config")
    return env


def run(
    binary: Path, env: dict[str, str], *arguments: str, check: bool = True
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [str(binary), *arguments], env=env, check=check, text=True,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )


def free_port() -> int:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def wait_for_port(port: int, process: subprocess.Popen[str]) -> None:
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"server exited early with status {process.returncode}")
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                return
        except OSError:
            time.sleep(0.05)
    raise RuntimeError(f"server did not listen on 127.0.0.1:{port}")


def start_server(
    binary: Path, env: dict[str, str], cortex: str, port: int, rust: bool
) -> subprocess.Popen[str]:
    arguments = [
        str(binary), "--cortex", cortex, "serve", "--transport", "http",
        "--host", "127.0.0.1", "--port", str(port),
    ]
    if rust:
        arguments.append("--no-watch")
    process = subprocess.Popen(
        arguments, env=env, text=True,
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    wait_for_port(port, process)
    return process


def stop_server(process: subprocess.Popen[str] | None) -> None:
    if process is None or process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()


def add_trace(
    binary: Path, env: dict[str, str], cortex: str, title: str, body: str
) -> str:
    completed = run(
        binary, env, "--cortex", cortex, "add", "--title", title,
        "--type", "fact", "--author", "test-agent", "--tag", "network-test",
        "--body", body,
    )
    return completed.stdout.strip().rsplit(": ", 1)[-1]


def set_go_federation_policy(path: Path) -> None:
    source = path.read_text()
    marker = "federation:\n"
    if marker not in source:
        raise RuntimeError("Go manifest has no federation block")
    path.write_text(source.replace(
        marker, "federation:\n    interval: 250ms\n    verify: enforce\n", 1
    ))


def set_rust_verify(path: Path) -> None:
    source = path.read_text()
    marker = "  verify: ''"
    if marker not in source:
        raise RuntimeError("Rust manifest has no default verify field")
    path.write_text(source.replace(marker, "  verify: enforce", 1))


def cursor(database: Path, peer: str) -> str:
    with sqlite3.connect(database) as connection:
        row = connection.execute(
            "SELECT value FROM federation_state WHERE key=?",
            (f"peer:{peer}:last_event",),
        ).fetchone()
    return "" if row is None else str(row[0])


def wait_for_trace(
    binary: Path, env: dict[str, str], cortex: str,
    trace_id: str, expected_body: str,
) -> None:
    deadline = time.monotonic() + 10
    last = ""
    while time.monotonic() < deadline:
        result = run(binary, env, "--cortex", cortex, "get", trace_id, check=False)
        last = result.stdout + result.stderr
        if result.returncode == 0 and expected_body in result.stdout:
            return
        time.sleep(0.1)
    raise RuntimeError(f"trace {trace_id} did not converge: {last}")


def rust_pulls_go(go: Path, rust: Path, root: Path) -> None:
    env = environment(root)
    cortexes = root / "cortexes"
    run(go, env, "init", "--name", "go-source", "--path", str(cortexes))
    run(rust, env, "init", "--name", "rust-target", "--path", str(cortexes))
    run(go, env, "--cortex", "go-source", "keygen")
    batch_ids = []
    for index in range(1, 206):
        batch_ids.append(
            add_trace(go, env, "go-source", f"batch-{index}", f"Go batching payload {index}")
        )

    port = free_port()
    run(
        rust, env, "--cortex", "rust-target", "federation", "add-peer",
        "go-source", f"http://127.0.0.1:{port}",
    )
    set_rust_verify(cortexes / "rust-target" / "cortex.md")
    source: subprocess.Popen[str] | None = start_server(
        go, env, "go-source", port, rust=False
    )
    try:
        synced = run(
            rust, env, "--cortex", "rust-target", "federation", "sync", "go-source"
        )
        assert "205 event(s)" in synced.stdout
        assert "3 batch(es)" in synced.stdout

        run(go, env, "--cortex", "go-source", "archive", batch_ids[0])
        run(
            go, env, "--cortex", "go-source", "memory", "promote",
            batch_ids[1], "--to", "mid",
        )
        run(go, env, "--cortex", "go-source", "remove", batch_ids[2])
        incremental = run(
            rust, env, "--cortex", "rust-target", "federation", "sync", "go-source"
        )
        assert "3 event(s)" in incremental.stdout
        assert (cortexes / "rust-target" / "archive" / "traces" / f"{batch_ids[0]}.md").is_file()
        assert (cortexes / "rust-target" / "trash" / "traces" / f"{batch_ids[2]}.md").is_file()

        database = cortexes / "rust-target" / "db" / "noema.db"
        stable_cursor = cursor(database, "go-source")
        stop_server(source)
        source = None
        unavailable = run(
            rust, env, "--cortex", "rust-target", "federation", "sync",
            "go-source", check=False,
        )
        assert unavailable.returncode != 0
        assert cursor(database, "go-source") == stable_cursor

        run(go, env, "init", "--name", "replacement", "--path", str(cortexes))
        run(go, env, "--cortex", "replacement", "keygen")
        replacement = start_server(go, env, "replacement", port, rust=False)
        try:
            mismatch = run(
                rust, env, "--cortex", "rust-target", "federation", "sync",
                "go-source", check=False,
            )
            assert mismatch.returncode != 0
            assert "identity mismatch" in mismatch.stderr
            assert cursor(database, "go-source") == stable_cursor
        finally:
            stop_server(replacement)

        recovery_id = add_trace(
            go, env, "go-source", "after-recovery", "available after retry"
        )
        source = start_server(go, env, "go-source", port, rust=False)
        recovered = run(
            rust, env, "--cortex", "rust-target", "federation", "sync", "go-source"
        )
        assert "1 event(s)" in recovered.stdout
        assert cursor(database, "go-source") > stable_cursor
        wait_for_trace(rust, env, "rust-target", recovery_id, "available after retry")
    finally:
        stop_server(source)


def go_pulls_rust(go: Path, rust: Path, root: Path) -> None:
    env = environment(root)
    cortexes = root / "cortexes"
    run(rust, env, "init", "--name", "rust-source", "--path", str(cortexes))
    run(go, env, "init", "--name", "go-target", "--path", str(cortexes))
    run(rust, env, "--cortex", "rust-source", "keygen")
    trace_id = add_trace(
        rust, env, "rust-source", "rust-to-go", "signed Rust event accepted by Go"
    )
    source_port = free_port()
    target_port = free_port()
    run(
        go, env, "--cortex", "go-target", "federation", "add-peer",
        "rust-source", f"http://127.0.0.1:{source_port}",
    )
    set_go_federation_policy(cortexes / "go-target" / "cortex.md")
    source = start_server(rust, env, "rust-source", source_port, rust=True)
    target: subprocess.Popen[str] | None = None
    try:
        target = start_server(go, env, "go-target", target_port, rust=False)
        wait_for_trace(
            go, env, "go-target", trace_id, "signed Rust event accepted by Go"
        )
    finally:
        stop_server(target)
        stop_server(source)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    go = args.go.resolve()
    rust = args.rust.resolve()
    for binary in (go, rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")

    with tempfile.TemporaryDirectory(prefix="noema-rust-federation-") as directory:
        root = Path(directory)
        rust_pulls_go(go, rust, root / "go-to-rust")
        go_pulls_rust(go, rust, root / "rust-to-go")
    print("mixed federation: 2 signed directions, 205-event batching, lifecycle, identity, retry PASS")


if __name__ == "__main__":
    main()
