#!/usr/bin/env python3
"""Offline descriptive analysis of public, already-scored turns (stdlib only).

This reproduces numerical analysis, not raw-response grading. No private plans,
answers, model clients or native binaries are imported or invoked.
"""
from __future__ import annotations

import argparse
from collections import Counter, defaultdict
import hashlib
import json
import math
from pathlib import Path
import random
import statistics
import sys

ARMS = ("native", "markdown", "noema")
SCORES = ("macro_f1", "exact_accuracy", "rationale_accuracy", "provenance_accuracy")
SAFETY = {"stale_rate": "stale_count", "hallucination_rate": "hallucination_count", "scope_leakage_rate": "leakage_count"}
TOKENS = ("total_tokens", "noncached_input_tokens", "cached_input_tokens", "output_tokens")
STRATA = ("small", "medium", "large")
BOUNDARY = {"sibling_workspace", "outside_work"}
EFFECTS = (("total", "native", "noema"), ("access", "native", "markdown"), ("retrieval", "markdown", "noema"))


SCENARIOS = {"same_workspace_exact", "same_workspace_rationale", "sibling_workspace", "outside_work", "supersession", "scope_distractor_abstention"}
DIRECTIONS = {"codex_to_opencode", "opencode_to_codex"}


def percentile(values, q):
    if not values:
        return None
    values = sorted(values)
    p = (len(values) - 1) * q
    lo, hi = math.floor(p), math.ceil(p)
    return values[lo] + (values[hi] - values[lo]) * (p - lo)


def validate(rows):
    if not isinstance(rows, list):
        raise ValueError("turn data must be an array")
    seen = set()
    metadata = {}
    triples = defaultdict(set)
    for row in rows:
        if not isinstance(row, dict):
            raise ValueError("turn must be an object")
        for key in ("case_id", "corpus_id", "direction", "scenario", "stratum"):
            if not isinstance(row.get(key), str) or not row[key]:
                raise ValueError("missing public turn identifier")
        if row["scenario"] not in SCENARIOS or row["direction"] not in DIRECTIONS:
            raise ValueError("unknown scenario or direction")
        if row["case_id"] in seen:
            raise ValueError("duplicate case ID")
        seen.add(row["case_id"])
        if row.get("condition") not in ARMS or row.get("status") not in ("completed", "failed"):
            raise ValueError("invalid arm or status")
        if type(row.get("record_count")) is not int or row["record_count"] < 1:
            raise ValueError("invalid record count")
        spec = (row["stratum"], row["record_count"])
        cid = row["corpus_id"]
        if cid in metadata and metadata[cid] != spec:
            raise ValueError("inconsistent corpus size metadata")
        metadata[cid] = spec
        key = (cid, row["direction"], row["scenario"])
        if row["condition"] in triples[key]:
            raise ValueError("duplicate matched-triple arm")
        triples[key].add(row["condition"])
        for field in SCORES:
            value = row.get(field)
            optional = field == "rationale_accuracy" and row["scenario"] not in {"same_workspace_rationale", "outside_work", "supersession"}
            if value is None and optional:
                continue
            if type(value) not in (int, float) or not math.isfinite(value) or not 0 <= value <= 1:
                raise ValueError("missing or invalid pre-scored metric")
        if type(row.get("answer_count")) is not int or row["answer_count"] < 1:
            raise ValueError("invalid safety denominator")
        for field in SAFETY.values():
            if type(row.get(field)) is not int or not 0 <= row[field] <= row["answer_count"]:
                raise ValueError("invalid safety count")
        for field in (*TOKENS, "latency_seconds", "cost_usd", "tool_call_count"):
            value = row.get(field)
            if value is not None and (type(value) not in (int, float) or not math.isfinite(value) or value < 0):
                raise ValueError("invalid observation metric")
    return metadata, triples


def condition(rows, available):
    out = {"turns": len(rows), "failed_turns": sum(r["status"] == "failed" for r in rows)}
    for field in SCORES:
        values = [r[field] for r in rows if r.get(field) is not None]
        out[field] = statistics.fmean(values) if available and values else None
    out["answer_count"] = sum(r["answer_count"] for r in rows)
    for rate, count in SAFETY.items():
        out[count] = sum(r[count] for r in rows)
        out[rate] = out[count] / out["answer_count"] if available and out["answer_count"] else None
    for field in (*TOKENS, "latency_seconds"):
        values = [r[field] for r in rows if r.get(field) is not None]
        out[field + "_observed_turns"] = len(values)
        out[field + "_median"] = statistics.median(values) if available and values else None
        if field == "latency_seconds":
            for name, q in (("p50", .5), ("p95", .95)):
                out["latency_" + name + "_seconds"] = percentile(values, q) if available else None
    out["tokens_median"] = out["total_tokens_median"]
    return out


def bootstrap(rows, metadata, samples):
    """Same ID-domain RNG, size order, interpolation and whole-cluster estimator as independent_corpus.py."""
    ids = tuple(sorted(metadata))
    strata = [[i for i, cid in enumerate(ids) if metadata[cid][0] == s] for s in STRATA]
    if sum(map(len, strata)) != len(ids):
        raise ValueError("unknown independent size stratum")
    digest = hashlib.sha256("\0".join(("independent-v1-cluster-bootstrap", *ids)).encode()).hexdigest()
    rng = random.Random(int(digest, 16))
    clusters = {arm: [[r for r in rows if r["corpus_id"] == cid and r["condition"] == arm] for cid in ids] for arm in ARMS}
    components = {}
    for arm in ARMS:
        for field in (*SCORES, *SAFETY):
            if field in SAFETY:
                nums = [sum(r[SAFETY[field]] for r in c) for c in clusters[arm]]
                dens = [sum(r["answer_count"] for r in c) for c in clusters[arm]]
            else:
                values = [[r[field] for r in c if r.get(field) is not None] for c in clusters[arm]]
                nums, dens = [sum(v) for v in values], [len(v) for v in values]
            components[arm, field] = nums, dens
    distributions = defaultdict(list)
    means = {arm: [statistics.fmean(r["macro_f1"] for r in c) for c in clusters[arm]] for arm in ARMS}
    for _ in range(samples):
        draw = tuple(rng.choice(indices) for indices in strata for _ in indices)
        for key, (nums, dens) in components.items():
            denominator = sum(dens[i] for i in draw)
            if denominator:
                distributions[key].append(sum(nums[i] for i in draw) / denominator)
        for name, left, right in EFFECTS:
            distributions[name].append(sum(100 * (means[right][i] - means[left][i]) for i in draw) / len(draw))
    interval = lambda values: [percentile(values, .025), percentile(values, .975)]
    return ({arm: {field: interval(distributions[arm, field]) for field in (*SCORES, *SAFETY)} for arm in ARMS},
            {name: interval(distributions[name]) for name, _, _ in EFFECTS})


def group(rows, available, samples, use_bootstrap):
    metadata, triples = validate(rows)
    matched = bool(rows) and all(arms == set(ARMS) for arms in triples.values())
    available = available and matched
    conditions = {arm: condition([r for r in rows if r["condition"] == arm], available) for arm in ARMS}
    effects = {name + "_pp": 100 * (conditions[right]["macro_f1"] - conditions[left]["macro_f1"]) if available else None for name, left, right in EFFECTS}
    effects.update(residual_pp=0.0 if available else None, bootstrap_95_pp={name: [None, None] for name, _, _ in EFFECTS})
    effects["bootstrap_95_pp"]["residual"] = [0.0, 0.0] if available else [None, None]
    if available and use_bootstrap and len(metadata) > 1:
        cis, effect_cis = bootstrap(rows, metadata, samples)
        for arm in ARMS:
            conditions[arm]["bootstrap_95"] = cis[arm]
        effects["bootstrap_95_pp"].update(effect_cis)
    reductions = {}
    for field in ("total_tokens", "latency_seconds"):
        baseline = conditions["markdown"][field + "_median"]
        value = conditions["noema"][field + "_median"]
        reductions[field] = {"markdown_pooled_median": baseline, "noema_pooled_median": value,
                             "reduction_percent": 100 * (1 - value / baseline) if baseline and value is not None else None}
    return {"available": available, "retrieval_turns": len(rows), "cluster_count": len(metadata),
            "matched_triples": sum(arms == set(ARMS) for arms in triples.values()),
            "conditions": conditions, "decomposition": effects, "pooled_median_reductions_vs_markdown": reductions}


def analyze(run, rows, smoke, samples):
    metadata, triples = validate(rows)
    expected, corpora = run["expected_turns"], run["expected_corpora"]
    if type(expected) is not int or type(corpora) is not int or min(expected, corpora) < 0:
        raise ValueError("invalid manifest campaign sizes")
    if len(rows) > expected or len(metadata) > corpora:
        raise ValueError("observed campaign exceeds manifest")
    complete = expected > 0 and len(rows) == expected and len(metadata) == corpora and all(a == set(ARMS) for a in triples.values())
    independent = corpora == 30 and expected == 1080
    if complete and independent:
        if Counter(s for s, _ in metadata.values()) != Counter(dict.fromkeys(STRATA, 10)) or any(n != 36 for n in Counter(r["corpus_id"] for r in rows).values()):
            raise ValueError("independent campaign is not balanced")
    result = group(rows, complete, samples, independent)
    if complete and independent and "analysis_corpus_order" in run:
        for field, interval in efficiency_intervals(rows, run["analysis_corpus_order"], samples).items():
            result["pooled_median_reductions_vs_markdown"][field]["bootstrap_95_percent"] = interval
        result["efficiency_bootstrap_method"] = "paired_size_stratified_plan_order_pooled_median"
    result.update(run_id=run["run_id"], role=run["role"], complete=complete, headline_available=complete,
                  expected_turns=expected, observed_turns=len(rows), expected_corpora=corpora,
                  bootstrap_samples=samples if independent else None,
                  bootstrap_method="paired_stratified_by_size_whole_corpus" if independent else "not_estimated",
                  descriptive_only=True, population_generalization_established=False,
                  grading_verifiable_from_public_rows=False)
    result["subsets"] = {"boundary": group([r for r in rows if r["scenario"] in BOUNDARY], complete, samples, independent)}
    result["strata"] = {s: group([r for r in rows if r["stratum"] == s], complete, samples, independent) for s in sorted({r["stratum"] for r in rows})}
    result["smoke"] = group(smoke, bool(smoke), samples, False)
    result["smoke"]["campaign_headline"] = False
    return result


def efficiency_intervals(rows, order, samples):
    """Frozen estimator: plan-order size strata, paired whole-corpus pooling."""
    metadata, _ = validate(rows)
    if not isinstance(order, dict) or set(order) != set(STRATA):
        raise ValueError("missing efficiency corpus order")
    flattened = [cid for s in STRATA for cid in order[s]]
    if len(flattened) != len(set(flattened)) or set(flattened) != set(metadata):
        raise ValueError("invalid efficiency corpus order")
    if any(metadata[cid][0] != s for s in STRATA for cid in order[s]):
        raise ValueError("efficiency corpus stratum mismatch")
    fields = ("total_tokens", "latency_seconds")
    clusters = {(arm, field, cid): [r[field] for r in rows if r["condition"] == arm and r["corpus_id"] == cid and r.get(field) is not None]
                for arm in ("markdown", "noema") for field in fields for cid in flattened}
    rng = random.Random("frozen-profile-efficiency-v1")
    values = {field: [] for field in fields}
    for _ in range(samples):
        draw = [rng.choice(order[s]) for s in STRATA for _ in order[s]]
        for field in fields:
            pooled = {arm: [v for cid in draw for v in clusters[arm, field, cid]] for arm in ("markdown", "noema")}
            if all(pooled.values()):
                baseline = statistics.median(pooled["markdown"])
                if baseline:
                    values[field].append(100 * (1 - statistics.median(pooled["noema"]) / baseline))
    return {field: [percentile(values[field], .025), percentile(values[field], .975)] for field in fields}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-id", action="append", help="Select a manifest run (repeatable); default: all")
    parser.add_argument("--bootstrap-samples", type=int, help="Override manifest bootstrap samples")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args(argv)
    root = Path(__file__).resolve().parent
    try:
        manifest = json.loads((root / "manifest.json").read_text())
        runs = manifest["runs"]
        if not isinstance(runs, list) or len({r["run_id"] for r in runs}) != len(runs):
            raise ValueError("invalid manifest run list")
        selected = set(args.run_id or [r["run_id"] for r in runs])
        if selected - {r["run_id"] for r in runs}:
            raise ValueError("unknown run ID")
        output = []
        for run in runs:
            rid = run["run_id"]
            if rid not in selected:
                continue
            if not isinstance(rid, str) or rid in (".", "..") or any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_" for c in rid):
                raise ValueError("unsafe manifest run ID")
            samples = args.bootstrap_samples if args.bootstrap_samples is not None else run.get("bootstrap_samples", 10000)
            if type(samples) is not int or samples < 1:
                raise ValueError("bootstrap samples must be positive")
            directory = root / "data" / rid
            rows = json.loads((directory / "turns.json").read_text())
            smoke = json.loads((directory / "smoke-turns.json").read_text())
            output.append(analyze(run, rows, smoke, samples))
        payload = {"schema_version": 1, "evidence_level": "analysis-reproducible; not raw-response grading-verifiable", "runs": output}
        args.output.write_text(json.dumps(payload, indent=2, allow_nan=False) + "\n")
    except (OSError, ValueError, KeyError, TypeError, ZeroDivisionError):
        print("analysis failed: invalid, missing, or inconsistent public evidence", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
