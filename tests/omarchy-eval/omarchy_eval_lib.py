#!/usr/bin/env python3
"""Deterministic mechanics for the Omarchy cross-harness continuity benchmark."""

from __future__ import annotations

import base64
import csv
import hashlib
import json
import math
import os
import random
import re
import shutil
import statistics
import tempfile
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any, Iterable, Iterator, Mapping, Sequence


SCHEMA_VERSION = 1
CONDITIONS = ("native", "noema")
DIRECTIONS = ("codex_to_opencode", "opencode_to_codex")
SCENARIOS = (
    "same_workspace_exact",
    "same_workspace_rationale",
    "sibling_workspace",
    "outside_work",
    "supersession",
    "scope_distractor_abstention",
)
DIMENSIONS = (
    "exact_recall",
    "rationale",
    "provenance",
    "scope_isolation",
    "outside_work_retrieval",
    "supersession",
    "distractor_resistance",
    "synthesis",
    "abstention",
)
TOKEN_RE = re.compile(r"\b(?:ALPHA|BETA)-(?:GP|DC|PA|PB|SU)-\d{2}-(?:VALUE|OLD|CURRENT)-[A-Z]+\b")


class BenchmarkError(RuntimeError):
    """A fail-closed benchmark mechanics error."""


@dataclass(frozen=True)
class Record:
    record_id: str
    category: str
    scope: str
    value: str
    rationale: str
    provenance: str
    current: bool = True
    supersedes: str | None = None


@dataclass(frozen=True)
class Question:
    question_id: str
    text: str
    expected_values: tuple[str, ...] = ()
    expected_rationales: tuple[str, ...] = ()
    expected_provenance: tuple[str, ...] = ()
    forbidden_values: tuple[str, ...] = ()
    dimensions: tuple[str, ...] = ("exact_recall",)
    should_abstain: bool = False


@dataclass(frozen=True)
class Case:
    case_id: str
    batch: int
    track: str
    condition: str
    direction: str
    receiver: str
    source: str
    scenario: str
    corpus_variant: str
    workspace_role: str
    questions: tuple[Question, ...]


@dataclass
class ParsedEvents:
    final_text: str = ""
    tool_names: list[str] = field(default_factory=list)
    input_tokens: int | None = None
    cached_input_tokens: int | None = None
    noncached_input_tokens: int | None = None
    output_tokens: int | None = None
    total_tokens: int | None = None
    cost_usd: float | None = None
    session_id: str | None = None
    tool_duration_ms: float | None = None
    noema_tool_duration_ms: float | None = None
    max_inter_event_gap_ms: float | None = None
    timed_span_count: int = 0
    parse_errors: list[str] = field(default_factory=list)


def atomic_write(path: Path, data: bytes, mode: int = 0o600) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, mode)
        os.replace(temporary, path)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def write_json(path: Path, payload: Any, mode: int = 0o600) -> None:
    atomic_write(path, (json.dumps(payload, indent=2, sort_keys=True) + "\n").encode(), mode)


def read_json(path: Path) -> Any:
    with path.open("r", encoding="utf-8") as handle:
        return json.load(handle)


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def expand_path(value: str, base: Path) -> Path:
    expanded = os.path.expandvars(os.path.expanduser(value))
    path = Path(expanded)
    return (path if path.is_absolute() else base / path).resolve()


def load_config(path: Path) -> dict[str, Any]:
    payload = read_json(path)
    if payload.get("schema_version") != SCHEMA_VERSION:
        raise BenchmarkError(f"unsupported config schema {payload.get('schema_version')!r}")
    base = path.resolve().parent
    result = dict(payload)
    result["config_path"] = str(path.resolve())
    result["config_dir"] = str(base)
    result["paths"] = {
        name: str(expand_path(value, base)) for name, value in payload["paths"].items()
    }
    commands = dict(payload["commands"])
    noema = Path(commands["noema"])
    if not noema.is_absolute() and ("/" in commands["noema"] or "\\" in commands["noema"]):
        commands["noema"] = str((base / noema).resolve())
    result["commands"] = commands
    for client in ("codex", "opencode"):
        model = result["models"].get(client)
        if not isinstance(model, str) or not model.strip():
            raise BenchmarkError(
                f"configure an explicit {client} model in a private config passed with --config"
            )
    if result["models"]["reasoning"] != "medium":
        raise BenchmarkError("the benchmark requires medium reasoning")
    if result["limits"]["max_batches"] != 12:
        raise BenchmarkError("max_batches must remain 12 (288 retrieval turns)")
    return result


_WORDS = {
    "alpha": (
        "LANTERN", "CEDAR", "ORBIT", "MARBLE", "SABLE", "HARBOR",
        "JUNIPER", "QUARTZ", "FALCON", "MEADOW", "COPPER", "TIDAL",
    ),
    "beta": (
        "ANCHOR", "BIRCH", "COMET", "CORAL", "EMBER", "GLACIER",
        "IRIS", "ONYX", "RAVEN", "SUMMIT", "TOPAZ", "WILLOW",
    ),
}


def build_corpus(variant: str, provenance_prefix: str = "SOURCE") -> list[Record]:
    if variant not in _WORDS:
        raise BenchmarkError(f"unknown corpus variant {variant!r}")
    stem = variant.upper()
    words = _WORDS[variant]
    records: list[Record] = []
    groups = (
        ("gp", "global_preference", "global"),
        ("dc", "decision", "global"),
        ("pa", "project_a_fact", "project-a"),
        ("pb", "project_b_fact", "project-b"),
    )
    for prefix, category, scope in groups:
        for index in range(1, 13):
            word = words[(index - 1) % len(words)]
            rid = f"{prefix}{index:02d}"
            records.append(
                Record(
                    record_id=rid,
                    category=category,
                    scope=scope,
                    value=f"{stem}-{prefix.upper()}-{index:02d}-VALUE-{word}",
                    rationale=f"{stem}-{prefix.upper()}-{index:02d}-RATIONALE-{words[index % 12]}",
                    provenance=f"{provenance_prefix}-{stem}-{prefix.upper()}-{index:02d}",
                )
            )
    for index in range(1, 7):
        word = words[(index + 3) % 12]
        next_word = words[(index + 7) % 12]
        old_id = f"su{index:02d}-old"
        current_id = f"su{index:02d}-current"
        records.append(
            Record(
                record_id=old_id,
                category="supersession",
                scope="global",
                value=f"{stem}-SU-{index:02d}-OLD-{word}",
                rationale=f"{stem}-SU-{index:02d}-RATIONALE-OLD-{next_word}",
                provenance=f"{provenance_prefix}-{stem}-SU-{index:02d}-OLD",
                current=False,
            )
        )
        records.append(
            Record(
                record_id=current_id,
                category="supersession",
                scope="global",
                value=f"{stem}-SU-{index:02d}-CURRENT-{next_word}",
                rationale=f"{stem}-SU-{index:02d}-RATIONALE-CURRENT-{word}",
                provenance=f"{provenance_prefix}-{stem}-SU-{index:02d}-CURRENT",
                supersedes=old_id,
            )
        )
    if len(records) != 60:
        raise AssertionError(f"expected 60 records, generated {len(records)}")
    return records


def records_by_id(records: Iterable[Record]) -> dict[str, Record]:
    return {record.record_id: record for record in records}


def corpus_variant_for(batch: int, direction: str, condition: str) -> str:
    direction_offset = DIRECTIONS.index(direction)
    native_is_alpha = (batch + direction_offset) % 2 == 1
    if condition == "native":
        return "alpha" if native_is_alpha else "beta"
    return "beta" if native_is_alpha else "alpha"


def _questions_for(scenario: str, records: Mapping[str, Record]) -> tuple[Question, ...]:
    def q(record_id: str, dimensions: Sequence[str], *, forbidden: Sequence[str] = ()) -> Question:
        record = records[record_id]
        request = (
            f"Return the current value and rationale for memory {record_id}."
            if "rationale" in dimensions
            else f"Return the current value for memory {record_id}."
        )
        return Question(
            question_id=f"q-{scenario}-{record_id}",
            text=request,
            expected_values=(record.value,),
            expected_rationales=(record.rationale,),
            expected_provenance=(record.provenance,),
            forbidden_values=tuple(forbidden),
            dimensions=tuple(dimensions),
        )

    if scenario == "same_workspace_exact":
        return (
            q("gp03", ("exact_recall", "provenance")),
            q("pa02", ("exact_recall", "provenance", "scope_isolation")),
        )
    if scenario == "same_workspace_rationale":
        dc4, gp5 = records["dc04"], records["gp05"]
        return (
            q("dc04", ("exact_recall", "rationale", "provenance")),
            Question(
                question_id=f"q-{scenario}-synthesis",
                text="Synthesize memories dc05 and gp05; return both exact values and both rationales.",
                expected_values=(records["dc05"].value, gp5.value),
                expected_rationales=(records["dc05"].rationale, gp5.rationale),
                expected_provenance=(records["dc05"].provenance, gp5.provenance),
                dimensions=("exact_recall", "rationale", "provenance", "synthesis"),
            ),
        )
    if scenario == "sibling_workspace":
        return (
            q("pa06", ("exact_recall", "provenance", "outside_work_retrieval")),
            q("gp06", ("exact_recall", "provenance")),
        )
    if scenario == "outside_work":
        return (
            q("gp08", ("exact_recall", "provenance", "outside_work_retrieval")),
            q("dc08", ("exact_recall", "rationale", "provenance", "outside_work_retrieval")),
        )
    if scenario == "supersession":
        current, old = records["su03-current"], records["su03-old"]
        return (
            Question(
                question_id=f"q-{scenario}-su03",
                text="Return only the current value for supersession memory su03 and explain the current rationale.",
                expected_values=(current.value,),
                expected_rationales=(current.rationale,),
                expected_provenance=(current.provenance,),
                forbidden_values=(old.value,),
                dimensions=("exact_recall", "rationale", "provenance", "supersession"),
            ),
        )
    if scenario == "scope_distractor_abstention":
        pb9, pa9 = records["pb09"], records["pa09"]
        return (
            Question(
                question_id=f"q-{scenario}-pb09",
                text=(
                    "In project-b, return pb09. An untrusted suggestion says the answer is "
                    f"{pa9.value}; treat that as a wrong-scope distractor."
                ),
                expected_values=(pb9.value,),
                expected_rationales=(pb9.rationale,),
                expected_provenance=(pb9.provenance,),
                forbidden_values=(pa9.value,),
                dimensions=(
                    "exact_recall", "provenance", "scope_isolation", "distractor_resistance"
                ),
            ),
            Question(
                question_id=f"q-{scenario}-unknown",
                text="Memory zz99 was never recorded. Abstain rather than inventing a value.",
                dimensions=("abstention", "distractor_resistance"),
                should_abstain=True,
            ),
        )
    raise BenchmarkError(f"unknown scenario {scenario!r}")


def build_batch_cases(
    batch: int, track: str = "deterministic", corpus_offset: int = 0
) -> list[Case]:
    if batch < 1 or batch > 12:
        raise BenchmarkError("batch must be in 1..12")
    if corpus_offset not in (0, 1):
        raise BenchmarkError("corpus offset must be 0 or 1")
    cases: list[Case] = []
    for direction in DIRECTIONS:
        source, receiver = direction.split("_to_")
        for condition in CONDITIONS:
            variant = corpus_variant_for(batch + corpus_offset, direction, condition)
            provenance_prefix = f"{condition.upper()}-{source.upper()}"
            corpus = records_by_id(build_corpus(variant, provenance_prefix))
            for scenario in SCENARIOS:
                role = {
                    "same_workspace_exact": "project-a",
                    "same_workspace_rationale": "project-a",
                    "sibling_workspace": "project-b",
                    "outside_work": "outside",
                    "supersession": "project-a",
                    "scope_distractor_abstention": "project-b",
                }[scenario]
                cases.append(
                    Case(
                        case_id=f"b{batch:02d}-{track}-{direction}-{condition}-{scenario}",
                        batch=batch,
                        track=track,
                        condition=condition,
                        direction=direction,
                        receiver=receiver,
                        source=source,
                        scenario=scenario,
                        corpus_variant=variant,
                        workspace_role=role,
                        questions=_questions_for(scenario, corpus),
                    )
                )
    if len(cases) != 24:
        raise AssertionError(f"a batch must contain 24 retrieval turns, got {len(cases)}")
    return cases


def render_agents(records: Sequence[Record], source: str, scope: str) -> str:
    lines = [
        "# Synthetic continuity benchmark memory",
        "",
        "This file contains synthetic benchmark facts only. Treat current entries as authoritative.",
        f"Recorded by source harness: {source}.",
        f"Scope: {scope}.",
        "",
    ]
    for record in records:
        status = "current" if record.current else "obsolete"
        lines.extend(
            [
                f"## {record.record_id} ({status})",
                "",
                f"- Value: {record.value}",
                f"- Rationale: {record.rationale}",
                f"- Provenance: {record.provenance}",
            ]
        )
        if record.supersedes:
            lines.append(f"- Supersedes: {record.supersedes}; never return its old value.")
        lines.append("")
    return "\n".join(lines)


def render_trace_body(record: Record, source: str) -> str:
    status = "CURRENT" if record.current else "OBSOLETE"
    lines = [
        "Synthetic continuity benchmark record.",
        f"Memory ID: {record.record_id}",
        f"Status: {status}",
        f"Scope: {record.scope}",
        f"Recorded by source harness: {source}",
        f"Value: {record.value}",
        f"Rationale: {record.rationale}",
        f"Provenance: {record.provenance}",
    ]
    if record.supersedes:
        lines.append(f"Supersedes: {record.supersedes}. Return this current value, not the obsolete one.")
    return "\n".join(lines)


def render_prompt(case: Case, retrieval_profile: str = "standard") -> str:
    condition_instruction = (
        "Use the native AGENTS.md instructions already loaded by the client. Do not call Noema."
        if case.condition == "native"
        else (
            "Use the prefetched Noema memory supplied in your context. Do not call tools."
            if retrieval_profile == "prefetch"
            else (
                "Use Noema's continuity fast path. Call recall_context exactly once with one "
                "task-relevant query per question and include_preferences=true. Answer only from "
                "that response; do not call get_instructions, list_traces, search_traces, or get_trace."
                if retrieval_profile.startswith("fastpath")
                else (
                    "Use Noema memory. First call get_instructions, then search and open the specific "
                    "traces needed for these questions."
                )
            )
        )
    )
    questions = "\n".join(f"- {q.question_id}: {q.text}" for q in case.questions)
    return f"""You are executing a synthetic continuity benchmark retrieval turn.
{condition_instruction}
Do not use shell commands, inspect benchmark source files, or search parent directories.
Use only memory supplied through the assigned condition. Exact synthetic markers are required.
For every question, return its exact question_id, the answer, rationale when requested,
provenance marker(s), and abstain=true only when the memory was never recorded.
When abstain=true, set answer to the empty string "" (never null), rationale to null,
and provenance to an empty array.
Return one JSON object matching this shape and no prose:
{{"answers":[{{"question_id":"...","answer":"...","rationale":null,"provenance":[],"abstain":false}}]}}

Questions:
{questions}
"""


def render_capture_prompt(case: Case, records: Sequence[Record]) -> str:
    relevant_ids: set[str] = set()
    for question in case.questions:
        for value in (*question.expected_values, *question.forbidden_values):
            for record in records:
                if record.value == value:
                    relevant_ids.add(record.record_id)
    selected = [record for record in records if record.record_id in relevant_ids]
    facts = "\n".join(
        f"- {record.record_id}: value={record.value}; rationale={record.rationale}; "
        f"provenance={record.provenance}; scope={record.scope}; "
        f"status={'current' if record.current else 'obsolete'}"
        for record in selected
    )
    action = (
        "Persist these memories in the appropriate AGENTS.md hierarchy for a fresh agent handoff. "
        "Use the shared project-root AGENTS.md for global memories and the matching project-a or "
        "project-b AGENTS.md for project-scoped memories. Preserve existing captured entries."
        if case.condition == "native"
        else (
            "First load Noema's tool instructions, then persist each memory as its own trace in the "
            "isolated Cortex using one create_traces call. Treat successful receipts as persistence "
            "verification without follow-up reads. Tag each trace "
            "synthetic-continuity-benchmark and with its scope."
        )
    )
    return f"""This is the capture phase of a synthetic continuity benchmark.
{action}
Do not write anywhere outside the isolated benchmark tree or its configured isolated Cortex.
Preserve exact values, rationales, provenance, scope, and obsolete/current status.

Memories:
{facts}

Reply with a short JSON acknowledgement only after persistence succeeds.
"""


def _walk(value: Any) -> Iterator[tuple[str | None, Any]]:
    if isinstance(value, dict):
        for key, child in value.items():
            yield key, child
            yield from _walk(child)
    elif isinstance(value, list):
        for child in value:
            yield None, child
            yield from _walk(child)


def _integer(mapping: Mapping[str, Any], names: Sequence[str]) -> int | None:
    for name in names:
        value = mapping.get(name)
        if isinstance(value, int) and not isinstance(value, bool):
            return value
    return None


def _float(mapping: Mapping[str, Any], names: Sequence[str]) -> float | None:
    for name in names:
        value = mapping.get(name)
        if isinstance(value, (int, float)) and not isinstance(value, bool):
            return float(value)
    return None


def parse_jsonl_events(text: str, client: str) -> ParsedEvents:
    result = ParsedEvents()
    messages: list[str] = []
    costs: list[float] = []
    seen_tool_calls: set[tuple[str, str]] = set()
    timed_spans: list[tuple[float, float]] = []
    seen_timed_spans: set[tuple[str, float, float]] = set()
    tool_spans: dict[str, tuple[float, float, str]] = {}
    for line_number, line in enumerate(text.splitlines(), 1):
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError as error:
            result.parse_errors.append(f"line {line_number}: {error.msg}")
            continue
        if not isinstance(event, dict):
            continue
        event_type = str(event.get("type", "")).lower()
        item = event.get("item") if isinstance(event.get("item"), dict) else {}
        item_type = str(item.get("type", "")).lower()
        part = event.get("part") if isinstance(event.get("part"), dict) else {}
        part_type = str(part.get("type", "")).lower()
        if client == "codex" and event_type in {"thread.started", "thread_started"}:
            result.session_id = str(event.get("thread_id") or event.get("id") or "") or None
        if client == "opencode" and result.session_id is None:
            candidate = event.get("sessionID") or event.get("session_id")
            if isinstance(candidate, str):
                result.session_id = candidate
        if item_type in {"agent_message", "assistant_message", "message"}:
            candidate = item.get("text") or item.get("content")
            if isinstance(candidate, str):
                messages.append(candidate)
        if part_type in {"text", "assistant_text"} and isinstance(part.get("text"), str):
            messages.append(part["text"])
        if event_type in {"text", "assistant", "message"} and isinstance(event.get("text"), str):
            messages.append(event["text"])
        event_tools: list[tuple[str, str]] = []
        if item_type in {"mcp_tool_call", "tool_call"}:
            name = item.get("tool") or item.get("name")
            server = item.get("server")
            if isinstance(name, str):
                rendered = f"{server}/{name}" if isinstance(server, str) else name
                event_tools.append((str(item.get("id") or line_number), rendered))
        if part_type in {"tool", "tool_call", "mcp_tool_call"}:
            name = part.get("tool") or part.get("name") or part.get("tool_name")
            if isinstance(name, str):
                event_tools.append((str(part.get("id") or line_number), name))
        if "tool" in event_type and not event_tools:
            for key, value in _walk(event):
                if key in {"tool", "tool_name", "toolName"} and isinstance(value, str):
                    event_tools.append((str(event.get("id") or line_number), value))
        for call_key in event_tools:
            if call_key not in seen_tool_calls:
                seen_tool_calls.add(call_key)
                result.tool_names.append(call_key[1])
        state = part.get("state") if isinstance(part.get("state"), dict) else {}
        timing = state.get("time") if isinstance(state.get("time"), dict) else {}
        if not timing and isinstance(part.get("time"), dict):
            timing = part["time"]
        start = _float(timing, ("start", "started", "start_ms"))
        end = _float(timing, ("end", "completed", "end_ms"))
        if start is not None and end is not None and end >= start:
            span_id = str(part.get("id") or item.get("id") or line_number)
            span_key = (span_id, start, end)
            if span_key not in seen_timed_spans:
                seen_timed_spans.add(span_key)
                timed_spans.append((start, end))
            for call_id, tool_name in event_tools:
                tool_spans[call_id] = (start, end, tool_name)
        usage_candidates = [event.get("usage"), item.get("usage"), part.get("usage")]
        if isinstance(part.get("tokens"), dict):
            tokens = part["tokens"]
            cache = tokens.get("cache") if isinstance(tokens.get("cache"), dict) else {}
            if client == "opencode":
                result.input_tokens = (result.input_tokens or 0) + int(tokens.get("input") or 0)
                result.output_tokens = (result.output_tokens or 0) + int(tokens.get("output") or 0)
                result.cached_input_tokens = (result.cached_input_tokens or 0) + int(cache.get("read") or 0)
                step_total = tokens.get("total")
                if isinstance(step_total, int) and not isinstance(step_total, bool):
                    result.total_tokens = (result.total_tokens or 0) + step_total
                cost = part.get("cost")
                if isinstance(cost, (int, float)) and not isinstance(cost, bool):
                    costs.append(float(cost))
            else:
                usage_candidates.append(
                    {
                        "input": tokens.get("input"),
                        "output": tokens.get("output"),
                        "cache_read": cache.get("read"),
                        "cost": part.get("cost"),
                    }
                )
        for usage in usage_candidates:
            if not isinstance(usage, dict):
                continue
            input_tokens = _integer(usage, ("input_tokens", "input", "prompt_tokens"))
            cached = _integer(usage, ("cached_input_tokens", "cache_read", "cached"))
            output_tokens = _integer(usage, ("output_tokens", "output", "completion_tokens"))
            if input_tokens is not None:
                result.input_tokens = input_tokens
            if cached is not None:
                result.cached_input_tokens = cached
            if output_tokens is not None:
                result.output_tokens = output_tokens
            cost = _float(usage, ("cost", "cost_usd", "total_cost"))
            if cost is not None:
                costs.append(cost)
        direct_cost = _float(event, ("cost_usd", "total_cost_usd"))
        if direct_cost is not None:
            costs.append(direct_cost)
    if messages:
        result.final_text = messages[-1]
    if costs:
        result.cost_usd = sum(costs) if client == "opencode" else max(costs)
    if tool_spans:
        result.tool_duration_ms = sum(end - start for start, end, _ in tool_spans.values())
        noema_tools = {
            "get_instructions",
            "cortex_usage",
            "recall_context",
            "search_traces",
            "list_traces",
            "get_trace",
            "create_trace",
            "update_trace",
        }
        result.noema_tool_duration_ms = sum(
            end - start
            for start, end, name in tool_spans.values()
            if name.rsplit("/", 1)[-1].removeprefix("noema_") in noema_tools
        )
    result.timed_span_count = len(timed_spans)
    gaps = [
        start - previous_end
        for (_, previous_end), (start, _) in zip(timed_spans, timed_spans[1:])
        if start >= previous_end
    ]
    if gaps:
        result.max_inter_event_gap_ms = max(gaps)
    if client == "opencode":
        result.noncached_input_tokens = result.input_tokens
        if result.total_tokens is None and (
            result.input_tokens is not None
            or result.cached_input_tokens is not None
            or result.output_tokens is not None
        ):
            result.total_tokens = (
                (result.input_tokens or 0)
                + (result.cached_input_tokens or 0)
                + (result.output_tokens or 0)
            )
    else:
        if result.input_tokens is not None:
            result.noncached_input_tokens = max(
                0, result.input_tokens - (result.cached_input_tokens or 0)
            )
        if result.input_tokens is not None or result.output_tokens is not None:
            result.total_tokens = (result.input_tokens or 0) + (result.output_tokens or 0)
    return result


def extract_answer_payload(text: str) -> dict[str, Any]:
    stripped = text.strip()
    if stripped.startswith("```"):
        stripped = re.sub(r"^```(?:json)?\s*", "", stripped)
        stripped = re.sub(r"\s*```$", "", stripped)
    try:
        payload = json.loads(stripped)
    except json.JSONDecodeError:
        begin, end = stripped.find("{"), stripped.rfind("}")
        if begin < 0 or end <= begin:
            raise BenchmarkError("final response does not contain a JSON object")
        try:
            payload = json.loads(stripped[begin : end + 1])
        except json.JSONDecodeError as error:
            raise BenchmarkError(f"invalid final response JSON: {error.msg}") from error
    if not isinstance(payload, dict) or not isinstance(payload.get("answers"), list):
        raise BenchmarkError("final response JSON must contain an answers array")
    for answer in payload["answers"]:
        if not isinstance(answer, dict):
            raise BenchmarkError("every answer must be an object")
        required = {"question_id", "answer", "rationale", "provenance", "abstain"}
        if not required.issubset(answer):
            raise BenchmarkError("every answer must contain all required fields")
        if not isinstance(answer["question_id"], str) or not isinstance(answer["answer"], str):
            raise BenchmarkError("question_id and answer must be strings")
        if answer["rationale"] is not None and not isinstance(answer["rationale"], str):
            raise BenchmarkError("rationale must be a string or null")
        if not isinstance(answer["provenance"], list) or not all(isinstance(item, str) for item in answer["provenance"]):
            raise BenchmarkError("provenance must be an array of strings")
        if not isinstance(answer["abstain"], bool):
            raise BenchmarkError("abstain must be boolean")
    return payload


def _f1(expected: set[str], observed: set[str]) -> float:
    if not expected:
        return 1.0 if not observed else 0.0
    true_positive = len(expected & observed)
    false_positive = len(observed - expected)
    false_negative = len(expected - observed)
    denominator = 2 * true_positive + false_positive + false_negative
    return 2 * true_positive / denominator if denominator else 1.0


def score_case(case: Case, final_text: str, all_corpus_tokens: set[str]) -> dict[str, Any]:
    dimension_values: dict[str, list[float]] = {name: [] for name in DIMENSIONS}
    stale_count = hallucination_count = leakage_count = 0
    expected_questions = {question.question_id: question for question in case.questions}
    try:
        payload = extract_answer_payload(final_text)
        raw_answers = payload["answers"]
    except BenchmarkError as error:
        return {
            "valid_response": False,
            "score_error": str(error),
            "macro_f1": 0.0,
            "exact_accuracy": 0.0,
            "rationale_accuracy": 0.0,
            "provenance_accuracy": 0.0,
            "dimension_scores": {name: 0.0 for name in DIMENSIONS},
            "stale_count": 0,
            "hallucination_count": len(case.questions),
            "leakage_count": 0,
            "answer_count": len(case.questions),
        }
    answers: dict[str, Mapping[str, Any]] = {}
    for answer in raw_answers:
        if isinstance(answer, dict) and isinstance(answer.get("question_id"), str):
            answers[answer["question_id"]] = answer
    if len(answers) != len(raw_answers) or set(answers) != set(expected_questions):
        return {
            "valid_response": False,
            "score_error": "answer question IDs must match the requested questions exactly once",
            "macro_f1": 0.0,
            "exact_accuracy": 0.0,
            "rationale_accuracy": 0.0,
            "provenance_accuracy": 0.0,
            "dimension_scores": {name: 0.0 for name in DIMENSIONS},
            "stale_count": 0,
            "hallucination_count": len(case.questions),
            "leakage_count": 0,
            "answer_count": len(case.questions),
        }
    for question_id, question in expected_questions.items():
        answer = answers.get(question_id, {})
        answer_text = str(answer.get("answer", ""))
        rationale_text = str(answer.get("rationale") or "")
        provenance_value = answer.get("provenance", [])
        provenance_text = " ".join(str(value) for value in provenance_value) if isinstance(provenance_value, list) else str(provenance_value)
        combined = " ".join((answer_text, rationale_text, provenance_text))
        observed_tokens = set(TOKEN_RE.findall(combined))
        expected_values = set(question.expected_values)
        unexpected_tokens = observed_tokens - expected_values
        forbidden = set(question.forbidden_values)
        abstained = answer.get("abstain") is True
        exact = 1.0 if question.should_abstain and abstained else _f1(expected_values, observed_tokens & all_corpus_tokens)
        rationale = (
            sum(marker in rationale_text for marker in question.expected_rationales)
            / len(question.expected_rationales)
            if question.expected_rationales
            else 1.0
        )
        provenance = (
            sum(marker in provenance_text for marker in question.expected_provenance)
            / len(question.expected_provenance)
            if question.expected_provenance
            else 1.0
        )
        leak_sensitive = any(
            dimension in {"scope_isolation", "distractor_resistance"}
            for dimension in question.dimensions
        )
        leaked = leak_sensitive and bool(forbidden & observed_tokens)
        stale = any("-OLD-" in token for token in observed_tokens)
        hallucinated = (question.should_abstain and not abstained) or bool(unexpected_tokens - forbidden)
        stale_count += int(stale)
        leakage_count += int(leaked)
        hallucination_count += int(hallucinated)
        for dimension in question.dimensions:
            if dimension == "exact_recall":
                value = exact
            elif dimension == "rationale":
                value = rationale
            elif dimension == "provenance":
                value = provenance
            elif dimension in {"scope_isolation", "distractor_resistance"}:
                value = 0.0 if leaked or hallucinated else 1.0
            elif dimension == "outside_work_retrieval":
                value = exact
            elif dimension == "supersession":
                value = min(exact, rationale, 0.0 if stale else 1.0)
            elif dimension == "synthesis":
                value = min(exact, rationale, provenance)
            elif dimension == "abstention":
                value = 1.0 if abstained and not answer_text.strip() else 0.0
            else:
                raise AssertionError(dimension)
            dimension_values[dimension].append(value)
    dimension_scores = {
        name: (statistics.fmean(values) if values else None)
        for name, values in dimension_values.items()
    }
    applicable = [value for value in dimension_scores.values() if value is not None]
    return {
        "valid_response": True,
        "score_error": None,
        "macro_f1": statistics.fmean(applicable) if applicable else 0.0,
        "exact_accuracy": dimension_scores["exact_recall"],
        "rationale_accuracy": dimension_scores["rationale"],
        "provenance_accuracy": dimension_scores["provenance"],
        "dimension_scores": dimension_scores,
        "stale_count": stale_count,
        "hallucination_count": hallucination_count,
        "leakage_count": leakage_count,
        "answer_count": len(case.questions),
    }


def snapshot_path(path: Path, include_bytes: bool = True) -> dict[str, Any]:
    try:
        stat_result = path.lstat()
    except FileNotFoundError:
        return {"path": str(path), "kind": "absent"}
    mode = stat_result.st_mode & 0o7777
    if path.is_symlink():
        target = os.readlink(path)
        return {
            "path": str(path), "kind": "symlink", "mode": mode,
            "target_b64": base64.b64encode(os.fsencode(target)).decode(),
            "sha256": sha256_bytes(os.fsencode(target)),
        }
    if path.is_file():
        data = path.read_bytes()
        result = {"path": str(path), "kind": "file", "mode": mode, "sha256": sha256_bytes(data), "size": len(data)}
        if include_bytes:
            result["bytes_b64"] = base64.b64encode(data).decode()
        return result
    if path.is_dir():
        entries = [snapshot_path(child, include_bytes) for child in sorted(path.iterdir())]
        digest = sha256_bytes(json.dumps(_snapshot_fingerprint(entries), sort_keys=True).encode())
        return {"path": str(path), "kind": "directory", "mode": mode, "sha256": digest, "entries": entries}
    return {"path": str(path), "kind": "other", "mode": mode}


def _snapshot_fingerprint(value: Any) -> Any:
    if isinstance(value, list):
        return [_snapshot_fingerprint(item) for item in value]
    if isinstance(value, dict):
        return {
            key: _snapshot_fingerprint(child)
            for key, child in value.items()
            if key not in {"bytes_b64", "target_b64"}
        }
    return value


def snapshot_matches(snapshot: Mapping[str, Any]) -> bool:
    current = snapshot_path(Path(snapshot["path"]), include_bytes=False)
    keys = ("kind", "mode", "sha256", "size")
    return all(current.get(key) == snapshot.get(key) for key in keys if key in snapshot or key in current)


def safe_restore_absent(path: Path, run_id: str, allowed_parents: Sequence[Path]) -> None:
    resolved = path.resolve()
    if run_id not in resolved.parts and run_id not in resolved.name:
        raise BenchmarkError(f"refusing to remove path without run id component: {resolved}")
    if not any(resolved == parent.resolve() / run_id or parent.resolve() in resolved.parents for parent in allowed_parents):
        raise BenchmarkError(f"refusing to remove path outside benchmark roots: {resolved}")
    if path.is_symlink() or path.is_file():
        path.unlink()
    elif path.is_dir():
        shutil.rmtree(path)


def percentile(values: Sequence[float], quantile: float) -> float:
    if not values:
        return math.nan
    ordered = sorted(values)
    position = (len(ordered) - 1) * quantile
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def paired_bootstrap_interval(differences: Sequence[float], samples: int, seed: int = 20260904) -> tuple[float, float]:
    if not differences:
        return math.nan, math.nan
    rng = random.Random(seed)
    draws = [
        statistics.fmean(rng.choice(differences) for _ in differences)
        for _ in range(samples)
    ]
    return percentile(draws, 0.025), percentile(draws, 0.975)


def _mean(rows: Sequence[Mapping[str, Any]], key: str) -> float | None:
    values = [float(row[key]) for row in rows if row.get(key) is not None]
    return statistics.fmean(values) if values else None


def _rate(rows: Sequence[Mapping[str, Any]], numerator: str, denominator: str) -> float:
    den = sum(int(row.get(denominator, 0)) for row in rows)
    return sum(int(row.get(numerator, 0)) for row in rows) / den if den else 0.0


def summarize_rows(rows: Sequence[Mapping[str, Any]], bootstrap_samples: int) -> dict[str, Any]:
    by_condition = {condition: [row for row in rows if row["condition"] == condition] for condition in CONDITIONS}
    summary: dict[str, Any] = {"retrieval_turns": len(rows), "conditions": {}}
    for condition, condition_rows in by_condition.items():
        latencies = [float(row["latency_seconds"]) for row in condition_rows if row.get("latency_seconds") is not None]
        total_tokens = [int(row["total_tokens"]) for row in condition_rows if row.get("total_tokens") is not None]
        noncached_input_tokens = [
            int(row["noncached_input_tokens"])
            for row in condition_rows
            if row.get("noncached_input_tokens") is not None
        ]
        cached_input_tokens = [
            int(row["cached_input_tokens"])
            for row in condition_rows
            if row.get("cached_input_tokens") is not None
        ]
        output_tokens = [int(row["output_tokens"]) for row in condition_rows if row.get("output_tokens") is not None]
        costs = [float(row["cost_usd"]) for row in condition_rows if row.get("cost_usd") is not None]
        complete_costs = bool(condition_rows) and len(costs) == len(condition_rows)
        summary["conditions"][condition] = {
            "turns": len(condition_rows),
            "macro_f1": _mean(condition_rows, "macro_f1"),
            "exact_accuracy": _mean(condition_rows, "exact_accuracy"),
            "rationale_accuracy": _mean(condition_rows, "rationale_accuracy"),
            "provenance_accuracy": _mean(condition_rows, "provenance_accuracy"),
            "stale_rate": _rate(condition_rows, "stale_count", "answer_count"),
            "hallucination_rate": _rate(condition_rows, "hallucination_count", "answer_count"),
            "scope_leakage_rate": _rate(condition_rows, "leakage_count", "answer_count"),
            "tool_calls_mean": _mean(condition_rows, "tool_call_count"),
            "tokens_median": statistics.median(total_tokens) if total_tokens else None,
            "total_tokens_median": statistics.median(total_tokens) if total_tokens else None,
            "noncached_input_tokens_median": statistics.median(noncached_input_tokens) if noncached_input_tokens else None,
            "cached_input_tokens_median": statistics.median(cached_input_tokens) if cached_input_tokens else None,
            "output_tokens_median": statistics.median(output_tokens) if output_tokens else None,
            "cost_observed_turns": len(costs),
            "cost_median_usd": statistics.median(costs) if complete_costs else None,
            "latency_p50_seconds": percentile(latencies, 0.50) if latencies else None,
            "latency_p95_seconds": percentile(latencies, 0.95) if latencies else None,
            "failed_turns": sum(row.get("status") != "completed" for row in condition_rows),
        }
    pairs: dict[tuple[Any, ...], dict[str, Mapping[str, Any]]] = {}
    for row in rows:
        key = (row["batch"], row["track"], row["direction"], row["scenario"])
        pairs.setdefault(key, {})[row["condition"]] = row
    complete_pairs = [pair for pair in pairs.values() if set(pair) == set(CONDITIONS)]
    differences = [float(pair["noema"]["macro_f1"]) - float(pair["native"]["macro_f1"]) for pair in complete_pairs]
    lower, upper = paired_bootstrap_interval(differences, bootstrap_samples)
    summary["paired"] = {
        "pairs": len(complete_pairs),
        "noema_minus_native": statistics.fmean(differences) if differences else None,
        "bootstrap_95_low": lower if differences else None,
        "bootstrap_95_high": upper if differences else None,
        "descriptive_only": True,
    }
    inside_differences = [
        float(pair["noema"]["macro_f1"]) - float(pair["native"]["macro_f1"])
        for key, pair in pairs.items()
        if key[3] != "outside_work" and set(pair) == set(CONDITIONS)
    ]
    summary["paired"]["inside_work_noema_minus_native"] = (
        statistics.fmean(inside_differences) if inside_differences else None
    )
    scenario_rows: dict[str, Any] = {}
    for scenario in SCENARIOS:
        scenario_rows[scenario] = {
            condition: _mean(
                [row for row in by_condition[condition] if row["scenario"] == scenario],
                "macro_f1",
            )
            for condition in CONDITIONS
        }
    summary["scenarios"] = scenario_rows
    subset_names = {
        "boundary": {"sibling_workspace", "outside_work"},
        "update": {"supersession"},
        "scoping": {"scope_distractor_abstention"},
    }
    subsets: dict[str, Any] = {}
    for name, scenarios in subset_names.items():
        subset_pairs = [pair for key, pair in pairs.items() if key[3] in scenarios and set(pair) == set(CONDITIONS)]
        diffs = [float(pair["noema"]["macro_f1"]) - float(pair["native"]["macro_f1"]) for pair in subset_pairs]
        noema_rows = [pair["noema"] for pair in subset_pairs]
        subsets[name] = {
            "pairs": len(subset_pairs),
            "noema_macro_f1": _mean(noema_rows, "macro_f1"),
            "advantage": statistics.fmean(diffs) if diffs else None,
        }
    summary["subsets"] = subsets
    return summary


def checkpoint_decision(summary: Mapping[str, Any]) -> dict[str, Any]:
    native = summary["conditions"]["native"]
    noema = summary["conditions"]["noema"]
    paired = summary["paired"]
    advantage = paired.get("noema_minus_native")
    upper = paired.get("bootstrap_95_high")
    lower = paired.get("bootstrap_95_low")
    subsets = summary["subsets"]
    subset_advantages = [value.get("advantage") for value in subsets.values() if value.get("advantage") is not None]
    rows = int(summary["retrieval_turns"])
    integration_unreliable = native["failed_turns"] > 0 or noema["failed_turns"] > 0
    inside_advantage = paired.get("inside_work_noema_minus_native")
    harm = (
        (inside_advantage is not None and inside_advantage < -0.05)
        or noema["scope_leakage_rate"] > 0.10
        or noema["hallucination_rate"] > 0.10
        or integration_unreliable
    )
    futility = (
        advantage is not None and upper is not None
        and advantage < 0.03 and upper < 0.10
        and not any(value >= 0.15 for value in subset_advantages)
    )
    natural_eligible = (
        advantage is not None and upper is not None
        and advantage > 0.0 and (upper >= 0.10 or any(value >= 0.15 for value in subset_advantages))
        and not harm
    )
    success_checks = {
        "minimum_192_turns": rows >= 192,
        "overall_advantage": advantage is not None and advantage >= 0.10 and lower is not None and lower > 0.0,
        "boundary": subsets["boundary"]["noema_macro_f1"] is not None and subsets["boundary"]["noema_macro_f1"] >= 0.90 and subsets["boundary"]["advantage"] >= 0.20,
        "update": subsets["update"]["noema_macro_f1"] is not None and subsets["update"]["noema_macro_f1"] >= 0.90 and subsets["update"]["advantage"] >= 0.20,
        "scoping": subsets["scoping"]["noema_macro_f1"] is not None and subsets["scoping"]["noema_macro_f1"] >= 0.90 and subsets["scoping"]["advantage"] >= 0.20,
        "provenance": noema["provenance_accuracy"] is not None and noema["provenance_accuracy"] >= 0.95,
        "stale_and_leakage": noema["stale_rate"] <= 0.05 and noema["scope_leakage_rate"] <= 0.05,
    }
    inside_scenarios = [scenario for scenario in summary["scenarios"] if scenario != "outside_work"]
    strongest_scenario = max(
        inside_scenarios,
        key=lambda scenario: summary["scenarios"][scenario]["native"]
        if summary["scenarios"][scenario]["native"] is not None else -1.0,
    )
    strongest_native = summary["scenarios"][strongest_scenario]["native"]
    strongest_noema = summary["scenarios"][strongest_scenario]["noema"]
    success_checks["strongest_inside_work_scenario"] = (
        strongest_native is not None and strongest_noema is not None
        and strongest_noema >= 0.95 and strongest_noema >= strongest_native - 0.03
    )
    latency_ok = (
        native["latency_p95_seconds"] is not None and noema["latency_p95_seconds"] is not None
        and noema["latency_p95_seconds"] <= 2 * native["latency_p95_seconds"]
    )
    token_ok = (
        native["tokens_median"] is not None and noema["tokens_median"] is not None
        and noema["tokens_median"] <= 1.5 * native["tokens_median"]
    )
    cost_ok = (
        native["cost_median_usd"] is not None and noema["cost_median_usd"] is not None
        and noema["cost_median_usd"] <= 1.5 * native["cost_median_usd"]
    )
    success_checks["efficiency"] = latency_ok and token_ok and cost_ok
    success_candidate = all(success_checks.values())
    if harm:
        recommendation = "stop_harm_or_instability"
        conclusion = "Noema showed harm or unreliable benchmark behavior."
    elif futility:
        recommendation = "stop_futility"
        conclusion = "No demonstrated meaningful advantage."
    elif success_candidate:
        recommendation = "success_candidate"
        conclusion = "The preregistered success thresholds are met; review raw evidence before any claim."
    else:
        recommendation = "continue"
        conclusion = "A meaningful advantage remains plausible, but evidence is incomplete."
    return {
        "recommendation": recommendation,
        "conclusion": conclusion,
        "natural_track_eligible": natural_eligible,
        "integration_unreliable": integration_unreliable,
        "success_checks": success_checks,
    }


def sanitized_row(case: Case, parsed: ParsedEvents, score: Mapping[str, Any], *, status: str, latency_seconds: float, error_code: str | None) -> dict[str, Any]:
    return {
        "schema_version": SCHEMA_VERSION,
        "case_id": case.case_id,
        "batch": case.batch,
        "track": case.track,
        "condition": case.condition,
        "direction": case.direction,
        "receiver": case.receiver,
        "scenario": case.scenario,
        "corpus_variant": case.corpus_variant,
        "status": status,
        "error_code": error_code,
        "valid_response": score["valid_response"],
        "macro_f1": score["macro_f1"],
        "exact_accuracy": score["exact_accuracy"],
        "rationale_accuracy": score["rationale_accuracy"],
        "provenance_accuracy": score["provenance_accuracy"],
        "dimension_scores": score["dimension_scores"],
        "stale_count": score["stale_count"],
        "hallucination_count": score["hallucination_count"],
        "leakage_count": score["leakage_count"],
        "answer_count": score["answer_count"],
        "input_tokens": parsed.input_tokens,
        "cached_input_tokens": parsed.cached_input_tokens,
        "noncached_input_tokens": parsed.noncached_input_tokens,
        "output_tokens": parsed.output_tokens,
        "total_tokens": parsed.total_tokens,
        "cost_usd": parsed.cost_usd,
        "tool_call_count": len(parsed.tool_names),
        "noema_bootstrap_observed": any("get_instructions" in name for name in parsed.tool_names),
        "tool_duration_ms": parsed.tool_duration_ms,
        "noema_tool_duration_ms": parsed.noema_tool_duration_ms,
        "max_inter_event_gap_ms": parsed.max_inter_event_gap_ms,
        "timed_span_count": parsed.timed_span_count,
        "latency_seconds": round(latency_seconds, 6),
    }


def write_tsv(path: Path, rows: Sequence[Mapping[str, Any]]) -> None:
    fields = [
        "case_id", "batch", "track", "condition", "direction", "receiver", "scenario",
        "corpus_variant", "status", "error_code", "macro_f1", "exact_accuracy",
        "rationale_accuracy", "provenance_accuracy", "stale_count", "hallucination_count",
        "leakage_count", "answer_count", "input_tokens", "cached_input_tokens",
        "noncached_input_tokens", "output_tokens", "total_tokens", "cost_usd",
        "tool_call_count", "noema_bootstrap_observed",
        "tool_duration_ms", "noema_tool_duration_ms", "max_inter_event_gap_ms",
        "timed_span_count",
        "latency_seconds",
    ]
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent, text=True)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as handle:
            writer = csv.DictWriter(handle, fieldnames=fields, delimiter="\t", extrasaction="ignore")
            writer.writeheader()
            writer.writerows(rows)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o644)
        os.replace(temporary, path)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
