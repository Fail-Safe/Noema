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

`federation_dynamic.py` starts a Rust server with no configured peers and a live
Go source. It adds, removes, and re-adds that source without restarting Rust,
verifying worker start/retirement counts, sync suppression while removed,
cursor preservation, pending-event recovery, and graceful shutdown.

`background_lock_contention.py` starts four same-cortex server processes per
implementation. It verifies that lock losers still serve MCP, a killed owner's
kernel lock is released, a replacement owner can acquire it while the original
loser stays alive, and further contenders remain MCP-only.

`io_failure_safety.py` makes an existing trace, its lifecycle target directory,
and the manifest read-only in turn. It checks that rejected append, archive, and
configuration mutations leave file bytes, SQLite metadata, event counts, and
database integrity unchanged in both builds.

`rust/tests/crash_recovery.rs` exercises Rust's post-filesystem/pre-commit
boundary directly. It pauses create, update, archive, hard-delete, and watcher
reconstruction operations after the durable file mutation, kills the writer,
and requires the next Rust open to restore the exact prior transaction state.
The archive and watcher cases also open the Cortex concurrently to prove that
recovery skips a live mutation owner.

`rust/tests/recovery_safety.rs` verifies that corrupt SQLite bytes, malformed
pending JSON, and pending path traversal all fail closed without rewriting the
database, trace, recovery record, or an outside path.

`compare.sh` also kills Rust after an uncommitted trace replacement, requires
the branch Go build to refuse takeover while the Rust recovery record exists,
opens Rust to recover the committed state, and then requires Go to open it.

`federation_tls.py` runs signed Go↔Rust federation over HTTPS with a temporary
private CA, CA-signed server certificate, and shared bearer key. It verifies
missing/wrong/correct authorization, custom-CA failure and recovery, secret-free
failure output, and the keyed-plaintext startup refusal.

`tls_certificate_lifecycle.py` compares the startup and background safety layer
around that transport. It covers expired and not-yet-valid refusal, the
seven-day warning, CLI-over-manifest certificate precedence, the explicit
expired-certificate escape hatch, immediate monitor bands, malformed-certificate
redaction, and graceful shutdown for both builds.

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

`tier_maintenance.py` drives the remaining deterministic cadence paths on
independent Go and Rust cortexes. A midnight cron run evaluates old mid-tier
traces against age, read/search, modification, vote, type, and active-state
graduation gates; a separate backdated event log exercises the idle trigger.
Both paths verify exact tier-change events, Markdown updates, and restart
idempotency.

`watcher_parity.py` starts independent Go and Rust HTTP servers with watchers
enabled, then applies the same external filesystem mutations. It compares
event and database outcomes for body/frontmatter edits, burst debounce, valid
file drops, archive/unarchive moves, atomic-save replacement, malformed-file
healing, raw Markdown onboarding, recoverable delete, external purge, and
source-lock enforcement.

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
