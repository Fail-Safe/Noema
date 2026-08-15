#!/usr/bin/env python3
"""Compare the Go/Rust MCP discovery contract and representative tool behavior."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import sqlite3
import subprocess
import tempfile

from federation_ring import Node, cortex_dir, database, environment, free_port, run


DEFAULT_ANNOTATIONS = {
    "destructiveHint": True,
    "idempotentHint": False,
    "openWorldHint": True,
    "readOnlyHint": False,
}


def initialize(node: Node, env: dict[str, str], parent: Path) -> None:
    subprocess.run(
        [str(node.binary), "init", "--name", node.name, "--path", str(parent)],
        env=env,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )


def add_seed(node: Node, env: dict[str, str]) -> str:
    completed = run(
        node,
        env,
        "add",
        "--title",
        "MCP Contract Seed",
        "--type",
        "preference",
        "--tag",
        "user-preference",
        "--body",
        "Synthetic preference body for MCP contract validation.",
    )
    return completed.stdout.strip().rsplit(": ", 1)[-1]


def exchange(
    node: Node,
    env: dict[str, str],
    method: str,
    params: dict[str, object],
) -> dict[str, object]:
    messages = [
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2025-03-26",
                "capabilities": {},
                "clientInfo": {"name": "mcp-contract", "version": "1"},
            },
        },
        {"jsonrpc": "2.0", "method": "notifications/initialized"},
        {"jsonrpc": "2.0", "id": 2, "method": method, "params": params},
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
    return next(response for response in responses if response.get("id") == 2)


def tool_call(
    node: Node,
    env: dict[str, str],
    name: str,
    arguments: dict[str, object],
) -> dict[str, object]:
    return exchange(
        node,
        env,
        "tools/call",
        {"name": name, "arguments": arguments},
    )


def successful_result(response: dict[str, object]) -> dict[str, object]:
    if "error" in response:
        raise AssertionError(f"MCP JSON-RPC error: {response['error']}")
    result = response.get("result")
    if not isinstance(result, dict) or result.get("isError"):
        raise AssertionError(f"MCP tool error: {response}")
    return result


def rejected(response: dict[str, object]) -> bool:
    if "error" in response:
        return True
    result = response.get("result")
    return isinstance(result, dict) and bool(result.get("isError"))


def result_text(result: dict[str, object]) -> str:
    content = result.get("content")
    if not isinstance(content, list) or not content:
        return ""
    first = content[0]
    return str(first.get("text", "")) if isinstance(first, dict) else ""


def result_json(result: dict[str, object]) -> dict[str, object]:
    value = json.loads(result_text(result))
    if not isinstance(value, dict):
        raise AssertionError(f"expected JSON object result, got {value!r}")
    return value


def created_trace_id(result: dict[str, object]) -> str:
    text = result_text(result)
    try:
        value = json.loads(text)
    except json.JSONDecodeError:
        value = None
    if isinstance(value, dict) and isinstance(value.get("id"), str):
        return value["id"]
    trace_id = text.rsplit(": ", 1)[-1]
    if not trace_id.startswith("20"):
        raise AssertionError(f"could not parse created trace ID from {text!r}")
    return trace_id


def seed_observability(path: Path, trace_id: str) -> None:
    with sqlite3.connect(path) as connection:
        connection.execute(
            "UPDATE traces SET tier='mid',created_at='2026-08-13T12:00:00Z' WHERE id=?",
            (trace_id,),
        )
        rows = [
            (
                "fixture-promote",
                "promote",
                trace_id,
                "fixture-cortex",
                "fixture",
                "2026-08-14T12:00:00Z",
                '{"from":"short","to":"mid"}',
            ),
            (
                "fixture-preempted",
                "consolidation_fail",
                "fixture-window-a",
                "fixture-cortex",
                "fixture",
                "2026-08-14T13:00:00Z",
                '{"reason":"peer_outranked"}',
            ),
            (
                "fixture-failure",
                "consolidation_fail",
                "fixture-window-b",
                "fixture-cortex",
                "fixture",
                "2026-08-14T14:00:00Z",
                '{"reason":"endpoint_error"}',
            ),
        ]
        connection.executemany(
            "INSERT INTO events(id,action,trace_id,cortex_id,origin,timestamp,data,vclock,signature,pubkey) "
            "VALUES (?,?,?,?,?,?,?,'{}','','')",
            rows,
        )


def read_count(path: Path, trace_id: str) -> int:
    with sqlite3.connect(path) as connection:
        row = connection.execute(
            "SELECT COALESCE(SUM(read_count),0) FROM trace_usage WHERE trace_id=?",
            (trace_id,),
        ).fetchone()
    return int(row[0])


def is_trashed(path: Path, trace_id: str) -> bool:
    with sqlite3.connect(path) as connection:
        row = connection.execute(
            "SELECT trashed_at IS NOT NULL FROM traces WHERE id=?", (trace_id,)
        ).fetchone()
    return bool(row[0])


def resolve_enum(schema: dict[str, object], field: dict[str, object]) -> list[str] | None:
    if isinstance(field.get("enum"), list):
        return list(field["enum"])  # type: ignore[arg-type]
    candidates = field.get("anyOf", [field])
    if not isinstance(candidates, list):
        return None
    for candidate in candidates:
        if not isinstance(candidate, dict):
            continue
        reference = candidate.get("$ref")
        if not isinstance(reference, str):
            continue
        definition = reference.rsplit("/", 1)[-1]
        definitions = schema.get("$defs", {})
        if isinstance(definitions, dict):
            target = definitions.get(definition)
            if isinstance(target, dict) and isinstance(target.get("enum"), list):
                return list(target["enum"])
    return None


def contract_shape(tools: list[dict[str, object]]) -> dict[str, object]:
    shape: dict[str, object] = {}
    for tool in tools:
        schema = tool.get("inputSchema", {})
        if not isinstance(schema, dict):
            schema = {}
        properties = schema.get("properties", {})
        if not isinstance(properties, dict):
            properties = {}
        missing_descriptions = sorted(
            name
            for name, field in properties.items()
            if isinstance(field, dict) and not field.get("description")
        )
        enums = {
            name: values
            for name, field in properties.items()
            if isinstance(field, dict)
            and (values := resolve_enum(schema, field)) is not None
        }
        annotations = tool.get("annotations") or DEFAULT_ANNOTATIONS
        shape[str(tool["name"])] = {
            "properties": sorted(properties),
            "required": sorted(schema.get("required", [])),
            "enums": enums,
            "output_schema": "outputSchema" in tool,
            "annotations": annotations,
            "missing_descriptions": missing_descriptions,
        }
    return shape


def exercise(node: Node, env: dict[str, str], root: Path) -> dict[str, object]:
    initialize(node, env, root / "cortexes")
    trace_id = add_seed(node, env)
    db = database(root, node)

    tools_result = successful_result(exchange(node, env, "tools/list", {}))
    tools = tools_result.get("tools")
    if not isinstance(tools, list):
        raise AssertionError(f"{node.name}: tools/list returned no tools")
    shape = contract_shape(tools)

    before = read_count(db, trace_id)
    get_default = successful_result(
        tool_call(node, env, "get_trace", {"id": trace_id})
    )
    after_default = read_count(db, trace_id)
    successful_result(
        tool_call(
            node,
            env,
            "get_trace",
            {"id": trace_id, "record_usage": True},
        )
    )
    after_recorded = read_count(db, trace_id)

    usage = successful_result(tool_call(node, env, "cortex_usage", {}))
    structured_usage = usage.get("structuredContent")
    if not isinstance(structured_usage, dict):
        raise AssertionError(f"{node.name}: cortex_usage has no structuredContent")

    set_tags = successful_result(
        tool_call(node, env, "set_trace_tags", {"id": trace_id, "tags": "alpha,beta"})
    )
    append_tags = successful_result(
        tool_call(
            node,
            env,
            "append_trace_tags",
            {"id": trace_id, "tags": "beta,gamma"},
        )
    )
    set_structured = set_tags.get("structuredContent")
    append_structured = append_tags.get("structuredContent")

    successful_result(
        tool_call(
            node,
            env,
            "append_trace",
            {"id": trace_id, "content": "MCP append marker."},
        )
    )
    successful_result(tool_call(node, env, "archive_trace", {"id": trace_id}))
    successful_result(tool_call(node, env, "unarchive_trace", {"id": trace_id}))
    search = successful_result(
        tool_call(node, env, "search_traces", {"query": "append marker"})
    )
    history = successful_result(
        tool_call(node, env, "trace_history", {"id": trace_id})
    )
    activity = result_json(
        successful_result(tool_call(node, env, "search_activity", {"top": 10}))
    )
    instructions = result_text(
        successful_result(tool_call(node, env, "get_instructions", {}))
    )

    derived_id = created_trace_id(
        successful_result(
            tool_call(
                node,
                env,
                "create_trace",
                {
                    "title": "MCP Observability Derived",
                    "type": "observation",
                    "body": "Synthetic derived body.",
                    "tags": "observability",
                    "derived_from": trace_id,
                },
            )
        )
    )
    seed_observability(db, derived_id)
    health = result_json(
        successful_result(
            tool_call(node, env, "consolidation_health", {"since": "30d"})
        )
    )

    divergence_id = created_trace_id(
        successful_result(
            tool_call(
                node,
                env,
                "create_trace",
                {
                    "title": "MCP Divergence Fixture",
                    "type": "divergence",
                    "body": "Synthetic conflict container.",
                    "derived_from": trace_id,
                },
            )
        )
    )
    successful_result(
        tool_call(
            node,
            env,
            "resolve_divergence",
            {"id": divergence_id, "body": "Merged MCP body."},
        )
    )
    resolved = successful_result(
        tool_call(node, env, "get_trace", {"id": trace_id})
    )

    invalid_vote = rejected(
        tool_call(
            node,
            env,
            "vote_trace",
            {"id": trace_id, "direction": "sideways"},
        )
    )
    invalid_type = rejected(
        tool_call(
            node,
            env,
            "create_trace",
            {"title": "Invalid", "type": "unknown", "body": "invalid"},
        )
    )

    with sqlite3.connect(db) as connection:
        tags = [
            str(row[0])
            for row in connection.execute(
                "SELECT tag FROM trace_tags WHERE trace_id=? ORDER BY tag", (trace_id,)
            )
        ]
        events = int(
            connection.execute(
                "SELECT COUNT(*) FROM events WHERE trace_id=?", (trace_id,)
            ).fetchone()[0]
        )

    return {
        "shape": shape,
        "usage_counts": (before, after_default, after_recorded),
        "get_contains_body": "Synthetic preference body" in result_text(get_default),
        "usage_keys": sorted(structured_usage),
        "startup_record_usage": structured_usage.get("startup", {})
        .get("preference_sequence", [{}, {}])[1]
        .get("arguments", {})
        .get("record_usage"),
        "set_structured": {
            "action": set_structured.get("action"),
            "tags": set_structured.get("tags"),
        }
        if isinstance(set_structured, dict)
        else None,
        "append_structured": {
            "action": append_structured.get("action"),
            "tags": append_structured.get("tags"),
        }
        if isinstance(append_structured, dict)
        else None,
        "tags": tags,
        "search_contains_id": trace_id in result_text(search),
        "history_contains_update": "update" in result_text(history),
        "search_activity": activity,
        "consolidation_health": {
            "schema_version": health.get("schema_version"),
            "activity": {
                key: health.get("activity", {}).get(key)
                for key in ("since", "daily", "totals")
            },
            "latency": health.get("latency"),
            "one_source_mid": health.get("one_source_mid"),
        },
        "divergence_resolved": "Merged MCP body." in result_text(resolved),
        "divergence_trashed": is_trashed(db, divergence_id),
        "event_count": events,
        "instruction_sections": all(
            section in instructions
            for section in [
                "## Agent Startup",
                "## MCP Usage",
                "## Memory Semantics",
                "## Creating Traces",
                "## Guardrails",
            ]
        ),
        "invalid_vote_rejected": invalid_vote,
        "invalid_type_rejected": invalid_type,
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    for binary in (args.go, args.rust):
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")

    with tempfile.TemporaryDirectory(prefix="noema-rust-mcp-contract-") as directory:
        root = Path(directory)
        (root / "home").mkdir()
        (root / "config").mkdir()
        env = environment(root)
        nodes = {
            "go": Node("go-mcp-contract", args.go.resolve(), False, free_port()),
            "rust": Node("rust-mcp-contract", args.rust.resolve(), True, free_port()),
        }
        reports = {
            label: exercise(node, env, root) for label, node in nodes.items()
        }

    deliberately_stricter = {"invalid_type_rejected"}
    for key in reports["go"]:
        if key in deliberately_stricter:
            continue
        if reports["go"][key] != reports["rust"][key]:
            raise AssertionError(
                f"MCP contract mismatch for {key}: "
                f"Go={reports['go'][key]!r}, Rust={reports['rust'][key]!r}"
            )
    if reports["go"]["usage_counts"] != (0, 0, 1):
        raise AssertionError(
            f"get_trace record_usage semantics failed: {reports['go']['usage_counts']}"
        )
    if not reports["rust"]["invalid_type_rejected"]:
        raise AssertionError("Rust accepted a trace type outside its advertised enum")
    if any(
        details["missing_descriptions"]
        for details in reports["rust"]["shape"].values()  # type: ignore[union-attr]
    ):
        raise AssertionError("Rust discovery has undocumented input fields")

    print("ok - Go/Rust MCP tool names, parameters, required fields, and enums")
    print("ok - Go/Rust output-schema and structured-content surfaces")
    print("ok - Go/Rust get_trace record_usage semantics")
    print("ok - Go/Rust tag, append, archive, search, and history behavior")
    print("ok - Go/Rust search activity and consolidation health aggregation")
    print("ok - Go/Rust custom divergence resolution lifecycle")
    print("ok - Go/Rust invalid vote rejection; Rust retains stricter type validation")
    print("ok - Go/Rust instruction and structured usage contracts")
    print("PASS: deterministic MCP contract parity fixture")


if __name__ == "__main__":
    main()
