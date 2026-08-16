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
| Events and signing | Unit + differential | Canonical signatures verify across languages; mutation clocks remain monotonic | Rust signing tests, Go Rust-wire fixture, mixed signed federation |
| MCP stdio | Contract | Exact tool-name set, compatible input schemas, result semantics, and errors | `mcp_contract.py` covers names, schemas, structured results, lifecycle, observability, candidates/distillation/lineage, identity/status/sync, non-mutating announcements, and invalid/missing-resource behavior |
| MCP HTTP | Integration | Both transports initialize and serve calls; auth, TLS, host validation, shutdown, and federation-mode restrictions match | `http_smoke.sh`; `federation_tls.py` covers authenticated HTTPS, custom CA, and plaintext refusal; `mcp_contract.py` covers every publish-mode mutation guard, local stdio exemption, and subscribe-mode publication guards |
| Watcher | Integration | External create/edit/rename is debounced, onboarded, indexed once, and healed after atomic save | `watcher_parity.py` covers edit bytes, metadata reindex, burst debounce, create, archive/unarchive, atomic save, heal, onboarding, delete/purge, and source locks |
| Federation | Multi-process | Multiple nodes converge under normal, duplicate, reordered, concurrent, signed, paused, and unavailable-peer cases | `federation_network.py` covers signed bidirectional batching/lifecycle; `federation_ring.py` covers three nodes, background sync, pause/recovery, concurrency, shutdown, and key-rotation recovery; `federation_tls.py` covers authenticated mixed-runtime TLS |
| Memory tiers | Unit + differential | Usage counters, scoring, promotion, graduation, votes, purge, and health output match | Per-peer usage publication/merge, short-to-mid scoring/promotion, strict mid-to-long graduation, search activity, promotion latency, election/failure totals, and one-source leak reporting automated |
| Semantic search | Contract + integration | Mock embedding endpoint, codec, stale detection, backfill, cosine ranking, and hybrid RRF match | `semantic_search.py` covers request/auth/order/text limits, exact BLOBs, freshness, bounded/idempotent backfill, ranking, archive/source rules, corrupt vectors, MCP degradation, and automatic maintenance |
| Consolidation | Deterministic + bounded real-model integration | Fake LLM covers election, gating, clustering, success/failure events, and idempotency; real model preserves planted synthetic facts and rejects unrelated buckets | Rank/election/watchdog/cadence, heuristic promotion, graduation, all three model profiles, CLI overrides/dry-run/JSON, distilled lineage/telemetry, malformed/offline fallback, restart idempotency, signed bidirectional replay, and seven-run synthetic real-model comparison automated |
| Plugins | File integration | Embedded payload hashes, install/check/force rules, target resolution, and drift reporting match | `plugin_lifecycle.py` covers all six payloads, independent CLI dispatch, target guards, dry-run non-mutation, idempotency, drift hashes/refusal, forced atomic file/symlink replacement, unmanaged files, and temp cleanup |
| TUI | State-machine + snapshot | Navigation, filters, tier actions, themes, resize, and error recovery match | Minimal smoke implementation only |
| Fault tolerance | Fault injection | Interrupted writes, malformed Markdown, corrupt DB, unavailable endpoints, and lock contention fail safely | Background-lock single-owner/reacquisition unit coverage; broader fault injection pending |
| Security | Negative integration | Path traversal, source-lock bypass, forged signatures, bearer auth, TLS expiry, and secret redaction match | Signing/source-lock, bearer rejection, private-CA rejection, plaintext-auth refusal, and secret-redaction cases present; TLS expiry pending |

## Performance protocol

Use stripped release binaries on the same host, with equivalent fresh
cortexes. Record at least five runs and report median plus spread rather than
selecting the fastest run.

- Process-level CLI: ingest, lexical search, filtered list, sync, verify.
- Long-lived MCP: warm-up, requests/second, median, p95, and maximum latency.
- Resource use: binary size, idle RSS, peak RSS during ingest/search, and CPU.
- Bounded federation soak: equal three-node mutation schedules, restart recovery,
  exact event counts, graceful shutdown, and sampled aggregate RSS/CPU.
- Scale: 100, 1,000, 10,000, and 100,000 traces where practical.
- Concurrency: parallel readers, reader/writer mix, watcher activity, and
  federation replay under the race detector or equivalent stress tooling.

Any benchmark run is invalid if its corresponding correctness gate fails.

## Current decision gaps

The branch is suitable for core-format, background-federation, and early
performance comparison, but is not yet a release replacement. The Rust HTTP
server serves shared-key-authenticated HTTPS, refuses authenticated plaintext,
and otherwise permits unauthenticated HTTP only on loopback. Semantic backfill,
ranking, MCP routing, and automatic maintenance now pass the deterministic Go
oracle. MCP discovery, successful advanced-tool results, invalid inputs,
missing resources, and federation-mode transport boundaries are automated.
Certificate-expiry monitoring, full TUI behavior, and broader adversarial
real-model quality evaluation still require ports and parity fixtures.
