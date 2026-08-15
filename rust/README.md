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

Run the cross-implementation gate and release benchmark from the repository
root:

```sh
make compare-rust
make benchmark-rust
make benchmark-mcp-rust
make soak-rust
```
