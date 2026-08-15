#!/usr/bin/env python3
"""Compare live Go/Rust threshold-triggered heuristic promotion."""

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
    add_trace,
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


def configure(manifest: Path) -> None:
    lines = manifest.read_text().splitlines()
    boundaries = [index for index, line in enumerate(lines) if line == "---"]
    if len(boundaries) < 2:
        raise RuntimeError(f"manifest has no closing frontmatter boundary: {manifest}")
    lines[boundaries[1] : boundaries[1]] = [
        "consolidation:",
        "  enabled: true",
        "  threshold_short: 1",
        "  window_hours: 24",
    ]
    manifest.write_text("\n".join(lines) + "\n")


def manifest_id(manifest: Path) -> str:
    for line in manifest.read_text().splitlines():
        if line.startswith("id: "):
            return line.split(": ", 1)[1].strip(" '\"")
    raise RuntimeError(f"manifest has no cortex id: {manifest}")


def seed_signals(
    database_path: Path, cortex_id: str, ids: dict[str, str]
) -> None:
    now = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    old = (datetime.now(timezone.utc) - timedelta(hours=25)).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )
    usage = [
        (ids["five-reads"], 5, 0, 0),
        (ids["single-source-search"], 0, 0, 10),
        (ids["source-search-modified"], 0, 1, 3),
        (ids["outside-window"], 5, 0, 0),
        (ids["archived-five-reads"], 5, 0, 0),
    ]
    with sqlite3.connect(database_path) as connection:
        for trace_id, reads, modifies, search_hits in usage:
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
        connection.execute(
            "UPDATE traces SET tier_votes=1 WHERE id=?", (ids["one-vote"],)
        )
        connection.executemany(
            "INSERT INTO trace_lineage(trace_id,derived_from) VALUES (?,?)",
            [
                (ids["lineage-child-a"], ids["lineage-parent"]),
                (ids["lineage-child-b"], ids["lineage-parent"]),
                (ids["single-source-search"], ids["cold"]),
                (ids["source-search-modified"], ids["outside-window"]),
            ],
        )
        connection.execute(
            "UPDATE traces SET created_at=? WHERE id=?",
            (old, ids["outside-window"]),
        )


def tier_map(database_path: Path) -> dict[str, str]:
    with sqlite3.connect(database_path) as connection:
        return {
            str(title): str(tier)
            for title, tier in connection.execute("SELECT title,tier FROM traces")
        }


def promotion_events(database_path: Path) -> list[tuple[str, dict[str, str]]]:
    with sqlite3.connect(database_path) as connection:
        rows = connection.execute(
            "SELECT t.title,e.data FROM events e JOIN traces t ON t.id=e.trace_id "
            "WHERE e.action='promote' ORDER BY t.title"
        ).fetchall()
    return [(str(title), json.loads(str(data))) for title, data in rows]


def frontmatter_tier(cortex: Path, trace_id: str) -> str:
    for line in (cortex / "traces" / f"{trace_id}.md").read_text().splitlines():
        if line.startswith("tier: "):
            return line.split(": ", 1)[1].strip(" '\"")
    return ""


def exercise_node(node: Node, env: dict[str, str], root: Path) -> dict[str, str]:
    parent = root / "cortexes"
    initialize(node, env, parent)
    manifest = cortex_dir(root, node) / "cortex.md"
    configure(manifest)
    titles = [
        "five-reads",
        "one-vote",
        "lineage-parent",
        "lineage-child-a",
        "lineage-child-b",
        "single-source-search",
        "source-search-modified",
        "outside-window",
        "archived-five-reads",
        "cold",
    ]
    ids = {title: add_trace(node, env, title, f"fixture {title}") for title in titles}
    seed_signals(database(root, node), manifest_id(manifest), ids)
    run(node, env, "archive", ids["archived-five-reads"])

    try:
        start(node, env)
        try:
            wait_until(
                f"four heuristic promotions on {node.name}",
                lambda: len(promotion_events(database(root, node))) == 4,
            )
        except RuntimeError as error:
            log_path = root / f"{node.name}.log"
            log = log_path.read_text() if log_path.exists() else ""
            raise RuntimeError(
                f"{error}; promotions={promotion_events(database(root, node))}; "
                f"log={log}"
            ) from error
        assert stop(node)

        expected_mid = {
            "five-reads",
            "one-vote",
            "lineage-parent",
            "source-search-modified",
        }
        tiers = tier_map(database(root, node))
        assert {title for title, tier in tiers.items() if tier == "mid"} == expected_mid
        assert all(
            frontmatter_tier(cortex_dir(root, node), ids[title]) == "mid"
            for title in expected_mid
        )
        events = promotion_events(database(root, node))
        assert {title for title, _ in events} == expected_mid
        assert all(data == {"from": "short", "to": "mid"} for _, data in events)

        start(node, env)
        time.sleep(0.5)
        assert len(promotion_events(database(root, node))) == 4
        return tiers
    finally:
        if not stop(node):
            raise RuntimeError(f"{node.name} did not stop gracefully")


def exercise(go: Path, rust: Path, root: Path) -> None:
    env = environment(root)
    go_node = Node("go-promotion", go, False, free_port())
    rust_node = Node("rust-promotion", rust, True, free_port())
    go_tiers = exercise_node(go_node, env, root)
    rust_tiers = exercise_node(rust_node, env, root)
    assert rust_tiers == go_tiers


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")
    with tempfile.TemporaryDirectory(prefix="noema-rust-promotion-") as directory:
        exercise(args.go.resolve(), args.rust.resolve(), Path(directory))
    print("Go/Rust heuristic promotion: scoring, filtering, events, idempotency PASS")


if __name__ == "__main__":
    main()
