# Go/Rust comparison

The comparison is split into correctness and performance gates so a faster
binary cannot hide a behavioral regression.

`compare.sh` runs both executables against one physical cortex. It checks
bidirectional create/read, FTS search, append, archive/unarchive,
trash/recover, sync, integrity verification, event persistence, SQLite
integrity, and exact MCP tool-name parity.

`http_smoke.sh` starts each implementation's Streamable HTTP server and checks
an MCP 2025-03-26 initialization exchange.

`benchmark.sh` creates independent but equivalent cortexes and reports TSV for
process-level ingest, full-text search, filtered list, sync, verification, and
release binary size. `mcp_benchmark.py` keeps each stdio MCP server alive and
reports steady-state lexical-search throughput plus median, p95, and maximum
latency. Defaults are intentionally quick; set
`NOEMA_BENCH_TRACES` and `NOEMA_BENCH_READS` for longer runs.

```sh
make compare-rust
make benchmark-rust
make benchmark-mcp-rust
```

The Go implementation remains the release baseline. Benchmark results are
evidence for the rewrite decision, not a replacement for the compatibility
gate.
