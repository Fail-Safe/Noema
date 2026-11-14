#!/usr/bin/env python3
"""Independent synthetic draws and corpus-cluster descriptive reporting.

Generator version 1 is deliberately separate from the historical benchmark.
The plan and generated contents are private; summaries whitelist aggregates only.
"""
from __future__ import annotations

import hashlib
import math
import random
import re
import statistics
from collections import Counter
from typing import Any, Mapping, Sequence

from omarchy_eval_lib import (
    BenchmarkError, Case, DIRECTIONS, Question, Record, SCENARIOS, percentile,
)

SIZES = {"small": 60, "medium": 180, "large": 600}
_ARMS = ("native", "markdown", "noema")
_SUBSETS = {
    "boundary": {"sibling_workspace", "outside_work"},
    "update": {"supersession"},
    "scoping": {"scope_distractor_abstention"},
    "same_workspace": {"same_workspace_exact", "same_workspace_rationale"},
}
_SCORES = ("macro_f1", "exact_accuracy", "rationale_accuracy", "provenance_accuracy")
_SAFETY = {"stale_rate": "stale_count", "hallucination_rate": "hallucination_count",
           "scope_leakage_rate": "leakage_count"}
_ADJECTIVES = ("amber", "silver", "quiet", "cobalt", "cedar", "copper", "velvet", "ivory",
               "crimson", "indigo", "misty", "golden", "ochre", "pale", "verdant", "violet")
_NOUNS = ("orchard", "harbor", "quarry", "meadow", "summit", "garden", "lagoon", "grove",
          "marsh", "valley", "ridge", "inlet", "beacon", "terrace", "estuary", "canyon")
_TEAMS = ("intake", "dispatch", "review", "assembly", "routing", "archive", "survey", "quality")
_TOPICS = ("cooling schedule", "handoff policy", "inspection interval", "packing method",
           "alert threshold", "retry budget", "retention window", "delivery cadence",
           "routing preference", "calibration rule", "escalation order", "release criterion")
_REASONS = ("reduces overnight queueing", "avoids duplicate handoffs", "keeps fragile batches stable",
            "preserves audit continuity", "reduces noisy alerts", "matches the revised staffing window",
            "limits unnecessary rework", "accommodates the seasonal load", "keeps calibration consistent")


def _digest(*parts: str) -> str:
    return hashlib.sha256("\0".join(parts).encode()).hexdigest()


def _rng(*parts: str) -> random.Random:
    return random.Random(int(_digest(*parts), 16))


def _validate_spec(spec: Mapping[str, Any]) -> int:
    cid = spec.get("corpus_id")
    if not isinstance(cid, str) or not re.fullmatch(r"(?:corpus|pilot)-\d{3}", cid):
        raise BenchmarkError("independent corpus requires a safe opaque corpus ID")
    index = int(cid.rsplit("-", 1)[1])
    if index < 1 or not isinstance(spec.get("seed"), str) or not spec["seed"]:
        raise BenchmarkError("independent corpus requires a positive ID and private seed")
    if not isinstance(spec.get("stratum"), str) or spec["stratum"] not in SIZES or type(spec.get("record_count")) is not int:
        raise BenchmarkError("independent corpus has an invalid stratum or count")
    if spec["record_count"] != SIZES[spec["stratum"]]:
        raise BenchmarkError("independent corpus size disagrees with its stratum")
    return index


def build_plan(master_seed: str) -> dict[str, Any]:
    """Create 30 independent domain-separated draws; do not expose this plan publicly."""
    if not isinstance(master_seed, str) or not master_seed:
        raise BenchmarkError("independent plan requires a nonempty master seed")
    specs = []
    for stratum, size in SIZES.items():
        for _ in range(10):
            cid = f"corpus-{len(specs) + 1:03d}"
            specs.append({"corpus_id": cid, "seed": _digest("independent-v1-holdout", master_seed, cid),
                          "stratum": stratum, "record_count": size})
    _rng("independent-v1-order", master_seed).shuffle(specs)
    return {"schema_version": 1, "corpora": specs, "expected_turns": 1080}


def build_smoke_specs() -> list[dict[str, Any]]:
    """Separate deterministic pilot domain, never part of the 1080-turn plan."""
    return [{"corpus_id": f"pilot-{index:03d}",
             "seed": _digest("independent-v1-pilot-only", str(index)),
             "stratum": stratum, "record_count": size}
            for index, (stratum, size) in enumerate(SIZES.items(), 1)]


def _variant(spec: Mapping[str, Any], direction: str) -> str:
    index = _validate_spec(spec)
    if direction not in DIRECTIONS:
        raise BenchmarkError("independent corpus has an unrecognized direction")
    return ("alpha", "beta")[(index - 1 + DIRECTIONS.index(direction)) % 2]


def _letters(hex_digest: str) -> str:
    # Hex-to-letters is injective and satisfies the established value-token grammar.
    return "".join(chr(ord("A") + int(char, 16)) for char in hex_digest)


def _draw(spec: Mapping[str, Any], direction: str) -> tuple[list[Record], dict[str, Any]]:
    variant = _variant(spec, direction)
    rng = _rng("independent-v1-records", spec["seed"], spec["corpus_id"], direction)
    records: list[Record] = []
    pools: dict[str, Any] = {"gp": [], "dc": [], "pa": [], "pb": [], "chains": [], "scopes": []}
    used_entities: set[str] = set()

    def entity() -> str:
        while True:
            name = f"{rng.choice(_ADJECTIVES)} {rng.choice(_NOUNS)} {rng.choice(_TEAMS)} team"
            if name not in used_entities:
                used_entities.add(name)
                return name

    def add(name: str, topic: str, prefix: str, scope: str, *, current: bool = True,
            predecessor: Record | None = None, revision: int = 1) -> Record:
        serial = len(records)
        salt = _digest("independent-v1-marker", spec["seed"], spec["corpus_id"], direction, str(serial))
        suffix = _letters(salt)
        state = ("CURRENT" if current else "OLD") if prefix == "su" else "VALUE"
        value = f"{variant.upper()}-{prefix.upper()}-{serial % 100:02d}-{state}-{suffix}"
        category = {"gp": "global_preference", "dc": "decision", "pa": "project_a_fact",
                    "pb": "project_b_fact", "su": "supersession"}[prefix]
        record = Record(
            record_id=f"{name} / {topic} / {scope} / revision {revision}",
            category=category, scope=scope, value=value,
            rationale=f"RATIONALE-{suffix}: {rng.choice(_REASONS)}.",
            provenance=f"SOURCE-{suffix}", current=current,
            supersedes=predecessor.record_id if predecessor else None,
        )
        records.append(record)
        if prefix != "su":
            pools[prefix].append(record)
        return record

    # Variable history lengths and family membership, not a relabelled fixed corpus.
    for _ in range(rng.randint(3, 5)):
        name, topic = entity(), rng.choice(_TOPICS)
        length = rng.randint(3, 5)
        chain: list[Record] = []
        for revision in range(1, length + 1):
            chain.append(add(name, topic, "su", "global", current=revision == length,
                             predecessor=chain[-1] if chain else None, revision=revision))
        pools["chains"].append(chain)
    # Same lexical entity/topic in distinct scopes; neither scope is an access boundary.
    for _ in range(rng.randint(3, 5)):
        name, topic = entity(), rng.choice(_TOPICS)
        pair = (add(name, topic, "pa", "project-a"), add(name, topic, "pb", "project-b"))
        pools["scopes"].append(pair)
    for prefix, scope in (("gp", "global"), ("dc", "global"), ("pa", "project-a"), ("pb", "project-b")):
        for _ in range(4):
            add(entity(), rng.choice(_TOPICS), prefix, scope)
    while len(records) < spec["record_count"]:
        prefix = rng.choices(("gp", "dc", "pa", "pb"), weights=(2, 2, 3, 3))[0]
        add(entity(), rng.choice(_TOPICS), prefix,
            {"gp": "global", "dc": "global", "pa": "project-a", "pb": "project-b"}[prefix])
    if len(records) != spec["record_count"]:
        raise AssertionError("generator core exceeds the contracted corpus size")
    rng.shuffle(records)
    for key in ("gp", "dc", "pa", "pb", "chains", "scopes"):
        rng.shuffle(pools[key])
    return records, pools


def records_for_case(spec: Mapping[str, Any], case: Case) -> list[Record]:
    """All arms of a direction see the same complete literal corpus."""
    if case.condition not in _ARMS or case.corpus_variant != _variant(spec, case.direction):
        raise BenchmarkError("case does not match its independent corpus variant or arm")
    prefix = f"{spec['corpus_id']}-{_digest('independent-v1-case', spec['seed'])[:16]}-"
    if not case.case_id.startswith(prefix):
        raise BenchmarkError("case does not belong to the selected independent corpus")
    return _draw(spec, case.direction)[0]


def _cue(record: Record) -> str:
    name, topic, _, _ = record.record_id.split(" / ")
    return f"the {topic} for the {name}"


def _questions(spec: Mapping[str, Any], direction: str, pools: Mapping[str, Any]) -> dict[str, tuple[Question, ...]]:
    rng = _rng("independent-v1-tasks", spec["seed"], spec["corpus_id"], direction)
    wording = rng.choice(("Which setting currently governs", "What is the current choice for",
                          "Which recorded setting applies to"))

    def ask(qid: str, targets: Sequence[Record], dimensions: Sequence[str], *,
            forbidden: Sequence[Record] = (), scope: str = "global") -> Question:
        cues = " and ".join(_cue(record) for record in targets)
        return Question(
            question_id=qid,
            text=(f"{wording} {cues}, in {scope}? Return the exact synthetic value(s), "
                  "the rationale marker(s) with the reason, and source provenance marker(s). "
                  "Use only the current revision and the requested scope, even if similarly named entries exist."),
            expected_values=tuple(record.value for record in targets),
            expected_rationales=tuple(record.rationale.split(":", 1)[0] for record in targets),
            expected_provenance=tuple(record.provenance for record in targets),
            forbidden_values=tuple(record.value for record in forbidden), dimensions=tuple(dimensions),
        )

    def pick(key: str, count: int = 1) -> list[Record]:
        return rng.sample(pools[key], count)

    chain = rng.choice(pools["chains"])
    pair = rng.choice(pools["scopes"])
    unknown_entity = f"{rng.choice(_ADJECTIVES)} {rng.choice(_NOUNS)} observatory team"
    unknown_topic = rng.choice(_TOPICS)
    return {
        "same_workspace_exact": (
            ask("q-global", pick("gp"), ("exact_recall", "provenance")),
            ask("q-local", pick("pa"), ("exact_recall", "provenance", "scope_isolation"), scope="project-a"),
        ),
        "same_workspace_rationale": (
            ask("q-decision", pick("dc"), ("exact_recall", "rationale", "provenance")),
            ask("q-synthesis", pick("gp") + pick("dc"), ("exact_recall", "rationale", "provenance", "synthesis")),
        ),
        "sibling_workspace": (
            ask("q-other-project", pick("pa"), ("exact_recall", "provenance", "outside_work_retrieval"), scope="project-a"),
            ask("q-shared", pick("gp"), ("exact_recall", "provenance")),
        ),
        "outside_work": (
            ask("q-preference", pick("gp"), ("exact_recall", "provenance", "outside_work_retrieval")),
            ask("q-outside-decision", pick("dc"), ("exact_recall", "rationale", "provenance", "outside_work_retrieval")),
        ),
        "supersession": (ask("q-revision", [chain[-1]], ("exact_recall", "rationale", "provenance", "supersession"),
                             forbidden=chain[:-1]),),
        "scope_distractor_abstention": (
            ask("q-scope", [pair[1]], ("exact_recall", "provenance", "scope_isolation", "distractor_resistance"),
                forbidden=[pair[0]], scope="project-b"),
            Question("q-unknown", f"What is the recorded {unknown_topic} for the {unknown_entity} in project-b? "
                     "Return the exact value, rationale marker and provenance if recorded; otherwise abstain with an empty answer.",
                     dimensions=("abstention", "distractor_resistance"), should_abstain=True),
        ),
    }


def build_independent_cases(spec: Mapping[str, Any]) -> list[Case]:
    """36 balanced turns; tasks and literal records are identical within each triple."""
    index = _validate_spec(spec) - 1
    rng = _rng("independent-v1-case-order", spec["seed"], spec["corpus_id"])
    scenarios = list(SCENARIOS)
    rng.shuffle(scenarios)
    directions = DIRECTIONS[index % 2:] + DIRECTIONS[:index % 2]
    cases = []
    for direction_index, direction in enumerate(directions):
        _, pools = _draw(spec, direction)
        questions = _questions(spec, direction, pools)
        source, receiver = direction.split("_to_")
        for scenario_index, scenario in enumerate(scenarios):
            rotation = (index + direction_index + scenario_index) % 3
            for arm in _ARMS[rotation:] + _ARMS[:rotation]:
                role = {"sibling_workspace": "project-b", "outside_work": "outside",
                        "scope_distractor_abstention": "project-b"}.get(scenario, "project-a")
                cases.append(Case(
                    case_id=f"{spec['corpus_id']}-{_digest('independent-v1-case', spec['seed'])[:16]}-{direction}-{arm}-{scenario}", batch=1,
                    track="deterministic", condition=arm, direction=direction, receiver=receiver,
                    source=source, scenario=scenario, corpus_variant=_variant(spec, direction),
                    workspace_role=role, questions=questions[scenario],
                ))
    return cases


def _validate_plan(plan: Mapping[str, Any]) -> list[Mapping[str, Any]]:
    if type(plan.get("schema_version")) is not int or plan["schema_version"] != 1 or type(plan.get("expected_turns")) is not int or plan["expected_turns"] != 1080:
        raise BenchmarkError("independent report requires the registered 1080-turn plan")
    specs = plan.get("corpora")
    if not isinstance(specs, list) or len(specs) != 30:
        raise BenchmarkError("independent report requires exactly 30 corpus specifications")
    for spec in specs:
        if not isinstance(spec, Mapping):
            raise BenchmarkError("independent corpus specification is not a mapping")
        _validate_spec(spec)
    if {spec["corpus_id"] for spec in specs} != {f"corpus-{i:03d}" for i in range(1, 31)}:
        raise BenchmarkError("independent plan contains duplicate or unknown corpus IDs")
    if Counter(spec["stratum"] for spec in specs) != Counter({key: 10 for key in SIZES}):
        raise BenchmarkError("independent plan requires ten corpora in each size stratum")
    if len({spec["seed"] for spec in specs}) != 30:
        raise BenchmarkError("independent plan requires distinct corpus seeds")
    return specs


def _validate_rows(rows: Sequence[Mapping[str, Any]], specs: Sequence[Mapping[str, Any]]) -> dict[str, Case]:
    expected = {case.case_id: case for spec in specs for case in build_independent_cases(spec)}
    by_id = {spec["corpus_id"]: spec for spec in specs}
    seen: set[str] = set()
    for row in rows:
        cid, case_id = row.get("corpus_id"), row.get("case_id")
        if not isinstance(cid, str) or cid not in by_id or not isinstance(case_id, str) or case_id not in expected:
            raise BenchmarkError("independent rows contain an unknown corpus or case ID")
        if case_id in seen:
            raise BenchmarkError("independent rows contain a duplicate case ID")
        seen.add(case_id)
        case, spec = expected[case_id], by_id[cid]
        if not case_id.startswith(cid + "-"):
            raise BenchmarkError("independent row corpus and case IDs disagree")
        for key in ("batch", "track", "condition", "direction", "receiver", "scenario", "corpus_variant"):
            if type(row.get(key)) is not type(getattr(case, key)) or row[key] != getattr(case, key):
                raise BenchmarkError("independent row metadata disagrees with its planned case")
        if row.get("stratum") != spec["stratum"] or row.get("record_count") != spec["record_count"]:
            raise BenchmarkError("independent row size metadata disagrees with the plan")
        if row.get("status") not in ("completed", "failed"):
            raise BenchmarkError("independent rows require a completed or failed scored attempt")
        for field in _SCORES:
            value = row.get(field)
            applicable = field != "rationale_accuracy" or case.scenario in {
                "same_workspace_rationale", "outside_work", "supersession"}
            if field not in row or (applicable and value is None):
                raise BenchmarkError("independent rows require scored failures, not missing scores")
            if value is not None and (isinstance(value, bool) or not isinstance(value, (int, float))
                                      or not math.isfinite(value) or not 0 <= value <= 1):
                raise BenchmarkError("independent row scores must be finite values in 0..1")
        if type(row.get("answer_count")) is not int or row["answer_count"] != len(case.questions):
            raise BenchmarkError("independent row answer denominator disagrees with its planned case")
        for field in _SAFETY.values():
            value = row.get(field)
            if type(value) is not int or not 0 <= value <= row["answer_count"]:
                raise BenchmarkError("independent safety counts require valid nonnegative counts")
        for field in ("total_tokens", "noncached_input_tokens", "cached_input_tokens", "output_tokens",
                      "tool_call_count", "latency_seconds", "cost_usd"):
            value = row.get(field)
            if value is not None and (isinstance(value, bool) or not isinstance(value, (int, float))
                                      or not math.isfinite(value) or value < 0):
                raise BenchmarkError("independent observation metrics must be finite and nonnegative")
    return expected


def _mean(rows: Sequence[Mapping[str, Any]], field: str) -> float | None:
    values = [float(row[field]) for row in rows if row.get(field) is not None]
    return statistics.fmean(values) if values else None


def _condition(rows: Sequence[Mapping[str, Any]], available: bool) -> dict[str, Any]:
    metrics: dict[str, Any] = {"turns": len(rows), "failed_turns": sum(row["status"] == "failed" for row in rows)}
    metrics["failed_corpora"] = len({row["corpus_id"] for row in rows if row["status"] == "failed"})
    metrics.update({field: _mean(rows, field) if available else None for field in _SCORES})
    answers = sum(row["answer_count"] for row in rows)
    metrics["answer_count"] = answers
    for rate, count in _SAFETY.items():
        metrics[count] = sum(row[count] for row in rows)
        metrics[count.removesuffix("_count") + "_corpora"] = len({row["corpus_id"] for row in rows if row[count]})
        metrics[rate] = metrics[count] / answers if available and answers else None
    for field, output in (("total_tokens", "total_tokens"), ("noncached_input_tokens", "noncached_input_tokens"),
                          ("cached_input_tokens", "cached_input_tokens"), ("output_tokens", "output_tokens")):
        values = [row[field] for row in rows if row.get(field) is not None]
        metrics[output + "_median"] = statistics.median(values) if available and values else None
        metrics[output + "_observed_turns"] = len(values)
    metrics["tokens_median"] = metrics["total_tokens_median"]
    latencies = [row["latency_seconds"] for row in rows if row.get("latency_seconds") is not None]
    metrics["latency_observed_turns"] = len(latencies)
    for name, quantile in (("p50", .5), ("p95", .95)):
        metrics[f"latency_{name}_seconds"] = percentile(latencies, quantile) if available and latencies else None
    costs = [row["cost_usd"] for row in rows if row.get("cost_usd") is not None]
    metrics["cost_observed_turns"] = len(costs)
    metrics["cost_median_usd"] = statistics.median(costs) if available and rows and len(costs) == len(rows) else None
    metrics["tool_calls_mean"] = _mean(rows, "tool_call_count") if available else None
    return metrics


def _coverage(rows: Sequence[Mapping[str, Any]], expected_turns: int) -> dict[str, Any]:
    counts = Counter(row["corpus_id"] for row in rows)
    return {"planned_turns": expected_turns, "observed_turns": len(rows), "missing_turns": expected_turns - len(rows),
            "completed_turns": sum(row["status"] == "completed" for row in rows),
            "failed_turns": sum(row["status"] == "failed" for row in rows),
            "attempted_corpora": len(counts)}


def _bootstrap_ratio(numerators: Sequence[float], denominators: Sequence[float],
                     draws: Sequence[tuple[int, ...]]) -> list[float | None]:
    if not draws or not any(denominators):
        return [None, None]
    # In every complete design group, per-corpus denominators are constant. Keep
    # the ratio form for safety rates, whose answer counts can differ by task.
    values = []
    for draw in draws:
        denominator = sum(denominators[i] for i in draw)
        if denominator:
            values.append(sum(numerators[i] for i in draw) / denominator)
    return [percentile(values, .025), percentile(values, .975)] if values else [None, None]


def _group(rows: Sequence[Mapping[str, Any]], specs: Sequence[Mapping[str, Any]], expected_turns: int,
           campaign_complete: bool, samples: int, draw_cache: dict[tuple[str, ...], list[tuple[int, ...]]],
           *, intervals: bool = True) -> dict[str, Any]:
    complete = len(rows) == expected_turns and expected_turns > 0
    available = complete and campaign_complete
    ids = tuple(sorted(spec["corpus_id"] for spec in specs))
    by_corpus = {cid: [row for row in rows if row["corpus_id"] == cid] for cid in ids}
    counts = Counter(row["corpus_id"] for row in rows)
    turns_per_corpus = expected_turns // len(specs)
    cluster_counts = {stratum: sum(spec["stratum"] == stratum for spec in specs) for stratum in SIZES}
    result: dict[str, Any] = {
        "available": available, "complete": complete, "retrieval_turns": len(rows),
        "matched_triples": sum(count == 3 for count in Counter(
            (row["corpus_id"], row["direction"], row["scenario"]) for row in rows).values()),
        "cluster_count": len(ids), "observed_cluster_count": len(counts),
        "complete_cluster_count": sum(counts[cid] == turns_per_corpus for cid in ids),
        "failed_cluster_count": len({row["corpus_id"] for row in rows if row["status"] == "failed"}),
        "clusters_by_stratum": cluster_counts, "coverage": _coverage(rows, expected_turns),
        "conditions": {arm: _condition([row for row in rows if row["condition"] == arm], available) for arm in _ARMS},
    }
    for arm, metrics in result["conditions"].items():
        metrics["coverage"] = _coverage([row for row in rows if row["condition"] == arm], expected_turns // 3)
    decomposition: dict[str, Any] = {"metric": "macro_f1", "units": "percentage_points",
        "descriptive_only": True, "available": available, "bootstrap_95_pp": {}}
    for name, left, right in (("total", "native", "noema"), ("access", "native", "markdown"),
                              ("retrieval", "markdown", "noema")):
        decomposition[name + "_pp"] = (100 * (result["conditions"][right]["macro_f1"] -
                                                     result["conditions"][left]["macro_f1"])) if available else None
        decomposition["bootstrap_95_pp"][name] = [None, None]
    decomposition["residual_pp"] = 0.0 if available else None
    decomposition["bootstrap_95_pp"]["residual"] = [0.0, 0.0] if available else [None, None]
    result["decomposition"] = decomposition
    for metrics in result["conditions"].values():
        metrics["bootstrap_95"] = {field: [None, None] for field in (*_SCORES, *_SAFETY)}
    if not available or not intervals:
        return result
    if ids not in draw_cache:
        spec_strata = {spec["corpus_id"]: spec["stratum"] for spec in specs}
        strata = [[i for i, cid in enumerate(ids) if spec_strata[cid] == stratum] for stratum in SIZES]
        rng = _rng("independent-v1-cluster-bootstrap", *ids)
        draw_cache[ids] = [tuple(rng.choice(indices) for indices in strata for _ in indices) for _ in range(samples)]
    draws = draw_cache[ids]
    corpus_means: dict[str, list[float]] = {}
    for arm in _ARMS:
        arm_clusters = [[row for row in by_corpus[cid] if row["condition"] == arm] for cid in ids]
        corpus_means[arm] = [statistics.fmean(row["macro_f1"] for row in cluster) for cluster in arm_clusters]
        for field in _SCORES:
            values = [[float(row[field]) for row in cluster if row.get(field) is not None] for cluster in arm_clusters]
            result["conditions"][arm]["bootstrap_95"][field] = _bootstrap_ratio(
                [sum(v) for v in values], [len(v) for v in values], draws)
        for rate, count in _SAFETY.items():
            result["conditions"][arm]["bootstrap_95"][rate] = _bootstrap_ratio(
                [sum(row[count] for row in cluster) for cluster in arm_clusters],
                [sum(row["answer_count"] for row in cluster) for cluster in arm_clusters], draws)
    for name, left, right in (("total", "native", "noema"), ("access", "native", "markdown"),
                              ("retrieval", "markdown", "noema")):
        differences = [100 * (r - l) for l, r in zip(corpus_means[left], corpus_means[right])]
        decomposition["bootstrap_95_pp"][name] = _bootstrap_ratio(differences, [1.0] * len(ids), draws)
    return result


def summarize_independent(rows: Sequence[Mapping[str, Any]], plan: Mapping[str, Any],
                          bootstrap_samples: int = 10000) -> dict[str, Any]:
    """Stratified paired corpus bootstrap; incomplete campaigns have no headline.

    Failed rows keep their established scores. Resampling selects whole corpus
    clusters within size strata and keeps all arms/scenarios/directions together.
    Neither raw row text nor private plan fields can enter the returned report.
    """
    if type(bootstrap_samples) is not int or bootstrap_samples < 1:
        raise BenchmarkError("independent bootstrap samples must be a positive integer")
    specs = _validate_plan(plan)
    _validate_rows(rows, specs)
    complete = len(rows) == 1080
    cache: dict[tuple[str, ...], list[tuple[int, ...]]] = {}
    summary = _group(rows, specs, 1080, complete, bootstrap_samples, cache)
    summary.update(schema_version=1, experiment_kind="independent_equal_access", descriptive_only=True,
                   bootstrap_samples=bootstrap_samples, uncertainty_cluster="corpus",
                   bootstrap_method="paired_stratified_by_size_corpus", headline_available=complete)
    summary["uncertainty"] = {
        "conditional_on_fixed_generator": True,
        "population_generalization_established": False,
        "zero_observed_errors_proves_zero_population_rate": False,
        "zero_width_interval_proves_no_population_uncertainty": False,
    }
    summary["coverage"]["planned_corpora"] = 30
    summary["coverage"]["complete_corpora"] = summary["complete_cluster_count"]
    summary["subsets"] = {
        name: _group([row for row in rows if row["scenario"] in scenarios], specs, 180 * len(scenarios),
                     complete, bootstrap_samples, cache) for name, scenarios in _SUBSETS.items()}
    summary["scenarios"] = {
        scenario: _group([row for row in rows if row["scenario"] == scenario], specs, 180,
                         complete, bootstrap_samples, cache) for scenario in SCENARIOS}
    summary["directions"] = {
        direction: _group([row for row in rows if row["direction"] == direction], specs, 540,
                          complete, bootstrap_samples, cache) for direction in DIRECTIONS}
    summary["strata"] = {
        stratum: _group([row for row in rows if row["stratum"] == stratum],
                        [spec for spec in specs if spec["stratum"] == stratum], 360,
                        complete, bootstrap_samples, cache) for stratum in SIZES}
    summary["per_corpus"] = {
        spec["corpus_id"]: dict(
            _group([row for row in rows if row["corpus_id"] == spec["corpus_id"]], [spec], 36,
                   True, bootstrap_samples, cache, intervals=False),
            stratum=spec["stratum"], record_count=spec["record_count"])
        for spec in sorted(specs, key=lambda spec: spec["corpus_id"])}
    summary["notes"] = [
        "Independent deterministic draws from one fixed synthetic generator, not a population sample of real tasks or generators.",
        "Thirty corpus uncertainty clusters (ten per size); the 1080 turns are not 1080 independent replications.",
        "Whole corpora are resampled within size strata; all scenarios, directions and arms remain paired. Size strata have equal weight, not record-count weight.",
        "Total = Noema - native; access = Markdown - native; retrieval package = Noema - Markdown, in percentage points. Residual zero is algebraic, not an identified causal interaction.",
        "Intervals are descriptive conditional on this fixed generator. A zero-width ceiling interval or zero observed safety errors does not imply zero population uncertainty or a population error rate of zero.",
        "Access and retrieval delivery, bounding and context load remain confounded; this campaign makes no superiority qualification decision.",
        "Literal records, questions, rationales and provenance match all arms within a triple. Scope labels govern answers, not permissions.",
        "Failed attempts retain their established scores, denominators and cluster weight; missing cases prevent a headline and are never silently filtered or retried.",
        "Per-corpus metrics are descriptive progress only; no uncertainty interval is estimated from a single corpus.",
    ]
    return summary
