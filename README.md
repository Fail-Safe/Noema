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
| `divergence` | A concurrent edit conflict, auto-created by federation sync |

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
make build                # dev build with debug info  -> ./noema
make release              # stripped build for this host -> dist/noema-<os>-<arch>
make release-linux        # stripped build for linux/amd64 -> dist/noema-linux-amd64
```

Dev builds keep the symbol table and DWARF info for debugging (~19 MB).
Release builds strip both and run with `-trimpath` for a ~13 MB static
binary with the git version embedded via `-ldflags`. `make help` lists
all targets.

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
noema cortex remove <name> [--purge] [--force]
                                          Unregister a Cortex (--purge also deletes its directory)
noema cortex backup <name> [-o <path>] [--force]
                                          Write a gzipped tarball of a Cortex
noema cortex restore <tarball> [--name <n>] [--path <dir>] [--force]
                                          Restore a Cortex from a backup tarball

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
noema sync [--recover]                    Re-index trace files; --recover rebuilds missing files from the event log
noema events [trace-id] [--since] [--limit]
                                          Show the event log (audit trail) for a trace, or recent events across all traces
noema resolve <divergence-id> --accept <origin> | --custom <body>
                                          Resolve a divergence (concurrent edit conflict)

noema federation status                   Show federation config, peer sync state, and vector clock
noema federation peers                    List configured federation peers
noema federation add-peer <name> <endpoint>
                                          Add a federation peer to cortex.md

noema serve [--transport stdio|sse] [--host <addr>] [--tls-cert <file> --tls-key <file>]
                                          Start the MCP server (SSE requires --host)
noema serve --print-config                Print a ready-to-use .mcp.json snippet and exit
noema serve ... --print-systemd-unit      Print a systemd service unit for the current serve flags
noema serve ... --print-launchd-plist     Print a launchd LaunchAgent plist for the current serve flags
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

**Tools exposed:**

| Tool | Purpose |
|---|---|
| `get_instructions` | Live reference guide for this Cortex (call first in any new session) |
| `list_traces` | List traces, filterable by `type`, `author`, `tag`, `origin`, `archived`, `all` |
| `get_trace` | Fetch a trace's full body, origin, and lineage |
| `create_trace` | Create a new trace (supports `derived_from`, `origin`) |
| `update_trace` | Update any subset of fields on an existing trace |
| `search_traces` | FTS5 full-text search |
| `archive_trace` / `unarchive_trace` | Archive a trace or restore it |
| `delete_trace` / `recover_trace` | Soft-delete (move to trash) or restore from trash |
| `trace_history` | Event log (audit trail) for a trace |
| `trace_lineage` | Derivation graph: `derived_from` + `derived_by` |
| `resolve_divergence` | Resolve a concurrent edit conflict by accepting an origin or supplying a merged body |
| `sync_events` | Pull events for federation sync (called by remote peers) |
| `federation_status` | Federation config, peer sync state, vector clock, unresolved divergences |
| `announce_peer` | Accept a peer announcement for mutual discovery |

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

### SSE (HTTP clients, GitHub Copilot, federation peers)

```bash
# Local-only listener
noema serve --transport sse --host 127.0.0.1 --port 3000

# LAN-reachable listener (for federation peers)
noema serve --transport sse --host 10.0.0.5 --port 3000

# HTTPS
noema serve --transport sse --host 10.0.0.5 --port 3000 \
            --tls-cert /path/server.crt --tls-key /path/server.key
```

`--host` is **required** in SSE mode and must be an explicit address — `0.0.0.0`/`::` are rejected to avoid accidentally exposing a Cortex on every interface. Pair `--tls-cert` with `--tls-key` to serve over HTTPS.

### Running as a persistent service

For ad-hoc use, backgrounding with `nohup` works fine:

```bash
nohup noema serve --cortex agentbrain --transport sse --host 127.0.0.1 \
  > ~/noema.log 2>&1 &
disown
```

For a real federation host you probably want a process supervisor — restart on crash, start at boot, logs aggregated. Noema can print a ready-to-install unit/plist that mirrors the serve command you've already validated:

**Linux (systemd)**

```bash
noema serve --cortex agentbrain --transport sse --host 192.168.1.10 --print-systemd-unit \
  | sudo tee /etc/systemd/system/noema-agentbrain.service
sudo systemctl daemon-reload
sudo systemctl enable --now noema-agentbrain
sudo journalctl -u noema-agentbrain -f
```

**macOS (launchd)**

```bash
noema serve --cortex agentbrain --transport sse --host 127.0.0.1 --print-launchd-plist \
  > ~/Library/LaunchAgents/com.fail-safe.noema.agentbrain.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.fail-safe.noema.agentbrain.plist
tail -f ~/Library/Logs/noema-agentbrain.log
```

Both flags require `--transport sse` (stdio has no endpoint to supervise) and an explicit `--cortex` (the unit/plist pins exactly one cortex — NOEMA_CORTEX and the config default aren't carried into the service environment). All the usual SSE flag invariants (`--host` not `0.0.0.0`, TLS pair symmetry) are validated at preview time, so you catch misconfigurations before installing.

The emitted unit filename convention is `noema-<cortex>.service` / `com.fail-safe.noema.<cortex>.plist`, so running multiple cortexes on one host never collides.

---

## Federation

A Cortex can sync with peer Cortexes over SSE: every mutation is recorded in an immutable event log, peers pull each other's events, and concurrent edits surface as **divergence traces** instead of silently overwriting. Federation is fully opt-in — a Cortex with no `federation` block in `cortex.md` runs exactly as before.

### Configure peers in `cortex.md`

```yaml
name: alpha
purpose: Primary research cortex
owner: mark
created: 2026-03-29
version: 1
federation:
  interval: 30s
  peers:
    - name: beta
      endpoint: http://192.168.1.10:3000
    - name: gamma
      endpoint: https://192.168.1.11:3000
      ca: /etc/noema/beta-ca.pem    # optional, for self-signed TLS
```

Or add a peer from the CLI:

```bash
noema federation add-peer beta http://192.168.1.10:3000
```

### How sync works

When `noema serve --transport sse` starts and `cortex.md` has peers, a background syncer polls each peer's `sync_events` MCP tool on the configured `interval` (default 30s). New events are replayed locally — files are written, the DB is updated, and the event is stored in the local log with its original ID and origin (no event amplification).

Every event carries a **vector clock** snapshot. Each Cortex tracks one counter per peer it has heard from; on every local mutation it bumps its own counter, and on every replayed remote event it merges the remote clock into its own.

### Divergence traces

When two peers update the same trace concurrently (their vector clocks neither dominate nor are dominated), neither edit overwrites the other. Instead, Noema creates a **divergence trace** with `type: divergence`, tags `[divergence, needs-resolution]`, and `derived_from: [<original-id>]`. Its body lists every conflicting version under `### Version from <origin>` headers, deterministically rendered (origins sorted by name) so every replica produces identical content.

Find unresolved divergences:

```bash
noema list --type divergence
noema federation status     # also shows the count
```

Resolve a divergence by picking a side or supplying a custom merge:

```bash
noema resolve <divergence-id> --accept beta
noema resolve <divergence-id> --custom "merged body content"
```

Either form updates the original trace, federates the resolution, and trashes the divergence trace. The MCP equivalent is the `resolve_divergence` tool.

### Audit trail and recovery

Every create / update / archive / unarchive / trash / recover / purge is recorded as an event with a ULID, timestamp, origin, and JSON snapshot. Inspect from the CLI:

```bash
noema events 20260329-why-we-chose-go     # full history for one trace
noema events --limit 50                   # recent events across all traces
noema events --since 01JQXYZ...           # cursor-based pagination
```

If a trace file is lost from disk but its event log survives, `noema sync --recover` rebuilds the file from the most recent `create`/`update` event snapshot.

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
derived_from: [20260328-language-candidates]
origin: research-cortex
created: 2026-03-29T14:23:00Z
updated: 2026-03-29T14:23:00Z
---

Go gives us pure-Go SQLite (no CGo), best-in-class TUI tooling, and fast
iteration. We can revisit Rust if the MCP server demands higher concurrency.
```

`derived_from` records which traces informed this one (used by `trace_lineage` to build a knowledge graph). `origin` is the name of the Cortex that created the trace — set automatically and used by federation to attribute remote traces. Both fields are optional; existing traces without them parse unchanged.

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
