# Omarchy cross-harness continuity benchmark

This directory contains a preregistered, sequential benchmark comparing native hierarchical
`AGENTS.md` continuity with Noema across Codex and OpenCode. It is intentionally fail-closed:
no evidence batch can run until a live mechanics smoke gate passes, and no third batch can run
until the first two-batch checkpoint has been analyzed and explicitly continued.

The public interpretation of the reviewed September 2026 qualification,
including limitations and product implications, is in
[`docs/continuity-evaluation.md`](../../docs/continuity-evaluation.md). Raw run
artifacts and detailed qualification records remain private.

The harness uses Python's standard library only. It does not install packages, modify an existing
Cortex, or edit user-level Codex, OpenCode, Noema, or `~/Work/AGENTS.md` configuration. Those files
are snapshotted as guards and hash-checked at every checkpoint and restore.

Preflight copies the resolved Codex, its code-mode companion, OpenCode, and Noema executables into
the private run directory and executes only those pinned copies. It also sets
`OPENCODE_DISABLE_AUTOUPDATE=1` for every probe and model turn. A client update elsewhere on the
host therefore cannot change an active run.

## Commands

Run all commands from the repository root. `preflight` creates a run ID and records it privately as
the default for following commands; pass `--run-id ID` to select a specific run.

The checked-in configuration is a template with unset model IDs. Before running,
copy it to an ignored local configuration under `tests/omarchy-eval/.private/`, set `models.codex` and
`models.opencode` to the same explicitly selected model (using the client's
provider prefix where needed), and retain medium reasoning. Pass
`--config /absolute/path/to/local-config.json` before the subcommand on every
invocation below. Paths in that file resolve relative to its directory; update
them when placing the configuration elsewhere. Missing model IDs fail before
any client is launched. Keep historical run configurations unchanged: these
instructions start a new campaign, not an exact reproduction of private runs.

```bash
python3 tests/omarchy-eval/benchmark.py preflight
python3 tests/omarchy-eval/benchmark.py smoke --confirm-live

python3 tests/omarchy-eval/benchmark.py run-batch --confirm-live
python3 tests/omarchy-eval/benchmark.py run-batch --confirm-live
python3 tests/omarchy-eval/benchmark.py analyze --checkpoint 1
```

The original `standard` profile uses model-selected Noema MCP retrieval and is
retained for the original checkpoint. A new run can preregister bounded prefetch
delivery without changing that evidence:

```bash
python3 tests/omarchy-eval/benchmark.py --run-id prefetch-checkpoint-YYYYMMDD \
  preflight --retrieval-profile prefetch
python3 tests/omarchy-eval/benchmark.py --run-id prefetch-checkpoint-YYYYMMDD \
  smoke --confirm-live
python3 tests/omarchy-eval/benchmark.py --run-id prefetch-checkpoint-YYYYMMDD \
  run-batch --confirm-live
python3 tests/omarchy-eval/benchmark.py --run-id prefetch-checkpoint-YYYYMMDD \
  run-batch --confirm-live
python3 tests/omarchy-eval/benchmark.py --run-id prefetch-checkpoint-YYYYMMDD \
  analyze --checkpoint 1
```

A fully restored passing smoke may gate a fresh run without repeating model turns only when every
core harness, configuration, model, executable, corpus-cycle, and deterministic-evidence hash is
identical. The importer fails closed on any mismatch, incomplete smoke, restoration failure, guard
drift, prior target evidence, or retained pinned executable:

```bash
python3 tests/omarchy-eval/reuse_smoke.py \
  --from-run natural-smoke-YYYYMMDD \
  --to-run natural-replication-YYYYMMDD
```

The target must already have a fresh passed preflight. The importer records source report hashes
and its own hash in a public `smoke-evidence.json`; it runs no model turns.

The retrieval profile is locked into preflight state and every smoke, batch,
row, and checkpoint validates it. This prevents mixing standard-MCP and
prefetch evidence. Natural capture additionally requires hash-locked eligible
deterministic evidence.

An additive Noema `recall_context` fast path can be evaluated separately from the preregistered
checkpoint sequence. Start a fresh run after building the current checkout; this command performs
four live turns (native and fast path in both harness directions), writes a descriptive report, and
restores its isolated runtime immediately:

```bash
make build
python3 tests/omarchy-eval/benchmark.py --run-id fastpath-YYYYMMDD preflight
python3 tests/omarchy-eval/benchmark.py --run-id fastpath-YYYYMMDD \
  experiment-fastpath --catalog minimal --confirm-live
```

Use `--catalog full` to isolate batching alone. The `minimal` profile advertises only
`recall_context` from the experimental MCP server process. The experiment is not checkpoint
evidence and cannot satisfy the preregistered success criteria.

The prefetch experiment removes the model-selected tool round trip entirely. Its thin Python
adapter invokes the native Rust `noema prefetch` command from Codex's `UserPromptSubmit` hook or
immediately before launching OpenCode, injects a bounded memory block into the first model request,
and advertises no Noema MCP tools. Native prefetch confidence-gates lexical candidates, so weak
matches produce empty context rather than a distractor block. OpenCode's
project plugin is not used because its 1.18.27 stdin-driven `run` lifecycle did not expose the
current user prompt to the tested message hooks. Run the four-turn smoke first; if it passes, the
challenge phase adds outside-`~/Work`, supersession, and scope/distractor/abstention cases:

```bash
make build
python3 tests/omarchy-eval/benchmark.py --run-id prefetch-YYYYMMDD preflight
python3 tests/omarchy-eval/benchmark.py --run-id prefetch-YYYYMMDD \
  experiment-prefetch --phase smoke --confirm-live
python3 tests/omarchy-eval/benchmark.py --run-id prefetch-YYYYMMDD \
  experiment-prefetch --phase challenge --confirm-live
python3 tests/omarchy-eval/benchmark.py --run-id prefetch-YYYYMMDD \
  experiment-prefetch --phase coverage --confirm-live
```

The optional `coverage` phase exercises rationale/synthesis and sibling-workspace retrieval, and
repeats outside-work retrieval with every rationale-scored question explicitly requesting its
rationale. This keeps prompt wording aligned with the deterministic scorer.

The private hook metrics contain only engine, client, success/error status, duration, hit count, and
context size. They never contain prompts, search queries, trace IDs, or retrieved bodies. As with the MCP
fast-path experiment, these results are descriptive and do not alter checkpoint evidence.

Stop after reviewing checkpoint 1 together. If the operator explicitly chooses to continue:

```bash
python3 tests/omarchy-eval/benchmark.py run-batch \
  --continue-after-checkpoint 1 --confirm-live
python3 tests/omarchy-eval/benchmark.py run-batch --confirm-live
python3 tests/omarchy-eval/benchmark.py analyze --checkpoint 2
```

The same pattern continues through checkpoint 6, which is the hard ceiling of 12 batches and 288
retrieval turns. `run-batch` always runs exactly 24 fresh retrieval turns: six scenarios, two memory
conditions, and both handoff directions. Completed cases are durable and an interrupted batch
resumes only its missing cases.

When work is complete—or immediately after any smoke/batch failure—restore all benchmark-owned
workspace and runtime paths:

```bash
python3 tests/omarchy-eval/benchmark.py restore
```

`restore` first archives the isolated runtime privately, removes only manifest-listed paths whose
name contains the run ID, removes benchmark parent directories only when the preflight recorded
them as absent and they are empty, removes the run-private executable copies, and verifies all
guard hashes. A second restore is idempotent. Managed-path restoration failure remains
`restore_failed`; when managed restoration succeeds but a live guard changed concurrently, the
distinct state is `restored_with_guard_drift`. Guard drift is reported by logical name and hashes
without publishing configuration contents or overwriting current user state.

## Fixed controls

- Codex model: explicitly selected in the private configuration, medium reasoning, `codex exec --ephemeral`, JSONL events, and a
  strict output schema. The generated isolated-home hook is locally vetted before Codex's automation-only
  hook-trust bypass is enabled for the Noema condition. Unrelated app/plugin catalogs are disabled;
  both conditions use the same workspace-write sandbox, and only Noema receives its isolated
  runtime directory as an additional writable root so the Cortex database can operate.
- OpenCode model: the same selection with its provider prefix, medium variant, JSON events, unique titles, and no
  continue/session flags.
- Native condition: only the synthetic `AGENTS.md` hierarchy; Codex receives an empty per-context
  `CODEX_HOME` (with an auth-file symlink but no live user configuration) and OpenCode receives an
  isolated `XDG_CONFIG_HOME`, preventing installed user integrations from contaminating the baseline.
  Natural runs initialize empty global, project-A, and project-B `AGENTS.md` targets before capture,
  avoiding a model-dependent missing-file recovery branch while leaving all memory content to the
  source capture turn.
- Noema condition: a unique Cortex for each batch and direction. The `standard`
  profile uses project-scoped Codex and OpenCode MCP integration; the `prefetch`
  profile uses Codex's isolated `UserPromptSubmit` hook and OpenCode's
  launcher-side context injection without advertising Noema MCP tools.
- Locations: same workspace and sibling workspaces beneath `~/Work`, plus a distinct workspace
  beneath `~/noema-omarchy-eval-outside`.
- Corpus: 60 records—12 preferences, 12 decisions, 12 project-A facts, 12 project-B facts, and six
  obsolete/current pairs. Equivalent alpha/beta corpora swap between conditions and directions on
  successive batches.

The deterministic track seeds both conditions programmatically. Direction remains isolated and
meaningful: each direction has its own native hierarchy or Cortex, source-harness author, corpus
assignment, and opposite-harness receiver. The natural track adds a real capture turn before each
retrieval and requires a hash-locked completed deterministic report with at least 192 retrieval
turns, a positive interval, acceptable safety, and successful managed restoration. It is never
started automatically. Start it as a fresh preregistered run that cites the immutable evidence:

```bash
python3 tests/omarchy-eval/benchmark.py --run-id natural-YYYYMMDD preflight \
  --retrieval-profile prefetch --track natural --corpus-cycle 1 \
  --evidence-run prefetch-qualification-YYYYMMDD \
  --evidence-checkpoint 4
python3 tests/omarchy-eval/benchmark.py --run-id natural-YYYYMMDD \
  smoke --confirm-live
python3 tests/omarchy-eval/benchmark.py --run-id natural-YYYYMMDD \
  run-batch --confirm-live
python3 tests/omarchy-eval/benchmark.py --run-id natural-YYYYMMDD \
  analyze --checkpoint 1
```

Use `--corpus-cycle 2` for a fresh replication run that mirrors the lexical assignment of cycle 1
without reopening or mutating a restored run. The selected cycle is locked at preflight and
included in the checkpoint summary.

A natural smoke has four retrieval cases and four source-agent capture turns. One natural batch has
24 retrieval cases and 24 capture turns, then pauses for review. At most two natural batches are
allowed, and the second requires `--continue-after-checkpoint 1`.

To diagnose native OpenCode capture failures without spending another retrieval batch, a separate
mechanics experiment compares missing `AGENTS.md` targets with pre-created empty targets. It runs
eight fresh native OpenCode capture turns and no retrieval turns: four matched prompt/corpus pairs,
two exact and two rationale/synthesis, with target state and alpha/beta corpus order balanced. Use a
fresh natural/prefetch preflight so the same isolation and qualified deterministic evidence checks
apply:

```bash
python3 tests/omarchy-eval/benchmark.py --run-id native-target-YYYYMMDD preflight \
  --retrieval-profile prefetch --track natural --corpus-cycle 1 \
  --evidence-run prefetch-qualification-YYYYMMDD \
  --evidence-checkpoint 4
python3 tests/omarchy-eval/benchmark.py --run-id native-target-YYYYMMDD \
  experiment-native-capture-target --confirm-live
python3 tests/omarchy-eval/benchmark.py --run-id native-target-YYYYMMDD restore
```

The command records raw prompts and events privately, publishes only target-state labels and
numeric/error summaries, and restores the run-owned workspace and runtime before reporting. Capture
failures are experiment outcomes rather than command failures; the command fails only if all eight
turns cannot execute or restoration/guard verification fails. This result is descriptive mechanics
evidence and is never pooled into native-versus-Noema retrieval qualification.

For Noema, the source capture turn receives write-capable MCP access in an isolated client home.
Its `continuity-capture` tool profile exposes only `get_instructions` and `create_traces`; the
capture prompt submits the independent memories in one bounded call and treats its ID/content-hash
receipts as verification. This measures the bounded batched write path without weakening per-trace
persistence or hiding explicit partial failures. Natural Noema capture turns launch through
`noema integrate CLIENT capture` using the preflight-pinned Noema and client executables, so the
benchmark exercises the same ephemeral product path offered to users. Normal Noema connections
retain the full tool set.
OpenCode's global Claude compatibility prompt is disabled so a real-home `~/.claude/CLAUDE.md`
cannot bypass the isolated benchmark instructions. Source capture also disables upward project
configuration discovery while explicitly loading the run-owned `.opencode` directory and MCP
configuration. Retrieval discovery remains enabled so native hierarchical `AGENTS.md` is tested
normally. External access remains limited to the isolated benchmark tree.
The receiving turn uses only bounded prefetch and a disabled-MCP Codex profile or an isolated
OpenCode configuration. Native capture writes the isolated `AGENTS.md` hierarchy. Capture success,
tokens, tool calls, and latency are reported separately from retrieval.
When JSONL events expose completed timestamps, reports also show p95 completed tool time, the
Noema-tool subtotal, and the largest observed gap between successive events. The gap is labeled
client/model time because the event stream cannot distinguish provider generation from local client
scheduling; it does distinguish both from completed MCP tool execution.

## Smoke gate

The deterministic smoke has four retrieval turns. The natural smoke has those four retrieval cases
plus one source-agent capture turn for each case. It must demonstrate:

- parsable JSON event streams, persisted capture artifacts, and final retrieval response JSON;
- successful exact scoring;
- the retrieval behavior required by the preregistered profile;
- isolated Cortex/client configuration;
- successful removal of all smoke-owned runtime/workspaces; and
- unchanged hashes for live user configuration guards.

Any failure marks the smoke failed and prevents evidence batches. The command still attempts
restoration in a `finally` path. Run `restore` if the process itself is killed.

## Evidence and privacy

Raw prompts, model responses, stdout/stderr JSONL, session identifiers, configuration snapshots,
and isolated Cortex data live under ignored `.private/runs/<run-id>/`. Permissions default to
owner-only. Do not publish that directory.

Sanitized checkpoint artifacts are written under ignored `reports/<run-id>/checkpoint-NNN/`:

- `summary.json` — aggregate metrics, decision, and restoration status;
- `turns.json` and `turns.tsv` — case metadata and numeric scores only;
- `report.md` — a review-ready checkpoint narrative.

Sanitized rows deliberately exclude prompts, answers, tool names, session IDs, paths, command
arguments, stderr, and Cortex names. Reports remain ignored until manually reviewed and deliberately
copied or staged.

Clients do not necessarily expose comparable cost data. The analyzer reports cost as unavailable
rather than estimating it from an unpinned price table. This prevents a success candidate until
comparable cost evidence exists; a reviewed local config may add a fixed pricing table in a future
run without changing old checkpoints.

## Scoring and stopping

Each answer is scored for applicable dimensions: exact recall, rationale, provenance, scope
isolation, outside-`~/Work` retrieval, supersession, distractor resistance, synthesis, and correct
abstention. Exact synthetic markers make scoring deterministic. Wrong-scope and obsolete markers
separately contribute leakage and stale-answer rates. Invalid output or failed turns remain in the
denominator with a zero score.

Each immutable checkpoint contains the macro F1, paired Noema-minus-native difference, exact/
rationale/provenance accuracy, stale/hallucination/leakage rates, tokens, reported cost, tool calls,
p50/p95 latency, failures, restoration status, and a deterministic descriptive 95% paired-bootstrap
interval.

The analyzer applies the preregistered rules:

- stop for futility when the advantage is below three points, the interval's upper bound is below
  the ten-point target, and no boundary/update/scoping subset has a 15-point advantage;
- stop for harm or instability when Noema trails by over five points inside `~/Work`, exceeds 10%
  leakage or hallucination, or any integration turn is unreliable;
- otherwise recommend continuing while a meaningful advantage remains plausible;
- never produce a success candidate before 192 retrieval turns, and only then if all accuracy,
  safety, subset, provenance, strongest-native-scenario, and efficiency gates pass.

A futility conclusion is exactly “No demonstrated meaningful advantage.” It is not a superiority
claim for either system.

## Local mechanics tests

These tests do not call a model or alter live configuration:

```bash
python3 -m unittest discover -s tests/omarchy-eval -p 'test_*.py' -v
```
