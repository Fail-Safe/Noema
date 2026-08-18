# Noema build and qualification targets.

BIN := noema
DIST_DIR := dist
REPORT_PYTHON ?= python3

HOST_OS := $(shell uname -s | tr '[:upper:]' '[:lower:]' | sed 's/mingw.*/windows/; s/msys.*/windows/')
HOST_ARCH := $(shell uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
HOST_ARTIFACT := $(DIST_DIR)/$(BIN)-$(HOST_OS)-$(HOST_ARCH)

.PHONY: help build check test release release-check clean obsidian-publish \
	tui-pty storage-fault historical-report

help:
	@echo "Noema build targets:"
	@echo ""
	@echo "  make build             Build the debug binary -> ./$(BIN)"
	@echo "  make check             Run formatting and strict Clippy checks"
	@echo "  make test              Run check plus the full Rust test suite"
	@echo "  make release           Build the optimized host binary -> $(HOST_ARTIFACT)"
	@echo "  make release-check     Build and smoke-test the host release binary"
	@echo "  make tui-pty           Run the TUI pseudo-terminal qualification"
	@echo "  make storage-fault     Run disposable macOS APFS recovery tests"
	@echo "  make historical-report Regenerate the Go-to-Rust cutover charts"
	@echo "  make obsidian-publish  Build and publish the Obsidian plugin"
	@echo "  make clean             Remove local binaries and Cargo output"

build:
	cargo build --locked
	cp target/debug/$(BIN) ./$(BIN)

check:
	cargo fmt --check
	cargo clippy --all-targets -- -D warnings

test: check
	cargo test --all-targets --locked

release: | $(DIST_DIR)
	cargo build --release --locked
	cp target/release/$(BIN) $(HOST_ARTIFACT)
	@ls -lh $(HOST_ARTIFACT)

release-check: release
	$(HOST_ARTIFACT) version

$(DIST_DIR):
	mkdir -p $(DIST_DIR)

tui-pty: build
	python3 ./tests/rust-rewrite/tui_pty.py --rust "$(CURDIR)/target/debug/$(BIN)"

storage-fault: build
	python3 -u ./tests/rust-rewrite/storage_fault_macos.py --rust "$(CURDIR)/target/debug/$(BIN)"

historical-report:
	$(REPORT_PYTHON) ./tests/rust-rewrite/render_report_charts.py

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
	cargo clean
	rm -f $(BIN)
	rm -rf $(DIST_DIR)
