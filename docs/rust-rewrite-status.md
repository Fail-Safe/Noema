# Rust rewrite handoff

Updated: 2026-08-16

## Pause point

- Branch: `experiment/rust-rewrite`
- Latest completed milestone: safe cross-runtime cortex backup and restore (this
  handoff commit).
- Previous milestone: `bc21b8d fix(recovery): guard mixed-runtime takeover`.
- Earlier recovery milestone: `5a9337e fix(rust): recover interrupted trace deletion`.
- Earlier recovery milestone: `0a753e5 fix(rust): recover interrupted trace mutations`.
- Earlier TLS milestone: `33424c0 feat(rust): add TLS certificate lifecycle parity`.
- Earlier TUI milestone: `a7e7196 feat(rust): add functional TUI parity`.
- Earlier advanced MCP milestone: `29cd610 feat(rust): complete advanced MCP parity`.
- Earlier MCP contract milestone: `0a6e721 feat(rust): tighten MCP contract parity`.
- Earlier semantic milestone: `b73a089 feat(rust): add semantic search parity`.
- Earlier watcher milestone: `c0f8c8a feat(rust): add watcher parity`.
- Earlier experiment milestone: `aa5a5ee test(rust): compare real-model consolidation`.
- Verify the exact commit ID and signature with `git log --show-signature -2`
  after resuming.
- The Go implementation remains the behavioral oracle. The Rust build is an
  experiment and is not ready to replace the release build.

## What is working

- Shared cortex manifests, Markdown traces, SQLite schema/migrations, lexical
  search, and core CRUD lifecycle operations.
- MCP stdio and Streamable HTTP transports, with the same 28 discovered tool
  names in the current comparison fixture.
- MCP discovery now matches tool parameter names, required fields, enums, field
  descriptions, and the three Go output-schema surfaces. Structured cortex
  usage and tag-mutation results match across runtimes.
- `get_trace` defaults to a policy read that does not affect promotion signals;
  explicit `record_usage=true` records exactly one read in both builds.
- Search activity aggregates per-peer search/read/modify counters across active
  traces and tags. Consolidation health matches daily success/failure/election,
  promotion/distillation totals, promotion-latency percentiles, and the
  one-source-mid leak detector.
- Divergence resolution accepts a named stored version or a custom merge,
  updates the original through normal source-lock and immutability checks, and
  moves the resolved conflict trace to recoverable trash.
- Advanced MCP federation and consolidation surfaces now match Go: rolling
  candidates retain usage/lineage signals, distillation preserves short-tier
  sources and telemetry, identity/status/sync wire shapes match, and peer
  announcements validate and acknowledge without mutating the manifest.
- HTTP serving enforces Go's federation-mode boundary: publish cortexes reject
  every remote mutation while retaining local stdio writes, and subscribe
  cortexes refuse event and usage-signal publication.
- The Rust TUI now has the Go interaction model rather than a static quit-only
  list: two-pane navigation and detail scrolling, search, short/mid/long and
  archive/trash filters, help, confirmation, archive/trash/recover/purge,
  session-scoped vote cycling, live/manual refresh, sticky selection, fading
  new-row highlights, editor handoff, theme loading, and terminal restoration.
- The Rust binary compile-time embeds the same three Hermes and three Obsidian
  runtime files as Go. Plugin list/status/install/check/force commands run
  without requiring a configured cortex and use the same target resolution.
- Plugin installation preserves unmanaged files, refuses changed managed files
  without `--force`, performs non-mutating check runs, atomically replaces
  regular files and symlinks, and reports matching SHA-256 drift details.
- Signed Go-to-Rust and Rust-to-Go federation with exact event-byte
  compatibility, identity pinning, vector clocks, pagination, lifecycle replay,
  divergence creation, source locks, and duplicate-event idempotency.
- Background federation workers with pause/resume, bounded exponential backoff,
  health state, usage-signal sync, configuration refresh, and graceful shutdown.
- A single supervisor now reconciles the live peer-name set: zero-peer servers
  can add peers without restart, removed peers cancel their workers, re-added
  peers resume from preserved cursors, unchanged workers retain their backoff
  state, and unexpectedly exited workers restart on the next interval.
- Explicit signing-key rotation recovery through `federation re-pin-peer`, while
  preserving the event cursor and keeping the peer paused until resumed.
- Rustls HTTPS serving, shared bearer authentication, custom peer CA trust, a
  TLS 1.2 minimum for outbound federation, and refusal to send bearer-protected
  MCP traffic over plaintext.
- TLS certificate paths use Go-compatible per-flag CLI-over-manifest precedence.
  The Rust server refuses expired and not-yet-valid leaf certificates, warns at
  seven days, exposes the same explicit temporary bypass, and re-reads the leaf
  hourly under the single-background-owner lock while logging only 90/30/7-day,
  expired, or unreadable band transitions.
- Access-key resolution compatible with Go:
  `NOEMA_MCP_KEY` first, then `access.shared_key_file`, then open mode.
- Secret-safe access-key fingerprints and failure output.
- Consolidation configuration parsing without discarding unknown YAML fields.
- Go-compatible endpoint eligibility, cryptographic rank generation, local and
  peer rank persistence, identity-handshake advertisement, quiet-period
  election, and deterministic lexical-ID tie-breaking.
- Federation status includes rank visibility, the current winner, and whether
  the local node should run.
- Signed consolidation claim/success/fail events replay into the event log
  without creating synthetic trace records.
- One cross-runtime background owner per cortex; lock losers continue serving
  MCP without starting duplicate federation or eligibility workers.
- Multi-process contention now proves that both builds keep lock losers in
  MCP-only mode, release the kernel lock after a killed owner, and allow a new
  owner to acquire it while the original loser remains alive.
- Rust trace writes now use synced same-directory temporary files and atomic
  rename while retaining the prior read-only-file rejection contract.
- Local create/update/visibility/tier operations and the equivalent federation
  replay paths create a durable recovery record before changing the filesystem.
  Removing that record is part of the same SQLite transaction as the trace and
  event mutation. A later Rust open restores exact bytes and permissions,
  reverses a move, or removes an uncommitted new file after process death.
- Per-mutation owner locks prevent a live writer from being recovered by a
  concurrent open. Stable per-trace locks serialize duplicate local or replay
  mutations before either worker touches the same Markdown path.
- Hard removal, automatic expiry purge, and federation purge replay now retain
  an exact journaled backup until their row transaction commits. Watcher repair
  journals its reconstructed trash copy with the matching visibility update,
  so failed or killed repair does not leave an uncommitted file behind.
- The Go build on this branch refuses to open a Cortex with a pending Rust
  mutation record. The Rust build must recover it first; after recovery, Go
  opens normally. The guard never parses or prints the journal value.
- Rust startup fails closed on an unreadable SQLite database, malformed pending
  record, or pending path traversal without rewriting the database, trace, or
  an outside path. Corrupt-database repair is not implemented.
- Rust backup now writes a single-root gzip tar archive directly, without
  platform `tar` metadata. It strips owner and extended-attribute metadata,
  supports long trace paths, rejects non-regular entries and output
  self-inclusion, and preserves an existing output until a forced replacement
  is ready.
- Rust restore accepts both Go and Rust archives through a private staging
  directory. It rejects unsafe, duplicate, linked, or multi-root entries;
  validates cortex names and identities; detects registered name, ID, and path
  collisions; and retains a forced destination until placement and
  configuration persistence succeed. Cross-filesystem placement copies only
  directories and regular files.
- A reusable pass gate with initial election, signed claim, cancellable quiet
  wait, re-election, distinct preemption reasons, in-flight tracking, pass-error
  closure, and signed success.
- A live stale-claim watchdog with a ten-minute default, manifest override,
  strict local timeout, two-sync-interval remote grace, in-flight suppression,
  and event-log deduplication.
- Go-compatible active short-tier candidate selection, 24-hour default window,
  aggregated usage/vote/lineage signals, and deterministic heuristic scoring.
- The single-source summary guard: passive search hits do not earn promotion
  credit without a modification or tier vote, while deliberate reads remain
  eligible.
- A live threshold scheduler with Go-compatible strict `count > threshold`
  activation, 80% hysteresis, immediate startup evaluation, and graceful
  cancellation.
- A unified cadence scheduler with Go's trigger priority (`cron`, threshold,
  idle), strict `HH:MM` parsing, once-per-local-day cron state, idle history
  requirement, and cooldown from any pass.
- Federated cron retries use the same five-minute window and three-retry budget
  as Go, stop after any locally observed or replayed success, and mark the day
  complete after success or exhaustion. Single-node cron marks the day on fire
  because it deliberately emits no coordination success event.
- Deterministic mid-to-long graduation with the Go defaults (14 days, three
  reads plus search hits, unmodified, no downvote) and automatic exclusion of
  preference, young, inactive, unstable, or negatively voted traces.
- Heuristic and graduation passes are chained in order behind one gate;
  graduation still runs if the heuristic pass fails.
- Short-to-mid promotion runs directly for single-node cortexes and behind the
  existing election gate/shared in-flight registry when peers are configured.
- Promotion updates both SQLite and Markdown frontmatter, emits the exact
  `{from: short, to: mid}` event data, federates across runtimes, and is
  idempotent after a trace leaves the short-tier candidate pool.
- Scheduled model-driven consolidation now runs before heuristic promotion and
  graduation. The Rust client supports the OpenAI-compatible request envelope,
  bearer-key indirection, bounded retries, cancellation, and the Go
  small/large/frontier response sequences without adding a dependency.
- LLM candidates use the same active short-tier window and exclude source IDs
  already consumed by a prior `consolidate` event. Sources remain short while
  the distilled observation lands at mid with exact `derived_from` lineage.
- Distillation emits separate create and consolidate events carrying model,
  profile, confidence, and source telemetry. Both event orders replay safely,
  and signed distilled traces now federate Go-to-Rust and Rust-to-Go.
- Malformed or unavailable model responses use the same score-gated heuristic
  fallback. Scheduled heuristic and graduation work continues when the model
  client or pipeline fails for a non-cancellation reason.
- `noema-rs consolidate` now matches Go's manifest/flag precedence for endpoint,
  model, profile, API-key environment, window, and retry budget. It handles
  Ctrl-C through the shared cancellation path and prints the same pass summary.
- CLI dry-run mode performs prompts and parsing but suppresses both distilled
  writes and heuristic fallback. `--emit-json` writes the Go-compatible run
  metadata, summary counters, per-cluster outcome, and source snapshot shape.
- External file edits use per-path debounce and update SQLite, FTS, lineage,
  tags, and the event log without rewriting editor-owned bytes.
- External create, archive/unarchive moves, recoverable deletion, trash purge,
  atomic-save replacement, raw Markdown onboarding, and frontmatter healing
  match the tested Go outcomes while preserving source locks.
- Watchers run for both stdio and HTTP under the existing single-background-
  owner lock, and server readiness now waits for successful registration.
- The native watcher is backed by a bounded, dependency-free metadata rescan
  because macOS FSEvents did not deliver events in the controlled temporary-
  cortex fixture. The fallback preserves sub-second modification precision and
  avoids hashing trace bodies on every pass.
- OpenAI-compatible embedding requests use the existing Reqwest/Rustls stack,
  API-key environment indirection, 64-input batches, response-index ordering,
  Unicode-safe text limits, and the exact Go vector codec/normalization bytes.
- Embedding status, missing/stale selection, force/limit controls, content-hash
  repair, batch commits, idempotent reruns, and model-change invalidation match
  the deterministic Go oracle.
- CLI and MCP semantic search use cosine ranking; hybrid search and similarity
  use Go-compatible weighted reciprocal-rank fusion. Archived visibility,
  source exclusion, dimension mismatches, and non-finite stored vectors match.
- Semantic MCP failures degrade to lexical results with a generic note and no
  endpoint detail. Both serve transports run the automatic embedding maintainer
  under the existing single-background-owner lock.

## Latest validation

The following passed for the combined backup/restore, crash recovery,
transaction rollback, fault-safety, dynamic federation, TLS lifecycle,
advanced MCP, and functional TUI tree:

- `make rust-test` — formatting, clippy with warnings denied, 97 unit tests,
  five subprocess crash-recovery scenarios, and three recovery-safety tests.
- Eight focused SQLite-abort tests prove byte- and permission-identical rollback
  for local update/tier/visibility/hard-delete operations, watcher
  reconstruction, and remote update/create/purge replay, including removal of
  a new file and preservation of an orphan file.
- Five subprocess scenarios pause Rust after a durable file mutation but before
  SQLite commit, send `SIGKILL`, and verify next-open recovery for exact-byte
  update, archive move, new trace creation, hard deletion, and watcher trash
  reconstruction. The move and watcher cases also prove that a concurrent open
  leaves the still-live writer alone.
- The differential compatibility fixture kills Rust after an uncommitted file
  replacement, proves Go refuses the pending recovery state, opens Rust to
  restore the committed bytes, and then proves Go opens normally.
- Three recovery-safety tests prove corrupt SQLite bytes are not rewritten,
  malformed records remain available for operator diagnosis, and traversal in
  a pending path cannot escape the Cortex.
- Ten restore unit tests cover round trips, metadata normalization, long paths,
  self-inclusion, forced replacement rollback, identity collisions, unsafe
  names, duplicate paths, multiple roots, links, and traversal.
- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `make compare-rust`, including:
  - Go/Rust differential cortex compatibility;
  - Go-to-Rust and Rust-to-Go archive restore, duplicate-ID refusal, forced
    destination replacement, transaction-artifact cleanup, traversal refusal,
    and link refusal;
  - HTTP initialization for both builds;
  - signed two-way federation and 205-event pagination;
  - three-node convergence, pause/recovery, outage handling, divergence, and
    signing-key rejection/recovery; and
  - live zero-peer startup, peer addition/removal/re-addition, exact worker
    start/stop counts, sync suppression while removed, cursor preservation,
    pending-event recovery, and graceful shutdown; and
  - same-cortex four-process lock contention, MCP availability for lock losers,
    killed-owner release, replacement acquisition, and continued exclusion of
    additional contenders in both builds; and
  - rejected trace overwrite, lifecycle rename, and manifest-write mutations
    with byte-identical files, unchanged database/event state, and successful
    SQLite integrity checks in both builds; and
  - authenticated Go/Rust HTTPS federation, CA rejection, bearer rejection,
    access-key recovery, plaintext refusal, and secret redaction; and
  - identical expired and not-yet-valid startup refusal, seven-day warnings,
    CLI-over-manifest TLS precedence, explicit bypass behavior, immediate
    monitor bands, malformed-certificate redaction, and graceful shutdown; and
  - mixed Go/Rust rank exchange, deterministic election, endpoint failover and
    recovery, signed Go claim/success replay, and a Rust-signed watchdog closure
    accepted by Go; and
  - identical Go/Rust threshold-triggered candidate choices, active/window
    filtering, lineage and single-source scoring, promotion event data,
    frontmatter mutation, and restart idempotency; and
  - identical midnight cron graduation and idle-triggered maintenance across
    age, read/search, modification, vote, type, and archive gates; and
  - identical small, large, and frontier fake-model request sequences,
    distilled lineage/telemetry, source preservation and exclusion, malformed
    response retries, offline fallback, and restart idempotency; and
  - identical CLI flag overrides, summary text, dry-run write suppression,
    fallback suppression, and emitted per-cluster JSON; and
  - identical external edit/reindex, debounce, create, archive/unarchive,
    atomic-save, heal, onboarding, delete/purge, and source-lock outcomes; and
  - identical deterministic embedding requests, bearer indirection, codec
    bytes, freshness transitions, bounded/idempotent backfill, cosine/RRF
    rankings, archive rules, corrupt-vector rejection, MCP degradation, and
    serve-time maintenance; and
  - identical MCP tool names, parameter/required/enum schemas, documented
    fields, output-schema presence, structured usage/tag results, policy-read
    accounting, tag/append/archive/search/history behavior, search activity,
    consolidation health, custom divergence resolution, consolidation
    candidates/distillation/lineage, identity/status/sync results,
    non-mutating announcements, invalid-argument behavior, and HTTP
    publish/subscribe restrictions; and
  - byte-identical embedded Hermes/Obsidian payloads plus matching inventory,
    target resolution, status/check/install/idempotency, drift refusal,
    force-check, forced replacement, unmanaged-file preservation, symlink
    safety, and temporary-file cleanup.
- Six focused TUI suites cover navigation/focus and detail scrolling,
  search/tier/help transitions, vote and visibility lifecycle mutations,
  sticky selection with fading live highlights, a 120x28 off-screen renderer,
  explicit dark/light themes, Unicode truncation, and body wrapping.
- An isolated managed-PTY launch restored the alternate screen safely but could
  not complete because the harness does not answer the terminal cursor-position
  query. Treat live-terminal behavior as unverified, not failed application
  behavior.
- The expanded model-driven scenario passed five consecutive repetitions. The
  mixed three-node ring and all earlier consolidation gates retained their full
  passing coverage.

One sandbox-only warning appeared while Go tried to update its module-download
stat cache outside the writable workspace. The Go build still succeeded and the
complete comparison target exited successfully.

The first post-plugin full comparison hit one timeout waiting for the existing
consolidation watchdog fixture. The exact fixture passed immediately on retry,
and a subsequent complete `make compare-rust` run passed, including the same
watchdog path and the new plugin fixture.

## Performance evidence so far

- At 250 traces, Rust showed higher request throughput, lower latency, roughly
  half the maximum RSS, and lower sampled CPU in the recorded local runs.
- At 1,000 traces, Rust was slightly slower than Go, so there is no general
  throughput win yet.
- In the bounded three-node federation soak, both builds converged to exactly
  80 unique events per node. Rust used about 62.6% of Go's peak aggregate RSS
  and about 81.2% of Go's mean sampled CPU.
- Across seven alternating-order real-model dry runs, both builds made all 21
  bucket decisions correctly. Rust retained 112/112 planted terms versus Go's
  111/112, used 36.0% less median peak RSS, and had 20.4% lower median wall
  time. In the two instrumented runs, Rust used 51.0% fewer prompt tokens, so
  the timing result primarily measures lower model workload rather than client
  execution speed.

See `docs/rust-rewrite-experiment-results.md` for the full tables and caveats.

## Important findings to retain

- Federation signatures cover the exact event payload bytes. Reordering JSON
  keys or pretty-printing signed event data breaks interoperability.
- Go may serialize an empty event batch as `null`; Rust must continue accepting
  both `null` and `[]`.
- A TLS test fixture must use a CA certificate plus a separately signed server
  leaf. Rustls correctly rejects a CA certificate used directly as an end
  entity, even though some other TLS stacks tolerate it.
- Health classification must match structured error shapes. A raw substring
  search for `401` misclassified connection failures when a random port happened
  to contain those digits.
- Re-pinning a rotated signing key must not reset or advance the peer cursor
  before a successful authenticated handshake.
- A hard signing-key rotation fixture must first prove every direct peer cursor
  consumed the old-key history. Transitive convergence alone does not establish
  that precondition.
- Short-lived MCP processes must not run background federation or eligibility
  loops. Enforcing one background owner removed hidden duplicate sync workers
  and made cursor/election behavior deterministic.
- A watchdog closure carries two identities: the event emitter identifies the
  observer that closed the orphan, while the payload identifies the winner that
  failed to emit a terminal event.
- The in-flight registry must remain process-local. A restart deliberately
  forgets active windows so the watchdog can close claims abandoned by a crash.
- A watcher must become ready before its serving socket is considered ready;
  otherwise the first edit can fall into a registration race.
- `notify`'s polling backend truncates modification times to whole seconds
  unless content comparison is enabled. It cannot safely observe sub-second
  editor bursts without hashing every watched file.
- MCP mutation methods must match Go's usage-signal policy as well as their
  visible result. The contract fixture caught Rust counting `append_trace` as a
  modification while Go deliberately does not.
- Rust rejects trace types outside the advertised enum. The Go runtime accepts
  them despite advertising the same enum; the fixture records this as a
  deliberate stricter-validation difference rather than weakening Rust.
- Plugin commands are distribution operations, not cortex operations. Routing
  them through default-cortex resolution made an otherwise valid install fail
  on a fresh machine; Rust now dispatches them before opening a cortex.
- Managed-plugin replacement must inspect symlinks without following them and
  replace the directory entry atomically. Following the link would overwrite
  operator data outside the managed plugin directory.
- Atomic rename can replace a read-only file when its directory remains
  writable. A non-truncating write-open check is required before creating the
  temporary trace so atomicity does not weaken the existing access contract.
- Per-mutation recovery locks do not serialize two independent workers. The
  mixed ring exposed duplicate create replay where the losing rollback removed
  the winner's file; a stable lock derived from the trace path must be acquired
  before the recovery record or filesystem mutation.
- An unaware Go build will read Rust's uncommitted file after a killed Rust
  writer because it does not recognize the recovery record. The current branch
  Go build therefore needs a narrow fail-closed startup interlock; older Go
  binaries remain unsafe for takeover until Rust has recovered first.
- The macOS system `tar` can add platform metadata that appears as extra
  top-level archive entries. A direct archive writer is required for one-root,
  cross-runtime restore compatibility and to avoid leaking owner or extended
  metadata.
- Allowing a backup output beneath the cortex being archived can recursively
  include the archive or its temporary file. Backup must resolve and reject
  that placement before creating output.

## Remaining replacement gaps

1. Pixel-level TUI parity and live human terminal validation across resize,
   dark/light auto-detection, and external-editor workflows.
2. Broader adversarial real-model quality evaluation beyond the current bounded
   synthetic corpus.
3. Backup restore now provides an explicit recovery path from a prior archive,
   but corrupt-database salvage and an operator recovery-status surface remain
   absent. Restore/configuration `SIGKILL` windows and real power-loss durability
   are not yet qualified. Older Go binaries also cannot safely take over until
   the Rust recovery record has been consumed.

## Recommended next milestone

Add an explicit recovery-status surface that can distinguish a pending
mutation, malformed journal, and unreadable database without exposing journal
contents. Then inject process death across archive replacement, destination
placement, and configuration persistence before qualifying filesystem and
SQLite durability under real power-loss simulation. Keep automatic startup
fail-closed: salvage or backup replacement should remain an explicit operator
action.

## Resume commands

```sh
git switch experiment/rust-rewrite
git status --short --branch
git log -2 --oneline
make rust-test
make compare-rust
```

After confirming the baseline, continue from the security and fault-tolerance
rows in `docs/rust-rewrite-test-plan.md` and keep the Go implementation as the
oracle.
