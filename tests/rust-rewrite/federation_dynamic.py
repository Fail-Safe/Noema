#!/usr/bin/env python3
"""Exercise Rust federation worker reconciliation against a live Go peer."""

from __future__ import annotations

import argparse
from pathlib import Path
import subprocess
import tempfile
import time

from federation_ring import (
    Node,
    add_trace,
    configure_policy,
    database,
    environment,
    free_port,
    has_trace,
    run,
    start,
    state,
    stop,
    wait_until,
)
from federation_tls import initialize_node


def remove_peer(manifest: Path, name: str) -> None:
    lines = manifest.read_text().splitlines()
    start_index = None
    item_indent = 0
    for index, line in enumerate(lines):
        if line.strip() == f"- name: {name}":
            start_index = index
            item_indent = len(line) - len(line.lstrip())
            break
    if start_index is None:
        raise RuntimeError(f"could not find peer {name!r} in {manifest}")
    end_index = start_index + 1
    while end_index < len(lines):
        line = lines[end_index]
        if line.strip() and len(line) - len(line.lstrip()) <= item_indent:
            break
        end_index += 1
    del lines[start_index:end_index]
    manifest.write_text("\n".join(lines) + "\n")


def log_contains(path: Path, text: str) -> bool:
    return path.is_file() and text in path.read_text()


def exercise(go: Path, rust: Path, root: Path) -> None:
    env = environment(root)
    parent = root / "cortexes"
    source = Node("peer-source", go, False, free_port())
    dynamic = Node("peer-dynamic", rust, True, free_port())
    for node in [source, dynamic]:
        initialize_node(node, env, parent)
    run(dynamic, env, "federation", "set-mode", "sync")
    configure_policy(parent / dynamic.name / "cortex.md", True)

    first = add_trace(source, env, "before-add", "created before dynamic peer add")
    dynamic_manifest = parent / dynamic.name / "cortex.md"
    dynamic_log = root / f"{dynamic.name}.log"

    try:
        start(source, env)
        start(dynamic, env)

        run(
            dynamic,
            env,
            "federation",
            "add-peer",
            source.name,
            f"http://127.0.0.1:{source.port}",
        )
        wait_until(
            "worker start after live peer addition",
            lambda: log_contains(
                dynamic_log, f"federation worker started for peer {source.name}"
            ),
        )
        wait_until(
            "trace sync after live peer addition",
            lambda: has_trace(dynamic, env, first, "created before dynamic peer add"),
        )
        cursor_before_removal = state(
            database(root, dynamic), f"peer:{source.name}:last_event"
        )
        assert cursor_before_removal

        remove_peer(dynamic_manifest, source.name)
        wait_until(
            "worker retirement after live peer removal",
            lambda: log_contains(
                dynamic_log, f"federation worker stopped for peer {source.name}"
            ),
        )
        second = add_trace(
            source, env, "while-removed", "created while dynamic peer was removed"
        )
        time.sleep(1)
        assert not has_trace(dynamic, env, second)
        assert (
            state(database(root, dynamic), f"peer:{source.name}:last_event")
            == cursor_before_removal
        )

        run(
            dynamic,
            env,
            "federation",
            "add-peer",
            source.name,
            f"http://127.0.0.1:{source.port}",
        )
        wait_until(
            "pending trace sync after live peer re-addition",
            lambda: has_trace(
                dynamic,
                env,
                second,
                "created while dynamic peer was removed",
            ),
        )
        cursor_after_readdition = state(
            database(root, dynamic), f"peer:{source.name}:last_event"
        )
        assert cursor_after_readdition > cursor_before_removal
    finally:
        if not all(stop(node) for node in [dynamic, source]):
            raise RuntimeError("one or more dynamic-federation servers did not stop")

    log = dynamic_log.read_text()
    assert log.count(f"federation worker started for peer {source.name}") == 2
    assert log.count(f"federation worker stopped for peer {source.name}") == 1


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")
    with tempfile.TemporaryDirectory(prefix="noema-rust-dynamic-federation-") as directory:
        exercise(args.go.resolve(), args.rust.resolve(), Path(directory))
    print("Rust dynamic federation: add, remove, cursor preservation, re-add PASS")


if __name__ == "__main__":
    main()
