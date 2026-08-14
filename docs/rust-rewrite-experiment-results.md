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

## Signed federation replay spike

The Rust cortex now has an experimental direct replay path for full-snapshot
`create` and `update` events. Its integration test covers:

- Ed25519 verification before any state mutation;
- trust-on-first-use key pinning under the Go-compatible `cortexkey:<id>` key;
- monotonic event ULIDs for lexical federation cursors;
- vector-clock merge and causal update convergence;
- duplicate-event idempotency;
- update-before-create materialization without rolling state backward;
- concurrent-update divergence traces containing both versions;
- tampered signed-payload rejection; and
- foreign source-lock enforcement.

This is a complexity probe, not complete federation parity. It does not yet
include the HTTP peer sync loop, cursor/health management, usage-signal merge,
all mutation actions, key rotation, multi-peer gossip, or crash-atomic file
rollback.

## Current interpretation

The additional work weakens the case for stopping immediately: Rust can express
the critical signing/vector-clock rules cleanly, uses materially less memory,
and is competitive for smaller cortexes. It still does not justify committing
to a full rewrite because larger-result throughput is not better and the replay
spike covers only a fraction of production federation behavior.
