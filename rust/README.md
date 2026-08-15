# Noema Rust comparison implementation

This directory contains a behavior-compatible Rust implementation of Noema.
It intentionally lives beside the Go implementation so the two binaries can
be exercised against identical Cortex fixtures and compared without changing
the release baseline.

Build and test:

```sh
cargo build --manifest-path rust/Cargo.toml
cargo test --manifest-path rust/Cargo.toml
```

The binary is `rust/target/debug/noema-rs`. It reads the same user config,
`cortex.md`, trace Markdown, SQLite database, and embedded SQL migrations as
the Go binary.

The HTTP transport supports the Go manifest's shared bearer key, TLS certificate
and key paths, and per-peer custom CA files. Access-key resolution follows
`NOEMA_MCP_KEY` first, then `access.shared_key_file`, then open mode. Keyed HTTP
must use TLS; open HTTP is restricted to loopback. A rotated peer signing key can
be recovered explicitly with `federation re-pin-peer` without resetting the
event cursor.

Run the cross-implementation gate and release benchmark from the repository
root:

```sh
make compare-rust
make benchmark-rust
make benchmark-mcp-rust
make soak-rust
```
