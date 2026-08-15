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
