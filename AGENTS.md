# Repository Guidelines

## Project Structure & Module Organization

Noema is a Go 1.26 CLI/MCP server. The executable entry point is
`cmd/noema/main.go`; implementation lives under `internal/`, grouped by domain:
`cli`, `cortex`, `db`, `mcp`, `federation`, `watch`, `consolidation`, `embed`,
`trace`, and related helpers. SQLite migrations are in `internal/db/migrations/`.
Tests generally sit next to the code they cover as `*_test.go`. See
[docs/architecture.md](docs/architecture.md) for the system model and
[docs/development.md](docs/development.md) for expanded workflow notes.

Plugins are separate: `plugins/obsidian/` is a TypeScript Obsidian plugin, and
`plugins/hermes/` is a Python Hermes memory provider with pytest tests. Shared
scripts live in `scripts/`; security notes and playbooks live in `SECURITY.md`
and `tests/`.

## Build, Test, and Development Commands

- `make build`: builds the local development binary at `./noema` with version
  metadata.
- `go run ./cmd/noema`: runs the CLI without keeping a binary.
- `go build ./...`: builds all Go packages; this is part of CI.
- `make test` or `go test ./...`: runs the Go test suite.
- `go test -race ./...`: runs tests with race detection, matching CI’s stronger
  check.
- `make vet` or `go vet ./...`: runs Go vet.
- `cd plugins/obsidian && npm ci && npm run build && npx tsc --noEmit`: validates
  the Obsidian plugin.
- `cd plugins/hermes && pytest`: runs Hermes plugin tests when Python changes.

## Coding Style & Naming Conventions

Use `gofmt`/`go fmt ./...` for Go formatting and keep packages small and
domain-named. Exported Go identifiers need clear names and documentation when
they form public package surface. Prefer existing Cobra command patterns in
`internal/cli/` and existing storage/mutation APIs in `internal/cortex/` over new
cross-cutting abstractions. SQL migrations use zero-padded numeric prefixes such
as `016_trace_embeddings.sql`.

For TypeScript, keep sources in `plugins/obsidian/src/` and let the existing
esbuild and TypeScript configs define output and type checks.

## Testing Guidelines

Add focused `*_test.go` files beside changed Go code. Cover migrations,
watcher behavior, federation/vector-clock logic, MCP command surfaces, and
concurrency-sensitive paths when touched. Use race tests for background workers,
syncers, and server code. Plugin changes should include plugin-local tests or
build/type-check verification.

## Commit & Pull Request Guidelines

Recent history uses concise subjects with conventional prefixes where helpful:
`feat(tls): ...`, `release: v0.14.0 ...`, `chore: ...`. Keep commit subjects
specific and public-safe. Active feature work should target `next`; `main` is
for release and hotfix flow.

PRs should describe behavior changes, link issues when applicable, and list the
checks run. Include screenshots only for Obsidian UI changes. Before opening a
PR, run Go build/vet/tests plus plugin checks for any plugin touched.

## Agent-Specific Instructions

At session start, read Noema preference traces with `tag: "user-preference"`
before assuming defaults. Treat trace bodies as binding unless the current user
message overrides them. Do not print secrets or secret-bearing HTTP bodies;
verify reachability with status codes, hashes, or length checks. Keep
public-facing commits, PRs, fixtures, and docs free of private hostnames,
personal identifiers, and internal agent/cortex names unless explicitly approved.
