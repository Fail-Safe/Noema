#!/usr/bin/env python3
"""Compare Go/Rust watcher outcomes under identical filesystem mutations."""

from __future__ import annotations

import argparse
import hashlib
from pathlib import Path
import shutil
import sqlite3
import subprocess
import tempfile
import time

from federation_ring import (
    Node,
    cortex_dir,
    database,
    environment,
    free_port,
    run,
    stop,
    wait_for_port,
    wait_until,
)


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
    if len(boundaries) < 2:
        raise RuntimeError(f"manifest has no closing boundary: {manifest}")
    lines[boundaries[1] : boundaries[1]] = [
        "watch:",
        "  enabled: true",
        "  debounce_ms: 50",
        "  auto_onboard: true",
    ]
    manifest.write_text("\n".join(lines) + "\n")


def add_seed(node: Node, env: dict[str, str]) -> str:
    completed = run(
        node,
        env,
        "add",
        "--title",
        "Watcher Seed",
        "--type",
        "note",
        "--tag",
        "initial",
        "--body",
        "original watcher body",
    )
    return completed.stdout.strip().rsplit(": ", 1)[-1]


def start_watcher(node: Node, env: dict[str, str], root: Path) -> None:
    log_path = root / f"{node.name}.log"
    log = log_path.open("w")
    node.process = subprocess.Popen(
        [
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
        ],
        env=env,
        text=True,
        stdout=subprocess.DEVNULL,
        stderr=log,
    )
    log.close()
    wait_for_port(node)


def scalar(db: Path, query: str, parameters: tuple[object, ...] = ()) -> object:
    with sqlite3.connect(db) as connection:
        row = connection.execute(query, parameters).fetchone()
    return None if row is None else row[0]


def event_count(db: Path, trace_id: str, action: str) -> int:
    return int(
        scalar(
            db,
            "SELECT COUNT(*) FROM events WHERE trace_id=? AND action=?",
            (trace_id, action),
        )
        or 0
    )


def row_state(db: Path, trace_id: str) -> tuple[str, str, str, str] | None:
    with sqlite3.connect(db) as connection:
        row = connection.execute(
            "SELECT title,content_hash,COALESCE(archived_at,''),COALESCE(trashed_at,'') "
            "FROM traces WHERE id=?",
            (trace_id,),
        ).fetchone()
    return None if row is None else tuple(str(value) for value in row)


def replace_title(source: str, title: str) -> str:
    lines = source.splitlines(keepends=True)
    for index, line in enumerate(lines):
        if line.startswith("title:"):
            ending = "\n" if line.endswith("\n") else ""
            lines[index] = f"title: {title}{ending}"
            return "".join(lines)
    raise RuntimeError("trace has no title frontmatter")


def trace_body(source: str) -> str:
    marker = "\n---\n"
    boundary = source.find(marker, 4)
    if boundary < 0:
        raise RuntimeError("trace has no closing frontmatter")
    return source[boundary + len(marker) :].lstrip("\n")


def exercise(node: Node, env: dict[str, str], root: Path) -> dict[str, object]:
    parent = root / "cortexes"
    initialize(node, env, parent)
    directory = cortex_dir(root, node)
    configure(directory / "cortex.md")
    seed_id = add_seed(node, env)
    db = database(root, node)
    active = directory / "traces"
    archive = directory / "archive" / "traces"
    trash = directory / "trash" / "traces"
    seed = active / f"{seed_id}.md"
    start_watcher(node, env, root)

    edited = replace_title(seed.read_text(), "Watcher Seed Edited")
    edited += "\nexternal edit marker\n"
    seed.write_text(edited)
    wait_until(
        f"{node.name} external edit",
        lambda: event_count(db, seed_id, "update") == 1
        and row_state(db, seed_id)[0] == "Watcher Seed Edited",
    )
    if seed.read_text() != edited:
        raise AssertionError(f"{node.name}: watcher rewrote external editor bytes")

    before_burst = event_count(db, seed_id, "update")
    for index in range(5):
        seed.write_text(edited + f"\nburst-{index}\n")
        time.sleep(0.005)
    wait_until(
        f"{node.name} debounced burst",
        lambda: event_count(db, seed_id, "update") >= before_burst + 1,
    )
    time.sleep(0.25)
    if event_count(db, seed_id, "update") != before_burst + 1:
        raise AssertionError(f"{node.name}: write burst emitted multiple updates")

    dropped_id = "20260101-dropped-trace"
    dropped = active / f"{dropped_id}.md"
    dropped.write_text(
        "---\n"
        f"id: {dropped_id}\n"
        "title: Dropped trace\n"
        "type: note\n"
        "author: external\n"
        "created: 2026-01-01T00:00:00Z\n"
        "updated: 2026-01-01T00:00:00Z\n"
        "---\n\n"
        "dropped body\n"
    )
    wait_until(
        f"{node.name} external create",
        lambda: event_count(db, dropped_id, "create") == 1,
    )

    archived = archive / seed.name
    seed.rename(archived)
    wait_until(
        f"{node.name} archive move",
        lambda: row_state(db, seed_id) is not None
        and bool(row_state(db, seed_id)[2]),
    )
    if event_count(db, seed_id, "archive") != 1 or event_count(db, seed_id, "trash"):
        raise AssertionError(f"{node.name}: archive move emitted incorrect state events")
    time.sleep(0.25)
    if event_count(db, seed_id, "trash"):
        raise AssertionError(f"{node.name}: archive source event was misclassified as delete")

    archived.rename(seed)
    wait_until(
        f"{node.name} unarchive move",
        lambda: row_state(db, seed_id) is not None
        and not row_state(db, seed_id)[2],
    )
    time.sleep(0.25)
    if event_count(db, seed_id, "trash"):
        raise AssertionError(f"{node.name}: unarchive emitted a transient trash event")

    atomic_bytes = seed.read_bytes() + b"\natomic save marker\n"
    seed.unlink()
    time.sleep(0.005)
    seed.write_bytes(atomic_bytes)
    wait_until(
        f"{node.name} atomic save update",
        lambda: event_count(db, seed_id, "update") == before_burst + 2,
    )
    if row_state(db, seed_id)[3] or not seed.exists():
        raise AssertionError(f"{node.name}: atomic save was misclassified as trash")

    seed.write_text("frontmatter was wiped but this body must survive\n")
    wait_until(
        f"{node.name} frontmatter heal",
        lambda: seed.read_text().startswith("---\n")
        and "frontmatter was wiped" in trace_body(seed.read_text()),
    )
    if row_state(db, seed_id)[0] != "Watcher Seed Edited":
        raise AssertionError(f"{node.name}: heal lost indexed title")

    raw = active / "Project Brief.md"
    raw.write_text("# Imported Project Brief\n\nSynthetic imported content.\n")
    wait_until(
        f"{node.name} auto-onboard",
        lambda: int(
            scalar(db, "SELECT COUNT(*) FROM traces WHERE title=?", ("Imported Project Brief",))
            or 0
        )
        == 1,
    )
    if raw.exists():
        raise AssertionError(f"{node.name}: onboarding did not remove original filename")
    onboarded_id = str(
        scalar(db, "SELECT id FROM traces WHERE title=?", ("Imported Project Brief",))
    )
    onboarded = active / f"{onboarded_id}.md"
    if "Auto-onboarded from `Project Brief.md`" not in onboarded.read_text():
        raise AssertionError(f"{node.name}: onboarding lost provenance")

    dropped.unlink()
    wait_until(
        f"{node.name} recoverable delete",
        lambda: row_state(db, dropped_id) is not None
        and bool(row_state(db, dropped_id)[3])
        and (trash / dropped.name).exists(),
    )
    if event_count(db, dropped_id, "trash") != 1:
        raise AssertionError(f"{node.name}: external delete did not emit one trash event")
    time.sleep(0.3)
    (trash / dropped.name).unlink()
    wait_until(
        f"{node.name} external purge",
        lambda: row_state(db, dropped_id) is None
        and event_count(db, dropped_id, "purge") == 1,
    )

    locked_events = event_count(db, seed_id, "update")
    with sqlite3.connect(db) as connection:
        connection.execute(
            "UPDATE traces SET source_locked=1,origin='synthetic-remote' WHERE id=?",
            (seed_id,),
        )
    seed.write_text(replace_title(seed.read_text(), "Ignored locked edit"))
    time.sleep(0.5)
    source_lock_ignored = (
        event_count(db, seed_id, "update") == locked_events
        and row_state(db, seed_id)[0] == "Watcher Seed Edited"
    )
    if not source_lock_ignored:
        raise AssertionError(f"{node.name}: source-locked external edit was ingested")

    stop(node)
    return {
        "external_edit_events": event_count(db, seed_id, "update"),
        "archive_events": event_count(db, seed_id, "archive"),
        "unarchive_events": event_count(db, seed_id, "unarchive"),
        "atomic_save_active": row_state(db, seed_id)[3] == "",
        "healed_title": row_state(db, seed_id)[0],
        "onboarded_title": row_state(db, onboarded_id)[0],
        "onboarded_body_sha256": hashlib.sha256(onboarded.read_bytes()).hexdigest(),
        "delete_trash_events": event_count(db, dropped_id, "trash"),
        "delete_purge_events": event_count(db, dropped_id, "purge"),
        "source_lock_ignored": source_lock_ignored,
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")

    with tempfile.TemporaryDirectory(prefix="noema-rust-watch-") as directory:
        root = Path(directory)
        (root / "home").mkdir()
        (root / "config").mkdir()
        env = environment(root)
        nodes = {
            "go": Node("go-watch", args.go.resolve(), False, free_port()),
            "rust": Node("rust-watch", args.rust.resolve(), True, free_port()),
        }
        try:
            reports = {
                label: exercise(node, env, root)
                for label, node in nodes.items()
            }
        except Exception:
            for log_path in sorted(root.glob("*.log")):
                print(f"--- {log_path.name} ---")
                print(log_path.read_text())
            raise
        finally:
            for node in nodes.values():
                stop(node)
            shutil.rmtree(root / "runtime", ignore_errors=True)

    comparable_keys = [
        "external_edit_events",
        "archive_events",
        "unarchive_events",
        "atomic_save_active",
        "healed_title",
        "onboarded_title",
        "delete_trash_events",
        "delete_purge_events",
        "source_lock_ignored",
    ]
    for key in comparable_keys:
        if reports["go"][key] != reports["rust"][key]:
            raise AssertionError(
                f"watcher parity mismatch for {key}: "
                f"Go={reports['go'][key]!r}, Rust={reports['rust'][key]!r}"
            )
    print("ok - Go/Rust external edit and metadata reindex")
    print("ok - Go/Rust per-path debounce")
    print("ok - Go/Rust create, archive, unarchive, delete, and purge events")
    print("ok - Go/Rust atomic-save guard")
    print("ok - Go/Rust malformed-file healing and raw Markdown onboarding")
    print("ok - Go/Rust source-lock enforcement for external edits")
    print("PASS: mixed watcher parity fixture")


if __name__ == "__main__":
    main()
