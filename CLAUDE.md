# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Also read .claude/layout.md when it exists.

## Project

**Noema** — "The intentional memory layer for your AI agents."

Noema         → The project
Cortex        → The collection / instance
Trace         → A memory or piece of knowledge

## Etymology

From Husserl's phenomenology: Noema is the intentional content of a mental act — the "what" that a thought is about, as it appears to consciousness.

The paired concept is Noesis — the act of thinking itself. Noema is what is thought; Noesis is the thinking of it. Traces are the collective units [memory entry] comprised of a markdown doc + corresponding DB rows. The Cortex is the collection of Traces.

## Commands

```bash
go build ./cmd/noema   # build the binary
go run ./cmd/noema     # run without building
go build ./...         # build all packages (CI check)
go test ./...          # run all tests
go vet ./...           # static analysis
```

## Branch Strategy

- `main` is the stable/release branch
- Use a `feature` or `bug` branch for active work; PRs go feature/bug-branch → `next`
- Use a `next` branch for staging/testing upcoming (beta) work; PRs go `next` → `main`

## Tech Stack

Super simple, super basic. Intentionally lightweight. Transparently open.

**Language:** Go (v1). Revisit Rust if MCP server concurrency demands it.
- SQLite: `modernc.org/sqlite` (pure-Go, no CGo required)
- TUI: Charm / Bubble Tea
- Search: SQLite FTS5 — no semantic/vector search in v1

Markdown files are the content holders of data. The SQLite DB is a basic schema to hold pieces of metadata about the markdown files (location, author, type, tag(s), ...) in order to provide mapping, related contexts, search performance.

The collective unit [memory entry] comprised of markdown doc + corresponding DB rows will be known as a 'Trace'.

A Trace shall have a single markdown doc, though the doc may be as small or large as needed. Traces may be categorized as one of several types based on intent. They may be tagged in such a way as to allow for distinct Traces to be formed into logical groupings or patterns as the data grows.

A collection or instance containing Traces will be known as a 'Cortex'.

### Trace Structure

Every Trace is a markdown file with YAML frontmatter followed by free-form content:

```markdown
---
id: 20260329-why-we-chose-go
title: Why we chose Go
type: decision
author: research-agent-1
tags: [go, language, architecture]
derived_from: [20260328-language-candidates]
origin: research-cortex
created: 2026-03-29T14:23:00Z
updated: 2026-03-29T14:23:00Z
---

Body content here.
```

All frontmatter fields are indexed in the DB. `author` is a free-form string — it can be a human username, an agent name, or omitted. Multi-agent systems use it to identify which peer wrote a given Trace.

`derived_from` is an optional list of trace IDs this Trace was derived from — it builds a lineage graph queryable via `trace_lineage`. `origin` is the name of the Cortex that produced the Trace; it is set automatically on creation and used by federation to attribute remote Traces. Both fields use `omitempty`, so older Traces without them parse unchanged.

### Trace Types

Every Trace has exactly one type:

| Type | Meaning |
|---|---|
| `fact` | A discrete thing that is true |
| `decision` | A choice made and why |
| `preference` | A behavioral or stylistic lean |
| `context` | Situational background |
| `skill` | A learned capability or procedure |
| `intent` | Something that needs to happen (pickup is autonomous) |
| `observation` | Something witnessed but not yet verified |
| `note` | Anything else |
| `divergence` | A concurrent edit conflict, auto-created by federation sync |

### Cortex Storage Layout

Each Cortex is a named directory the user manages. Layout:

```
<cortex-dir>/
  cortex.md         ← Cortex manifest (see below)
  archive/
    traces/         ← Archived traces, same format as active traces
  db/
    noema.db        ← SQLite DB: metadata, tags, FTS5 index
  traces/
    20260329-why-we-chose-go.md
    20260329-another-trace.md
  trash/
    traces/         ← Removed traces, kept for 30 days [user-configurable]
```

Trace filenames follow the pattern `YYYYMMDD-slugified-title.md` (ISO 8601). The markdown files are the source of truth for content; the DB is the index.

**`cortex.md` manifest** (YAML, minimal — not a config file):

```yaml
name: my-cortex
purpose: "Primary memory for the research agent cluster"
owner: mark
created: 2026-03-29
version: 1
```

### Archiving

Archiving is non-destructive and fully reversible:

- `noema archive <id>` — moves the markdown file to `archive/traces/`, sets `archived_at` timestamp in DB
- `noema unarchive <id>` — moves it back to `traces/`, clears `archived_at`
- Archived Traces are **hidden by default** in all list/search output
- Surface them with `--archived` (archived only) or `--all` (active + archived)
- DB query pattern: active = `WHERE archived_at IS NULL`; archived = `WHERE archived_at IS NOT NULL`

### Event Log, Lineage, and Federation

Every mutation (create / update / archive / unarchive / trash / recover / purge) is recorded as an immutable event in the `events` table inside the same SQLite transaction as the mutation itself. Events carry a ULID, an action, the trace ID, the **stable cortex ID** (`cortex_id`, a ULID), the originating Cortex display name (`origin`, mutable), an RFC3339 timestamp, a JSON snapshot of the trace state (for create/update), and a vector clock snapshot keyed on cortex IDs. `Remove()` (hard delete) and `Sync()` (filesystem reconciliation) intentionally emit no events — hard deletes are local-only escape hatches and Sync is reconciliation, not a semantic mutation.

**Filesystem watcher.** Under `noema serve` (stdio OR http), a background watcher in `internal/watch/` observes `traces/`, `archive/traces/`, and `trash/traces/` with fsnotify. External edits (Obsidian, VS Code, Finder drag, `rm`, iCloud sync from another device, a second noema process on the same cortex) are dispatched through the same `Cortex` mutation methods as MCP tools — `Add`, `Update`, and the `MarkArchivedNoMove` / `MarkTrashedNoMove` / `IngestExternalDelete` / `ApplyExternalPurge` helpers that update DB state and emit events without attempting a file move the OS already did. Loopback is prevented by content-hash comparison: a write whose body hash matches the DB's `content_hash` is Noema's own and gets skipped. Source-locked foreign traces are refused on external edit. The watcher is on by default; opt out with `watch: { enabled: false }` in `cortex.md`. Debounce window defaults to 300ms to collapse editor save bursts. See `internal/watch/reconcile.go` for the action dispatch table. Federation propagation is still HTTP-only (peers need a network endpoint), but external-edit events land in the local log during stdio sessions and flow outward the next time an HTTP serve runs.

Lineage is a separate `trace_lineage` join table populated from the `derived_from` frontmatter field. `trace_lineage` supports both directions: `cortex.Get()` reads `derived_from`, and `cortex.DerivedBy()` reads the reverse edge.

**Cortex identity.** Every Cortex has a stable ULID stored in `cortex.md` as `id` and surfaced via the `cortex_identity` MCP tool. The display name (`name`) can be renamed without affecting federation; the ID is the federation key. Vector clocks are keyed on cortex IDs, not names, so two peers that share a display name can no longer silently collapse into one bucket. The federation syncer pins a peer's ID on first contact and refuses to talk to an endpoint whose advertised ID has changed (peer reset, replaced, or restored from another cortex's backup). The same-name guardrail is still enforced at config time as a UX safety net even though identity is now ID-based. See `docs/design/cortex-uuid-plan.md` for the full design.

Federation is opt-in via a `federation:` block in `cortex.md` (peers + interval). When `noema serve --transport http` starts on a Cortex with peers configured, a background syncer polls each peer's `sync_events` MCP tool over Streamable HTTP (the `/mcp` endpoint, MCP 2025-03-26), replays new events into the local Cortex, and merges the remote vector clock into the local one. Replayed events are stored with their original ID, cortex_id, and origin so the event log becomes the union of all peers' events (no event amplification). When two peers update the same trace concurrently — vector clocks neither dominate nor are dominated — Noema creates a `type: divergence` Trace preserving every conflicting version under deterministically-sorted `### Version from <name> (<id-prefix>)` headers, rather than overwriting. Resolve via `noema resolve <divergence-id> --accept <name|id-prefix>` or `--custom <body>` (or the `resolve_divergence` MCP tool).

**Federation modes.** The `federation.mode` field in `cortex.md` controls sync directionality:

| Mode | Syncer runs? | `sync_events` serves? | HTTP write tools? |
|------|-------------|----------------------|-------------------|
| `sync` (default) | Yes | Yes | Yes |
| `publish` | No | Yes | Blocked (read-only for remote peers) |
| `subscribe` | Yes | Blocked | Yes |

In publish mode, mutating MCP tools (`create_trace`, `update_trace`, `append_trace`, `delete_trace`, `recover_trace`, `archive_trace`, `unarchive_trace`, `resolve_divergence`) return a clear error on the HTTP transport. The local stdio transport retains full write access so operators can manage content locally. In subscribe mode, `sync_events` returns an error so remote peers cannot pull events from this cortex.

Each peer can also declare `mode: paused` to temporarily skip syncing without losing cursor/identity state. CLI commands: `noema federation set-mode <sync|publish|subscribe>`, `noema federation pause-peer <name>`, `noema federation resume-peer <name>`.

The full design rationale, edge cases, and phase breakdown live in `docs/design/federation-plan.md`.

**Content hashing and source-locking.** Every trace mutation (`Add`, `Update`, `Append`) computes a SHA-256 hash of the body (`sha256:<hex>`) and stores it in the `content_hash` frontmatter field and DB column. The hash travels through federation events so peers receive it. Three frontmatter fields support integrity:

- `content_hash` — current body hash, recomputed on every write
- `source_hash` — the origin's `content_hash` at publish time, carried through federation
- `source_locked` — when `true`, consumer-side tooling refuses `Update`, `Trash`, and `Remove` on this trace (only enforced when `origin != local cortex name`; the publisher can always edit its own traces)

`Archive` and `Unarchive` remain allowed on source-locked traces (non-destructive). CLI commands accept `--force` to bypass the lock. MCP tools surface `ErrSourceLocked` as a tool error. The TUI blocks the `e` key with a status message.

CLI: `noema verify` checks all trace file hashes against their frontmatter `content_hash` (with `--backfill` for old traces). `noema drift` checks federated traces against their `source_hash`.

**MCP access posture.** The HTTP MCP endpoint runs either in **open mode** (no auth, the default — suitable only for loopback) or in **keyed mode** (every request must carry `Authorization: Bearer <key>`). Keyed mode is mandatory for federation rings and any non-loopback deployment, and it also requires TLS — the server refuses to start with a bearer key over plaintext HTTP. The key is supplied via `NOEMA_MCP_KEY` (env var, wins if both are set) or via an optional `access.shared_key_file` block in `cortex.md` pointing at a 0600 sidecar file. The server logs the active posture on startup as `access=keyed source=env|file fingerprint=SHA256:...`, and `noema federation key fingerprint` reproduces the same non-secret fingerprint for out-of-band verification across ring members. The federation syncer automatically injects the local host's bearer key into every outbound `sync_events` call; there is no per-peer key config, so every host in a ring must share one key. Mixed-mode rings (some keyed, some open) are not supported — they isolate by design. See `docs/design/mcp-auth-plan.md` for the full threat model and decision log, and `internal/mcp/middleware.go` for the CORS + auth middleware chain (CORS outermost so browser preflights bypass the auth gate as the spec requires).

### MCP Tools

The MCP server (`internal/mcp/server.go`) exposes the full Cortex surface: `get_instructions`, `list_traces`, `get_trace`, `create_trace`, `update_trace`, `append_trace`, `delete_trace`, `recover_trace`, `archive_trace`, `unarchive_trace`, `search_traces`, `trace_history`, `trace_lineage`, `resolve_divergence`, `cortex_identity`, `sync_events`, `federation_status`, and `announce_peer`. The `get_instructions` tool returns a live reference guide for the active Cortex; agents should call it first in any new session. `cortex_identity` returns the stable ULID + display name + manifest version and is called by federation peers on every sync to verify the remote endpoint still belongs to the cortex they paired with. `cortex_identity` also piggybacks the local consolidation rank (see below) so remote peers can observe eligibility without a separate round-trip.

### Mid → long graduation (Phase 15)

The three-tier model completes with an automatic mid→long promoter. A graduation pass runs on the same scheduler cadence as the heuristic short→mid pass (`ChainPasses` composes them into one PassFn) and evaluates every mid-tier trace older than `consolidation.graduation.min_age_days` (default 14) against a simple AND-gate: `read_count >= min_read_count` (default 3) AND (modify_count == 0 if `require_unmodified`) AND `tier_votes >= 0`. The AND-gate is deliberate — the long tier is meant to be auditable and slow-moving, not probabilistic. Explicit curation is available via `noema memory promote <id> [--to mid|long]`; `noema memory demote <id>` handles mid→short only (long-term demotion goes through `noema memory purge` to preserve the ceremony). The underlying `internal/cortex/candidates.go:GraduationCandidates` query flips `PromotionCandidates`' `created_at >= cutoff` to `<= cutoff` so it narrows to traces that have had time to prove durability. Every tier transition emits `ActionPromote` / `ActionDemote` which replicate through federation via the new replay handlers in `internal/cortex/cortex.go:replayTierChange`.

### Consolidation coordination in a federation

When multiple federated peers have `consolidation.enabled` + `consolidation.llm_enabled` with a reachable `local_llm_endpoint`, exactly one peer runs each consolidation cycle. Each peer's `internal/consolidation.EligibilityLoop` advertises a random rank in `[1,99]` via `federation_state` and the `cortex_identity` heartbeat. At trigger time the `Election.Decide` evaluator picks the highest-ranked eligible peer (with lex-max `cortex_id` breaking ties); the `consolidation.WithElection` wrapper emits `consolidation_claim`, waits one quiet period (`2 × federation.interval`) for conflicting claims, runs the pass, and emits `consolidation_success` or `consolidation_fail`. Subscribe-mode cortexes advertise rank 0 and never win; paused peers naturally drop out via staleness. Zero new config knobs — coordination is automatic when `federation.peers` is non-empty. Single-node cortexes skip the gate entirely to avoid emitting noise for degenerate one-peer elections.

### Hermes Memory Provider Plugin

A Python plugin at `plugins/hermes/` implements the Hermes `MemoryProvider` ABC, letting any [Hermes](https://hermes-agent.nousresearch.com/) agent use a Noema Cortex as its memory backend. The plugin communicates with Noema via MCP (JSON-RPC 2.0 over stdio or HTTP) — it does not import any Go code.

**Files:** `__init__.py` (NoemaMemoryProvider + register()), `transport.py` (StdioTransport, HttpTransport), `plugin.yaml` (metadata), `README.md` (setup).

**Agent-facing tools:** `noema_search`, `noema_remember`, `noema_recall`, `noema_list`, `noema_update`, `noema_lineage` — mapped to `search_traces`, `create_trace`, `get_trace`, `list_traces`, `update_trace`, `trace_lineage` respectively.

**Internal tools used:** `get_instructions` (system prompt), `cortex_identity` (connectivity check), `append_trace` (session log), `archive_trace` (session end).

### DB Schema Migrations

Schema changes must always be transparent and non-destructive. Rules:

- Migrations are versioned SQL files embedded in the binary (`embed.FS`), run in order on startup if the DB is behind the current version
- The DB tracks the applied version in a `schema_migrations` table (one row per applied version)
- Migrations only use `ALTER TABLE ... ADD COLUMN`, new table creation, or new index creation — never `DROP`, never destructive `ALTER`
- If a migration would require removing or restructuring data, provide a separate explicit `noema migrate` command with a clear description of what it does, requiring user confirmation
- Migration failures abort startup with a clear error; they never partially apply

The runner is a thin hand-rolled loop over embedded `*.sql` files in `internal/db/migrations/`, sorted by leading version number. Currently applied: `001_initial.sql`, `002_trash.sql`, `003_events_and_lineage.sql` (event log + `trace_lineage` + `traces.origin` column), `004_federation_state.sql` (key-value store for vector clocks and per-peer cursors), `005_cortex_identity.sql` (`cortex_id` column on `traces` and `events`), `006_content_hash.sql` (`content_hash` column on `traces`), and `007_source_locking.sql` (`source_locked` + `source_hash` columns on `traces`).

`cortex.md` carries a `version` field. The current `ManifestVersion` is **2** — version 2 carries a stable cortex `id` (a ULID) and re-keys the local event log + vector clock from cortex-name to cortex-id. Cortexes written by older binaries must run `noema migrate cortex-id` (an explicit, interactive, backed-up migration in `internal/cli/cmd_migrate.go`) before they can be opened by this binary; the `--reset` flag handles the case where the directory is a copy of another cortex (Gotcha #3 in the design doc).

### Cortex Creation

```
noema init --name <name>               # creates ~/.noema/<name>/
noema init --name <name> --path <dir>  # creates <dir>/<name>/
```

Both forms scaffold the full Cortex layout (see above) and write `cortex.md`.

### Config File

`~/.config/noema/config.yaml` (XDG standard). Stores the active default Cortex name and any user-level settings.

### Cortex Selection

Priority order (highest first):

1. `--cortex <name>` flag on any command
2. `NOEMA_CORTEX` environment variable
3. Default Cortex set via `noema use <name>` (persisted in config)

## Usage Intents

### CLI

The binary shall provide straightforward capabilities to insert, edit, list, and remove Traces in the Cortex. Entry and modification of Traces should be in a user-friendly way, such as:

Enter/Modify Trace
Subject/Title: <enter subject/title>
Tag(s) [comma or semicolon separated]: <one, or; more-tags, here>
Body/Content:
<enter body/content and press Ctrl+D to end + save. Esc to cancel Trace>

Additionally, for other tooling not supporting MCP, we should allow for CLI arguments (positional or named) to be consumed for usage against the Cortex.

### TUI

This should provide an intuitive one-stop-shop approach to insertion, modification, listing, and removal of Traces in the Cortex.

### MCP

A lightweight MCP server is spawned with `noema serve` (or `noema server`). It exposes Cortex operations — list, create, update, delete, search, audit, lineage, federation — to any MCP consumer.

**Transports:** both `stdio` and Streamable HTTP must be supported, so the server works with Claude Code agents, GitHub Copilot extensions, Zed, and any other MCP-compatible tooling. The transport is selected by `--transport stdio|http`. The HTTP transport is the **Streamable HTTP** transport from the [MCP 2025-03-26 spec](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports#streamable-http) — a single endpoint at `/mcp` that handles JSON-RPC POSTs and optional SSE streaming on the same path. The legacy two-endpoint SSE transport has been removed; passing `--transport sse` returns a clear migration error. HTTP mode requires an explicit `--host` (binding to `0.0.0.0`/`::` is rejected to prevent accidental network exposure) and supports `--tls-cert`/`--tls-key` for HTTPS. Federation only runs in HTTP mode because peers need a network endpoint to call `sync_events` on; a single Cortex can serve stdio (for a local agent) and HTTP (for federation) from separate processes thanks to SQLite WAL mode.
