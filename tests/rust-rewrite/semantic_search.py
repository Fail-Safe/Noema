#!/usr/bin/env python3
"""Compare Go/Rust semantic indexing and ranking against a deterministic endpoint."""

from __future__ import annotations

import argparse
from contextlib import contextmanager
import json
import math
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import re
import sqlite3
import struct
import subprocess
import tempfile
import threading
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
)


class EmbeddingHandler(BaseHTTPRequestHandler):
    requests: list[dict[str, object]] = []

    def do_POST(self) -> None:
        if self.path != "/v1/embeddings":
            self.send_error(404)
            return
        length = int(self.headers.get("Content-Length", "0"))
        request = json.loads(self.rfile.read(length))
        EmbeddingHandler.requests.append(
            {
                "model": request.get("model"),
                "input": request.get("input"),
                "authorization": self.headers.get("Authorization", ""),
            }
        )
        inputs = request.get("input", [])
        data = [
            {"index": index, "embedding": topic_vector(text)}
            for index, text in enumerate(inputs)
        ]
        data.reverse()  # prove both clients honor a valid index permutation
        body = json.dumps({"data": data}, separators=(",", ":")).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format: str, *_arguments: object) -> None:
        pass


def topic_vector(text: str) -> list[float]:
    lowered = text.lower()
    return [
        0.01 + lowered.count("alpha"),
        0.01 + lowered.count("beta"),
        0.01 + lowered.count("gamma"),
    ]


@contextmanager
def embedding_server() -> tuple[str, list[dict[str, object]]]:
    EmbeddingHandler.requests = []
    server = ThreadingHTTPServer(("127.0.0.1", free_port()), EmbeddingHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}/v1", EmbeddingHandler.requests
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def initialize(node: Node, env: dict[str, str], parent: Path) -> None:
    subprocess.run(
        [str(node.binary), "init", "--name", node.name, "--path", str(parent)],
        env=env,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )


def configure(manifest: Path, endpoint: str, model: str = "topic-v1") -> None:
    lines = manifest.read_text().splitlines()
    boundaries = [index for index, line in enumerate(lines) if line == "---"]
    if len(boundaries) < 2:
        raise RuntimeError(f"manifest has no closing boundary: {manifest}")
    lines[boundaries[1] : boundaries[1]] = [
        "search:",
        "  semantic_enabled: true",
        f"  embedding_endpoint: {endpoint}",
        f"  embedding_model: {model}",
        "  api_key_env: NOEMA_SYNTHETIC_EMBED_KEY",
        "  default_mode: lexical",
        "  hybrid_weight: 0.5",
        "  max_chars: 40",
        "  embed_interval_seconds: 1",
    ]
    manifest.write_text("\n".join(lines) + "\n")


def set_model(manifest: Path, model: str) -> None:
    source = manifest.read_text()
    source, count = re.subn(
        r"(?m)^  embedding_model: .+$", f"  embedding_model: {model}", source
    )
    if count != 1:
        raise RuntimeError("could not replace embedding model")
    manifest.write_text(source)


def set_endpoint(manifest: Path, endpoint: str) -> None:
    source = manifest.read_text()
    source, count = re.subn(
        r"(?m)^  embedding_endpoint: .+$",
        f"  embedding_endpoint: {endpoint}",
        source,
    )
    if count != 1:
        raise RuntimeError("could not replace embedding endpoint")
    manifest.write_text(source)


def mcp_text(
    node: Node, env: dict[str, str], tool: str, arguments: dict[str, object]
) -> str:
    messages = [
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2025-03-26",
                "capabilities": {},
                "clientInfo": {"name": "semantic-test", "version": "1"},
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
    completed = subprocess.run(
        [
            str(node.binary),
            "--cortex",
            node.name,
            "serve",
            "--transport",
            "stdio",
        ],
        env=env,
        input="".join(json.dumps(message) + "\n" for message in messages),
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    responses = [json.loads(line) for line in completed.stdout.splitlines() if line]
    response = next(item for item in responses if item.get("id") == 2)
    if "error" in response or response.get("result", {}).get("isError"):
        raise RuntimeError(f"{node.name} MCP call {tool} failed")
    return str(response["result"]["content"][0]["text"])


def add(node: Node, env: dict[str, str], title: str, body: str) -> str:
    completed = run(
        node,
        env,
        "add",
        "--title",
        title,
        "--type",
        "note",
        "--body",
        body,
    )
    return completed.stdout.strip().rsplit(": ", 1)[-1]


def status_counts(output: str) -> tuple[int, int, int, int]:
    patterns = [
        r"Embeddable traces:\s*(\d+)",
        r"embedded \(up-to-date\):\s*(\d+)",
        r"stale \(changed or other model\):\s*(\d+)",
        r"missing:\s*(\d+)",
    ]
    values = []
    for pattern in patterns:
        match = re.search(pattern, output)
        if match is None:
            raise AssertionError(f"status output did not match {pattern!r}:\n{output}")
        values.append(int(match.group(1)))
    return tuple(values)  # type: ignore[return-value]


def ordered_titles(output: str, ids: dict[str, str]) -> list[str]:
    positions = [
        (output.find(trace_id), title)
        for title, trace_id in ids.items()
        if output.find(trace_id) >= 0
    ]
    return [title for _, title in sorted(positions)]


def embedding_rows(path: Path) -> dict[str, tuple[str, int, bytes, str, str]]:
    with sqlite3.connect(path) as connection:
        rows = connection.execute(
            "SELECT t.title,te.embedding_model,te.dim,te.embedding,te.source_hash,t.content_hash "
            "FROM trace_embeddings te JOIN traces t ON t.id=te.trace_id"
        ).fetchall()
    return {
        str(title): (str(model), int(dim), bytes(blob), str(source_hash), str(content_hash))
        for title, model, dim, blob, source_hash, content_hash in rows
    }


def wait_for_fresh_embedding(path: Path, title: str, timeout: float = 5) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        with sqlite3.connect(path) as connection:
            count = connection.execute(
                "SELECT COUNT(*) FROM traces t JOIN trace_embeddings te ON te.trace_id=t.id "
                "WHERE t.title=? AND te.source_hash=t.content_hash",
                (title,),
            ).fetchone()[0]
        if count == 1:
            return
        time.sleep(0.05)
    raise RuntimeError(f"timed out waiting for automatic embedding of {title}")


def assert_unit_blob(blob: bytes, dim: int) -> None:
    if blob[:1] != b"\x01" or len(blob) != 1 + dim * 4:
        raise AssertionError("embedding codec layout mismatch")
    vector = struct.unpack(f"<{dim}f", blob[1:])
    norm = math.sqrt(sum(value * value for value in vector))
    if not math.isclose(norm, 1.0, rel_tol=1e-6, abs_tol=1e-6):
        raise AssertionError(f"stored embedding is not normalized: norm={norm}")


def exercise(
    node: Node,
    env: dict[str, str],
    root: Path,
    endpoint: str,
    request_log: list[dict[str, object]],
) -> dict[str, object]:
    initialize(node, env, root / "cortexes")
    directory = cortex_dir(root, node)
    configure(directory / "cortex.md", endpoint)
    ids = {
        "Alpha Systems": add(node, env, "Alpha Systems", "alpha alpha network design"),
        "Beta Planning": add(node, env, "Beta Planning", "beta beta roadmap notes"),
        "Gamma Research": add(node, env, "Gamma Research", "gamma gamma paper notes"),
    }

    initial = status_counts(run(node, env, "embeddings", "status").stdout)
    first = run(node, env, "embeddings", "backfill").stdout
    current = status_counts(run(node, env, "embeddings", "status").stdout)
    request_count = len(request_log)
    second = run(node, env, "embeddings", "backfill").stdout
    idempotent_requests = len(request_log) - request_count

    rows = embedding_rows(database(root, node))
    for _, dim, blob, source_hash, content_hash in rows.values():
        assert_unit_blob(blob, dim)
        if source_hash != content_hash:
            raise AssertionError(f"{node.name}: embedding freshness hash mismatch")

    semantic_alpha = ordered_titles(
        run(node, env, "search", "alpha alpha alpha", "--semantic").stdout, ids
    )
    hybrid_alpha = ordered_titles(
        run(node, env, "search", "alpha alpha", "--hybrid").stdout, ids
    )
    semantic_similar = ordered_titles(
        run(node, env, "similar", ids["Alpha Systems"], "--semantic").stdout,
        ids,
    )
    hybrid_similar = ordered_titles(
        run(node, env, "similar", ids["Alpha Systems"], "--hybrid").stdout,
        ids,
    )

    run(node, env, "archive", ids["Beta Planning"])
    beta_default = ordered_titles(
        run(node, env, "search", "beta beta", "--semantic").stdout, ids
    )
    beta_all = ordered_titles(
        run(node, env, "search", "beta beta", "--semantic", "--all").stdout,
        ids,
    )

    with sqlite3.connect(database(root, node)) as connection:
        connection.execute(
            "UPDATE trace_embeddings SET dim=2,embedding=? WHERE trace_id=?",
            (b"\x01" + struct.pack("<2f", 1.0, 0.0), ids["Gamma Research"]),
        )
    dimension_mismatch = ordered_titles(
        run(node, env, "search", "gamma gamma", "--semantic").stdout, ids
    )
    run(node, env, "embeddings", "backfill", "--force")
    with sqlite3.connect(database(root, node)) as connection:
        connection.execute(
            "UPDATE trace_embeddings SET dim=3,embedding=? WHERE trace_id=?",
            (
                b"\x01" + struct.pack("<3f", math.inf, math.inf, math.inf),
                ids["Gamma Research"],
            ),
        )
    non_finite = ordered_titles(
        run(node, env, "search", "gamma gamma", "--semantic").stdout, ids
    )
    run(node, env, "embeddings", "backfill", "--force")

    run(
        node,
        env,
        "append",
        ids["Alpha Systems"],
        "--content",
        "alpha update makes the cached vector stale",
    )
    stale_after_edit = status_counts(run(node, env, "embeddings", "status").stdout)
    edit_backfill = run(node, env, "embeddings", "backfill").stdout

    set_model(directory / "cortex.md", "topic-v2")
    stale_after_model = status_counts(run(node, env, "embeddings", "status").stdout)
    model_backfill = run(node, env, "embeddings", "backfill", "--limit", "2").stdout
    limited = status_counts(run(node, env, "embeddings", "status").stdout)
    run(node, env, "embeddings", "backfill")
    final = status_counts(run(node, env, "embeddings", "status").stdout)

    mcp_semantic_text = mcp_text(
        node,
        env,
        "search_traces",
        {"query": "alpha alpha alpha", "mode": "semantic"},
    )
    mcp_semantic = ordered_titles(mcp_semantic_text, ids)
    mcp_similar_text = mcp_text(
        node,
        env,
        "find_similar_traces",
        {"trace_id": ids["Alpha Systems"], "mode": "hybrid"},
    )
    mcp_similar = ordered_titles(mcp_similar_text, ids)

    node.port = free_port()
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
    node.process = subprocess.Popen(
        arguments,
        env=env,
        text=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        wait_for_port(node)
        add(node, env, "Delta Automatic", "delta automatic embedding maintenance")
        wait_for_fresh_embedding(database(root, node), "Delta Automatic")
        maintainer_embedded = True
    finally:
        stop(node)

    set_endpoint(directory / "cortex.md", "http://127.0.0.1:1/v1")
    fallback = mcp_text(
        node,
        env,
        "search_traces",
        {"query": "alpha", "mode": "semantic"},
    )
    fallback_generic = (
        "temporarily unavailable" in fallback
        and "127.0.0.1" not in fallback
        and ids["Alpha Systems"] in fallback
    )

    requests = list(request_log)
    if not requests or any(
        request["authorization"] != "Bearer synthetic-embedding-token"
        for request in requests
    ):
        raise AssertionError(f"{node.name}: bearer-key indirection was not preserved")
    if any(
        len(text) > 40
        for request in requests
        for text in request.get("input", [])  # type: ignore[union-attr]
    ):
        raise AssertionError(f"{node.name}: max_chars was not enforced")

    return {
        "initial": initial,
        "current": current,
        "stale_after_edit": stale_after_edit,
        "stale_after_model": stale_after_model,
        "limited": limited,
        "final": final,
        "idempotent_requests": idempotent_requests,
        "first_summary": tuple(map(int, re.findall(r"\d+", first)[-2:])),
        "second_summary": tuple(map(int, re.findall(r"\d+", second)[-2:])),
        "edit_summary": tuple(map(int, re.findall(r"\d+", edit_backfill)[-2:])),
        "model_summary": tuple(map(int, re.findall(r"\d+", model_backfill)[-2:])),
        "semantic_alpha": semantic_alpha,
        "hybrid_alpha": hybrid_alpha,
        "semantic_similar": semantic_similar,
        "hybrid_similar": hybrid_similar,
        "beta_default": beta_default,
        "beta_all": beta_all,
        "dimension_mismatch": dimension_mismatch,
        "non_finite": non_finite,
        "mcp_semantic": mcp_semantic,
        "mcp_similar": mcp_similar,
        "mcp_fallback_generic": fallback_generic,
        "maintainer_embedded": maintainer_embedded,
        "rows": rows,
        "request_models": [request["model"] for request in requests],
        "request_inputs": [request["input"] for request in requests],
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")

    with tempfile.TemporaryDirectory(prefix="noema-rust-semantic-") as directory:
        root = Path(directory)
        (root / "home").mkdir()
        (root / "config").mkdir()
        env = environment(root)
        env["NOEMA_SYNTHETIC_EMBED_KEY"] = "synthetic-embedding-token"
        nodes = {
            "go": Node("go-semantic", args.go.resolve(), False, free_port()),
            "rust": Node("rust-semantic", args.rust.resolve(), True, free_port()),
        }
        reports: dict[str, dict[str, object]] = {}
        with embedding_server() as (endpoint, request_log):
            for label, node in nodes.items():
                request_log.clear()
                reports[label] = exercise(node, env, root, endpoint, request_log)

    for key in reports["go"]:
        if reports["go"][key] != reports["rust"][key]:
            raise AssertionError(
                f"semantic parity mismatch for {key}: "
                f"Go={reports['go'][key]!r}, Rust={reports['rust'][key]!r}"
            )
    if reports["go"]["initial"] != (3, 0, 0, 3):
        raise AssertionError(f"unexpected initial coverage: {reports['go']['initial']}")
    if reports["go"]["current"] != (3, 3, 0, 0):
        raise AssertionError(f"unexpected indexed coverage: {reports['go']['current']}")
    if reports["go"]["semantic_alpha"][0] != "Alpha Systems":  # type: ignore[index]
        raise AssertionError("semantic topic ranking did not put alpha first")
    if "Beta Planning" in reports["go"]["beta_default"]:  # type: ignore[operator]
        raise AssertionError("archived trace leaked into default semantic search")
    if "Beta Planning" not in reports["go"]["beta_all"]:  # type: ignore[operator]
        raise AssertionError("archived trace missing from --all semantic search")
    if "Gamma Research" in reports["go"]["dimension_mismatch"]:  # type: ignore[operator]
        raise AssertionError("dimension-mismatched vector was ranked")
    if "Gamma Research" in reports["go"]["non_finite"]:  # type: ignore[operator]
        raise AssertionError("non-finite vector was ranked")

    print("ok - Go/Rust embedding request, auth, batching, and max_chars parity")
    print("ok - Go/Rust codec bytes, normalization, freshness, and idempotency")
    print("ok - Go/Rust edit/model staleness and bounded backfill")
    print("ok - Go/Rust semantic cosine and hybrid RRF ranking")
    print("ok - Go/Rust semantic/hybrid similar and archive visibility")
    print("ok - Go/Rust corrupt dimension and non-finite vector rejection")
    print("ok - Go/Rust MCP semantic routing and generic lexical fallback")
    print("ok - Go/Rust serve-time automatic embedding maintenance")
    print("PASS: deterministic semantic-search parity fixture")


if __name__ == "__main__":
    main()
