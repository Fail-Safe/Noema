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
`--dry-run`. Actual model quality and resource consumption have not yet been
measured.

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
graduation, and model-driven distillation are now present. Real-model quality
and resource measurements remain outstanding.

## Current interpretation

The additional work further weakens the case for stopping immediately: Rust can
interoperate with the Go federation wire protocol in both directions, express
the critical signing/vector-clock rules cleanly, use materially less memory,
remain competitive for smaller cortexes, and now sustain lower measured RSS and
CPU during a short replicated workload. It still does not justify an all-in
rewrite because larger-result throughput is not better and certificate-lifecycle
checks, semantic search, plugins, watcher parity, real-model distillation
evaluation, and broader operator recovery behavior remain incomplete.
