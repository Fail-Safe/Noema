<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/assets/brand/noema-dark.svg">
    <img alt="Noema." src=".github/assets/brand/noema-light.svg" width="600">
  </picture>
</p>

<p align="center">
  <a href="https://github.com/Fail-Safe/Noema/actions/workflows/ci.yml"><img src="https://github.com/Fail-Safe/Noema/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI"></a>
  <a href="https://github.com/Fail-Safe/Noema/releases/latest"><img src="https://img.shields.io/github/v/release/Fail-Safe/Noema" alt="Latest Release"></a>
  <a href="https://github.com/Fail-Safe/Noema/blob/main/go.mod"><img src="https://img.shields.io/github/go-mod/go-version/Fail-Safe/Noema" alt="Go Version"></a>
  <a href="https://github.com/Fail-Safe/Noema/blob/main/LICENSE"><img src="https://img.shields.io/github/license/Fail-Safe/Noema" alt="License"></a>
</p>

**The intentional memory layer for your AI agents.**

Noema gives AI agents — and the humans working alongside them — a persistent, structured place to record what they know, decide, observe, and intend. Every memory is a plain markdown file. The index is a local SQLite database. Nothing lives in the cloud; nothing requires an API key.

**In short:**

- **Local-first agent memory** exposed as an [MCP](https://modelcontextprotocol.io/) server (stdio + Streamable HTTP). Works out of the box with Claude Code, GitHub Copilot, Zed, Cursor, Aider, and anything else that speaks MCP.
- **Plain markdown on disk** as the source of truth; a local SQLite database with FTS5 as the index. No cloud, no API keys, no telemetry.
- **Peer-to-peer federation** across Cortexes with vector clocks, event-log audit trail, and `divergence` traces on concurrent edits.
- **Your editor is a first-class client.** A filesystem watcher turns Obsidian / VS Code / Finder / iCloud edits into real mutation events — same events as MCP-initiated writes, propagated through federation.
- **Standards-friendly.** Ships an auto-generated [`AGENTS.md`](https://agents.md/) per Cortex, a native [Hermes](https://hermes-agent.nousresearch.com/) memory-provider plugin, and SHA-256 content hashing with optional source-locking for publishers.

---

## Concepts

| Term | Meaning |
|---|---|
| **Trace** | A single memory — one markdown file + its database row |
| **Cortex** | A named collection of Traces, stored in a directory you control |

Contributor and architecture notes:

- [Repository Guidelines](AGENTS.md)
- [Architecture](docs/architecture.md)
- [Development Guide](docs/development.md)

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

### With Homebrew (macOS + Linux)

The fastest path. One command taps `Fail-Safe/homebrew-tap` and
installs the cross-platform formula covering `darwin/{amd64,arm64}`
and `linux/{amd64,arm64}`:

```bash
brew install Fail-Safe/tap/noema
```

On macOS a cask is also published for users who prefer the cask
ecosystem:

```bash
brew install --cask Fail-Safe/tap/noema
```

> The formula path is the default on macOS — when a tap contains both a
> formula and a cask of the same name, `brew install` (without `--cask`)
> resolves the formula first. Pass `--cask` to opt into the cask. Linux
> users get the formula automatically.

Prefer the two-step form? It works the same:

```bash
brew tap Fail-Safe/tap
brew install noema          # formula (macOS + Linux)
brew install --cask noema   # cask (macOS only)
```

#### Beta channel

Prerelease builds (`v*-beta*`, `v*-rc*`, `v*-alpha*`) publish to a
parallel formula so you can track the edge without the stable channel
moving under you:

```bash
brew install Fail-Safe/tap/noema-beta
```

`noema-beta` installs the same `noema` binary and conflicts with the
stable formula — Homebrew will refuse to install both at once. To
switch channels:

```bash
brew uninstall noema      # (or noema-beta)
brew install Fail-Safe/tap/noema-beta   # (or noema)
```

`brew upgrade` on `noema-beta` pulls the newest prerelease; stable tags
(`v0.10.0` vs `v0.10.0-beta.1`) stay on their respective channels.

### Download a pre-built binary

Grab the archive for your OS/arch from the
[Releases page](https://github.com/Fail-Safe/Noema/releases), verify it
against `checksums.txt`, and put `noema` somewhere on your `$PATH`:

```bash
# macOS (Apple Silicon) — adjust VERSION and Arch as needed.
# Pre-built binaries start at v0.3.0; earlier tags (v0.1.x, v0.2.x)
# exist in git history but were never published as downloadable
# releases.
VERSION=0.3.0
curl -LO https://github.com/Fail-Safe/Noema/releases/download/v${VERSION}/noema_${VERSION}_darwin_arm64.tar.gz
curl -LO https://github.com/Fail-Safe/Noema/releases/download/v${VERSION}/checksums.txt
shasum -a 256 -c checksums.txt --ignore-missing
tar -xzf noema_${VERSION}_darwin_arm64.tar.gz
sudo mv noema /usr/local/bin/
noema version
```

Release archives are fully static (pure-Go SQLite, `CGO_ENABLED=0`) so
there's nothing to install alongside the binary. Supported targets:
`darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`,
`windows/amd64`, `windows/arm64`.

### With the Go toolchain

If you already have Go 1.25+ installed:

```bash
go install github.com/Fail-Safe/Noema/cmd/noema@latest
```

### Build from source

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

> **Pre-1.0 notice.** Noema is currently on the `v0.x` line. Expect
> breaking changes between minor releases until v1.0. Cortex data on
> disk is forward-compatible via non-destructive migrations; any
> `cortex.md` version bump that requires a manual step ships with an
> explicit `noema migrate` command.

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

> `noema init` also writes an `AGENTS.md` at the cortex root — the
> [agents.md](https://agents.md/) convention that Codex, Cursor, Aider, Zed,
> Copilot, and most other coding-agent tooling pick up automatically. Any
> agent pointed at the directory will find a complete reference for the
> Trace format and the MCP tools without any additional onboarding.

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
noema append <id> [--content <text>]      Append to a Trace body (pipe-friendly: `echo X | noema append <id>`)
noema remove <id>                         Move a Trace to trash (--force to hard-delete)
noema recover <id>                        Restore a Trace from trash
noema purge [--days N]                    Permanently delete all trashed Traces older than N days
noema search <query> [flags]              Full-text search (FTS5). --semantic / --hybrid
                                          rank by embedding similarity (needs a search: block + backfill)
noema similar <id> [--limit N]            Find traces related to <id> (BM25; --semantic / --hybrid for embeddings)
noema embeddings status                   Show semantic-search embedding coverage (embedded / stale / missing)
noema embeddings backfill [--force] [--limit N]
                                          Embed traces that are missing or stale (for semantic search)

noema archive <id>                        Archive a Trace
noema unarchive <id>                      Restore an archived Trace
noema sync [--recover]                    Re-index trace files; --recover rebuilds missing files from the event log
noema events [trace-id] [--since] [--limit]
                                          Show the event log (audit trail) for a trace, or recent events across all traces
noema events backfill [--dry-run] [--yes]
                                          Synthesize create events for active traces missing one (e.g. traces added via `noema sync`)
noema resolve <divergence-id> --accept <origin> | --custom <body>
                                          Resolve a divergence (concurrent edit conflict)
noema verify [--backfill]                 Run integrity checks (alias for `verify traces` for back-compat);
                                          --backfill populates content_hash for old traces
noema verify traces [--backfill]          Check trace content hashes against frontmatter content_hash
noema verify cortex                       Validate manifest, config, db, access posture, and federation
noema verify drift                        Check federated traces for drift from their source hash

noema memory stats [--detailed]           Show tier counts and (with --detailed) engagement signal
noema memory health [--since 24h]         Show consolidation activity over the window, promotion-latency
                                          percentiles, and the 1-source mid leak detector
noema memory popular [--top 10]           Top traces by search popularity and top tags by aggregate engagement
noema memory promote <id> [--to mid|long] Advance a trace one tier (short→mid or mid→long)
noema memory demote <id>                  Step a mid trace back to short
noema memory purge <id> --tier <t> --reason "..." --confirm [--hard]
                                          Ceremoniously destroy a trace with audit trail (GDPR path)

noema federation status                   Show federation config, MCP access posture, peer sync state, and vector clock
noema federation peers                    List configured federation peers
noema federation add-peer <name> <endpoint>
                                          Add a federation peer to cortex.md
noema federation reset-peer <name>...     Clear stored state for a peer (forces a fresh handshake; use after a peer
                                          ran `noema migrate cortex-id --reset` and the syncer is now reporting an
                                          identity mismatch)
noema federation set-mode <sync|publish|subscribe>
                                          Set the cortex-level federation mode
noema federation pause-peer <name>        Pause syncing with a peer (preserves cursor + identity)
noema federation resume-peer <name>       Resume syncing with a paused peer
noema federation key fingerprint          Print the SHA-256 fingerprint of the active MCP shared key (safe to
                                          say aloud over an out-of-band channel to confirm a pairing)

noema keygen [--force]                    Generate this cortex's Ed25519 federation signing key so it can sign
                                          the events it emits (--force rotates it; peers must re-pin)

noema serve [--transport stdio|http] [--host <addr>] [--tls-cert <file> --tls-key <file>]
                                          Start the MCP server (http requires --host; endpoint is /mcp)
noema serve --print-config                Print a ready-to-use .mcp.json snippet and exit
noema serve ... --print-systemd-unit      Print a systemd service unit for the current serve flags
noema serve ... --print-launchd-plist     Print a launchd LaunchAgent plist for the current serve flags
noema tui [--theme auto|dark|light]       Open the interactive TUI
noema config get <key>                    Print a user-level setting (ui.theme, trash_days)
noema config set <key> <value>            Update and persist a user-level setting
noema config list                         List every known config key with its current value
noema completion [bash|zsh|fish|install]  Generate shell completions
noema version                             Print version, commit, and build date
```

**TUI theme priority** (highest wins):

1. `--theme` flag on `noema tui`
2. `NOEMA_THEME` environment variable
3. `ui.theme` in `~/.config/noema/config.yaml` (`noema config set ui.theme dark`)
4. `auto` — detected from the terminal's reported background color

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
| `append_trace` | Append content to an existing trace without reading it first (fire-and-forget logging) |
| `search_traces` | Search traces. `mode`: `lexical` (FTS5, default), `semantic` (embedding similarity), or `hybrid` (RRF fusion); semantic/hybrid need a configured `search:` block and fall back to lexical otherwise |
| `find_similar_traces` | Surface traces related to one you hold. Default ranks by BM25 vocabulary overlap; `mode=semantic`/`hybrid` ranks by embedding similarity to the source trace's vector |
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

**Log destination.** In stdio mode, operational logs (watcher events, federation sync status, startup messages) are written to `$XDG_STATE_HOME/noema/<cortex>.log` — defaulting to `~/.local/state/noema/<cortex>.log` — so MCP clients that inherit the spawning terminal's stderr (Claude Code, Copilot, Zed, Cursor, Aider, etc.) don't dump logs into your active terminal. The single `[serve] logs -> <path>` line printed at startup tells you exactly where to `tail -f`. Override the destination with `--log-file <path>`, or force logs back to stderr with `--log-stderr` for interactive triage.

### Streamable HTTP (remote clients, GitHub Copilot, federation peers)

Noema speaks the **Streamable HTTP** transport from the [MCP 2025-03-26 spec](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports#streamable-http) — a single endpoint at `/mcp` that handles JSON-RPC requests and optional SSE streaming on the same path. This is the transport native MCP clients (Zed, Claude Desktop's HTTP support, GitHub Copilot's MCP integration) speak today; the older two-endpoint legacy SSE transport has been removed.

```bash
# Local-only listener
noema serve --cortex my-cortex --transport http --host 127.0.0.1 --port 3000

# LAN-reachable listener (for federation peers)
noema serve --cortex my-cortex --transport http --host 10.0.0.5 --port 3000

# HTTPS
noema serve --cortex my-cortex --transport http --host 10.0.0.5 --port 3000 \
            --tls-cert /path/server.crt --tls-key /path/server.key
```

`--host` is **required** in HTTP mode and must be an explicit address — `0.0.0.0`/`::` are rejected to avoid accidentally exposing a Cortex on every interface. Pair `--tls-cert` with `--tls-key` to serve over HTTPS. The endpoint is `/mcp` (not configurable).

#### Connecting from Zed

Add the running endpoint to Zed's `settings.json`:

```json
{
  "context_servers": {
    "noema-my-cortex": {
      "url": "https://10.0.0.5:3000/mcp"
    }
  }
}
```

Any MCP client that supports Streamable HTTP works the same way — point its `url` field at `<scheme>://<host>:<port>/mcp`.

### Shared-key authentication

The HTTP endpoint can be gated behind a shared bearer key so only clients that know the secret can reach it. This is the recommended posture for any non-local deployment — federation peers, remote IDE clients, multi-host clusters. The HTTP endpoint runs in **open mode** by default, so existing deployments keep working until you opt in. Stdio is unaffected (stdio implies local-process trust).

**Two ways to configure a key** (in priority order):

1. **`NOEMA_MCP_KEY` environment variable** — the simplest form. Ideal for `systemd` with an `EnvironmentFile=` drop-in.
2. **`access.shared_key_file` in `cortex.md`** — a path (absolute or relative to the cortex directory) pointing at a sidecar file that contains the key on its first non-empty line. The file **must** be mode `0600`; Noema refuses to load a file that's group- or world-readable. Useful when you want the key to travel with the cortex directory rather than with the service environment.

If both are set, the env var wins and the server logs a warning so operators notice the override.

**TLS is required.** Keyed mode refuses to start over plaintext HTTP — a bearer token sent without TLS is stolen by the first adversary on the network path. Pair `--tls-cert` with `--tls-key`, or run in open mode.

**TLS cert lifecycle.** Cert paths can be configured once in `cortex.md` so you don't re-type them on every restart:

```yaml
access:
  shared_key_file: .access.secret
  tls_cert_path: /path/server.crt
  tls_key_path:  /path/server.key
```

CLI flags still win when both are present. With paths configured, `noema serve` parses the leaf cert at startup and:

- **Refuses to start on an expired or not-yet-valid cert.** Clients reject it anyway — better to fail loud than serve a cert nothing will accept. Pass `--insecure-allow-expired` to bypass briefly for in-place rotation; the override logs a loud warning.
- **Warns at startup if the cert expires within 7 days.**
- **Runs a background cert monitor** that re-checks every hour while serving and logs a `[cert-monitor]` line as the cert crosses 90 / 30 / 7 / expired bands. Lines fire on band transitions only, not every hour.

`noema verify cortex` reads the same manifest fields, so you can audit cert health without restarting the server.

**Sidecar-file example:**

```markdown
<!-- cortex.md -->
---
name: my-cortex
purpose: Primary memory
owner: mark
created: 2026-03-29
version: 2
access:
  shared_key_file: .access.secret
---
```

```bash
openssl rand -base64 32 > /path/to/my-cortex/.access.secret
chmod 600 /path/to/my-cortex/.access.secret

noema serve --cortex my-cortex --transport http --host 10.0.0.5 \
            --tls-cert /path/server.crt --tls-key /path/server.key
```

On startup the server logs the active posture:

```
[serve] access=keyed source=file fingerprint=SHA256:8e:76:62:80:f0:85:9c:05:...
```

**Verifying a pairing.** The fingerprint is a non-secret SHA-256 of the key, safe to say aloud over an out-of-band channel. Every host in a federation ring should produce the **same** fingerprint:

```bash
noema federation key fingerprint
```

If two hosts report different fingerprints, they have different keys and will 401 each other on federation sync. If a host reports `access=open` while its peers are keyed, it will be fully isolated.

MCP clients talking to a keyed endpoint must send `Authorization: Bearer <key>`. The `.mcp.json` snippet emitted by `noema serve --print-config` already uses `"Bearer ${NOEMA_MCP_KEY}"` — clients that support env interpolation (Claude Code) resolve it at runtime; clients that don't will produce a searchable 401.

### Running as a persistent service

For ad-hoc use, backgrounding with `nohup` works fine:

```bash
nohup noema serve --cortex mycortex --transport http --host 127.0.0.1 \
  > ~/noema.log 2>&1 &
disown
```

For a real federation host you probably want a process supervisor — restart on crash, start at boot, logs aggregated. Noema can print a ready-to-install unit/plist that mirrors the serve command you've already validated:

**Linux (systemd)**

```bash
noema serve --cortex mycortex --transport http --host 192.168.1.10 --print-systemd-unit | sudo tee /etc/systemd/system/noema-mycortex.service
sudo systemctl daemon-reload
sudo systemctl enable --now noema-mycortex
sudo journalctl -u noema-mycortex -f
```

**macOS (launchd)**

```bash
noema serve --cortex mycortex --transport http --host 127.0.0.1 --print-launchd-plist > ~/Library/LaunchAgents/com.fail-safe.noema.mycortex.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.fail-safe.noema.mycortex.plist
tail -f ~/Library/Logs/noema-mycortex.log
```

Both flags require `--transport http` (stdio has no endpoint to supervise) and an explicit `--cortex` (the unit/plist pins exactly one cortex — NOEMA_CORTEX and the config default aren't carried into the service environment). All the usual HTTP flag invariants (`--host` not `0.0.0.0`, TLS pair symmetry) are validated at preview time, so you catch misconfigurations before installing.

The emitted unit filename convention is `noema-<cortex>.service` / `com.fail-safe.noema.<cortex>.plist`, so running multiple cortexes on one host never collides.

**Keyed mode under a supervisor.** When `NOEMA_MCP_KEY` gates the endpoint, the unit/plist needs a way to reach the secret without embedding it in a world-readable file. The emitted templates don't inline keys — they leave you a seam:

- **systemd** — the unit already contains `EnvironmentFile=-%h/.config/noema/<cortex>.env` (the leading `-` makes the file optional, so open-mode installs keep working). Create the env file with `NOEMA_MCP_KEY=...`, mode `0600`, owned by the user the unit runs as, and `systemctl restart` picks it up.
- **launchd** — the plist includes an `EnvironmentVariables` dict with a commented `NOEMA_MCP_KEY` placeholder. Uncomment and fill it in, or prefer the `access.shared_key_file` sidecar path inside `cortex.md` so the secret travels with the cortex directory instead of the plist.

Either way you still need `--tls-cert`/`--tls-key` on the serve command that generated the template — the preview flag validates the TLS-for-keyed-mode rule before emitting, so you catch the footgun at install time. See [Shared-key authentication](#shared-key-authentication) above for the full rollout story.

### External edits as first-class mutations

Whenever `noema serve` is running on a cortex (under **either** `--transport stdio` or `--transport http`), a background watcher observes the `traces/`, `archive/traces/`, and `trash/traces/` directories and turns external filesystem changes into real mutation events. Edit a trace in Obsidian, drop a new `.md` into `traces/`, drag a file to the archive directory in Finder, or `rm` one from the terminal — Noema ingests the change, writes an event to the log, and (under HTTP) propagates it to federation peers. Deletes are restored to `trash/traces/` from the last event snapshot so `noema recover` still works.

```yaml
# cortex.md frontmatter fragment
watch:
  enabled: true      # default; set false to opt out
  debounce_ms: 300   # default; collapse editor save bursts
```

Loopback is prevented by content hash comparison: when the watcher sees a write, it hashes the body and compares to the DB's `content_hash` — matches are skipped, so Noema's own writes don't trigger duplicate events. Source-locked foreign traces are refused on external edit and logged (the watcher will not silently circumvent a publisher's authority). Malformed drop-in files (missing frontmatter, invalid id) are skipped with a warning.

The watcher runs under both transports because stdio sessions are not always the only mutator — the user may be editing in Obsidian while Claude Code holds a stdio connection, iCloud may deliver deltas from another device, or another noema process may be writing over HTTP on the same cortex. Federation propagation still only happens under `--transport http` (peers need a network endpoint), but external-edit events land in the local log and flow outward the next time an HTTP serve is running. Without any running `serve` process, external edits sit on disk safely, but stay invisible to the DB until `noema sync` (which, by design, emits no events — federation peers won't see those edits).

---

## Semantic search (optional)

Lexical FTS5 search is always on. An **opt-in semantic layer** adds embedding-based ranking — useful when a query matches by *concept* rather than shared words (e.g. "how do we authenticate agents" surfacing a trace titled "bearer-key posture for the MCP endpoint"). It stays true to Noema's lightweight, local-first posture: embeddings are stored as a SQLite `BLOB` and cosine similarity is computed in pure Go — no CGo, no vector extension, no external vector database. Embeddings are a **local index** (never federated), like the FTS5 index.

Enable it with a `search:` block in `cortex.md`:

```yaml
search:
  semantic_enabled: true
  embedding_model: nomic-embed-text          # required — no default
  embedding_endpoint: http://localhost:11434/v1   # OpenAI-compatible /embeddings;
                                             # inherits consolidation.local_llm_endpoint if unset
  # default_mode: hybrid    # lexical | semantic | hybrid (default lexical)
  # hybrid_weight: 0.5      # vector weight in hybrid fusion (0..1)
  # max_chars: 32000        # per-trace embed-text budget; lower for small-context models
```

Then build the index and search:

```sh
noema embeddings backfill            # embed existing traces (idempotent)
noema embeddings status              # coverage: embedded / stale / missing
noema search "concept query" --semantic
noema search "concept query" --hybrid   # fuse FTS5 + embeddings (reciprocal rank fusion)
noema similar <id> --semantic
```

Over MCP, `search_traces` and `find_similar_traces` take a `mode` arg (`lexical` | `semantic` | `hybrid`). If semantic search isn't configured or the embedding endpoint is unreachable, both **degrade to lexical results with a note** rather than erroring. Under `noema serve`, a background maintainer re-embeds new and edited traces on an interval, so the index stays fresh without a manual backfill.

---

## Federation

A Cortex can sync with peer Cortexes over Streamable HTTP: every mutation is recorded in an immutable event log, peers pull each other's events, and concurrent edits surface as **divergence traces** instead of silently overwriting. Federation is fully opt-in — a Cortex with no `federation` block in `cortex.md` runs exactly as before.

### Configure peers in `cortex.md`

```markdown
<!-- cortex.md -->
---
name: alpha
purpose: Primary research cortex
owner: mark
created: 2026-03-29
version: 2
federation:
  interval: 30s
  peers:
    - name: beta
      endpoint: http://192.168.1.10:3000
    - name: gamma
      endpoint: https://192.168.1.11:3000
      ca: /etc/noema/beta-ca.pem    # optional, for self-signed TLS
---
```

Or add a peer from the CLI:

```bash
noema federation add-peer beta http://192.168.1.10:3000
```

### Federation modes

The `federation.mode` field controls how a Cortex participates in the ring:

| Mode | Syncer runs? | `sync_events` serves? | HTTP write tools? |
|------|-------------|----------------------|-------------------|
| `sync` (default) | Yes | Yes | Yes |
| `publish` | No | Yes | Blocked |
| `subscribe` | Yes | Blocked | Yes |

**Publish mode** is for source-of-truth cortexes: a company knowledgebase, a curated dataset, a reference corpus. Content is managed locally via stdio; remote peers pull events via `sync_events` but cannot write back. **Subscribe mode** is the complement: pull everything, share nothing.

```yaml
# cortex.md frontmatter fragment
federation:
  mode: publish
  interval: 30s
  peers:
    - name: consumer-1
      endpoint: https://consumer-1.example:3000
    - name: consumer-2
      endpoint: https://consumer-2.example:3000
      mode: paused    # temporarily skip this peer
```

Individual peers can be paused without affecting the rest of the ring:

```bash
noema federation pause-peer consumer-2   # skip until resumed
noema federation resume-peer consumer-2  # re-enable
noema federation set-mode subscribe      # switch cortex mode
```

Changes take effect on the next `noema serve` restart.

### Consolidation coordination in a federation

When multiple peers have `consolidation.enabled`, `consolidation.llm_enabled`, and a reachable `local_llm_endpoint`, only one peer runs each consolidation cycle — without any additional configuration. Each peer advertises a random rank (1..99) on the `cortex_identity` heartbeat; the highest-ranked eligible peer wins (cortex ID breaks ties), runs the pass, and emits `consolidation_success`/`fail` events that replicate through the standard event log. `subscribe`-mode cortexes advertise rank 0 and can never win; `paused` peers naturally drop out via staleness. `federation_status` shows each peer's current rank.

### Mid → long graduation

The three-tier model (short → mid → long) completes with an automatic mid→long promoter. Alongside the short→mid heuristic pass, the scheduler evaluates every mid-tier trace older than 14 days against a simple AND-gate — minimum combined read count (default 3, summing deliberate `get_trace` reads with top-N `search_traces` / `find_similar_traces` hits), optional "unmodified since creation" stability requirement, and no active downvotes. Traces clearing every threshold graduate to long and are locked by the DB-level immutability trigger. Thresholds live under `consolidation.graduation` in cortex.md:

```yaml
consolidation:
  enabled: true
  llm_enabled: true
  auto_distillation_enabled: true     # run LLM distillation on every trigger
  local_llm_endpoint: http://localhost:11434/v1
  model_name: llama3.1:70b
  window_hours: 6
  graduation:
    enabled: true
    min_age_days: 14
    min_read_count: 3
    require_unmodified: true
```

Explicit curation is always available: `noema memory promote <id>` advances a trace one tier (short→mid or mid→long), and `noema memory demote <id>` steps mid→short. Long-term demotion goes through `noema memory purge` because undoing a base truth deserves the same ceremony as destroying it.

### Automatic LLM distillation

By default, `consolidation.llm_enabled: true` wires up the `noema consolidate` CLI but leaves the in-process agent on cheap heuristic-only work — you'd run the CLI from a separate system cron to get clusters distilled. Set `auto_distillation_enabled: true` to fold the LLM pipeline into every scheduled trigger instead, so each cron/idle/threshold fire runs **distillation → heuristic → graduation** in sequence on the elected peer.

Requires `llm_enabled`, `local_llm_endpoint`, and `model_name` to all be set — Noema refuses to start otherwise. If the LLM endpoint is unreachable at trigger time, distillation is logged and skipped; the chained heuristic + graduation passes still run so an offline LLM doesn't block cheap maintenance.

### Content hashing and source-locking

Every trace carries a `content_hash` (SHA-256 of the body, recomputed on every write). This enables integrity verification and federation sync optimization.

Publishers can mark traces as **source-locked** — immutable on the consumer side:

```yaml
---
id: 20260329-api-rate-limits
title: API Rate Limits
type: fact
origin: company-kb
source_hash: sha256:a3f2b8c...
source_locked: true
---
```

Source-locked traces refuse `update`, `delete`, and `remove` operations when the local cortex is not the origin. `archive`/`unarchive` remain allowed (non-destructive). Use `--force` on CLI commands to override in emergencies.

```bash
noema verify traces           # check trace content hashes against frontmatter
noema verify traces --backfill # populate content_hash for old traces
noema verify cortex           # validate manifest, config, db, access, federation
noema verify drift            # check federated traces against source hashes
noema edit <id> --force       # override source-lock
```

Locally, source-locking is enforced by refusing foreign-origin mutations. Across a federation, that guarantee is only as strong as the events that carry it — which is what **event signing** (below) protects.

### Federation event signing

Content hashing proves a body matches its hash; it does not prove *who* wrote the event. On a shared-key federation, any peer holding the key could otherwise forge events under another cortex's identity, overwrite source-locked traces, or rewrite `source_hash` so drift checks still pass. Event signing closes that gap by authenticating the originating cortex with an Ed25519 signature.

```bash
noema keygen                  # generate this cortex's Ed25519 signing key
noema keygen --force          # rotate the key (peers must re-pin)
```

Once a cortex has a key, it signs every event it emits. Peers learn its public key through the `cortex_identity` handshake and pin it per cortex id (trust-on-first-use); a later key change is refused until you run `noema federation reset-peer`. Verification of *incoming* events is staged so a mixed-version ring never hard-partitions, controlled by `federation.verify` in `cortex.md`:

| `federation.verify` | Behavior on a bad/unsigned event |
|---|---|
| `off` (default) | accept — no change for cortexes that haven't enabled signing |
| `warn` | accept, but log every unsigned/forged/unverifiable event |
| `enforce` | reject — only correctly-signed events from their owning cortex are replayed |

> **Signing only protects you under `enforce`.** Generating a key and signing emitted events changes nothing about what a cortex *accepts*: in the default `off` mode no signature is checked, and `warn` logs problems but still applies the event. Until every cortex you trust is set to `verify: enforce`, the forgery and source-lock-bypass risks above remain fully open — `off`/`warn` are a rollout on-ramp, not protection. Move to `enforce` once you have confirmed the peers it talks to are signing.

Under `enforce`, source-lock enforcement extends to replay: a locked trace can only be mutated by the cortex that owns it.

Trust-on-first-use has a first-contact window. For a high-assurance peer you can skip it by hard-pinning the peer's key out-of-band — add `pubkey: ed25519:<base64>` to that peer's entry under `federation.peers` in `cortex.md`. The peer must then advertise exactly that key at the handshake or the sync is refused (overriding TOFU); to rotate, edit the pinned value.

**Rotating a key.** `noema keygen --force` retires the old key and signs future events with the new one. Peers that pinned the old key refuse the rotated cortex until they re-pin. For a peer that was already caught up, recover with `noema federation reset-peer <name> --key-rotated`: it drops only the pinned key (re-pinned on the next handshake) while keeping the cursor, so the peer pulls only post-rotation events. **Limitation under `enforce`:** a *from-scratch* resync of a rotated cortex — a brand-new peer, or a full `reset-peer` — cannot replay that cortex's **pre-rotation** events, because they were signed with the now-retired key and there is no key history. The peer pins the new key and rejects the older events. Rotate only when forfeiting verifiable replay of pre-rotation history to peers that resync from zero is acceptable; existing caught-up peers recovered with `--key-rotated` are unaffected.

### Authentication

Federation peers share a single bearer key — the same `NOEMA_MCP_KEY` / `access.shared_key_file` described in [Shared-key authentication](#shared-key-authentication) above. When the syncer polls a peer's `sync_events` tool, it automatically attaches `Authorization: Bearer <key>` from the local host's active key; nothing peer-specific lives in `cortex.md`. This means:

- Every host in a federation ring must produce the **same** fingerprint. Verify on each box with `noema federation key fingerprint` and compare out-of-band.
- If one host rotates its key without the others, the rotated host will 401 its peers on the next sync tick. Federation status on both sides reports the failure, and the syncer falls back to exponential backoff (`2m → 4m → 8m`) until the mismatch is resolved.
- A host running in open mode while its peers are keyed — or vice versa — is effectively isolated: keyed peers reject its unauthenticated requests, and it rejects theirs. Mixed-mode rings aren't supported; roll the whole ring in one window.

Because the bearer key is required for every MCP call on a keyed endpoint, the same key also gates any human or tooling that wants to call `federation_status`, `sync_events`, or any other MCP tool over HTTP — there is no federation-only carve-out.

### How sync works

When `noema serve --transport http` starts and `cortex.md` has peers, a background syncer polls each peer's `sync_events` MCP tool (over the `/mcp` Streamable HTTP endpoint) on the configured `interval` (default 30s). New events are replayed locally — files are written, the DB is updated, and the event is stored in the local log with its original ID and origin (no event amplification).

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

**Trace IDs and titles.** The ID is `YYYYMMDD-<slug>` — today's date prepended to a slugified title. The slug portion is capped at 100 characters (longer titles are silently truncated); aim for titles under 80 characters for scannability. Don't include the date in the title — `noema` prepends today's date automatically, and leading `YYYYMMDD-` / `YYYY-MM-DD-` prefixes in the title are stripped to prevent doubled-date IDs. Mid-title date fragments aren't stripped, so put date context in a tag (`tags: [event-2026-04-02]`) or the body instead of the title.

**Tag conventions.** The cortex accepts any string as a tag — the constraints here are about what *renders correctly in Obsidian's tag panel* if a user is authoring through Obsidian (a common but optional surface). Non-Obsidian authoring (CLI, MCP, TUI, agent integrations) is unaffected.

- **Tags must contain at least one letter.** A pure-numeric tag like `2026` or `42` is stored, indexed, and searchable via `search_traces` / FTS5, but Obsidian silently drops it from the tag panel. Use `y2026`, `2026q1`, or `event-2026-04-02` instead.
- **Avoid dots inside a tag.** Obsidian interprets `release.candidate` as a nested tag tree (`release` / `candidate`), which is rarely the intent for a flat tag. Use hyphens: `release-candidate`.
- **No spaces.** Use hyphens or underscores: `network-troubleshooting`, not `network troubleshooting`.

The Obsidian plugin's "New trace" command surfaces these as inline warnings as you type — non-blocking hints, not validation errors. The cortex still accepts the tag.

**Cortex layout on disk:**

```
my-cortex/
  AGENTS.md           ← agent guide (generated by noema init, see below)
  cortex.md           ← manifest: YAML frontmatter + optional prose body
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

Read and write markdown files directly. `AGENTS.md` at the Cortex root (generated by `noema init` — this is the [agents.md](https://agents.md/) convention picked up by Codex, Cursor, Aider, Zed, Copilot, and most other coding-agent tooling) explains the file format, directory layout, Trace types, and naming conventions for agents that arrive with only file access. After making changes, run `noema sync` when the binary next becomes available to reconcile the database.

---

Each agent identifies itself via the `author` field when creating Traces. Filter by `author` to read only a specific agent's prior work. Because Traces are plain markdown files, a human can inspect, edit, or audit the Cortex at any time without any special tooling.

---

## Hermes Integration

Noema ships a [Hermes](https://hermes-agent.nousresearch.com/) memory provider plugin in `plugins/hermes/`. It implements the Hermes `MemoryProvider` ABC so any Hermes agent can use a Noema Cortex as its memory backend — structured traces with types, tags, lineage, and full-text search, all through the standard Hermes lifecycle hooks.

**Quick setup:**

```bash
# Download the plugin from the latest release
curl -LO https://github.com/Fail-Safe/Noema/releases/latest/download/noema-hermes-plugin.tar.gz
mkdir -p <hermes-install>/plugins/memory/noema
tar -xzf noema-hermes-plugin.tar.gz -C <hermes-install>/plugins/memory/noema/

# Configure
hermes memory setup    # select "noema", provide your cortex name
```

The plugin exposes six tools to the agent (`noema_search`, `noema_remember`, `noema_recall`, `noema_list`, `noema_update`, `noema_lineage`), manages a session log trace across turns, creates a summary trace at session end, and mirrors Hermes built-in memory writes as Noema traces.

By default the plugin spawns `noema serve --transport stdio` as a subprocess — no network config needed. For remote cortexes or multi-agent setups, set `transport=http` to connect to an already-running HTTP endpoint.

See [`plugins/hermes/README.md`](plugins/hermes/README.md) for full configuration and usage details.

---

## License

MIT — see [LICENSE](LICENSE).
