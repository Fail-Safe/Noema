# Rust rewrite experiment results

Date: 2026-08-14. Host: Apple Silicon macOS. Both implementations were built
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

The Rust cortex now has direct replay plus a one-shot Streamable HTTP peer pull.
The unit and mixed-process tests cover:

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
- signed Go-to-Rust and Rust-to-Go convergence under `verify: enforce`.

The network experiment found two interoperability defects that in-process tests
did not expose. Rust's first identity response did not use Go's `version` and
`pubkey` fields. More subtly, Noema signatures cover the exact event payload
bytes: decoding JSON into a sorted map or pretty-printing `sync_events` changed
those bytes and invalidated an otherwise correct signature. The Rust build now
preserves insertion order, omits fields exactly like Go's event snapshot, emits
compact federation JSON, and has a Go-side signed Rust wire fixture.

This remains a complexity probe, not complete federation parity. The Rust path
is an explicit one-shot pull rather than the production background scheduler.
It does not yet sync usage signals, populate structured peer health/backoff,
support shared-key auth or custom CAs, cover every consolidation/purge action,
exercise multi-peer gossip/key rotation, or provide crash-atomic file rollback.

## Current interpretation

The additional work further weakens the case for stopping immediately: Rust can
interoperate with the Go federation wire protocol in both directions, express
the critical signing/vector-clock rules cleanly, use materially less memory,
and remain competitive for smaller cortexes. It still does not justify an
all-in rewrite because larger-result throughput is not better and production
background sync, usage aggregation, security transports, consolidation,
semantic search, plugins, and operator behavior remain unported.
