# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

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
- Use a `dev` or `next` branch for active feature work; PRs go feature-branch → `main`

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
created: 2026-03-29T14:23:00Z
updated: 2026-03-29T14:23:00Z
---

Body content here.
```

All frontmatter fields are indexed in the DB. `author` is a free-form string — it can be a human username, an agent name, or omitted. Multi-agent systems use it to identify which peer wrote a given Trace.

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

### DB Schema Migrations

Schema changes must always be transparent and non-destructive. Rules:

- Migrations are versioned SQL files embedded in the binary (`embed.FS`), run in order on startup if the DB is behind the current version
- The DB tracks the applied version in a `schema_migrations` table (single-row: `version INTEGER`)
- Migrations only use `ALTER TABLE ... ADD COLUMN`, new table creation, or new index creation — never `DROP`, never destructive `ALTER`
- If a migration would require removing or restructuring data, provide a separate explicit `noema migrate` command with a clear description of what it does, requiring user confirmation
- Migration failures abort startup with a clear error; they never partially apply

Use `pressly/goose` or a thin hand-rolled runner over embedded `*.sql` files — whichever stays closer to the "no magic" principle.

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

A lightweight MCP server is spawned with `noema serve` (or `noema server`). It exposes Cortex operations — list, create, update, delete, search — to any MCP consumer.

**Transports:** both `stdio` and SSE must be supported, so the server works with Claude Code agents, GitHub Copilot extensions, and any other MCP-compatible tooling. The transport is selected by how the server is invoked (flag or auto-detected).
