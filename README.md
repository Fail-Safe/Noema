# Noema

**The intentional memory layer for your AI agents.**

Noema gives AI agents — and the humans working alongside them — a persistent, structured place to record what they know, decide, observe, and intend. Every memory is a plain markdown file. The index is a local SQLite database. Nothing lives in the cloud; nothing requires an API key.

---

## Concepts

| Term | Meaning |
|---|---|
| **Trace** | A single memory — one markdown file + its database row |
| **Cortex** | A named collection of Traces, stored in a directory you control |

A Trace has a **type** that describes its intent:

| Type | Meaning |
|---|---|
| `fact` | A discrete thing that is true |
| `decision` | A choice made and why |
| `preference` | A behavioral or stylistic lean |
| `context` | Situational background |
| `skill` | A learned capability or procedure |
| `intent` | Something that needs to happen |
| `observation` | Something witnessed but not yet verified |
| `note` | Anything else |

---

## Installation

Requires Go 1.21+.

```bash
go install github.com/Fail-Safe/Noema/cmd/noema@latest
```

Or build from source:

```bash
git clone https://github.com/Fail-Safe/Noema.git
cd Noema
go build -o noema ./cmd/noema
```

---

## Quick Start

```bash
# Create a Cortex
noema init --name my-cortex

# Add a Trace interactively
noema add

# Add a Trace with flags
noema add --title "We chose Go" --type decision --tag go --body "Pure-Go SQLite, fast iteration."

# List Traces
noema list

# Search
noema search "sqlite"

# View a Trace
noema get 20260329-we-chose-go
```

---

## CLI Reference

```
noema init --name <name> [--path <dir>]   Create a new Cortex
noema use <name>                          Set the default Cortex
noema cortex list                         List all known Cortexes

noema add [flags]                         Add a Trace (interactive if flags omitted)
noema list [flags]                        List Traces
noema get <id>                            Show a Trace
noema edit <id>                           Edit a Trace in $EDITOR
noema remove <id>                         Move a Trace to trash (--force to hard-delete)
noema recover <id>                        Restore a Trace from trash
noema purge [--days N]                    Permanently delete all trashed Traces older than N days
noema search <query> [flags]              Full-text search (FTS5)

noema archive <id>                        Archive a Trace
noema unarchive <id>                      Restore an archived Trace
noema sync                                Re-index trace files into the database

noema serve [--transport stdio|sse]       Start the MCP server
noema serve --print-config                Print a ready-to-use .mcp.json snippet and exit
noema tui                                 Open the interactive TUI
noema completion [bash|zsh|fish|install]  Generate shell completions
```

**Common flags:**

```
--cortex <name>       Target a specific Cortex (overrides NOEMA_CORTEX env and config default)
--type <type>         Filter by Trace type
--author <name>       Filter by author
--tag <tag>           Filter by tag
--archived            Show only archived Traces
--trashed             Show only trashed Traces
--all                 Show active and archived Traces
```

**Cortex selection priority** (highest wins):

1. `--cortex` flag
2. `NOEMA_CORTEX` environment variable
3. Default set via `noema use <name>`

---

## MCP Server

Noema can run as an [MCP](https://modelcontextprotocol.io) server, giving any MCP-compatible AI tool direct access to your Cortex.

**Tools exposed:** `get_instructions`, `list_traces`, `get_trace`, `create_trace`, `update_trace`, `delete_trace`, `recover_trace`, `search_traces`, `archive_trace`, `unarchive_trace`

`delete_trace` moves a trace to trash (soft-delete, recoverable). Use `recover_trace` to restore it.

Call `get_instructions` first in any new session — it returns a live reference guide covering Trace types, field definitions, filtering options, and tool usage, with the active Cortex's name and purpose already filled in.

### stdio (Claude Desktop, Claude Code, any MCP client)

Generate a ready-to-use config snippet for the current machine and cortex:

```bash
noema serve --print-config
```

This prints a `.mcp.json` block with the correct binary path and cortex already filled in. Pipe it to a file to use it:

```bash
# Claude Code (project-level)
noema serve --print-config > .mcp.json

# Claude Desktop — merge the "noema" block into ~/Library/Application Support/Claude/claude_desktop_config.json
noema serve --print-config
```

The `--cortex` flag, `NOEMA_CORTEX` env, and config default are all respected, so `--print-config` always reflects the cortex you would actually use.

### SSE (HTTP clients, GitHub Copilot)

```bash
noema serve --transport sse --port 3000
# Endpoint: http://localhost:3000/sse
```

---

## Data Model

Traces are plain markdown files with YAML frontmatter. The markdown file is the source of truth; the SQLite database is a derived index that enables fast filtering and full-text search.

```markdown
---
id: 20260329-why-we-chose-go
title: Why we chose Go
type: decision
author: research-agent-1
tags: [go, architecture]
created: 2026-03-29T14:23:00Z
updated: 2026-03-29T14:23:00Z
---

Go gives us pure-Go SQLite (no CGo), best-in-class TUI tooling, and fast
iteration. We can revisit Rust if the MCP server demands higher concurrency.
```

**Cortex layout on disk:**

```
my-cortex/
  AGENT.md            ← agent guide (generated by noema init, see below)
  cortex.md           ← manifest: name, purpose, owner, created
  traces/             ← active Traces
  archive/
    traces/           ← archived Traces (hidden by default, fully reversible)
  trash/
    traces/           ← soft-deleted Traces (auto-purged after 30 days by default)
  db/
    noema.db          ← SQLite index (metadata, tags, FTS5)
```

The `author` field is free-form — a human username, an agent name, or omitted. Multi-agent systems use it to track which peer wrote a given Trace.

---

## Agent Access

Noema supports three access patterns, depending on what tooling an agent has available:

### MCP (preferred)

Connect via `noema serve` and use the MCP tools. Call `get_instructions` at the start of a session for a live reference guide — it includes the Cortex name, purpose, Trace type definitions, and a full tool reference. Changes are indexed immediately; no manual sync needed.

### `noema` binary

Use the CLI commands directly. `noema sync` re-indexes any files written directly to disk by other agents or humans.

### Filesystem only (no binary, no MCP)

Read and write markdown files directly. `AGENT.md` at the Cortex root (generated by `noema init`) explains the file format, directory layout, Trace types, and naming conventions for agents that arrive with only file access. After making changes, run `noema sync` when the binary next becomes available to reconcile the database.

---

Each agent identifies itself via the `author` field when creating Traces. Filter by `author` to read only a specific agent's prior work. Because Traces are plain markdown files, a human can inspect, edit, or audit the Cortex at any time without any special tooling.

---

## License

MIT — see [LICENSE](LICENSE).
