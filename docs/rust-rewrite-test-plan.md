# Rust rewrite comparison plan

The Go executable is the behavioral oracle until the Rust implementation
passes every required parity gate. Performance wins do not waive correctness,
integrity, or security requirements.

## Required gates

| Area | Test level | Acceptance criterion | Current automation |
| --- | --- | --- | --- |
| Trace/frontmatter and IDs | Unit + property | Go-compatible parse/write, validation, hashing, and collision behavior | Rust unit tests; expand with proptest corpus |
| Database and migrations | Unit + integration | Exact Go migration chain through schema 19; SQLite integrity remains `ok` | Rust migration test and differential suite |
| CRUD lifecycle | Differential | Either build can create, read, search, append, archive, trash, recover, sync, and verify the same cortex | `compare.sh` |
| Events and signing | Unit + differential | Canonical signatures verify across languages; mutation clocks remain monotonic | Rust signing tests and Go-key/Rust-event check |
| MCP stdio | Contract | Exact tool-name set, compatible input schemas, result semantics, and errors | Tool-name parity automated; schema/result fixtures pending |
| MCP HTTP | Integration | Both transports initialize and serve calls; auth, TLS, host validation, and shutdown match | `http_smoke.sh`; security cases pending |
| Watcher | Integration | External create/edit/rename is debounced, onboarded, indexed once, and healed after atomic save | Pending parity fixtures |
| Federation | Multi-process | Two nodes converge under normal, duplicate, reordered, concurrent, signed, paused, and unavailable-peer cases | Pending two-node harness |
| Memory tiers | Unit + differential | Usage counters, scoring, promotion, graduation, votes, purge, and health output match | Basic tier mutations implemented; scoring parity pending |
| Semantic search | Contract + integration | Mock embedding endpoint, codec, stale detection, backfill, cosine ranking, and hybrid RRF match | Codec unit test only |
| Consolidation | Deterministic integration | Fake LLM covers election, gating, clustering, success/failure events, and idempotency | Pending |
| Plugins | File integration | Embedded payload hashes, install/check/force rules, target resolution, and drift reporting match | Pending |
| TUI | State-machine + snapshot | Navigation, filters, tier actions, themes, resize, and error recovery match | Minimal smoke implementation only |
| Fault tolerance | Fault injection | Interrupted writes, malformed Markdown, corrupt DB, unavailable endpoints, and lock contention fail safely | Pending |
| Security | Negative integration | Path traversal, source-lock bypass, forged signatures, bearer auth, TLS expiry, and secret redaction match | Signing/source-lock core present; full gate pending |

## Performance protocol

Use stripped release binaries on the same host, with equivalent fresh
cortexes. Record at least five runs and report median plus spread rather than
selecting the fastest run.

- Process-level CLI: ingest, lexical search, filtered list, sync, verify.
- Long-lived MCP: warm-up, requests/second, median, p95, and maximum latency.
- Resource use: binary size, idle RSS, peak RSS during ingest/search, and CPU.
- Scale: 100, 1,000, 10,000, and 100,000 traces where practical.
- Concurrency: parallel readers, reader/writer mix, watcher activity, and
  federation replay under the race detector or equivalent stress tooling.

Any benchmark run is invalid if its corresponding correctness gate fails.

## Current decision gaps

The branch is suitable for core-format and early performance comparison, but
is not yet a release replacement. Federation replay/sync, semantic backfill
and ranking, full consolidation, TLS/shared-key HTTP security, plugin
installation, watcher onboarding/healing, divergence resolution, and full TUI
behavior still require ports and parity fixtures.
