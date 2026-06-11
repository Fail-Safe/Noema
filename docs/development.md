# Development Guide

This guide covers local development and release-facing workflow. For the
system model, see [Architecture](architecture.md).

## Toolchain

Noema is a Go 1.26 module. The main binary is built from `./cmd/noema`; most
implementation code is under `internal/`. The project intentionally uses a
small dependency set:

- SQLite through `modernc.org/sqlite`, so normal release builds can be static
  with `CGO_ENABLED=0`.
- Cobra for CLI commands in `internal/cli/`.
- Bubble Tea and related Charm libraries for TUI code.
- SQLite FTS5 for default lexical search.

The Obsidian plugin in `plugins/obsidian/` uses Node 20, TypeScript, and
esbuild. The Hermes plugin in `plugins/hermes/` is Python and talks to Noema
through MCP; it does not import Go code.

## Common Commands

```sh
make build
go run ./cmd/noema
go build ./...
go test ./...
go test -race ./...
go vet ./...
```

`make build` writes a development binary to `./noema` and injects
`internal/cli.Version` from `git describe --tags --always --dirty`.

Release-style local builds:

```sh
make release
make release-linux
```

Release targets use `CGO_ENABLED=0`, `-trimpath`, and stripped linker flags,
then write artifacts to `dist/` with OS/architecture suffixes.

## Plugin Checks

Validate the Obsidian plugin when changing `plugins/obsidian/`:

```sh
cd plugins/obsidian
npm ci
npm run build
npx tsc --noEmit
```

Validate the Hermes plugin when changing `plugins/hermes/`:

```sh
cd plugins/hermes
pytest
```

## Testing Expectations

Add focused `*_test.go` files beside changed Go code. Migration, watcher,
federation, MCP, and CLI behavior should be covered at the package level. Use
`go test -race ./...` for changes involving background workers, HTTP serving,
federation sync, SQLite concurrency, or filesystem watching.

CI runs `go mod tidy` and fails if `go.mod` or `go.sum` would change, then
runs `go build ./...`, `go vet ./...`, Staticcheck, `go test -race ./...`, and
the Obsidian plugin build/typecheck.

## Branches, Commits, And PRs

`main` is the stable release branch. Active feature and bug branches should
target `next`; release PRs move `next` to `main`.

Commit subjects are concise and often use conventional prefixes, for example
`feat(tls): refuse serve on expired certs`, `release: v0.14.0 ...`, or
`chore: normalize fixture names`. Keep subjects and PR text public-safe.

PRs should describe behavior changes, link issues when relevant, and list the
checks run. Include screenshots only for UI changes, especially Obsidian plugin
work.

## Release Notes

Release automation runs from `v*` tags through GoReleaser. For a local dry run:

```sh
goreleaser release --snapshot --clean --skip=publish
```

The release workflow builds cross-platform archives, checksums, and Homebrew
formula/cask updates. Regular pushes to `main` or `next` do not publish
release artifacts.
