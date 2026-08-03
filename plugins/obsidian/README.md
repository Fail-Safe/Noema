# Noema — Obsidian plugin

Lineage view and tier visibility for [Noema](https://github.com/Fail-Safe/Noema) cortex traces, surfaced inside Obsidian.

## What it does

- **Lineage sidebar.** When you open a trace, the sidebar shows its `derived_from` ancestors and the traces derived from it, both clickable. Useful for navigating "where did this come from / what came out of this" without leaving the editor.
- **Tier badge in the status bar.** Shows `[s]` / `[m]` / `[L]` for the currently-open trace and a tooltip note that long-tier traces are immutable.
- **Connection status.** The same status bar item shows whether the plugin is connected to a `noema serve --transport http` endpoint. A keyed-mode server that rejects (or requires) the bearer key shows `noema: unauthorized` instead of `noema: disconnected`, and pops a one-time notice pointing you at the bearer-key setting — so a wrong key reads as a credential problem, not an unreachable server.
- **Noema-backed trace search.** `Noema: Search traces` calls the connected cortex's `search_traces` MCP tool and opens the selected trace in Obsidian. The plugin setting chooses `hybrid`, `semantic`, or `lexical`; the modal can show 5 or 10 results. Server-side `cortex.md` still owns embedding configuration and `hybrid_weight`.

That's intentionally the whole feature set for v0.3. File-explorer decorations and federation status panels are reasonable next-version additions but aren't here yet.

## Setup

1. Open the cortex directory (the one with `cortex.md`) as an Obsidian vault.
2. Run `noema serve --transport http` somewhere that Obsidian can reach. For a local-only setup, that can be the same machine: `noema serve --transport http --host 127.0.0.1 --port 3000`. For multi-host, point at any peer's HTTPS endpoint.
3. Install the plugin files embedded in the Noema binary:

   ```sh
   noema plugin obsidian install --vault <vault>
   ```

   This is local and network-free. Matching files are skipped; changed managed
   files are preserved unless you explicitly pass `--force`. Use
   `noema plugin obsidian status --check --vault <vault>` for a read-only drift
   check. The matching release's `noema-obsidian-plugin.tar.gz` remains the
   manual/offline fallback.
4. Enable the plugin in Obsidian's Community Plugins settings.
5. Open the plugin's settings tab and set:
   - **HTTP endpoint** — e.g. `https://noema.local:3000`
   - **Bearer key** — required if the server is in keyed mode (`NOEMA_MCP_KEY` or `access.shared_key_file`); leave empty for open-mode (loopback only).
   - **Test connection** — click to probe the endpoint immediately and get a notice telling you whether it connected, was rejected (HTTP 401, fix the key), or was unreachable.
   - **Noema search mode** — used by `Noema: Search traces`; defaults to `Hybrid`.
6. Open the lineage sidebar via the command palette: `Noema: Open lineage view`.

The status bar will show `noema: <cortex-name>` once the connection succeeds.

## Tag conventions

The cortex accepts any string as a tag — these constraints are about what Obsidian's tag panel surfaces:

- **Tags must contain at least one letter.** Pure-numeric tags like `2026` or `42` are stored on the trace and remain searchable through Noema's MCP / FTS5 surfaces, but Obsidian silently drops them from its tag panel. Use `y2026`, `2026q1`, or `event-2026-04-02`.
- **Avoid dots in a tag.** Obsidian interprets `release.candidate` as a nested tag tree (`release` / `candidate`). Use hyphens: `release-candidate`.
- **No spaces.** Use hyphens or underscores: `network-troubleshooting`, not `network troubleshooting`.

The "New trace" command shows inline warnings as you type a non-conformant tag. Submission proceeds normally — these are hints, not errors. Authoring through CLI / MCP / TUI is unaffected.

## Building

```sh
cd plugins/obsidian
npm install
npm run build
```

Produces `main.js` next to `manifest.json`. For development, `npm run dev` watches and rebuilds on save.

## Why so small

Noema is intentionally lightweight infrastructure — markdown files plus a SQLite index, no opinion about your editor. This plugin matches that spirit: it adds the pieces of UI that genuinely benefit from being inside Obsidian (lineage navigation, tier visibility, trace creation, appends, and Noema-ranked trace search) and stays out of the way for everything else. Editing happens in Obsidian's native editor and file management uses Obsidian's native file explorer.

If you want richer integration (file-explorer decorations, saved search views, federation status panel), file an issue against the main repo with the use case — they're easy to add as separate, opt-in commands.
