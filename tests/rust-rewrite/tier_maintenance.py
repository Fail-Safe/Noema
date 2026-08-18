#!/usr/bin/env python3
"""Compare Go/Rust cron graduation and idle-triggered maintenance."""

from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
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
    start,
    stop,
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


def add_trace(
    node: Node, env: dict[str, str], title: str, trace_type: str = "fact"
) -> str:
    result = run(
        node,
        env,
        "add",
        "--title",
        title,
        "--type",
        trace_type,
        "--author",
        "test-agent",
        "--tag",
        "tier-maintenance-test",
        "--body",
        f"fixture {title}",
    )
    return result.stdout.strip().rsplit(": ", 1)[-1]


def configure(manifest: Path, block: list[str]) -> None:
    lines = manifest.read_text().splitlines()
    boundaries = [index for index, line in enumerate(lines) if line == "---"]
    if len(boundaries) < 2:
        raise RuntimeError(f"manifest has no closing frontmatter boundary: {manifest}")
    lines[boundaries[1] : boundaries[1]] = ["consolidation:", *block]
    manifest.write_text("\n".join(lines) + "\n")


def manifest_id(manifest: Path) -> str:
    for line in manifest.read_text().splitlines():
        if line.startswith("id: "):
            return line.split(": ", 1)[1].strip(" '\"")
    raise RuntimeError(f"manifest has no cortex id: {manifest}")


def set_usage(
    database_path: Path,
    cortex_id: str,
    trace_id: str,
    reads: int = 0,
    modifies: int = 0,
    search_hits: int = 0,
) -> None:
    now = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    with sqlite3.connect(database_path) as connection:
        connection.execute(
            "INSERT INTO trace_usage("
            "trace_id,peer_cortex_id,read_count,modify_count,search_hit_count,"
            "last_read_at,updated_at) VALUES (?,?,?,?,?,?,?) "
            "ON CONFLICT(trace_id,peer_cortex_id) DO UPDATE SET "
            "read_count=excluded.read_count,"
            "modify_count=excluded.modify_count,"
            "search_hit_count=excluded.search_hit_count,"
            "last_read_at=excluded.last_read_at,"
            "updated_at=excluded.updated_at",
            (trace_id, cortex_id, reads, modifies, search_hits, now, now),
        )


def tiers(database_path: Path) -> dict[str, str]:
    with sqlite3.connect(database_path) as connection:
        return {
            str(title): str(tier)
            for title, tier in connection.execute("SELECT title,tier FROM traces")
        }


def promote_events(database_path: Path, trace_id: str) -> list[dict[str, str]]:
    with sqlite3.connect(database_path) as connection:
        rows = connection.execute(
            "SELECT data FROM events WHERE trace_id=? AND action='promote' ORDER BY id",
            (trace_id,),
        ).fetchall()
    return [json.loads(str(row[0])) for row in rows]


def frontmatter_tier(cortex: Path, trace_id: str) -> str:
    for line in (cortex / "traces" / f"{trace_id}.md").read_text().splitlines():
        if line.startswith("tier: "):
            return line.split(": ", 1)[1].strip(" '\"")
    return ""


def graduation_scenario(
    node: Node, env: dict[str, str], root: Path
) -> dict[str, str]:
    parent = root / "cortexes"
    initialize(node, env, parent)
    manifest = cortex_dir(root, node) / "cortex.md"
    configure(
        manifest,
        [
            "  enabled: true",
            "  cron: 00:00",
            "  graduation:",
            "    enabled: true",
            "    min_age_days: 1",
            "    min_read_count: 3",
            "    require_unmodified: true",
        ],
    )
    specs = {
        "durable-reads": "fact",
        "durable-search": "fact",
        "too-young": "fact",
        "modified": "fact",
        "downvoted": "fact",
        "preference": "preference",
        "archived": "fact",
    }
    ids = {
        title: add_trace(node, env, title, trace_type)
        for title, trace_type in specs.items()
    }
    for trace_id in ids.values():
        run(node, env, "memory", "promote", trace_id, "--to", "mid")

    cortex_id = manifest_id(manifest)
    set_usage(database(root, node), cortex_id, ids["durable-reads"], reads=3)
    set_usage(database(root, node), cortex_id, ids["durable-search"], search_hits=3)
    set_usage(database(root, node), cortex_id, ids["too-young"], reads=3)
    set_usage(database(root, node), cortex_id, ids["modified"], reads=3, modifies=1)
    set_usage(database(root, node), cortex_id, ids["downvoted"], reads=3)
    set_usage(database(root, node), cortex_id, ids["preference"], reads=99)
    set_usage(database(root, node), cortex_id, ids["archived"], reads=3)
    old = (datetime.now(timezone.utc) - timedelta(days=2)).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )
    with sqlite3.connect(database(root, node)) as connection:
        connection.execute(
            "UPDATE traces SET created_at=? WHERE id!=?", (old, ids["too-young"])
        )
        connection.execute(
            "UPDATE traces SET tier_votes=-1 WHERE id=?", (ids["downvoted"],)
        )
    run(node, env, "archive", ids["archived"])

    expected_long = {"durable-reads", "durable-search"}
    try:
        start(node, env)
        wait_until(
            f"cron graduation on {node.name}",
            lambda: {
                title for title, tier in tiers(database(root, node)).items() if tier == "long"
            }
            == expected_long,
        )
        assert stop(node)
        result = tiers(database(root, node))
        assert {title for title, tier in result.items() if tier == "long"} == expected_long
        for title in expected_long:
            assert frontmatter_tier(cortex_dir(root, node), ids[title]) == "long"
            assert promote_events(database(root, node), ids[title]) == [
                {"from": "short", "to": "mid"},
                {"from": "mid", "to": "long"},
            ]

        start(node, env)
        time.sleep(0.5)
        for title in expected_long:
            assert len(promote_events(database(root, node), ids[title])) == 2
        return result
    finally:
        if not stop(node):
            raise RuntimeError(f"{node.name} did not stop gracefully")


def idle_scenario(node: Node, env: dict[str, str], root: Path) -> dict[str, str]:
    parent = root / "cortexes"
    initialize(node, env, parent)
    manifest = cortex_dir(root, node) / "cortex.md"
    configure(
        manifest,
        [
            "  enabled: true",
            "  idle_minutes: 1",
            "  graduation:",
            "    enabled: false",
        ],
    )
    hot = add_trace(node, env, "idle-hot")
    add_trace(node, env, "idle-cold")
    old = (datetime.now(timezone.utc) - timedelta(minutes=2)).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )
    with sqlite3.connect(database(root, node)) as connection:
        connection.execute("UPDATE traces SET tier_votes=1 WHERE id=?", (hot,))
        connection.execute("UPDATE events SET timestamp=?", (old,))

    try:
        start(node, env)
        wait_until(
            f"idle promotion on {node.name}",
            lambda: tiers(database(root, node)).get("idle-hot") == "mid",
        )
        assert stop(node)
        assert promote_events(database(root, node), hot) == [
            {"from": "short", "to": "mid"}
        ]
        log = (root / f"{node.name}.log").read_text()
        assert "trigger=idle" in log

        start(node, env)
        time.sleep(0.5)
        assert len(promote_events(database(root, node), hot)) == 1
        return tiers(database(root, node))
    finally:
        if not stop(node):
            raise RuntimeError(f"{node.name} did not stop gracefully")


def exercise(go: Path, rust: Path, root: Path) -> None:
    env = environment(root)
    go_graduation = graduation_scenario(
        Node("go-graduation", go, False, free_port()), env, root
    )
    rust_graduation = graduation_scenario(
        Node("rust-graduation", rust, True, free_port()), env, root
    )
    assert rust_graduation == go_graduation

    go_idle = idle_scenario(Node("go-idle", go, False, free_port()), env, root)
    rust_idle = idle_scenario(Node("rust-idle", rust, True, free_port()), env, root)
    assert rust_idle == go_idle


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")
    with tempfile.TemporaryDirectory(prefix="noema-rust-tier-maintenance-") as directory:
        exercise(args.go.resolve(), args.rust.resolve(), Path(directory))
    print("Go/Rust tier maintenance: cron graduation, idle cadence, idempotency PASS")


if __name__ == "__main__":
    main()
