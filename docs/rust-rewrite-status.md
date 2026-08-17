# Rust rewrite status (historical)

Updated: 2026-08-17

## Current state

- Branch: `experiment/rust-rewrite`
- The Rust implementation is the production implementation on this branch.
- The repository root, active build, CI, release automation, and plugin
  integration tests are Rust-first; the Go source and module graph were
  removed after the comparison baseline was signed.
- The final side-by-side state remains available at the signed
  `rust-cutover-baseline` tag.
- The detailed parity and benchmark notes below are retained as historical
  evidence for the cutover decision.
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
  - 127 Rust unit tests;
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
- Final report validation exposed a rotation-boundary race in the three-node
  fixture: it could snapshot its cursor while a same-origin old-key event was
  still in flight. The gate now waits for that origin's signed event set to
  converge. Three consecutive ring runs and the full comparison gate passed
  after the correction.
- A same-trace Go/Rust screenshot comparison found remaining renderer-level
  differences. Rust now right-aligns the trace count, applies the Go tier heat
  colors and explicit title ellipsis, renders punctuated type/tag chips with
  wrapping, and keeps metadata pinned above a full-width scroll-aware rule.
  A subsequent identical-size capture found the Rust divider one cell left of
  Go because its border consumed list content and its table reserved an extra
  prefix cell. The pane and table geometry now match Go, and fixed-size
  dark/light buffer tests assert the divider, title start, cell placement, and
  colors. A final identical-size release-binary recapture visually matches Go;
  the live PTY and complete differential suite also pass after the changes.

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
- The initial harder comparison exposed under-prompting in Rust: Go won 14
  blind pairs to Rust's 3, and Rust produced malformed tags and source-ID
  leakage. Full prompt/request parity, explicit zero temperature, ID-free model
  input, a polysemy rule, and strict shape validation closed those defects.
- The exact-artifact untagged six-run comparison produced equal 48/48
  decisions, 203/204 adjudicated semantic retention, one isolated numeric
  substitution per runtime, and zero forbidden claims, metadata leaks, or
  schema defects. Blind review scored Go 473/480 and Rust 467/480, with 36
  ties; the preceding independent batch had narrowly favored Rust. Prompt
  tokens are equal, model-dominated wall time is effectively tied, and Rust
  retained 37.0% lower median peak RSS.
- Three-run real-model qualification of the other profiles found equal 24/24
  decisions and 102/102 retention for `large`. An initial shared `frontier`
  precision gap was traced to missing grounding instructions; after adding the
  same rules to Go and Rust, both reached 24/24 decisions and 102/102 retention.
  Blind frontier review was effectively tied at 239/240 for Go and 238/240 for
  Rust, with one win each and 22 ties.
- The default `standard` durability profile now matches Go's direct-file and
  single-SQLite-transaction mutation posture. `NOEMA_DURABILITY=strong` opts
  into the enhanced recovery protocol, unknown values fail closed, and MCP
  usage reports the selected profile.
- The profile experiment exposed an unrelated create-path FTS scan. Rust used
  update-style delete-and-insert helpers even for new traces; insert-only tag,
  lineage, and FTS helpers now match Go's create semantics and retain upserts
  for updates.
- Final five-run 10,000-trace standard-profile testing has Rust ahead in every
  throughput median: seed 3.096 versus 6.596 seconds, mixed 4,311 versus 3,413
  ops/s, and median writes 0.301 versus 0.354 ms. Rust used less RSS in every
  isolated scenario except serial broad.
- Final five-run 100,000-scale standard-profile testing used byte-identical verified
  corpus clones and four clients. Rust led every throughput median, including
  mixed 4,203 versus 2,977 ops/s and median writes 0.301 versus 0.393 ms. Rust
  used much less RSS for selective and mixed work; single-process full-corpus
  broad search retained about 642 MiB versus Go's 298 MiB, while four-process
  broad RSS was tied near 957 MiB.
- The optimized Rust `strong` profile still pays for its enhanced crash consistency:
  10,000-trace seed was 92.987 versus 6.491 seconds, mixed throughput was 253
  versus 3,299 ops/s, and median writes were 9.128 versus 0.348 ms. This is now
  an explicit durability-policy tradeoff rather than an unresolved Rust
  performance defect.
- The first scale campaign's large Rust RSS readings are superseded. Rust had
  emitted pretty full-row JSON while Go emitted compact rows, and later
  scenarios reused broad-search processes. Rust now matches Go's compact MCP
  list/search rows, and the harness isolates each scenario.
- The macOS APFS storage-fault harness passed four complete runs, including
  three consecutive qualification cycles. Forced detach/remount preserved an
  explicitly rollbackable restore after placement, an explicitly resumable
  restore interrupted before config rename, strong-mode archive rollback and
  pending-journal cleanup with SQLite integrity, and an acknowledged
  standard-mode archive. No disposable images remained attached afterward.

See `docs/rust-rewrite-experiment-results.md` for tables and caveats.

## Post-cutover qualification follow-ups

These are not missing command or protocol ports:

1. Repeat the now-passing APFS forced-detach qualification on a hard-powered
   VM or dedicated disposable target that can discard volatile device-cache
   writes. Forced detach exercises real storage disappearance and remount, but
   may still honor completed flushes and cannot model a controller that lies
   about `fsync` completion.
2. Decide whether to implement corrupt-database salvage. Rust currently fails
   closed and preserves corrupt bytes; it does not attempt repair.
3. Do not downgrade a cortex while a `strong` pending-mutation record exists;
   complete recovery with the current binary first.
4. Perform multi-emulator human validation for any terminal-specific cell-width
   or color differences. The final identical-size capture in the original
   terminal visually matches Go after the one-cell pane-geometry correction.
5. Extend the now-passing synthetic qualitative gate to a redacted
   representative corpus, additional models, and multiple reviewers.
6. Define the final physical-power-loss promises for the selected `standard`
   default and opt-in `strong` profile. Standard avoids a migration-time
   performance regression and intentionally shares Go's weaker mid-operation
   window; strong preserves the tested pending-record/fsync recovery protocol
   at a substantial write cost.
7. Validate the native six-platform archive workflow, checksums, plugin bundles,
   and Homebrew metadata on the first release-candidate tag, then repeat the
   mutation benchmark against that exact commit.

Automatic restore reconciliation remains deliberately absent: ambiguous or
tampered restore state requires an explicit operator `resume` or `rollback`.

## Current validation commands

```sh
git switch experiment/rust-rewrite
git status --short --branch
git log --show-signature -3
make test
make release-check
make storage-fault  # macOS; requires DiskImages access
```

Use `docs/rust-rewrite-test-plan.md` for the original acceptance gates. To
reproduce Go/Rust comparisons, check out the signed `rust-cutover-baseline` tag.
