# Rust rewrite experiment results

Date: 2026-08-15. Host: Apple Silicon macOS. Both implementations were built
with release optimizations. Results are local comparisons, not portable
performance guarantees.

## Persistent MCP connection

The first Rust MCP implementation reopened the cortex for every tool call. At
100 traces in a single preliminary run it delivered 698 searches/second versus
791 for Go. The server now retains one Tokio-mutex-protected cortex connection
for its lifetime.

Five alternating-order runs after that change produced:

| Corpus | Metric | Go median | Rust median | Rust relative result |
| ---: | --- | ---: | ---: | ---: |
| 250 | requests/second | 386.91 | 409.83 | 5.9% higher |
| 250 | median latency | 2.537 ms | 2.425 ms | 4.4% lower |
| 250 | p95 latency | 3.078 ms | 2.645 ms | 14.1% lower |
| 250 | maximum RSS | 33,200 KiB | 16,944 KiB | 49.0% lower |
| 250 | sampled CPU utilization | 115.7% | 86.1% | 25.6% lower |
| 1,000 | requests/second | 105.31 | 101.01 | 4.1% lower |
| 1,000 | median latency | 9.455 ms | 9.857 ms | 4.3% higher |
| 1,000 | p95 latency | 10.109 ms | 10.287 ms | 1.8% higher |

The throughput crossover suggests that result construction/serialization at
larger result sets matters more than connection setup. Rust has a clear memory
and CPU advantage at the measured 250-trace workload, but no general
steady-state throughput win has been established.

## Signed federation and mixed-process sync

The Rust cortex now has direct replay, a one-shot Streamable HTTP peer pull, and
a cancellable background scheduler with per-peer exponential backoff. The unit
and mixed-process tests cover:

- Ed25519 verification before any state mutation;
- trust-on-first-use key pinning under the Go-compatible `cortexkey:<id>` key;
- stable peer-identity pinning and endpoint-replacement rejection;
- monotonic event ULIDs for lexical federation cursors;
- 205-event pagination across three batches;
- per-event cursor advancement, unavailable-peer pinning, and retry recovery;
- vector-clock merge and causal update convergence;
- duplicate-event idempotency;
- update-before-create materialization without rolling state backward;
- concurrent-update divergence traces containing both versions;
- tampered signed-payload rejection; and
- foreign source-lock enforcement;
- metadata, archive/unarchive, trash/recover, tier, vote, and purge replay; and
- signed Go-to-Rust and Rust-to-Go convergence under `verify: enforce`;
- independent per-peer usage cursors and monotonic `MAX` counter merges;
- three-node background convergence with transitive duplicate delivery;
- live pause/resume, outage classification and recovery, and graceful SIGTERM;
- concurrent edits across two disconnected replicas; and
- fail-closed signing-key rotation with event cursors left unchanged, followed
  by explicit hard-pin recovery without replaying old signatures;
- shared bearer-key authentication with environment-over-sidecar precedence;
- Rustls HTTPS serving and TLS 1.2-or-newer outbound federation;
- private-CA trust for both Go-to-Rust and Rust-to-Go federation; and
- missing/wrong bearer rejection, missing-CA rejection, live outbound access-key
  reload, plaintext-auth refusal, and secret-redacted failure output.

The network experiment found two interoperability defects that in-process tests
did not expose. Rust's first identity response did not use Go's `version` and
`pubkey` fields. More subtly, Noema signatures cover the exact event payload
bytes: decoding JSON into a sorted map or pretty-printing `sync_events` changed
those bytes and invalidated an otherwise correct signature. The Rust build now
preserves insertion order, omits fields exactly like Go's event snapshot, emits
compact federation JSON, and has a Go-side signed Rust wire fixture.

The three-node test found another wire edge: Go serializes an empty nil event
slice as `null`, while the original Rust client accepted only `[]`. Rust now
normalizes either form to an empty batch. The same tolerance applies to empty
usage-signal results.

## Consolidation election foundation

The Rust server now implements the coordination layer below the actual
distillation pass:

- Go-compatible eligibility gates for feature enablement, scheduling triggers,
  federation mode, and an OpenAI-compatible `/models` health probe;
- cryptographically random ranks from 1 through 99, with rank zero reserved for
  ineligible nodes;
- persisted local and per-peer rank advertisements exchanged through the
  existing federation identity handshake;
- quiet-period filtering, highest-rank selection, and lexical cortex-ID
  tie-breaking;
- operator-visible winner and local-run decisions in federation status;
- signed claim, success, and failure coordination events that replay into the
  audit log without materializing synthetic traces; and
- a cross-runtime background lock so only one server process per cortex runs
  federation and eligibility workers.

The next coordination layer is also present. A reusable Rust pass gate now
matches Go's initial election, signed claim, quiet-period wait, re-election,
preemption classification, in-flight tracking, inner-pass error closure, and
success behavior. The live Rust server runs a stale-claim watchdog with a strict
local timeout, extra remote propagation grace, in-flight suppression, and
event-log-based deduplication.

Rust now also runs a real deterministic pass from the threshold trigger. Its
candidate query matches Go's active-state and rolling-window filters and sums
usage across peers. Its score matches the Go weights for reads/search hits,
modifications, inbound lineage, and votes, including the two-inbound-reference
minimum and the single-source passive-search guard. Qualifying traces move from
short to mid in both SQLite and Markdown, emit the same promotion event data,
and leave the candidate pool so a restart cannot promote them twice. Federated
cortexes execute this pass through the existing election gate; single-node
cortexes avoid unnecessary claim/success events.

The mixed Go/Rust fixture exercises deterministic winner selection, endpoint
loss and rank-zero failover, endpoint recovery, and replay of a real Go
threshold pass's signed claim/success pair. Both the election and three-node
ring scenarios passed five consecutive repetitions after their timing
preconditions were made explicit. The fixture now also removes a terminal event
from the Rust replica to model an orphaned Go claim, then verifies Rust emits a
signed `watchdog_expired` closure that Go accepts under enforced verification.

The independent Go/Rust promotion fixture gives both implementations identical
read, search, edit, vote, lineage, age, and archive inputs. It checks exact
candidate choices, event data, frontmatter mutation, and restart idempotency;
the scenario passed five consecutive repetitions.

The rest of deterministic tier maintenance is now present as one cadence owner,
preserving Go's cron-before-threshold-before-idle priority. Cron fires once per
local day; federated runs wait for a success event and retry three times at
five-minute intervals, while single-node runs mark the day immediately. Idle
requires a real event-history timestamp and uses any prior pass as its cooldown
boundary. Graduation runs after the heuristic pass and uses the same AND-gates:
minimum age, reads plus search hits, optional unmodified requirement, no active
downvote, and no automatic promotion for preference traces.

A second independent Go/Rust fixture drives midnight cron and a backdated idle
log, checking old/young, read/search, modified, downvoted, preference, and
archived cases. It verifies exact `mid` to `long` event data, Markdown updates,
and restart idempotency. The scenario passed five consecutive repetitions.

Model-driven consolidation is now wired ahead of those maintenance passes. The
Rust candidate query excludes source IDs already recorded by a prior
`consolidate` event without promoting the sources themselves. Deterministic
type-and-day buckets honor the small/large/frontier cluster ceilings, call an
OpenAI-compatible endpoint with bounded retries, normalize returned tags, and
materialize a mid-tier observation with exact `derived_from` lineage plus a
separate telemetry event. Invalid or unreachable model responses fall back to
the same score-gated heuristic, while cadence still reaches graduation.

A controlled-endpoint fixture now compares Go and Rust success output, request
shape, malformed-response retries, offline behavior, source-tier preservation,
lineage, telemetry, and restart idempotency. The signed federation fixture also
creates distilled traces through each runtime's MCP tool and proves the other
runtime replays both the create snapshot and consolidate event exactly once.
The same fixture now drives each operator CLI with manifest values deliberately
overridden by flags. It compares summary text and emitted JSON, and proves both
successful distillation and malformed-response fallback remain write-free under
`--dry-run`.

## Real-model consolidation

Seven alternating-order runs drove the release Go and Rust CLIs against the
same trusted LAN OpenAI-compatible endpoint and `qwen3.5-9b` model. Each run
created fresh cortexes containing nine synthetic traces: one cohesive decision
bucket, one deliberately unrelated fact bucket, and one cohesive observation
bucket. Both CLIs used `--dry-run`, so the model saw only synthetic fixture data
and neither cortex was mutated by consolidation.

| Metric | Go | Rust | Rust relative result |
| --- | ---: | ---: | ---: |
| Bucket decisions | 21/21 | 21/21 | equal |
| Planted-term retention | 111/112 | 112/112 | one additional term retained |
| Median wall time | 17.19 s | 13.68 s | 20.4% lower |
| Wall-time range | 16.55-18.45 s | 11.85-18.10 s | wider model variance |
| Median peak RSS | 23.06 MiB | 14.75 MiB | 36.0% lower |
| Peak-RSS range | 22.77-23.33 MiB | 14.69-14.88 MiB | consistently lower |

Every run made five successful HTTP requests per implementation. The final two
runs added request accounting: Go sent 2,488 prompt tokens and 11,071 request
bytes per run, while Rust sent 1,220 prompt tokens and 5,206 request bytes. Rust
therefore used 51.0% fewer prompt tokens and 53.0% fewer request bytes on this
fixture. Completion-token counts varied with model output, as expected.

The lower Rust wall time is principally a model-workload result, not evidence
that its HTTP client executes inference faster: nearly the entire measured wall
time was spent awaiting the endpoint, while each CLI consumed only about 0.00
to 0.02 seconds of user plus system CPU. The experiment does establish that the
shorter Rust prompt retained the tested decisions, values, and unrelated-bucket
rejection while materially reducing prompt cost. It does not establish general
distillation-quality parity; larger, ambiguous, and adversarial corpora remain
useful follow-up work. The reusable runner is
`tests/rust-rewrite/real_model_distillation.py`; it records only counts,
timings, and synthetic result text, never endpoint response bodies.

## Filesystem watcher parity

The original Rust watcher slept once per raw notification and called whole-
cortex `sync`. That approach reindexed files but did not emit mutation events,
distinguish moves from deletes, preserve source locks, rescue malformed files,
or guarantee true per-path debounce.

The replacement owns one Cortex connection under the existing per-cortex
background lock and reconciles each settled path against its SQLite state. A
mixed-process fixture now gives Go and Rust identical external edits and checks:

- unchanged editor bytes after body/frontmatter reindex;
- one update for a five-write burst;
- valid create plus archive/unarchive transitions without transient trash;
- atomic remove-and-replace without delete misclassification;
- frontmatter reconstruction with user body and indexed title preserved;
- raw Markdown onboarding with canonical rename and provenance;
- recoverable active-file deletion followed by external trash purge; and
- refusal to ingest edits to foreign source-locked traces.

The complete fixture passes in `make compare-rust`, and three earlier
repetitions passed concurrently as a timing-stress check. Watcher startup is
also synchronous with server readiness, so a server cannot begin accepting MCP
traffic before directory registration succeeds.

On this macOS host, `notify` 8.2's default FSEvents backend registered without
error but did not deliver events for the temporary-cortex fixture. Its polling
fallback also missed same-second writes unless content hashing was enabled,
which would impose repeated full-file reads. The Rust watcher therefore keeps
the existing native backend for low-latency delivery and adds a dependency-free
metadata rescan using full `SystemTime` precision and file length. The rescan
interval is five debounce windows bounded to 250 milliseconds through two
seconds; a focused test pins same-second detection. A post-reconcile snapshot
also records files created internally during recoverable delete or onboarding,
so an immediate follow-up purge or edit remains observable.

## Semantic-search parity

The Rust build now uses the existing Reqwest/Rustls client for the same
OpenAI-compatible `/embeddings` contract as Go. A deterministic three-topic
endpoint reverses every response batch while retaining valid `index` fields,
which proves both clients restore input order rather than trusting wire order.
The fixture also verifies environment-based bearer authentication and the
configured Unicode-safe 40-character input cap without recording the token.

Both builds produce byte-identical version-1 little-endian float32 BLOBs after
normalization. The lifecycle fixture covers initial missing rows, idempotent
reruns, body-edit staleness, model-change invalidation, force and limit controls,
source/content-hash equality, and automatic maintenance under `noema serve`.
Malformed stored candidates with the wrong dimension or non-finite elements are
skipped rather than allowed to poison a result set.

Cosine search, stored-vector similarity, and weighted reciprocal-rank fusion
return the same topic ordering across Go and Rust. The fixture also pins source
exclusion and archived visibility. CLI flags and MCP modes use the same core;
when an MCP embedding endpoint is unavailable, both return lexical results with
a generic note that does not expose the endpoint address.

This is a deterministic contract test, not a semantic-quality benchmark. A
real-model comparison would be useful only after selecting a representative
embedding model and query corpus; model quality and index throughput remain
separate questions from the wire/storage/ranking parity established here.

## MCP contract parity

The MCP comparison now snapshots both servers' discovered tool contracts and
normalizes only protocol-equivalent schema dialect differences. The 28 tool
names, parameter sets, required fields, advertised enums, field descriptions,
and output-schema presence match. The fixture also exercises structured cortex
usage and tag results, startup policy reads, explicit usage recording, tag and
append mutations, archive transitions, lexical search, history, invalid votes,
and instruction sections.

Three previously shallow Rust tools now use the shared Cortex storage and event
contracts. `search_activity` aggregates federation-wide usage rows for traces
and tags. `consolidation_health` reports daily activity, separates election
preemption from pipeline failure, derives short-to-mid and mid-to-long latency,
and exposes the one-source-mid leak detector. `resolve_divergence` applies a
selected or custom body through the guarded update path and trashes the resolved
conflict. Synthetic fixed-time rows make these outputs deterministic across Go
and Rust.

The fixture found one invisible side-effect mismatch: Rust's MCP append path
incremented `modify_count`, while Go's fire-and-forget append does not. Aligning
that policy restored identical tag-activity output. Rust deliberately retains
stricter runtime rejection for trace types outside the enum that both servers
advertise; Go currently accepts such input despite its discovery schema.

This is representative contract parity, not exhaustive coverage of every
successful and failing result from all 28 tools. Advanced federation and
consolidation surfaces plus read-only mode restrictions remain to be added.

## Bounded federation soak

Equivalent homogeneous three-node clusters ran 80 paced create mutations over
eight seconds, with one node gracefully restarted halfway through. Both builds
converged to exactly 80 unique event rows per node and shut down gracefully.

| Metric (three-server aggregate) | Go | Rust | Rust/Go |
| --- | ---: | ---: | ---: |
| Wall time | 9.586 s | 9.112 s | 0.950 |
| Peak sampled RSS | 90.266 MiB | 56.469 MiB | 0.626 |
| Median sampled RSS | 86.594 MiB | 56.172 MiB | 0.649 |
| Mean sampled CPU | 7.913% | 6.429% | 0.812 |
| Peak sampled CPU | 12.7% | 9.1% | 0.717 |

This is a short local soak, not a stability claim. It does establish that the
Rust scheduler retained its earlier memory advantage under active replication
and recovery rather than only under a single-process search benchmark.

This remains a complexity probe, not complete federation parity. The Rust path
does not yet dynamically add new workers without restart, monitor certificate
expiry, or provide crash-atomic file rollback. Its consolidation election and
coordination wire, pass gate, watchdog recovery, full cadence, heuristic pass,
graduation, and model-driven distillation are now present. A bounded real-model
comparison also passes, but broader distillation-quality evaluation remains
advisable.

## Current interpretation

The additional work further weakens the case for stopping immediately: Rust can
interoperate with the Go federation wire protocol in both directions, express
the critical signing/vector-clock rules cleanly, use materially less memory,
remain competitive for smaller cortexes, and now sustain lower measured RSS and
CPU during a short replicated workload. The real-model fixture also indicates
that Rust's shorter consolidation prompt can preserve the tested information at
about half the prompt-token cost. Watcher behavior now also matches the tested
Go outcomes, as do deterministic semantic indexing/ranking and the
representative MCP contract. It still does not justify an all-in rewrite
because larger-result throughput is not better and certificate-lifecycle
checks, exhaustive advanced MCP errors, plugins, broader distillation-quality
evaluation, and operator recovery behavior remain incomplete.
