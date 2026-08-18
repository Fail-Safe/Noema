# Development Guide

This guide covers local development and release-facing workflow. For the
system model, see [Architecture](architecture.md).

## Toolchain

Noema is a Rust 2024 crate with a minimum supported Rust version of 1.88. The
binary starts in `src/main.rs`, and domain code lives in sibling modules under
`src/`. Its main building blocks are:

- Rusqlite with bundled SQLite and FTS5 for local indexing and search.
- Clap for the CLI.
- Tokio, Axum, and rustls-backed clients/servers for MCP and federation.
- Ratatui and Crossterm for the terminal UI.

The Obsidian plugin in `plugins/obsidian/` uses Node 20, TypeScript, and
esbuild. The Hermes plugin in `plugins/hermes/` is Python and communicates with
Noema through MCP; it does not import Rust code.

## Common Commands

```sh
make build
cargo run -- version
cargo build --locked
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test --all-targets --locked
```

`make build` writes a development binary to `./noema`. Cargo derives the
reported version from `Cargo.toml`.

Release-style local build and smoke test:

```sh
make release-check
```

The release profile enables thin LTO, uses one codegen unit, strips symbols,
and writes the host artifact to `dist/noema-<os>-<arch>`.

## Plugin Checks

Validate the Obsidian plugin when changing `plugins/obsidian/`:

```sh
cd plugins/obsidian
npm ci
npm run build
npx tsc --noEmit
git diff --exit-code -- main.js
```

`main.js` is generated and committed because the Noema binary embeds it. The
final check ensures the tracked runtime bundle matches the TypeScript source
while keeping ordinary Rust builds independent of Node.

Validate the Hermes plugin when changing `plugins/hermes/`:

```sh
cd plugins/hermes
pytest
```

## Testing Expectations

Add focused Rust unit tests beside changed modules. Use integration tests under
`tests/` when behavior crosses process, filesystem, SQLite, or crash-recovery
boundaries. Migration, watcher, federation, MCP, CLI, durability, and recovery
behavior should be covered when touched.

CI runs formatting, strict Clippy, the full Rust suite, an optimized build and
version smoke test, plus Hermes, Obsidian, release-metadata, and repository
script checks.

## Branches, Commits, And PRs

`main` is the stable release branch. Active feature and bug branches should
target `next`; release PRs move `next` to `main`.

Commit subjects are concise and often use conventional prefixes, for example
`feat(tls): refuse serve on expired certs`, `release: v0.20.0 ...`, or
`chore: normalize fixture names`. Keep subjects and PR text public-safe.

PRs should describe behavior changes, link issues when relevant, and list the
checks run. Include screenshots only for UI changes, especially Obsidian plugin
work.

## Releases

Release automation runs from `v*` tags. The tag version must exactly match the
package version in `Cargo.toml`; a mismatch fails before publication.

The workflow builds natively for macOS, Linux, and Windows on x64 and arm64,
smoke-tests every binary, creates archives and checksums, bundles both plugins,
publishes the GitHub release, and updates stable or prerelease Homebrew metadata.
Regular pushes to `main` or `next` do not publish release artifacts.

To qualify the host artifact locally without publishing:

```sh
make test
make release-check
```
