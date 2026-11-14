# Omarchy cross-harness continuity benchmark

This directory contains a preregistered, sequential benchmark comparing native hierarchical
`AGENTS.md` continuity with Noema across Codex and OpenCode. It is intentionally fail-closed:
no evidence batch can run until a live mechanics smoke gate passes, and no third batch can run
until the first two-batch checkpoint has been analyzed and explicitly continued.

The public interpretation of the reviewed September 2026 qualification,
including limitations and product implications, is in
[`docs/continuity-evaluation.md`](../../docs/continuity-evaluation.md). Raw run
artifacts and detailed qualification records remain private.

The separate [November public evidence bundle](published/continuity-2026-11/README.md)
contains reviewed numerical snapshots and portable offline analysis, including
unfavorable and aborted stages. It does not expose private raw evidence or change
the September qualification. Fresh 8k-profile replication has its own explicit
driver; the default benchmark profile remains unchanged.

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

### Prefetch delivery budgets

The checked-in adapter has distinct receiving-client limits:

| Delivery path | Maximum context characters |
|---|---:|
| Codex `UserPromptSubmit` task evidence | 2,500 |
| Codex `SessionStart` preferences | 32,000 |
| OpenCode pre-request context | 8,000 |

These caps include the preamble and provenance metadata, not just trace bodies.
A positive hit count does not prove that complete values, rationales, or
provenance survived rendering. Inspect native `truncated` output and run
model-free checks at the actual receiving client's cap, not just the CLI default.
For a private synthetic corpus, a direct budget check reads its prompt from stdin:

```bash
noema --cortex "$EVAL_CORTEX" prefetch --input raw --output json \
  --max-results 8 --max-preferences 0 --max-chars 2500 < "$PRIVATE_PROMPT_FILE"
```

A budget-sensitivity run is a **separate descriptive profile**. Use a fresh run
ID, fresh confirmation corpus, and an experiment-owned adapter; freeze and report
both client limits and the adapter hash. Keep the checked-in defaults, historical
configurations, and earlier unfavorable results unchanged. Larger bounds can
recover evidence at additional context cost; they do not establish better search
ranking or repair a historical qualification result.

The private hook metrics contain only engine, client, success/error status, duration, hit count, and
context size. They never contain prompts, search queries, trace IDs, or retrieved bodies. As with the MCP
fast-path experiment, these results are descriptive and do not alter checkpoint evidence.

### Equal-access Markdown gap decomposition (new descriptive experiment)

This experiment adds a third condition without replacing hierarchical native
continuity or reopening the September 2026 qualification:

- `native`: the existing isolated hierarchical `AGENTS.md` tree, including its
  sibling-workspace and outside-`~/Work` discovery boundaries.
- `markdown`: shared Markdown supplied through a local `AGENTS.md` in each
  receiving workspace, including the sibling and outside workspace. It contains
  the full synthetic corpus reachable in the corresponding selected Cortex,
  not an answer-key subset or Noema's prefetch output. No Cortex, Noema tools,
  MCP configuration, or prefetch hooks are installed for this condition.
- `noema`: the existing bounded prefetch condition, with no Noema retrieval
  tools advertised to the receiving model.

The extra Markdown exposure is intentional: it equalizes record reachability,
not prompt size or retrieval work. The client reads the full Markdown memory;
Noema selects and bounds its prompt-time evidence. Any remaining difference
therefore measures the retrieval/delivery package, including context load and
presentation, rather than isolating search-ranking quality.

Corpus selection is explicit. The existing deterministic Cortex contains all 60
records, including both project scopes and obsolete/current pairs. The Markdown
arm exposes that same set, retaining scope and supersession labels. The sibling
case must be able to reach project-A's record from project-B. The scoping case
tests rejecting a wrong-scope answer, not hiding that record from the model:
wrong-scope memories are reachable in both Markdown and the selected Cortex.
This does not flatten unrelated Cortexes or other directions into shared memory.
Neither condition establishes filesystem permission enforcement. Experiments
requiring inaccessible project memories must select separate Cortexes and
matching separate Markdown sets; that access-control design is not tested here.

Markdown uses Noema's literal tokens, questions, and provenance, so the
equal-access contrast has the same reachable records on each turn. Hierarchical
native retains its equivalent counterbalanced alpha/beta assignment; directions
swap the vocabularies and `--corpus-cycle 2` mirrors the assignment. Exact-value
F1, rationale, provenance, stale/hallucination/scope scoring, model configuration,
medium reasoning,
isolated client homes, pinned executables, and guarded restoration are reused.
The new experiment is deterministic only; it does not requalify natural capture.

Use a **new run ID** and an ignored private model configuration as described
above. Commands below are documentation for an operator-authorized live run;
local mechanics tests do not execute them:

```bash
python3 tests/omarchy-eval/benchmark.py --config /absolute/path/to/local-config.json \
  --run-id equal-access-YYYYMMDD preflight \
  --track deterministic --retrieval-profile prefetch --corpus-cycle 1
python3 tests/omarchy-eval/benchmark.py --config /absolute/path/to/local-config.json \
  --run-id equal-access-YYYYMMDD experiment-equal-access --phase smoke --confirm-live
python3 tests/omarchy-eval/benchmark.py --config /absolute/path/to/local-config.json \
  --run-id equal-access-YYYYMMDD experiment-equal-access --phase campaign --confirm-live
python3 tests/omarchy-eval/benchmark.py --config /absolute/path/to/local-config.json \
  --run-id equal-access-YYYYMMDD restore
```

The six-turn experiment-specific smoke exercises all three conditions in both directions.
The campaign requires that smoke's passing mechanics and restoration gates; a
checkpoint smoke or imported smoke cannot substitute. The 36-turn campaign covers
all six scenario families in both directions, including both boundary cases,
same-workspace exact/rationale, supersession, and scoping/abstention. Each phase
restores its managed runtime even on failure. Finish with `restore` to close the
run and remove its pinned executables. For a mirrored replication, use another
fresh run ID with `--corpus-cycle 2`; never edit an existing run configuration.
An equal-access run cannot subsequently supply checkpoint qualification evidence.
Condition order rotates across scenarios and reverses between directions. Smoke
requires perfect exact/provenance scores and clean safety/mechanics; campaign
completion requires mechanics and restoration, not an accuracy advantage.

Each phase writes `equal-access-{phase}-experiment.json` and
`equal-access-{phase}-turns.json` under `reports/<run-id>/`. The experiment report
contains the decomposition under `summary` and local prefetch observations under
`prefetch_metrics`; the turns file contains sanitized per-turn metrics. An
interrupted phase marks its summary unavailable even if some triples completed.

#### Reading the decomposition

On the same matched scenario/direction/batch triples, let H be hierarchical
native macro F1, M equal-access Markdown macro F1, and N Noema prefetch macro F1.
The sanitized experiment report supplies aggregate metrics and these descriptive
differences overall, for the boundary subset (`sibling_workspace`, `outside_work`),
and by scenario/subset:

| Component | Percentage-point difference | Interpretation |
|---|---|---|
| Total gap | 100 × (N − H) | Repeated hierarchy-to-prefetch contrast in this new run |
| Access / reachability | 100 × (M − H) | Change after making shared Markdown evidence reachable |
| Retrieval / evidence quality | 100 × (N − M) | Remaining retrieval/delivery package contrast |
| Residual | Total − access − retrieval | Algebraic closure on the same triples |

The summary schema uses `conditions.native`, `conditions.markdown`, and
`conditions.noema` for existing accuracy, safety, token, and latency metrics.
`decomposition` contains `total_pp`, `access_pp`, `retrieval_pp`, `residual_pp`,
and descriptive paired `bootstrap_95_pp` intervals. The same structure appears
under `subsets.boundary`, `subsets.update`, `subsets.scoping`,
`subsets.same_workspace`, and each entry in `scenarios`. `matched_triples`,
`available`, and `complete` distinguish scored comparisons from unavailable
failure reports; no unavailable effect should be interpreted as zero.

The residual is zero by construction apart from floating-point roundoff. It is
not evidence that causal confounding vanished: Markdown auto-loading versus
bounded prefetch, serialization, client discovery, and equivalent token swaps
remain design differences. Negative components are possible; do not clip them
or interpret the components as causal percentages of the historical 27.8-point
gap. Missing, duplicate, or incomplete triples cannot support a complete
decomposition; failed mechanics remain visible rather than being silently
dropped to improve the comparison.

Reports contain only sanitized condition/case metadata and numeric outcomes.
Raw memory files, prompts, responses, Cortexes, and client events stay owner-only
under `.private/`. This experiment has no formal superiority/success candidate,
does not pool with checkpoint rows, and supplies no new numbers for the published
September tables until a separately reviewed live campaign exists.

### Independent-corpus, size-stratified campaign

The separate [`independent-protocol.md`](independent-protocol.md) defines the
expanded fixed-model target: 30 new synthetic corpus draws, 10 each at 60, 180,
and 600 records; six scenario families, both directions, and all three arms
produce 1,080 campaign retrieval turns. A separate 18-turn pilot smoke covers
all three corpus sizes and is excluded from campaign statistics.

Admission requires valid model/prefetch delivery, actual seed counts, successful
direct-retrieval positive controls, and clean restoration/guards. Natural-query
accuracy and safety errors remain outcomes for every arm, not admission criteria.
This gate revision was operator-approved after an initial pilot failed its
perfect-accuracy rule; that failed pilot and its source snapshot are preserved
separately. Neither the corpus generator nor Noema is tuned to improve that
pilot's natural-query result. The protocol records the revision before holdout
collection.

This is not 1,080 independent observations. The corpus is the uncertainty
cluster: all correlated questions, directions, and conditions remain together
in a size-stratified paired cluster bootstrap. Reports separate size strata,
directions, scenario families, and boundary/update/scoping/same-workspace
subsets. Within-stratum estimates have only 10 corpus clusters. All corpora are
draws from the same frozen synthetic generator; they are not real project data
or newly held-out scenario families.

Generated entity/topic questions avoid literal memory-ID lookup. Seeded draws
vary record selection and placement, wording, distractors, and multi-step
supersession. Unlike the older alpha/beta comparison, this new campaign gives
all arms identical literal records and questions within a task; vocabulary
varies by corpus and direction. Shared Markdown still exposes the whole selected
Cortex-equivalent corpus, not prefetch's selected records, and unrelated corpora
remain isolated.

Use a new private configuration with an explicit shared model and medium
reasoning. The driver locks a privately generated plan, protocol, source hashes,
configuration, and pinned executables before the live smoke:

```bash
python3 tests/omarchy-eval/independent_campaign.py --config /absolute/path/to/local-config.json \
  --run-id independent-YYYYMMDD preflight
python3 tests/omarchy-eval/independent_campaign.py --config /absolute/path/to/local-config.json \
  --run-id independent-YYYYMMDD smoke --confirm-live
python3 tests/omarchy-eval/independent_campaign.py --config /absolute/path/to/local-config.json \
  --run-id independent-YYYYMMDD run --confirm-live
python3 tests/omarchy-eval/independent_campaign.py --config /absolute/path/to/local-config.json \
  --run-id independent-YYYYMMDD analyze
python3 tests/omarchy-eval/independent_campaign.py --config /absolute/path/to/local-config.json \
  --run-id independent-YYYYMMDD restore
```

Collection is sequential. Each corpus has isolated workspaces, Cortexes, client
homes, raw records, and managed restoration. Accuracy and safety errors remain
outcomes; failed integration/prefetch/event/schema/restoration gates stop
collection and produce an explicit incomplete report, not a success-only
headline. Durable evidence must never be silently overwritten or retried.

Every arm uses the same explicit 1 MiB Codex instruction byte limit in its
isolated configuration so larger Markdown is not silently capped by the
ordinary default. Reports identify corpus sizes and materialized memory bytes;
provider context-limit failures remain visible. This does not attest to model
attention or undocumented upstream truncation. Full Markdown context versus
bounded prefetch remains a retrieval/delivery-package contrast.

The generated reports are `independent-smoke.json` and
`independent-smoke-turns.json`, then `independent-campaign.json` and
`independent-turns.json`, under `reports/<run-id>/`. The campaign report's
`summary` contains condition metrics, corpus-cluster intervals, decomposition,
coverage, per-corpus progress, size strata, directions, and scenario/subset
breakdowns. `restoration.json` records the final managed-path and guard checks.
Missing observations prevent a complete campaign headline; no single-corpus
interval is reported.

Holdout prompts, answers, seeds, plan contents, Cortexes, and event streams stay
owner-only under `.private/`. Do not tune against the holdout after collection
starts. This campaign has no formal superiority/equivalence decision and does
not pool with the older qualification or two-vocabulary runs. Natural capture
and additional models remain separate replications.



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

- Smoke stops after the first capture or retrieval integration failure and still
  restores its managed runtime. `model_turns` counts attempted capture/retrieval
  invocations, including failed attempts, rather than assuming every case ran both.
- Recall uses synthetic value tokens in the answer field only. Rationale and
  provenance are scored in their own fields; stale and forbidden tokens are
  checked across all three fields. The `exact_accuracy` metric is token-set F1,
  not binary exact-string accuracy. Hallucination counts detect unexpected
  synthetic tokens, nonempty answers without value tokens, and invalid abstention;
  they do not establish the absence of arbitrary false claims in free text.

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
