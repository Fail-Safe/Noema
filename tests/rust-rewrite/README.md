# Go/Rust comparison

The comparison is split into correctness and performance gates so a faster
binary cannot hide a behavioral regression.

`compare.sh` runs both executables against one physical cortex. It checks
bidirectional create/read, FTS search, append, archive/unarchive,
trash/recover, sync, integrity verification, event persistence, SQLite
integrity, and exact MCP tool-name parity.

`http_smoke.sh` starts each implementation's Streamable HTTP server and checks
an MCP 2025-03-26 initialization exchange.

`federation_network.py` runs signed mixed-implementation federation in both
directions. It covers a 205-event, three-batch Go-to-Rust pull; incremental
archive/promote/trash replay; unreachable-peer cursor pinning; endpoint identity
replacement rejection; retry recovery; and a signed Rust-to-Go pull under
`verify: enforce`.

`federation_ring.py` runs one Go and two Rust servers as a signed three-node
mesh using their background schedulers. It checks eventual convergence and
event de-duplication, independent usage-signal replication, live peer
pause/resume, classified outage and recovery health, concurrent-edit divergence,
graceful SIGTERM handling, and fail-closed signing-key rotation.
The rotation case then stages an explicit hard pin without resetting the event
cursor, resumes the peer, and verifies recovery under the new key.

`federation_tls.py` runs signed Go↔Rust federation over HTTPS with a temporary
private CA, CA-signed server certificate, and shared bearer key. It verifies
missing/wrong/correct authorization, custom-CA failure and recovery, secret-free
failure output, and the keyed-plaintext startup refusal.

`consolidation_election.py` runs Go and Rust eligibility loops against temporary
OpenAI-compatible `/models` probes. It checks rank advertisement and exchange,
deterministic winner agreement, endpoint-loss demotion and failover, recovery,
and signed Go claim/success replay without creating synthetic trace rows. The
Go threshold-election restart uses a delayed eligibility probe so its fixed,
quiet rank remains stable while the real consolidator claims the test window.
The fixture then makes that signed Go claim orphaned on the Rust replica and
checks that Rust's live watchdog emits a signed `watchdog_expired` closure which
Go accepts. Finally, it makes Rust the stable winner, runs the real threshold
heuristic behind the pass gate, and verifies Go receives both the promotion and
the Rust-signed claim/success window. Active endpoint-probe delays are applied
to the live fixture servers so fixed quiet ranks cannot be overwritten during
either gated startup.

`heuristic_promotion.py` gives independent Go and Rust cortexes the same usage,
vote, inbound-lineage, outbound-source, and age signals. It starts each live
threshold scheduler and checks identical short-to-mid choices, exact promotion
event data, Markdown frontmatter updates, window filtering, the single-source
search-hit guard, and restart idempotency.

`benchmark.sh` creates independent but equivalent cortexes and reports TSV for
process-level ingest, full-text search, filtered list, sync, verification, and
release binary size. `mcp_benchmark.py` keeps each stdio MCP server alive and
reports steady-state lexical-search throughput plus median, p95, and maximum
latency, along with sampled RSS and CPU use. It alternates implementation order
and defaults to five runs. Defaults are intentionally quick; set
`NOEMA_BENCH_TRACES` and `NOEMA_BENCH_READS` for longer runs.

`federation_soak.py` gives homogeneous three-node Go and Rust clusters the same
bounded mutation schedule, restarts one node halfway through, verifies exact
event convergence, and reports sampled cluster RSS and CPU as JSON. Its default
is eight seconds and 80 mutations per cluster.

```sh
make compare-rust
make benchmark-rust
make benchmark-mcp-rust
make soak-rust
```

The Go implementation remains the release baseline. Benchmark results are
evidence for the rewrite decision, not a replacement for the compatibility
gate.
