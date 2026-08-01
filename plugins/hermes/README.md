# Noema Memory Provider for Hermes

A [Hermes](https://hermes-agent.nousresearch.com/) memory provider plugin that
gives any Hermes agent structured, persistent memory backed by a
[Noema](https://github.com/Fail-Safe/Noema) Cortex.

Every memory is a plain markdown file (a "Trace") with typed frontmatter —
searchable via FTS5, linked through derivation lineage, and federable across
Cortexes.

## Installation

1. Install the Noema binary:

   ```
   brew install Fail-Safe/tap/noema
   ```

2. Create a Cortex if you don't already have one:

   ```
   noema init --name my-cortex
   ```

3. Install the plugin files embedded in the Noema binary:

   ```
   noema plugin hermes install --hermes-home <hermes-install>
   ```

   This is local and network-free. Matching files are skipped; changed managed
   files are preserved unless you explicitly pass `--force`. To check for
   drift without changing anything:

   ```
   noema plugin hermes status --check --hermes-home <hermes-install>
   ```

   The matching release's `noema-hermes-plugin.tar.gz` remains available for
   manual/offline installation.

4. Run the Hermes memory setup wizard:

   ```
   hermes memory setup
   ```

   Select **noema** when prompted and provide your cortex name.

## Configuration

| Key | Required | Secret | Env var | Default | Description |
|-----|----------|--------|---------|---------|-------------|
| `cortex_name` | Yes | No | `NOEMA_CORTEX` | — | Noema cortex to use |
| `noema_binary` | No | No | `NOEMA_BINARY` | auto-detect | Path to noema binary |
| `transport` | No | No | — | `stdio` | `stdio` or `http` |
| `http_url` | No | No | `NOEMA_HTTP_URL` | — | MCP endpoint URL (HTTP transport only) |
| `bearer_key` | No | Yes | `NOEMA_MCP_KEY` | — | Bearer key for keyed mode |

Config is saved to `{hermes_home}/noema.json`.

### Transport modes

**stdio** (default) — the plugin spawns `noema serve --transport stdio` as a
subprocess. No network configuration needed. The process lifecycle is fully
managed: started on `initialize()`, terminated on `shutdown()`.

**http** — the plugin connects to an already-running `noema serve --transport http`
endpoint. Use this for remote Cortexes or multi-agent setups sharing one server.
Set `http_url` to the server's base URL (e.g., `http://localhost:3000`).

## Agent tools

Six Noema tools are exposed to the Hermes agent:

| Hermes tool | Noema tool | Purpose |
|-------------|------------|---------|
| `noema_search` | `search_traces` | Full-text search across traces |
| `noema_remember` | `create_trace` | Create a new memory trace |
| `noema_recall` | `get_trace` | Read a specific trace by ID |
| `noema_list` | `list_traces` | Browse/filter by type, tag, author |
| `noema_update` | `update_trace` | Modify an existing trace |
| `noema_lineage` | `trace_lineage` | Follow derivation chains |

## Session lifecycle

The plugin automatically manages session state:

- **On initialize** — creates a session log trace (`type: context`) and caches
  the Cortex instructions for system prompt injection.
- **Each turn** — appends the user/assistant exchange to the session log
  (non-blocking, runs in a background thread).
- **On context compression** — appends the compressed context to the session log
  and returns a breadcrumb pointing back to the trace.
- **On session end** — creates a summary trace (`type: observation`) derived
  from the session log, then archives the session log.

## Prefetch

On each turn, `prefetch()` searches the Cortex with the user's message and
returns the top 5 matching traces (excluding verbose session logs). FTS5 on
local SQLite is sub-millisecond, so no cache warming is needed.

## Memory mirroring

When Hermes writes to its built-in memory via `on_memory_write()`, the plugin
mirrors the operation as a Noema trace tagged `hermes-mirror`:

- `add` creates a new trace (`type: note`)
- `update` updates the matching mirrored trace
- `delete` archives the mirrored trace (never hard-deletes)

## Requirements

- Python 3.9+
- Noema binary (v0.6.0+) installed and accessible
- A Noema Cortex initialized with `noema init`

## License

MIT — same as Noema.
