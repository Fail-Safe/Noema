# Noema — Obsidian plugin

Lineage view and tier visibility for [Noema](https://github.com/Fail-Safe/Noema) cortex traces, surfaced inside Obsidian.

## What it does

- **Lineage sidebar.** When you open a trace, the sidebar shows its `derived_from` ancestors and the traces derived from it, both clickable. Useful for navigating "where did this come from / what came out of this" without leaving the editor.
- **Tier badge in the status bar.** Shows `[s]` / `[m]` / `[L]` for the currently-open trace and a tooltip note that long-tier traces are immutable.
- **Connection status.** The same status bar item shows whether the plugin is connected to a `noema serve --transport http` endpoint.

That's intentionally the whole feature set for v0.1. File-explorer decorations, trace creation UI, and FTS5-backed search are reasonable next-version additions but aren't here yet.

## Setup

1. Open the cortex directory (the one with `cortex.md`) as an Obsidian vault.
2. Run `noema serve --transport http` somewhere that Obsidian can reach. For a local-only setup, that can be the same machine: `noema serve --transport http --host 127.0.0.1 --port 3000`. For multi-host, point at any peer's HTTPS endpoint.
3. Drop this plugin's `main.js`, `manifest.json`, and `styles.css` into `<vault>/.obsidian/plugins/noema/`.
4. Enable the plugin in Obsidian's Community Plugins settings.
5. Open the plugin's settings tab and set:
   - **HTTP endpoint** — e.g. `https://noema.local:3000`
   - **Bearer key** — required if the server is in keyed mode (`NOEMA_MCP_KEY` or `access.shared_key_file`); leave empty for open-mode (loopback only).
6. Open the lineage sidebar via the command palette: `Noema: Open lineage view`.

The status bar will show `noema: <cortex-name>` once the connection succeeds.

## Building

```sh
cd plugins/obsidian
npm install
npm run build
```

Produces `main.js` next to `manifest.json`. For development, `npm run dev` watches and rebuilds on save.

## Why so small

Noema is intentionally lightweight infrastructure — markdown files plus a SQLite index, no opinion about your editor. This plugin matches that spirit: it adds the two pieces of UI that genuinely benefit from being inside Obsidian (lineage navigation and tier visibility) and stays out of the way for everything else. Editing happens in Obsidian's native editor, search uses Obsidian's native search, file management uses Obsidian's native file explorer.

If you want richer integration (FTS5-backed search, trace creation modals, federation status panel), file an issue against the main repo with the use case — they're easy to add as separate, opt-in commands.
