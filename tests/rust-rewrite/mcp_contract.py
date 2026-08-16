#!/usr/bin/env python3
"""Compare the Go/Rust MCP discovery contract and representative tool behavior."""

from __future__ import annotations

import argparse
import http.client
import json
from pathlib import Path
import sqlite3
import subprocess
import tempfile

from federation_ring import (
    Node,
    cortex_dir,
    database,
    environment,
    free_port,
    run,
    start,
    stop,
)


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


def result_json_value(result: dict[str, object]) -> object:
    return json.loads(result_text(result))


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
    return trace_id.split(" ", 1)[0]


class HttpMcpClient:
    def __init__(self, node: Node) -> None:
        self.node = node
        self.session_id = ""

    def request(self, payload: dict[str, object]) -> dict[str, object]:
        connection = http.client.HTTPConnection(
            "127.0.0.1", self.node.port, timeout=5
        )
        headers = {
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
        }
        if self.session_id:
            headers["Mcp-Session-Id"] = self.session_id
        connection.request("POST", "/mcp", json.dumps(payload), headers)
        response = connection.getresponse()
        body = response.read().decode()
        session_id = response.getheader("Mcp-Session-Id")
        connection.close()
        if session_id:
            self.session_id = session_id
        if response.status >= 400:
            raise AssertionError(
                f"{self.node.name}: HTTP {response.status} from MCP: {body}"
            )
        if "id" not in payload:
            return {}
        if not body:
            return {}
        data_lines = [
            line.removeprefix("data:").strip()
            for line in body.splitlines()
            if line.startswith("data:")
        ]
        return json.loads(data_lines[-1] if data_lines else body)

    def initialize(self) -> None:
        response = self.request(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {
                    "protocolVersion": "2025-03-26",
                    "capabilities": {},
                    "clientInfo": {"name": "mcp-contract-http", "version": "1"},
                },
            }
        )
        if response.get("id") != 1 or "error" in response:
            raise AssertionError(f"{self.node.name}: initialize failed: {response}")
        self.request({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def tool_call(
        self, name: str, arguments: dict[str, object]
    ) -> dict[str, object]:
        return self.request(
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": "tools/call",
                "params": {"name": name, "arguments": arguments},
            }
        )


def rejection_text(response: dict[str, object]) -> str:
    error = response.get("error")
    if isinstance(error, dict):
        return str(error.get("message", ""))
    result = response.get("result")
    return result_text(result) if isinstance(result, dict) else ""


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


def normalize_candidates(
    payload: dict[str, object], labels: dict[str, str]
) -> dict[str, object]:
    raw = payload.get("candidates")
    if not isinstance(raw, list):
        raise AssertionError(f"candidate response has no list: {payload!r}")
    candidates: list[dict[str, object]] = []
    for item in raw:
        if not isinstance(item, dict):
            raise AssertionError(f"candidate is not an object: {item!r}")
        candidate = dict(item)
        candidate["ID"] = labels.get(str(candidate.get("ID")), "unknown")
        candidate["CreatedAt"] = "<timestamp>"
        candidates.append(candidate)
    candidates.sort(key=lambda item: str(item.get("ID")))
    return {"window_hours": payload.get("window_hours"), "candidates": candidates}


def distilled_state(
    path: Path, distilled_id: str, labels: dict[str, str]
) -> dict[str, object]:
    with sqlite3.connect(path) as connection:
        trace_row = connection.execute(
            "SELECT tier,type FROM traces WHERE id=?", (distilled_id,)
        ).fetchone()
        source_ids = [
            labels.get(str(row[0]), "unknown")
            for row in connection.execute(
                "SELECT derived_from FROM trace_lineage WHERE trace_id=? "
                "ORDER BY derived_from",
                (distilled_id,),
            )
        ]
        event_row = connection.execute(
            "SELECT data FROM events WHERE trace_id=? AND action='consolidate'",
            (distilled_id,),
        ).fetchone()
        source_tiers = [
            str(row[0])
            for row in connection.execute(
                "SELECT tier FROM traces WHERE id IN (?,?) ORDER BY id",
                tuple(labels),
            )
        ]
    if trace_row is None or event_row is None:
        raise AssertionError("distillation did not persist trace and telemetry event")
    telemetry = json.loads(str(event_row[0]))
    telemetry["source_ids"] = [
        labels.get(str(source_id), "unknown")
        for source_id in telemetry.get("source_ids", [])
    ]
    telemetry["distilled_id"] = "distilled"
    return {
        "tier": trace_row[0],
        "type": trace_row[1],
        "source_ids": sorted(source_ids),
        "source_tiers": source_tiers,
        "telemetry": telemetry,
    }


def trace_count(path: Path) -> int:
    with sqlite3.connect(path) as connection:
        return int(connection.execute("SELECT count(*) FROM traces").fetchone()[0])


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

    second_source_id = created_trace_id(
        successful_result(
            tool_call(
                node,
                env,
                "create_trace",
                {
                    "title": "MCP Consolidation Source",
                    "type": "fact",
                    "body": "A second deterministic source for MCP consolidation.",
                    "tags": "consolidation-source",
                },
            )
        )
    )
    source_labels = {trace_id: "source-a", second_source_id: "source-b"}
    candidates = normalize_candidates(
        result_json(
            successful_result(
                tool_call(node, env, "list_consolidation_candidates", {})
            )
        ),
        source_labels,
    )
    one_source_rejected = rejected(
        tool_call(
            node,
            env,
            "record_consolidation_result",
            {
                "title": "Invalid Distillation",
                "body": "One source is not a consolidation.",
                "source_ids": trace_id,
            },
        )
    )
    distilled_id = created_trace_id(
        successful_result(
            tool_call(
                node,
                env,
                "record_consolidation_result",
                {
                    "title": "MCP Distilled Result",
                    "body": "A deterministic distilled result.",
                    "source_ids": f"{trace_id},{second_source_id}",
                    "tags": "distilled,mcp-contract",
                    "author": "test-agent",
                    "model_name": "fixture-model",
                    "model_tier_profile": "frontier",
                    "cohesion_confidence": 0.875,
                },
            )
        )
    )
    distilled = distilled_state(db, distilled_id, source_labels)
    lineage = result_text(
        successful_result(tool_call(node, env, "trace_lineage", {"id": distilled_id}))
    )

    identity = result_json(
        successful_result(tool_call(node, env, "cortex_identity", {}))
    )
    identity_report = {
        "keys": sorted(identity),
        "id_nonempty": bool(identity.get("id")),
        "name_matches": identity.get("name") == node.name,
        "version": identity.get("version"),
        "mode": identity.get("mode"),
        "pubkey_present": "pubkey" in identity,
        "rank_present": "rank" in identity,
    }
    status = result_text(
        successful_result(tool_call(node, env, "federation_status", {}))
    )
    normalized_status = status.replace(node.name, "<node>").replace(
        str(identity["id"]), "<cortex-id>"
    )

    manifest = cortex_dir(root, node) / "cortex.md"
    manifest_before_announce = manifest.read_bytes()
    invalid_announce = rejected(
        tool_call(
            node,
            env,
            "announce_peer",
            {"name": "peer-invalid", "endpoint": "file:///tmp/not-http"},
        )
    )
    self_announce = rejected(
        tool_call(
            node,
            env,
            "announce_peer",
            {"name": node.name, "endpoint": "https://peer.example.com"},
        )
    )
    announce = result_text(
        successful_result(
            tool_call(
                node,
                env,
                "announce_peer",
                {"name": "peer-new", "endpoint": "https://peer.example.com"},
            )
        )
    ).replace(node.name, "<node>")
    announce_manifest_unchanged = manifest.read_bytes() == manifest_before_announce

    invalid_sync_cursor = rejected(
        tool_call(node, env, "sync_events", {"since": "not-a-ulid"})
    )
    one_event = result_json_value(
        successful_result(tool_call(node, env, "sync_events", {"limit": 1}))
    )
    if not isinstance(one_event, list) or len(one_event) != 1:
        raise AssertionError(f"{node.name}: sync_events limit was not honored")
    event = one_event[0]
    if not isinstance(event, dict):
        raise AssertionError(f"{node.name}: sync_events returned a non-object")
    all_events = result_json_value(
        successful_result(tool_call(node, env, "sync_events", {"limit": 0}))
    )
    if not isinstance(all_events, list):
        raise AssertionError(f"{node.name}: sync_events returned a non-list")
    usage_rows = result_json_value(
        successful_result(tool_call(node, env, "sync_read_signal", {"limit": 100}))
    )
    if not isinstance(usage_rows, list):
        raise AssertionError(f"{node.name}: sync_read_signal returned a non-list")
    normalized_usage = []
    for raw_row in usage_rows:
        if not isinstance(raw_row, dict):
            raise AssertionError(f"{node.name}: usage row is not an object")
        row = dict(raw_row)
        row["trace_id"] = source_labels.get(str(row.get("trace_id")), "other")
        row["peer_cortex_id"] = (
            "self" if row.get("peer_cortex_id") == identity.get("id") else "other"
        )
        if "last_read_at" in row:
            row["last_read_at"] = "<timestamp>"
        row["updated_at"] = "<timestamp>"
        normalized_usage.append(row)
    normalized_usage.sort(key=lambda row: str(row.get("trace_id")))

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
    invalid_health_since = rejected(
        tool_call(node, env, "consolidation_health", {"since": "not-a-duration"})
    )
    missing_get = rejected(
        tool_call(node, env, "get_trace", {"id": "20990101-missing"})
    )
    missing_lineage = rejected(
        tool_call(node, env, "trace_lineage", {"id": "20990101-missing"})
    )
    missing_history = result_text(
        successful_result(
            tool_call(node, env, "trace_history", {"id": "20990101-missing"})
        )
    )
    resolve_missing_choice = rejected(
        tool_call(
            node,
            env,
            "resolve_divergence",
            {"id": "20990101-missing"},
        )
    )
    resolve_both_choices = rejected(
        tool_call(
            node,
            env,
            "resolve_divergence",
            {"id": "20990101-missing", "accept": "peer-a", "body": "merged"},
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
        "consolidation_candidates": candidates,
        "one_source_rejected": one_source_rejected,
        "distilled": distilled,
        "lineage_contains_sources": all(
            source_id in lineage for source_id in source_labels
        ),
        "identity": identity_report,
        "federation_status": normalized_status,
        "invalid_announce_rejected": invalid_announce,
        "self_announce_rejected": self_announce,
        "announce": announce,
        "announce_manifest_unchanged": announce_manifest_unchanged,
        "invalid_sync_cursor_rejected": invalid_sync_cursor,
        "sync_event": {
            "keys": sorted(event),
            "action": event.get("action"),
            "trace_id": source_labels.get(str(event.get("trace_id")), "other"),
            "cortex_id_matches": event.get("cortex_id") == identity.get("id"),
            "origin_matches": event.get("origin") == node.name,
        },
        "sync_default_limit_count": len(all_events),
        "sync_read_signal": normalized_usage,
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
        "invalid_health_since_rejected": invalid_health_since,
        "missing_get_rejected": missing_get,
        "missing_lineage_rejected": missing_lineage,
        "missing_history": missing_history,
        "resolve_missing_choice_rejected": resolve_missing_choice,
        "resolve_both_choices_rejected": resolve_both_choices,
    }


def exercise_http_modes(
    node: Node, env: dict[str, str], root: Path
) -> dict[str, object]:
    initialize(node, env, root / "cortexes")
    seed_id = add_seed(node, env)
    db = database(root, node)
    run(node, env, "federation", "set-mode", "publish")

    fake_id = "20990101-missing"
    mutation_calls = {
        "create_trace": {
            "title": "Blocked Remote Creation",
            "type": "fact",
            "body": "This must not be persisted over HTTP in publish mode.",
        },
        "delete_trace": {"id": fake_id},
        "recover_trace": {"id": fake_id},
        "archive_trace": {"id": fake_id},
        "unarchive_trace": {"id": fake_id},
        "update_trace": {"id": fake_id, "body": "blocked"},
        "set_trace_tags": {"id": fake_id, "tags": "blocked"},
        "append_trace_tags": {"id": fake_id, "tags": "blocked"},
        "vote_trace": {"id": fake_id, "direction": "up"},
        "record_consolidation_result": {
            "title": "Blocked Distillation",
            "body": "blocked",
            "source_ids": f"{fake_id},20990101-also-missing",
        },
        "append_trace": {"id": fake_id, "content": "blocked"},
        "resolve_divergence": {"id": fake_id, "body": "blocked"},
    }
    before_publish_calls = trace_count(db)
    publish_client: HttpMcpClient | None = None
    try:
        start(node, env)
        publish_client = HttpMcpClient(node)
        publish_client.initialize()
        successful_result(publish_client.tool_call("list_traces", {}))
        successful_result(publish_client.tool_call("sync_events", {"limit": 1}))
        guarded = {
            name: rejected(response)
            and "publish mode" in rejection_text(response)
            for name, arguments in mutation_calls.items()
            for response in [publish_client.tool_call(name, arguments)]
        }
        if not all(guarded.values()):
            raise AssertionError(f"{node.name}: incomplete publish guards: {guarded}")
        after_publish_calls = trace_count(db)
        local_id = created_trace_id(
            successful_result(
                tool_call(
                    node,
                    env,
                    "create_trace",
                    {
                        "title": "Allowed Local Creation",
                        "type": "fact",
                        "body": "Local stdio remains writable in publish mode.",
                    },
                )
            )
        )
    finally:
        stop(node)

    if before_publish_calls != after_publish_calls:
        raise AssertionError(f"{node.name}: publish-mode HTTP call mutated traces")
    if trace_count(db) != after_publish_calls + 1:
        raise AssertionError(f"{node.name}: publish-mode stdio write did not persist")

    run(node, env, "federation", "set-mode", "subscribe")
    try:
        start(node, env)
        subscribe_client = HttpMcpClient(node)
        subscribe_client.initialize()
        successful_result(subscribe_client.tool_call("list_traces", {}))
        events_response = subscribe_client.tool_call("sync_events", {})
        usage_response = subscribe_client.tool_call("sync_read_signal", {})
        subscribe_guards = {
            "sync_events": rejected(events_response)
            and "subscribe mode" in rejection_text(events_response),
            "sync_read_signal": rejected(usage_response)
            and "subscribe mode" in rejection_text(usage_response),
        }
        if not all(subscribe_guards.values()):
            raise AssertionError(
                f"{node.name}: incomplete subscribe guards: {subscribe_guards}"
            )
    finally:
        stop(node)

    return {
        "publish_guarded_tools": sorted(mutation_calls),
        "publish_read_succeeded": True,
        "publish_sync_succeeded": True,
        "publish_remote_unchanged": before_publish_calls == after_publish_calls,
        "publish_local_write_succeeded": local_id.startswith("20"),
        "subscribe_guarded_tools": sorted(subscribe_guards),
        "subscribe_read_succeeded": True,
        "seed_preserved": trace_count(db) >= 2 and bool(seed_id),
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
        http_nodes = {
            "go": Node("go-mcp-http-modes", args.go.resolve(), False, free_port()),
            "rust": Node(
                "rust-mcp-http-modes", args.rust.resolve(), True, free_port()
            ),
        }
        http_reports = {
            label: exercise_http_modes(node, env, root)
            for label, node in http_nodes.items()
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
    if http_reports["go"] != http_reports["rust"]:
        raise AssertionError(
            "MCP HTTP mode mismatch: "
            f"Go={http_reports['go']!r}, Rust={http_reports['rust']!r}"
        )

    print("ok - Go/Rust MCP tool names, parameters, required fields, and enums")
    print("ok - Go/Rust output-schema and structured-content surfaces")
    print("ok - Go/Rust get_trace record_usage semantics")
    print("ok - Go/Rust tag, append, archive, search, and history behavior")
    print("ok - Go/Rust search activity and consolidation health aggregation")
    print("ok - Go/Rust consolidation candidates, distillation, and lineage")
    print("ok - Go/Rust custom divergence resolution lifecycle")
    print("ok - Go/Rust identity, federation status, sync, and announcement behavior")
    print("ok - Go/Rust publish/subscribe HTTP transport guards")
    print("ok - Go/Rust missing-resource and invalid-argument behavior")
    print("ok - Go/Rust invalid vote rejection; Rust retains stricter type validation")
    print("ok - Go/Rust instruction and structured usage contracts")
    print("PASS: deterministic MCP contract parity fixture")


if __name__ == "__main__":
    main()
