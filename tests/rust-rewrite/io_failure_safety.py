#!/usr/bin/env python3
"""Compare safe failure before Markdown, lifecycle, and manifest writes."""

from __future__ import annotations

import argparse
from pathlib import Path
import sqlite3
import tempfile

from federation_ring import Node, add_trace, environment, free_port, run
from federation_tls import initialize_node


def snapshot(database: Path, trace_id: str) -> tuple[object, ...]:
    with sqlite3.connect(database) as connection:
        row = connection.execute(
            "SELECT title,updated_at,content_hash,archived_at,trashed_at "
            "FROM traces WHERE id=?",
            (trace_id,),
        ).fetchone()
        events = connection.execute(
            "SELECT count(*) FROM events WHERE trace_id=?", (trace_id,)
        ).fetchone()
    assert row is not None and events is not None
    return (*row, int(events[0]))


def exercise(binary: Path, rust: bool, root: Path) -> None:
    implementation = "rust" if rust else "go"
    env = environment(root)
    parent = root / "cortexes"
    node = Node(f"{implementation}-io", binary, rust, free_port())
    initialize_node(node, env, parent)
    trace_id = add_trace(node, env, "stable trace", "original body")
    cortex = parent / node.name
    database = cortex / "db" / "noema.db"
    trace_path = cortex / "traces" / f"{trace_id}.md"
    manifest = cortex / "cortex.md"
    archive = cortex / "archive" / "traces"

    original_file = trace_path.read_bytes()
    original_row = snapshot(database, trace_id)
    trace_path.chmod(0o400)
    try:
        append = run(
            node,
            env,
            "append",
            trace_id,
            "--content",
            "must not persist",
            check=False,
        )
    finally:
        trace_path.chmod(0o640)
    assert append.returncode != 0
    assert trace_path.read_bytes() == original_file
    assert snapshot(database, trace_id) == original_row

    archive.chmod(0o500)
    try:
        archived = run(node, env, "archive", trace_id, check=False)
    finally:
        archive.chmod(0o700)
    assert archived.returncode != 0
    assert trace_path.read_bytes() == original_file
    assert snapshot(database, trace_id) == original_row

    original_manifest = manifest.read_bytes()
    manifest.chmod(0o400)
    try:
        changed = run(
            node,
            env,
            "federation",
            "set-mode",
            "publish",
            check=False,
        )
    finally:
        manifest.chmod(0o640)
    assert changed.returncode != 0
    assert manifest.read_bytes() == original_manifest

    with sqlite3.connect(database) as connection:
        integrity = connection.execute("PRAGMA integrity_check").fetchone()
    assert integrity == ("ok",)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")
    with tempfile.TemporaryDirectory(prefix="noema-go-io-failure-") as directory:
        exercise(args.go.resolve(), False, Path(directory))
    with tempfile.TemporaryDirectory(prefix="noema-rust-io-failure-") as directory:
        exercise(args.rust.resolve(), True, Path(directory))
    print("Go/Rust I/O failure safety: trace, lifecycle, manifest, integrity PASS")


if __name__ == "__main__":
    main()
