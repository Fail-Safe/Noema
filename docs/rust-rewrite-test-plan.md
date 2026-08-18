# Rust rewrite comparison plan

The Go executable is the behavioral oracle until the Rust implementation
passes every required parity gate. Performance wins do not waive correctness,
integrity, or security requirements.

## Required gates

| Area | Test level | Acceptance criterion | Current automation |
| --- | --- | --- | --- |
| Trace/frontmatter and IDs | Unit + property | Go-compatible parse/write, validation, hashing, and collision behavior | Rust unit tests; expand with proptest corpus |
| Database and migrations | Unit + integration | Exact Go migration chain through schema 19; SQLite integrity remains `ok` | Rust migration test and differential suite |
| Backup and restore | Differential + negative integration | Go and Rust archives restore across runtimes without identity duplication, destination loss, metadata leakage, or archive traversal | `restore_parity.py` covers both archive directions, trace preservation, duplicate-ID refusal, forced replacement, transaction cleanup, traversal, and links; Rust unit tests also cover names, paths, roots, duplicates, rollback, metadata, long paths, self-inclusion, the exact legacy Obsidian-facing `.trash -> trash/traces/` alias emitted by Go backup, and rejection of every other link; eight subprocess scenarios cover journaled resume/rollback, tamper refusal, redaction, permissions, contention, and killed-owner lock release |
| CRUD lifecycle | Differential | Either build can create, read, search, append, archive, trash, recover, sync, and verify the same cortex | `compare.sh` |
| Events and signing | Unit + differential | Canonical signatures verify across languages; mutation clocks remain monotonic | Rust signing tests, Go Rust-wire fixture, mixed signed federation |
| MCP stdio | Contract | Exact tool-name set, compatible input schemas, result semantics, and errors | `mcp_contract.py` covers names, schemas, structured results, lifecycle, observability, candidates/distillation/lineage, identity/status/sync, non-mutating announcements, and invalid/missing-resource behavior |
| MCP HTTP | Integration | Both transports initialize and serve calls; auth, TLS, host validation, shutdown, and federation-mode restrictions match | `http_smoke.sh` covers both builds plus Rust repeated dual-stack static listeners and dynamic local-address reconciliation; unit and deployment-canary checks cover a DNS Host-header allowlist distinct from listener binding while retaining rejection of unknown names; `federation_tls.py` covers authenticated HTTPS, custom CA, and plaintext refusal; `tls_certificate_lifecycle.py` covers validity gates, CLI precedence, monitor bands, and redaction; `mcp_contract.py` covers every publish-mode mutation guard, local stdio exemption, and subscribe-mode publication guards |
| Watcher | Integration | External create/edit/rename is debounced, onboarded, indexed once, and healed after atomic save | `watcher_parity.py` covers edit bytes, metadata reindex, burst debounce, create, archive/unarchive, atomic save, heal, onboarding, delete/purge, and source locks |
| Federation | Multi-process | Multiple nodes converge under normal, duplicate, reordered, concurrent, signed, paused, and unavailable-peer cases | `federation_network.py` covers signed bidirectional batching/lifecycle; `federation_ring.py` covers three nodes, background sync, pause/recovery, concurrency, shutdown, and key-rotation recovery; `federation_dynamic.py` covers live worker add/remove/re-add and cursor preservation; `federation_tls.py` covers authenticated mixed-runtime TLS |
| Memory tiers | Unit + differential | Usage counters, scoring, promotion, graduation, votes, purge, and health output match | Per-peer usage publication/merge, short-to-mid scoring/promotion, strict mid-to-long graduation, search activity, promotion latency, election/failure totals, and one-source leak reporting automated |
| Semantic search | Contract + integration | Mock embedding endpoint, codec, stale detection, backfill, cosine ranking, and hybrid RRF match | `semantic_search.py` covers request/auth/order/text limits, exact BLOBs, freshness, bounded/idempotent backfill, ranking, archive/source rules, corrupt vectors, MCP degradation, and automatic maintenance |
| Consolidation | Deterministic + bounded real-model integration | Fake LLM covers election, gating, clustering, success/failure events, and idempotency; real model preserves planted synthetic facts and rejects unrelated buckets | Rank/election/watchdog/cadence, heuristic promotion, graduation, all three model profiles, exact cross-runtime prompt/request contracts, ID-free model input, strict output shape, CLI overrides/dry-run/JSON, malformed/offline fallback, restart idempotency, signed bidirectional replay, seven-run baseline comparison, four-run diagnostic comparison, two independent six-run untagged small-profile comparisons, and three-run large/frontier real-model qualification automated |
| Plugins | File integration | Embedded payload hashes, install/check/force rules, target resolution, and drift reporting match | `plugin_lifecycle.py` covers all six payloads, independent CLI dispatch, target guards, dry-run non-mutation, idempotency, drift hashes/refusal, forced atomic file/symlink replacement, unmanaged files, and temp cleanup |
| TUI | State-machine + styled-buffer + live PTY | Navigation, filters, tier actions, themes, resize, editor handoff, exit, terminal recovery, and Go visual hierarchy match | Eight Rust TUI tests cover navigation/focus/scroll, search/tier/help transitions, lifecycle and vote mutations, sticky live refresh/highlights, right-aligned header state, Go-matched divider/title columns, explicit title ellipsis, dark/light tier and chip cell colors, wrapped tags, full-width scroll indicators, and pinned metadata; `tui_pty.py` covers the real backend's editor leave/show/re-enter sequence, 24-to-32-row redraw, keyboard exit, canonical/echo mode, alternate screen, and cursor restoration. The resize/quit sequence passed 30 consecutive runs with Crossterm's Unix polling backend; an identical-size capture exposed and drove a one-cell pane correction, and the final release-binary recapture visually matches Go in the original terminal. Multi-emulator human review remains |
| Fault tolerance | Fault injection | Interrupted writes, malformed Markdown, corrupt DB, unavailable endpoints, and lock contention fail safely | `background_lock_contention.py` covers MCP-only losers, killed-owner release, and replacement acquisition; `io_failure_safety.py` covers pre-mutation trace, lifecycle, and manifest I/O refusal with unchanged bytes/DB/events; Rust unit tests inject returned SQLite aborts after local, replay, destructive, watcher reconstruction, and restore mutations; five subprocess scenarios `SIGKILL` create/replace/move/delete/reconstruction after the filesystem mutation; nine restore subprocess scenarios exercise hash-bound resume/rollback, config-replace death, malformed/tampered journals, contention, and killed-owner lock release; an independent config subprocess proves byte-identical old YAML and clean retry after a killed writer; `storage_fault_macos.py` forcibly detaches disposable APFS images and verifies restore rollback/resume, config rename, strong journal cleanup, SQLite integrity, and acknowledged standard state across remount; the differential fixture proves Go refuses pending Rust recovery until Rust repairs it; five safety tests cover corrupt DB bytes, malformed records, path traversal, non-mutating status, and CLI redaction; corrupt-DB salvage, older-Go takeover, automatic startup restore reconciliation, and device-cache-loss qualification remain pending |
| Security | Negative integration | Path traversal, source-lock bypass, forged signatures, bearer auth, TLS expiry, and secret redaction match | Signing/source-lock, archive path/link/multi-root/duplicate rejection, bearer rejection, private-CA rejection, plaintext-auth refusal, expired/future validity refusal, explicit bypass, monitor transitions, and secret/certificate-content redaction automated |

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

The final scale gate has five alternating runs at 10,000 traces and five runs
on a nominal 100,000-trace, independently cloned, byte-identical verified
corpus, all with four MCP clients. Each scenario used fresh server processes,
both builds passed `verify`, and mixed writes matched exact expected SQLite
counts. With `NOEMA_DURABILITY=standard`, Rust leads every measured 10,000-
and 100,000-scale throughput median, including mixed writes. The default
`strong` profile remains about 13x behind Go on the 10,000-scale mixed workload
because it provides a stronger recovery protocol. Standard is therefore the
selected default, while `NOEMA_DURABILITY=strong` is the explicit opt-in. The
performance gate is closed; the remaining profile work is power-loss
qualification rather than an unexplained implementation gap.

## Current decision gaps

The Rust implementation is feature-complete against the public Go surface
covered by this repository and is now a replacement candidate. It is not yet a
release replacement: device-cache-loss durability, corrupt-database salvage
policy, takeover by older Go binaries, multi-emulator human TUI validation,
representative multi-model/multi-reviewer quality evaluation, and staged
packaging / rollback policy remain. Standard is the selected release default
to preserve Go's performance and crash posture; strong remains available for
users who explicitly value the pending-mutation/fsync recovery protocol.
Standard mode still needs an explicit physical-power-loss promise because it
intentionally omits the pending-mutation journal and per-file fsyncs. Automatic
restore reconciliation is deliberately
not a goal because ambiguous or tampered state requires an explicit operator
choice.
