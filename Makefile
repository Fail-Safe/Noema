# Noema build targets.
#
# Dev builds live at the repo root as ./noema (matching the convention in
# AGENTS.md and .gitignore). Release builds land in ./dist/ with an
# explicit <os>-<arch> suffix so cross-compiled artifacts are self-
# identifying when scp'd onto a peer host.
#
# The version string is injected into internal/cli.Version at link time
# from `git describe --tags --always --dirty`. A build from a clean tag
# emits e.g. "v0.3.0"; a build with uncommitted changes emits
# "v0.3.0-5-gabcdef-dirty" so --version output makes it obvious when a
# deployed binary doesn't match any commit in the repo.

PKG         := ./cmd/noema
BIN         := noema
DIST_DIR    := dist
VERSION_PKG := github.com/Fail-Safe/Noema/internal/cli

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Dev builds keep the symbol table and DWARF info so stack traces,
# delve, and pprof all work out of the box. Release builds strip both
# (-s -w) for ~25% size reduction and run with -trimpath to drop
# absolute source paths from panics and the binary's metadata.
LDFLAGS_DEV     := -X $(VERSION_PKG).Version=$(VERSION)
LDFLAGS_RELEASE := -s -w -X $(VERSION_PKG).Version=$(VERSION)

HOST_OS   := $(shell go env GOOS)
HOST_ARCH := $(shell go env GOARCH)

.PHONY: help build release release-linux test vet obsidian-publish clean \
	rust-build rust-release rust-test comparison-release compare-rust benchmark-rust \
	benchmark-mcp-rust

help:
	@echo "Noema build targets:"
	@echo ""
	@echo "  make build           Dev build with debug info      -> ./$(BIN)"
	@echo "  make release         Stripped build for this host   -> $(DIST_DIR)/$(BIN)-$(HOST_OS)-$(HOST_ARCH)"
	@echo "  make release-linux   Stripped build for linux/amd64 -> $(DIST_DIR)/$(BIN)-linux-amd64"
	@echo "  make test            go test ./..."
	@echo "  make vet             go vet ./..."
	@echo "  make rust-build      Build the Rust comparison binary"
	@echo "  make rust-test       Run Rust formatting, lint, and tests"
	@echo "  make compare-rust    Run Go/Rust differential compatibility tests"
	@echo "  make benchmark-rust  Benchmark both release binaries"
	@echo "  make benchmark-mcp-rust"
	@echo "                       Benchmark steady-state MCP request handling"
	@echo "  make obsidian-publish"
	@echo "                       Build and copy Obsidian plugin into the active cortex vault"
	@echo "  make clean           Remove ./$(BIN) and ./$(DIST_DIR)/"
	@echo ""
	@echo "Version string for the next build: $(VERSION)"

build:
	go build -ldflags "$(LDFLAGS_DEV)" -o $(BIN) $(PKG)

# CGO_ENABLED=0 is set on both release targets even for the host build.
# Noema's only native-code dependency is modernc.org/sqlite (pure-Go
# translation of C SQLite), so disabling CGo produces a fully static
# binary with no surprise libc linkage on any host.
release: | $(DIST_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS_RELEASE)" \
		-o $(DIST_DIR)/$(BIN)-$(HOST_OS)-$(HOST_ARCH) $(PKG)
	@ls -lh $(DIST_DIR)/$(BIN)-$(HOST_OS)-$(HOST_ARCH)

release-linux: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags "$(LDFLAGS_RELEASE)" \
		-o $(DIST_DIR)/$(BIN)-linux-amd64 $(PKG)
	@ls -lh $(DIST_DIR)/$(BIN)-linux-amd64

$(DIST_DIR):
	@mkdir -p $(DIST_DIR)

test:
	go test ./...

vet:
	go vet ./...

rust-build:
	cargo build --manifest-path rust/Cargo.toml

rust-release:
	cargo build --release --manifest-path rust/Cargo.toml

rust-test:
	cargo fmt --manifest-path rust/Cargo.toml --check
	cargo clippy --manifest-path rust/Cargo.toml --all-targets -- -D warnings
	cargo test --manifest-path rust/Cargo.toml

comparison-release: release rust-release

compare-rust: build rust-build
	NOEMA_GO_BIN="$(CURDIR)/noema" \
	NOEMA_RUST_BIN="$(CURDIR)/rust/target/debug/noema-rs" \
	./tests/rust-rewrite/compare.sh
	NOEMA_GO_BIN="$(CURDIR)/noema" \
	NOEMA_RUST_BIN="$(CURDIR)/rust/target/debug/noema-rs" \
	./tests/rust-rewrite/http_smoke.sh
	python3 ./tests/rust-rewrite/federation_network.py \
		--go "$(CURDIR)/noema" \
		--rust "$(CURDIR)/rust/target/debug/noema-rs"
	python3 ./tests/rust-rewrite/federation_ring.py \
		--go "$(CURDIR)/noema" \
		--rust "$(CURDIR)/rust/target/debug/noema-rs"
	python3 ./tests/rust-rewrite/federation_tls.py \
		--go "$(CURDIR)/noema" \
		--rust "$(CURDIR)/rust/target/debug/noema-rs"
	python3 ./tests/rust-rewrite/consolidation_election.py \
		--go "$(CURDIR)/noema" \
		--rust "$(CURDIR)/rust/target/debug/noema-rs"
	python3 ./tests/rust-rewrite/heuristic_promotion.py \
		--go "$(CURDIR)/noema" \
		--rust "$(CURDIR)/rust/target/debug/noema-rs"
	python3 ./tests/rust-rewrite/tier_maintenance.py \
		--go "$(CURDIR)/noema" \
		--rust "$(CURDIR)/rust/target/debug/noema-rs"

benchmark-rust: comparison-release
	./tests/rust-rewrite/benchmark.sh

benchmark-mcp-rust: comparison-release
	python3 ./tests/rust-rewrite/mcp_benchmark.py \
		--go "$(CURDIR)/dist/noema-$(HOST_OS)-$(HOST_ARCH)" \
		--rust "$(CURDIR)/rust/target/release/noema-rs"

soak-rust: comparison-release
	python3 ./tests/rust-rewrite/federation_soak.py \
		--go "$(CURDIR)/dist/noema-$(HOST_OS)-$(HOST_ARCH)" \
		--rust "$(CURDIR)/rust/target/release/noema-rs"

obsidian-publish:
	npm --prefix plugins/obsidian run build
	@set -eu; \
	cortex_dir="$(OBSIDIAN_CORTEX_DIR)"; \
	if [ -z "$$cortex_dir" ]; then \
		cortex_dir="$$(noema cortex list | sed -n 's/^[^[:space:]][^[:space:]]*[[:space:]][[:space:]]*\(.*\)[[:space:]][[:space:]]*\*$$/\1/p' | sed 's/[[:space:]]*$$//' | head -n 1)"; \
	fi; \
	if [ -z "$$cortex_dir" ]; then \
		echo "error: no active cortex found; run 'noema use <name>' or pass OBSIDIAN_CORTEX_DIR=/path/to/cortex" >&2; \
		exit 1; \
	fi; \
	dest="$$cortex_dir/.obsidian/plugins/noema"; \
	install -d "$$dest"; \
	install -m 0644 plugins/obsidian/main.js "$$dest/main.js"; \
	install -m 0644 plugins/obsidian/manifest.json "$$dest/manifest.json"; \
	install -m 0644 plugins/obsidian/styles.css "$$dest/styles.css"; \
	echo "Obsidian plugin published to $$dest"

clean:
	rm -f $(BIN)
	rm -rf $(DIST_DIR)
