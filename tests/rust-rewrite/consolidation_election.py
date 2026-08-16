#!/usr/bin/env python3
"""Exercise Go/Rust consolidation-rank exchange and deterministic failover."""

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
    configure_policy,
    cortex_dir,
    database,
    environment,
    free_port,
    run,
    start,
    stop,
    wait_until,
)


class ModelHandler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        if self.path == "/models":
            time.sleep(self.server.probe_delay)
            body = b'{"data":[]}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_error(404)

    def log_message(self, _format: str, *_arguments: object) -> None:
        return


class ReusableHTTPServer(ThreadingHTTPServer):
    allow_reuse_address = True
    probe_delay = 0.0


class ModelEndpoint:
    def __init__(self, port: int):
        self.port = port
        self.probe_delay = 0.0
        self.server: ReusableHTTPServer | None = None
        self.thread: threading.Thread | None = None

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    def start(self) -> None:
        self.server = ReusableHTTPServer(("127.0.0.1", self.port), ModelHandler)
        self.server.probe_delay = self.probe_delay
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def stop(self) -> None:
        if self.server is None:
            return
        self.server.shutdown()
        self.server.server_close()
        assert self.thread is not None
        self.thread.join(timeout=3)
        self.server = None
        self.thread = None

    def set_probe_delay(self, delay: float) -> None:
        self.probe_delay = delay
        if self.server is not None:
            self.server.probe_delay = delay


def initialize_node(node: Node, env: dict[str, str], parent: Path) -> None:
    subprocess.run(
        [str(node.binary), "init", "--name", node.name, "--path", str(parent)],
        env=env,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    run(node, env, "keygen")


def insert_consolidation(manifest: Path, endpoint: str, rust: bool) -> None:
    lines = manifest.read_text().splitlines()
    boundaries = [index for index, line in enumerate(lines) if line == "---"]
    if len(boundaries) < 2:
        raise RuntimeError(f"manifest has no closing frontmatter boundary: {manifest}")
    block = [
        "consolidation:",
        "  enabled: true",
        "  threshold_short: 1000000",
        "  llm_enabled: true",
        f"  local_llm_endpoint: {endpoint}",
    ]
    if rust:
        block.append("  watchdog_timeout: 1s")
    lines[boundaries[1]:boundaries[1]] = block
    manifest.write_text("\n".join(lines) + "\n")


def manifest_id(manifest: Path) -> str:
    for line in manifest.read_text().splitlines():
        if line.startswith("id: "):
            return line.split(": ", 1)[1].strip(" '\"")
    raise RuntimeError(f"manifest has no cortex id: {manifest}")


def state_rank(database_path: Path, key: str) -> dict[str, object]:
    with sqlite3.connect(database_path) as connection:
        row = connection.execute(
            "SELECT value FROM federation_state WHERE key=?", (key,)
        ).fetchone()
    if row is None or not row[0]:
        return {}
    return json.loads(str(row[0]))


def set_rank(database_path: Path, key: str, entry: dict[str, object]) -> None:
    with sqlite3.connect(database_path) as connection:
        connection.execute(
            "INSERT INTO federation_state(key,value) VALUES (?,?) "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (key, json.dumps(entry, separators=(",", ":"))),
        )


def valid_rank(entry: dict[str, object], cortex_id: str) -> bool:
    return (
        entry.get("cortex_id") == cortex_id
        and 1 <= int(entry.get("rank", 0)) <= 99
        and bool(entry.get("observed_at"))
    )


def rust_status(node: Node, env: dict[str, str]) -> dict[str, object]:
    return json.loads(run(node, env, "federation", "status").stdout)


def coordination_windows(
    database_path: Path, action: str, cortex_id: str
) -> set[str]:
    with sqlite3.connect(database_path) as connection:
        return {
            str(row[0])
            for row in connection.execute(
                "SELECT trace_id FROM events WHERE action=? AND cortex_id=?",
                (action, cortex_id),
            )
        }


def watchdog_fail(
    database_path: Path, window_id: str, emitter_id: str, winner_id: str
) -> bool:
    with sqlite3.connect(database_path) as connection:
        row = connection.execute(
            "SELECT data FROM events "
            "WHERE action='consolidation_fail' AND trace_id=? AND cortex_id=? "
            "ORDER BY id DESC LIMIT 1",
            (window_id, emitter_id),
        ).fetchone()
    if row is None:
        return False
    data = json.loads(str(row[0]))
    return (
        data.get("window_id") == window_id
        and data.get("cortex_id") == winner_id
        and data.get("reason") == "watchdog_expired"
    )


def trace_tier(database_path: Path, trace_id: str) -> str:
    with sqlite3.connect(database_path) as connection:
        row = connection.execute(
            "SELECT tier FROM traces WHERE id=?", (trace_id,)
        ).fetchone()
    return "" if row is None else str(row[0])


def action_count(database_path: Path, trace_id: str, action: str) -> int:
    with sqlite3.connect(database_path) as connection:
        row = connection.execute(
            "SELECT count(*) FROM events WHERE trace_id=? AND action=?",
            (trace_id, action),
        ).fetchone()
    assert row is not None
    return int(row[0])


def set_threshold(manifest: Path, threshold: int) -> None:
    source = manifest.read_text()
    updated = source.replace(
        "  threshold_short: 1000000", f"  threshold_short: {threshold}", 1
    )
    if updated == source:
        raise RuntimeError(f"could not update consolidation threshold: {manifest}")
    manifest.write_text(updated)


def exercise(go: Path, rust: Path, root: Path) -> None:
    env = environment(root)
    parent = root / "cortexes"
    go_endpoint = ModelEndpoint(free_port())
    rust_endpoint = ModelEndpoint(free_port())
    go_endpoint.start()
    rust_endpoint.start()
    peer_a = Node("peer-a", go, False, free_port())
    peer_b = Node("peer-b", rust, True, free_port())
    nodes = [peer_a, peer_b]

    for node in nodes:
        initialize_node(node, env, parent)
    for node, peer, endpoint in [
        (peer_a, peer_b, go_endpoint),
        (peer_b, peer_a, rust_endpoint),
    ]:
        run(
            node,
            env,
            "federation",
            "add-peer",
            peer.name,
            f"http://127.0.0.1:{peer.port}",
        )
        manifest = cortex_dir(root, node) / "cortex.md"
        configure_policy(manifest, node.rust)
        insert_consolidation(manifest, endpoint.url, node.rust)

    identities = {
        node.name: manifest_id(cortex_dir(root, node) / "cortex.md") for node in nodes
    }

    try:
        for node in nodes:
            start(node, env)
        for node in nodes:
            wait_until(
                f"eligible local rank on {node.name}",
                lambda n=node: valid_rank(
                    state_rank(database(root, n), "consolidation:rank"),
                    identities[n.name],
                ),
            )

        observed = (datetime.now(timezone.utc) - timedelta(seconds=5)).isoformat().replace(
            "+00:00", "Z"
        )
        fixed = {
            peer_a.name: {
                "cortex_id": identities[peer_a.name],
                "rank": 40,
                "observed_at": observed,
            },
            peer_b.name: {
                "cortex_id": identities[peer_b.name],
                "rank": 80,
                "observed_at": observed,
            },
        }
        for node in nodes:
            set_rank(database(root, node), "consolidation:rank", fixed[node.name])
        for node, peer in [(peer_a, peer_b), (peer_b, peer_a)]:
            wait_until(
                f"{peer.name} rank mirrored on {node.name}",
                lambda n=node, p=peer: state_rank(
                    database(root, n), f"peer:{p.name}:consolidation_rank"
                )
                == fixed[p.name],
            )

        status = rust_status(peer_b, env)
        assert status["consolidation"]["winner"] == identities[peer_b.name]
        assert status["consolidation"]["should_run_here"] is True

        assert stop(peer_b)
        rust_endpoint.stop()
        start(peer_b, env)
        wait_until(
            "Rust endpoint failure demotes its local rank",
            lambda: state_rank(database(root, peer_b), "consolidation:rank").get("rank")
            == 0,
        )
        wait_until(
            "Go observes the Rust rank demotion",
            lambda: state_rank(
                database(root, peer_a), f"peer:{peer_b.name}:consolidation_rank"
            ).get("rank")
            == 0,
        )
        status = rust_status(peer_b, env)
        assert status["consolidation"]["winner"] == identities[peer_a.name]
        assert status["consolidation"]["should_run_here"] is False

        assert stop(peer_a)
        set_threshold(cortex_dir(root, peer_a) / "cortex.md", 1)
        add_trace(peer_a, env, "election-a", "first election payload")
        add_trace(peer_a, env, "election-b", "second election payload")
        set_rank(database(root, peer_a), "consolidation:rank", fixed[peer_a.name])
        go_endpoint.set_probe_delay(2.0)
        start(peer_a, env)
        wait_until(
            "signed Go coordination claim reaches Rust",
            lambda: bool(
                coordination_windows(
                    database(root, peer_b),
                    "consolidation_claim",
                    identities[peer_a.name],
                )
            ),
        )
        wait_until(
            "signed Go coordination success reaches Rust",
            lambda: bool(
                coordination_windows(
                    database(root, peer_b),
                    "consolidation_claim",
                    identities[peer_a.name],
                )
                & coordination_windows(
                    database(root, peer_b),
                    "consolidation_success",
                    identities[peer_a.name],
                )
            ),
        )
        windows = coordination_windows(
            database(root, peer_b),
            "consolidation_success",
            identities[peer_a.name],
        )
        watchdog_window = sorted(windows)[-1]
        with sqlite3.connect(database(root, peer_b)) as connection:
            for window in windows:
                assert (
                    connection.execute(
                        "SELECT count(*) FROM traces WHERE id=?", (window,)
                    ).fetchone()[0]
                    == 0
                )

        set_rank(database(root, peer_a), "consolidation:rank", fixed[peer_a.name])
        wait_until(
            "Rust observes the fixed post-election Go rank",
            lambda: state_rank(
                database(root, peer_b), f"peer:{peer_a.name}:consolidation_rank"
            )
            == fixed[peer_a.name],
        )

        assert stop(peer_b)
        with sqlite3.connect(database(root, peer_b)) as connection:
            removed = connection.execute(
                "DELETE FROM events WHERE "
                "action IN ('consolidation_success','consolidation_fail') "
                "AND trace_id=?",
                (watchdog_window,),
            ).rowcount
            remaining = connection.execute(
                "SELECT count(*) FROM events WHERE "
                "action IN ('consolidation_success','consolidation_fail') "
                "AND trace_id=?",
                (watchdog_window,),
            ).fetchone()[0]
        assert removed >= 1
        assert remaining == 0
        time.sleep(2)
        rust_endpoint.start()
        start(peer_b, env)
        wait_until(
            "Rust watchdog closes the orphaned Go claim",
            lambda: watchdog_fail(
                database(root, peer_b),
                watchdog_window,
                identities[peer_b.name],
                identities[peer_a.name],
            ),
        )
        wait_until(
            "signed Rust watchdog closure reaches Go",
            lambda: watchdog_fail(
                database(root, peer_a),
                watchdog_window,
                identities[peer_b.name],
                identities[peer_a.name],
            ),
        )
        wait_until(
            "Rust endpoint recovery restores eligibility",
            lambda: valid_rank(
                state_rank(database(root, peer_b), "consolidation:rank"),
                identities[peer_b.name],
            ),
        )
        set_rank(database(root, peer_b), "consolidation:rank", fixed[peer_b.name])
        set_rank(database(root, peer_a), "consolidation:rank", fixed[peer_a.name])
        wait_until(
            "Go observes the recovered Rust rank",
            lambda: state_rank(
                database(root, peer_a), f"peer:{peer_b.name}:consolidation_rank"
            )
            == fixed[peer_b.name],
        )
        wait_until(
            "Rust observes the stable recovered Go rank",
            lambda: state_rank(
                database(root, peer_b), f"peer:{peer_a.name}:consolidation_rank"
            )
            == fixed[peer_a.name],
        )
        status = rust_status(peer_b, env)
        assert status["consolidation"]["winner"] == identities[peer_b.name], status

        assert stop(peer_b)
        assert stop(peer_a)
        set_threshold(cortex_dir(root, peer_b) / "cortex.md", 1)
        promoted_id = add_trace(
            peer_b, env, "rust-threshold-promotion", "real gated pass payload"
        )
        with sqlite3.connect(database(root, peer_b)) as connection:
            connection.execute(
                "UPDATE traces SET tier_votes=1 WHERE id=?", (promoted_id,)
            )
        set_rank(database(root, peer_b), "consolidation:rank", fixed[peer_b.name])
        set_rank(
            database(root, peer_b),
            f"peer:{peer_a.name}:consolidation_rank",
            fixed[peer_a.name],
        )
        rust_endpoint.set_probe_delay(2.0)
        start(peer_b, env)
        wait_until(
            "Rust winner runs the real gated promotion pass",
            lambda: trace_tier(database(root, peer_b), promoted_id) == "mid"
            and action_count(database(root, peer_b), promoted_id, "promote") == 1,
        )
        wait_until(
            "Rust records its gated claim and success locally",
            lambda: bool(
                coordination_windows(
                    database(root, peer_b),
                    "consolidation_claim",
                    identities[peer_b.name],
                )
                & coordination_windows(
                    database(root, peer_b),
                    "consolidation_success",
                    identities[peer_b.name],
                )
            ),
        )
        start(peer_a, env)
        wait_until(
            "Rust promotion replays to Go",
            lambda: trace_tier(database(root, peer_a), promoted_id) == "mid"
            and action_count(database(root, peer_a), promoted_id, "promote") == 1,
        )
        wait_until(
            "Rust gated claim and success replay to Go",
            lambda: bool(
                coordination_windows(
                    database(root, peer_a),
                    "consolidation_claim",
                    identities[peer_b.name],
                )
                & coordination_windows(
                    database(root, peer_a),
                    "consolidation_success",
                    identities[peer_b.name],
                )
            ),
        )
    finally:
        graceful = [stop(node) for node in nodes]
        go_endpoint.stop()
        rust_endpoint.stop()
        if not all(graceful):
            raise RuntimeError("one or more consolidation test servers did not stop")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")
    with tempfile.TemporaryDirectory(prefix="noema-rust-election-") as directory:
        exercise(args.go.resolve(), args.rust.resolve(), Path(directory))
    print(
        "mixed consolidation election: rank exchange, failover, coordination, "
        "watchdog, recovery, gated promotion PASS"
    )


if __name__ == "__main__":
    main()
