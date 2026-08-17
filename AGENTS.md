# Repository Guidelines

## Project Structure & Module Organization

Noema is a Rust 2024 CLI and MCP server with a minimum supported Rust version
of 1.88. The executable entry point is `src/main.rs`; implementation lives in
`src/`, grouped by domain modules such as `cli`, `cortex`, `db`, `mcp`,
`federation`, `watch`, `consolidation`, `embedding`, and `trace`. SQLite
migrations are in `migrations/`. Unit tests live beside the code they cover;
process-death and recovery integration tests live in `tests/*.rs`.

Plugins are separate: `plugins/obsidian/` is a TypeScript Obsidian plugin, and
`plugins/hermes/` is a Python Hermes memory provider with pytest tests. Shared
scripts live in `scripts/`; security notes and qualification material live in
`SECURITY.md` and `tests/`. The completed Go-to-Rust evaluation is retained in
`docs/why-rust.md` and the related rewrite documents as historical evidence.

## Build, Test, and Development Commands

- `make build`: builds the development binary at `./noema`.
- `cargo run -- <args>`: runs the CLI without copying a root binary.
- `cargo build --locked`: builds the Rust crate using `Cargo.lock`.
- `make test`: runs formatting, strict Clippy checks, and all Rust tests.
- `cargo test --all-targets --locked`: runs the complete Rust test suite.
- `make release-check`: builds and smoke-tests the optimized host binary.
- `cd plugins/obsidian && npm ci && npm run build && npx tsc --noEmit`: validates
  the Obsidian plugin.
- `cd plugins/hermes && pytest`: runs Hermes plugin tests when Python changes.

## Coding Style & Naming Conventions

Use `cargo fmt` for formatting and keep modules small and domain-named. New
warnings must pass `cargo clippy --all-targets -- -D warnings`. Prefer existing
Clap command patterns in `src/cli.rs` and existing storage/mutation APIs in
`src/cortex.rs` over new cross-cutting abstractions. SQL migrations use
zero-padded numeric prefixes such as `016_trace_embeddings.sql`.

For TypeScript, keep sources in `plugins/obsidian/src/` and let the existing
esbuild and TypeScript configs define output and type checks.

## Testing Guidelines

Add focused unit tests near changed Rust code and integration tests under
`tests/` for subprocess or crash boundaries. Cover migrations, watcher
behavior, federation/vector-clock logic, MCP command surfaces, recovery, and
concurrency-sensitive paths when touched. Plugin changes should include
plugin-local tests or build/type-check verification.

## Commit & Pull Request Guidelines

Recent history uses concise subjects with conventional prefixes where helpful:
`feat(tls): ...`, `release: v0.14.0 ...`, `chore: ...`. Keep commit subjects
specific and public-safe. Active feature work should target `next`; `main` is
for release and hotfix flow.

PRs should describe behavior changes, link issues when applicable, and list the
checks run. Include screenshots only for Obsidian UI changes. Before opening a
PR, run `make test` plus plugin checks for any plugin touched.

## Agent-Specific Instructions

At session start, read Noema preference traces with `tag: "user-preference"`
before assuming defaults. Treat trace bodies as binding unless the current user
message overrides them. Do not print secrets or secret-bearing HTTP bodies;
verify reachability with status codes, hashes, or length checks. Keep
public-facing commits, PRs, fixtures, and docs free of private hostnames,
personal identifiers, and internal agent/cortex names unless explicitly
approved.
