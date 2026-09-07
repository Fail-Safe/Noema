# Cross-agent continuity evaluation

Noema's bounded prefetch path was evaluated against native hierarchical
`AGENTS.md` continuity across fresh Codex and OpenCode sessions. The reviewed
September 2026 evidence shows a large advantage at workspace boundaries without
an accuracy or safety regression in the tested same-workspace, update, or
scoping cases.

The benchmark harness and reproduction procedure are documented in
[`tests/omarchy-eval/README.md`](../tests/omarchy-eval/README.md).

## What was tested

- Codex and OpenCode handed synthetic memories to one another in both
  directions using fresh, non-resumed sessions.
- Cases covered the same workspace, a sibling workspace beneath `~/Work`, and
  a workspace outside `~/Work`, plus rationale, provenance, supersession,
  distractor resistance, scope isolation, synthesis, and abstention.
- Both clients used the same pinned model and medium reasoning.
- Native continuity used only the isolated `AGENTS.md` hierarchy. Noema used
  isolated Cortexes and bounded prompt-time prefetch with no retrieval tools
  advertised to the receiving model.
- The deterministic track seeded equivalent corpora programmatically. A
  separate natural track exercised the product capture launcher before each
  retrieval.

Raw prompts, model responses, event streams, client configuration snapshots,
isolated Cortex contents, and detailed qualification records remain private.
Only the reviewed aggregate findings are presented here.

## Deterministic result

The deterministic campaign completed 192 retrieval turns: 96 native and 96
Noema, balanced across conditions, scenarios, and handoff directions.

| Measure | Native | Noema |
|---|---:|---:|
| Macro F1 | 72.2% | 100.0% |
| Exact-value accuracy | 75.0% | 100.0% |
| Rationale accuracy | 66.7% | 100.0% |
| Provenance accuracy | 75.0% | 100.0% |
| Stale-value rate | 0.0% | 0.0% |
| Hallucination rate | 0.0% | 0.0% |
| Scope-leakage rate | 0.0% | 0.0% |
| Median total tokens | 11,823 | 10,724 |
| p50 latency | 5.34 s | 5.55 s |
| p95 latency | 8.32 s | 7.56 s |

Noema's paired macro-F1 advantage was 27.8 percentage points. The descriptive
paired-bootstrap 95% interval was 19.8 to 35.8 points. Local prefetch succeeded
on all 96 Noema turns with 13.96 ms median and 16.53 ms p95 latency.

The advantage was concentrated where hierarchical project instructions have a
natural boundary: Noema scored 100% on the boundary subset with an 83.3-point
advantage. Both conditions scored 100% on the tested update and scoping
subsets, so Noema had no regression there but also could not produce the
preregistered 20-point advantage.

## Natural capture-and-handoff replication

The final clean replication used the public `noema integrate CLIENT capture`
launcher, followed by bounded prefetch in the receiving client. It completed
24 capture turns and 24 retrieval turns after an eight-model-turn mechanics
smoke.

| Measure | Native | Noema |
|---|---:|---:|
| Successful capture turns | 12/12 | 12/12 |
| Retrieval macro F1 | 72.2% | 100.0% |
| Capture median total tokens | 44,488 | 42,582 |
| Capture p50 latency | 21.41 s | 14.71 s |
| Capture p95 latency | 38.87 s | 20.98 s |
| Retrieval median total tokens | 10,442 | 10,606 |
| Retrieval p50 latency | 7.54 s | 6.13 s |
| Retrieval p95 latency | 11.61 s | 16.63 s |

Noema again achieved 100% exact-value, rationale, and provenance accuracy with
zero stale answers, hallucinations, or scope leakage. Its paired macro-F1
advantage was 27.8 points, with a wider descriptive interval of 5.6 to 52.8
points because this replication was smaller.

One 27.44-second Noema retrieval raised its p95. The same turn's local prefetch
took 11.38 ms, and the next-slowest Noema retrieval took 7.78 seconds. The
outlier is retained; the timing evidence attributes it to the client/model span,
not local Noema retrieval.

The final run restored its benchmark-owned state, verified the guarded files,
removed its pinned executables, and reported no integration failure.

The earlier deterministic campaign restored its managed paths but detected one
guard-file change. That limits its environment-isolation evidence; the clean
natural run does not retroactively remove that limitation.

## Development history and limits

These tables describe selected stages of an iterative development campaign.
Ten completed natural checkpoints were retained, each with 24 retrieval turns.
Two stopped for harm or integration instability and one stopped as inconclusive.
Across those checkpoints, Noema macro F1 ranged from 83.3% to 100%; native
ranged from 55.6% to 72.2%. The final table is the corrected product-path
replication, not a pooled estimate across those changing implementations.
Additional mechanics attempts and interrupted runs are excluded from those
checkpoint counts, not evidence of successful runs.

The same synthetic scenario families informed development and evaluation.
Repetition and swapping equivalent token vocabularies do not constitute a
held-out natural-language corpus. The intervals summarize these paired cases;
they do not establish generalization to other projects, models, or memory sets.
Exact model identifiers and detailed run manifests are retained privately, so
the public harness reproduces the method rather than the exact campaign.

Scope scores measure answer behavior. Prefetch searches the selected Cortex;
it does not enforce workspace access permissions. Use separate Cortexes where
memory must be inaccessible across projects.

## What the result supports

The evidence supports these bounded conclusions:

- Noema materially improved cross-workspace continuity in this corpus and
  retained perfect performance where native hierarchy was already strong.
- Bounded prefetch removed the earlier large token regression. In the
  deterministic campaign, Noema used fewer median tokens than native and had a
  lower p95 latency.
- The optimized capture launcher reproduced successfully and did not impose a
  capture token or latency regression in the final clean run.
- The useful product split is proactive bounded retrieval for ordinary sessions
  and an explicit temporary capture session for natural-language authoring.

This is not evidence that Noema is universally superior to hierarchical
instructions. The corpus was synthetic, the model and client versions were
fixed, the bootstrap interval is descriptive, and the natural replication was
small. Comparable per-turn cost was unavailable. Because the preregistered
success rule also required a 20-point advantage on update and scoping cases
where both systems were perfect, the analyzer did not emit a formal success
candidate. The correct claim is a demonstrated boundary advantage with no
material regression in the tested conditions.

## Product use

For general continuity, install prompt-time prefetch while retaining the normal
MCP connection:

```bash
noema integrate codex install --scope user --prefetch --check
noema integrate codex install --scope user --prefetch
```

Use `--prefetch-only` to preserve a hand-managed Codex MCP configuration, or
`--continuity` to create a selectable read-only `noema-continuity` profile.
When a deliberate capture session is useful, launch it explicitly:

```bash
noema integrate codex capture --scope user
noema integrate opencode capture --scope user
```

The capture launcher changes only the child process's tool exposure. It does
not rewrite the installed connection, and ordinary sessions retain the full
Noema tool set.
