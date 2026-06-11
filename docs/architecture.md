# Architecture

Noema is a local-first memory layer for humans and AI agents. It exposes a CLI,
a TUI, and an MCP server over stdio or Streamable HTTP. The core domain terms
are:

- **Noema**: the project and binary.
- **Cortex**: one named memory collection stored in a directory.
- **Trace**: one memory entry, represented by a markdown file plus indexed
  SQLite metadata.

## Storage Model

A Cortex is a user-managed directory:

```text
<cortex-dir>/
  cortex.md
  db/noema.db
  traces/
  archive/traces/
  trash/traces/
```

Trace markdown files are the source of truth. Each file contains YAML
frontmatter followed by free-form body content:

```markdown
---
id: 20260329-why-we-chose-go
title: Why we chose Go
type: decision
author: research-agent-1
tags: [go, architecture]
derived_from: [20260328-language-candidates]
origin: research-cortex
created: 2026-03-29T14:23:00Z
updated: 2026-03-29T14:23:00Z
---

Body content here.
```

Valid trace types are `fact`, `decision`, `preference`, `context`, `skill`,
`intent`, `observation`, `note`, and `divergence`.

## Database And Migrations

SQLite stores indexes, tags, lineage, event history, federation state, usage
signals, and optional embeddings. Migrations live in
`internal/db/migrations/`, are embedded into the binary, and run in version
order. Schema changes must be transparent and non-destructive: add columns,
tables, or indexes; do not drop or reshape stored data inside automatic
migrations. Destructive or structural migrations need an explicit CLI command
with clear operator confirmation.

## Event Log, Lineage, And Federation

Mutations create immutable events in the SQLite event log within the same
transaction as the content change. Events include a ULID, action, trace ID,
cortex ID, origin, timestamp, vector clock snapshot, and trace state where
needed. Lineage is tracked separately from `derived_from` so Noema can query
both ancestors and descendants.

Federation is opt-in through the `federation:` block in `cortex.md`. Peers sync
over HTTP by pulling `sync_events`, replaying remote events, and merging vector
clocks. Concurrent updates that neither vector clock dominates create a
`divergence` trace rather than silently overwriting content.

Events are authenticated with per-cortex Ed25519 signatures. A cortex that has
run `noema keygen` signs every event it emits and advertises its public key
through the `cortex_identity` handshake; peers pin that key per cortex id
(trust-on-first-use) and, under `federation.verify: enforce`, reject events that
are not correctly signed by their owning cortex. This is what makes source-lock
enforceable on the replay path and not just for local mutations.

## Watcher And External Edits

`noema serve` starts a filesystem watcher unless disabled in `cortex.md`. It
observes active, archived, and trashed trace directories, then reconciles
external edits through the same mutation paths used by CLI and MCP tools.
Content hashes prevent Noema from reacting to its own writes. The watcher also
guards against atomic-save gaps and can heal missing frontmatter from the DB row
for locally-owned traces.

## MCP And Access Posture

The MCP server exposes Cortex operations such as list, get, create, update,
archive, search, lineage, history, federation status, and sync. Stdio mode is
for local agent integration. HTTP mode uses the Streamable HTTP `/mcp` endpoint
and requires an explicit host. Keyed HTTP mode requires TLS and authenticates
requests with `Authorization: Bearer <key>` from `NOEMA_MCP_KEY` or a configured
sidecar key file.

## Search And Consolidation

Lexical FTS5 search is always available. Semantic and hybrid search are opt-in:
embeddings are stored locally as a derived index and are never federated.

Noema supports short, mid, and long memory tiers. Consolidation can promote
short-term traces heuristically or through an optional local LLM pipeline; a
separate graduation pass promotes durable mid-tier traces to long-term memory.
