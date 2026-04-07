# Noema build targets.
#
# Dev builds live at the repo root as ./noema (matching the convention in
# CLAUDE.md and .gitignore). Release builds land in ./dist/ with an
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

.PHONY: help build release release-linux test vet clean

help:
	@echo "Noema build targets:"
	@echo ""
	@echo "  make build           Dev build with debug info      -> ./$(BIN)"
	@echo "  make release         Stripped build for this host   -> $(DIST_DIR)/$(BIN)-$(HOST_OS)-$(HOST_ARCH)"
	@echo "  make release-linux   Stripped build for linux/amd64 -> $(DIST_DIR)/$(BIN)-linux-amd64"
	@echo "  make test            go test ./..."
	@echo "  make vet             go vet ./..."
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

clean:
	rm -f $(BIN)
	rm -rf $(DIST_DIR)
