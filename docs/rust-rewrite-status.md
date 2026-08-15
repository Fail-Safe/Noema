# Rust rewrite handoff

Updated: 2026-08-15

## Pause point

- Branch: `experiment/rust-rewrite`
- Latest completed milestone: `feat(rust): add consolidate CLI parity`.
- Previous milestone: `2a326c7 feat(rust): add model-driven consolidation`
- Verify the exact commit ID and signature with `git log --show-signature -2`
  after resuming.
- The Go implementation remains the behavioral oracle. The Rust build is an
  experiment and is not ready to replace the release build.

## What is working

- Shared cortex manifests, Markdown traces, SQLite schema/migrations, lexical
  search, and core CRUD lifecycle operations.
- MCP stdio and Streamable HTTP transports, with the same 28 discovered tool
  names in the current comparison fixture.
- Signed Go-to-Rust and Rust-to-Go federation with exact event-byte
  compatibility, identity pinning, vector clocks, pagination, lifecycle replay,
  divergence creation, source locks, and duplicate-event idempotency.
- Background federation workers with pause/resume, bounded exponential backoff,
  health state, usage-signal sync, configuration refresh, and graceful shutdown.
- Explicit signing-key rotation recovery through `federation re-pin-peer`, while
  preserving the event cursor and keeping the peer paused until resumed.
- Rustls HTTPS serving, shared bearer authentication, custom peer CA trust, a
  TLS 1.2 minimum for outbound federation, and refusal to send bearer-protected
  MCP traffic over plaintext.
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

## Latest validation

The following passed for the consolidate CLI milestone:

- `make rust-test` — formatting, clippy with warnings denied, and 49 Rust tests.
- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `make compare-rust`, including:
  - Go/Rust differential cortex compatibility;
  - HTTP initialization for both builds;
  - signed two-way federation and 205-event pagination;
  - three-node convergence, pause/recovery, outage handling, divergence, and
    signing-key rejection/recovery; and
  - authenticated Go/Rust HTTPS federation, CA rejection, bearer rejection,
    access-key recovery, plaintext refusal, and secret redaction; and
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
    fallback suppression, and emitted per-cluster JSON.
- The expanded model-driven scenario passed five consecutive repetitions. The
  mixed three-node ring and all earlier consolidation gates retained their full
  passing coverage.

One sandbox-only warning appeared while Go tried to update its module-download
stat cache outside the writable workspace. The Go build still succeeded and the
complete comparison target exited successfully.

## Performance evidence so far

- At 250 traces, Rust showed higher request throughput, lower latency, roughly
  half the maximum RSS, and lower sampled CPU in the recorded local runs.
- At 1,000 traces, Rust was slightly slower than Go, so there is no general
  throughput win yet.
- In the bounded three-node federation soak, both builds converged to exactly
  80 unique events per node. Rust used about 62.6% of Go's peak aggregate RSS
  and about 81.2% of Go's mean sampled CPU.

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

## Remaining replacement gaps

1. Real-model distillation quality/resource evaluation. Scheduled and CLI
   small, large, and frontier protocol paths pass deterministic endpoints.
2. Watcher onboarding/healing and atomic-save behavior.
3. Semantic embedding backfill, stale detection, cosine ranking, and hybrid RRF.
4. Exact MCP schemas, result semantics, and error parity beyond tool-name parity.
5. Plugin installation/check/force behavior and embedded-payload verification.
6. Full TUI behavior.
7. Certificate expiry validation/monitoring, dynamic peer-worker addition without
   restart, crash-atomic file rollback, lock contention, and broader fault
   injection.

## Recommended next milestone

Run both CLIs in dry-run/emit-JSON mode against the same non-private fixture
corpus and an actual trusted model endpoint. Compare cluster decisions and
distillation fidelity first, then measure wall time, CPU, peak RSS, request
counts, and retry behavior. Keep these quality/resource results separate from
the already-passing deterministic protocol gate.

Any new Rust packages still require explicit approval before adding them.

## Resume commands

```sh
git switch experiment/rust-rewrite
git status --short --branch
git log -2 --oneline
make rust-test
make compare-rust
```

After confirming the baseline, start from the consolidation rows in
`docs/rust-rewrite-test-plan.md` and keep the Go implementation as the oracle.
