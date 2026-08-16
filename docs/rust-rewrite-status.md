# Rust rewrite status

Updated: 2026-08-16

## Current state

- Branch: `experiment/rust-rewrite`
- Go remains the behavioral oracle.
- The Rust implementation is feature-complete against the current public Go
  CLI, storage, MCP, federation, consolidation, plugin, watcher, and TUI
  surfaces exercised by this repository.
- The branch is a replacement candidate, not yet the release implementation.
  The remaining work is qualification and rollout policy, listed below.
- Port-completion baseline:
  `8cd9719 feat(rust): complete migration and operational parity`.
  Verify the current signed commits with `git log --show-signature -3`.

## Implemented parity

### Storage and lifecycle

- Go-compatible cortex manifests, framed Markdown, ULIDs, SQLite migrations
  through schema 19, FTS, lineage, embeddings, events, vector clocks, and
  Ed25519 signatures.
- CRUD, guided add and collision recovery, append/edit source-lock overrides,
  archive/trash/recover, hard removal, sync, event history/backfill,
  divergence resolution, memory-tier operations, and ceremonial purge.
- `verify traces`, optional hash backfill, `verify cortex`, `verify drift`,
  legacy `drift`, read-only recovery status, and `sync --recover`.
- Explicit v1-to-v2 cortex-ID migration with locking, backups, WAL checkpoint,
  durable phase journal, stable resume identity, reset semantics, database
  re-keying, manifest/config persistence, and cross-runtime validation.
- Go-compatible backup/restore formats plus hash-bound restore transactions,
  explicit resume/rollback, collision and traversal defenses, and atomic
  configuration persistence.
- Durable mutation journals cover create, update, visibility, tier, watcher
  reconstruction, hard delete, expiry purge, and federation replay. A current
  Go build refuses takeover until Rust completes pending recovery.

### Interfaces

- Public Go top-level commands and subcommands have Rust counterparts. The
  compatibility surface includes `remove -f`, bare `verify --backfill`,
  `tui --theme`, `NOEMA_THEME`, shell completions, config keys, service config
  printers, and Go-shaped text/JSON output where consumers depend on it.
- MCP stdio and Streamable HTTP expose the same 28 tools, compatible input and
  output schemas, usage semantics, errors, federation restrictions, and
  semantic-search fallback behavior.
- HTTP supports repeated static listeners, dynamically reconciled local
  addresses, shared-key authentication, Rustls HTTPS, custom peer CAs, TLS 1.2
  minimum outbound federation, certificate validity gates, lifecycle warnings,
  and secret-safe diagnostics.
- The TUI supports navigation, detail scrolling, search, filters, help,
  lifecycle actions, vote cycling, live refresh, editor handoff, themes,
  Unicode-safe rendering, resize, and complete terminal restoration.
- Embedded Hermes and Obsidian plugins match Go payload hashes and implement
  list/status/check/install/force behavior with atomic, symlink-safe updates.

### Background behavior

- Signed two-way federation, batching, replay, identity pinning and rotation,
  pause/resume, dynamic peer reconciliation, retries, health, usage sync, and
  graceful shutdown.
- One background owner per cortex; lock losers remain available for MCP while
  avoiding duplicate watcher, federation, embedding, or consolidation workers.
- Consolidation election, claims, watchdog closure, cron/threshold/idle
  cadence, heuristic promotion, strict graduation, OpenAI-compatible
  distillation, retry/fallback behavior, lineage, and telemetry.
- Native watcher behavior for create/edit/rename/delete, atomic-save debounce,
  raw Markdown onboarding, healing, source locks, and serve-readiness ordering.
- Embedding request batching, vector codec and normalization, stale selection,
  bounded backfill, cosine search, hybrid reciprocal-rank fusion, and automatic
  maintenance.

## Final validation

The completion tree passed:

- `make rust-test`
  - rustfmt and Clippy with warnings denied;
  - 119 Rust unit tests;
  - one killed-config-writer subprocess test;
  - five active mutation crash-recovery scenarios (plus one ignored child
    entry point);
  - five recovery-safety tests; and
  - nine restore crash/recovery scenarios.
- `go build ./...`
- `go vet ./...`
- `go test ./...`
- `go test -race ./...`
- `make compare-rust`, including:
  - shared-format CRUD, FTS, signing, lifecycle, recovery, integrity, drift,
    guided add, event backfill, observability, purge, collision, migration, and
    28-tool discovery;
  - cross-runtime archive restore and destructive/path/link negative cases;
  - Go and Rust HTTP initialization, Rust dual-stack repeated static listeners,
    and Rust dynamic local-address reconciliation;
  - mixed-runtime federation network, three-node ring, dynamic peers,
    background lock contention, I/O fault safety, authenticated TLS, and
    certificate lifecycle;
  - consolidation election/watchdog, heuristic promotion, tier maintenance,
    three LLM profiles, watcher parity, semantic search, MCP contract, and
    embedded-plugin lifecycle; and
  - live PTY render, editor handoff, help, resize, quit, cursor restoration,
    alternate-screen exit, and canonical/echo restoration.
- The formerly intermittent resize/quit PTY failure was reproduced under
  Crossterm's macOS `mio` source. Selecting Crossterm's Unix polling backend
  removed the lost-input race; the strengthened scenario then passed 30
  consecutive runs and the complete comparison gate.

The Go build can emit a sandbox-only warning when it cannot update the module
download stat cache outside this workspace. It still exits successfully; do
not treat that warning as a failed build.

## Performance evidence

- At 250 traces, recorded Rust runs had higher request throughput, lower
  latency, roughly half the maximum RSS, and lower sampled CPU.
- At 1,000 traces, Rust was slightly slower than Go, so there is no universal
  throughput win.
- In the bounded three-node federation soak, both builds converged to 80
  unique events per node. Rust used about 62.6% of Go's peak aggregate RSS and
  81.2% of Go's mean sampled CPU.
- Across seven alternating-order real-model dry runs, both builds made all 21
  bucket decisions correctly. Rust retained 112/112 planted terms versus
  Go's 111/112, used 36.0% less median peak RSS, and had 20.4% lower median wall
  time. The instrumented Rust runs used fewer prompt tokens, so that timing is
  not a pure client-performance comparison.
- A harder four-run, 32-pair blinded comparison found equal 136/136 semantic
  retention and no forbidden or novel numeric claims. Rust used 46.6% fewer
  prompt tokens, 49.8% fewer request bytes, and 37.2% less median peak RSS, but
  Go won 14 blind quality pairs to Rust's 3, with 15 ties. Rust's main defects
  were collapsed/misplaced tags, source-ID leakage, and awkward ambiguity
  prose; both builds struggled with a lexical-polysemy rejection case.

See `docs/rust-rewrite-experiment-results.md` for tables and caveats.

## Remaining qualification and rollout decisions

These are not missing command or protocol ports:

1. Run archive placement, config rename, journal cleanup, and SQLite recovery
   under a disposable real-power-loss/storage harness. `SIGKILL` validates
   process-death recovery but does not prove persistence across power loss.
2. Decide whether to implement corrupt-database salvage. Rust currently fails
   closed and preserves corrupt bytes; it does not attempt repair.
3. Define takeover policy for older Go binaries. Only the Go build on this
   branch understands the pending-Rust-mutation interlock.
4. Perform multi-emulator human TUI validation and any desired pixel-level
   comparison.
5. Close the observed Rust distillation-quality gap, then repeat the blinded
   evaluation with a redacted representative corpus, additional models, and
   multiple reviewers.
6. Before release replacement, choose packaging, binary naming, migration,
   rollback, and staged deployment policy, then repeat benchmarks on stripped
   release binaries at larger corpus sizes.

Automatic restore reconciliation remains deliberately absent: ambiguous or
tampered restore state requires an explicit operator `resume` or `rollback`.

## Resume commands

```sh
git switch experiment/rust-rewrite
git status --short --branch
git log --show-signature -3
make rust-test
make compare-rust
```

Use `docs/rust-rewrite-test-plan.md` for acceptance gates and keep Go as the
oracle until the rollout decisions above are closed.
