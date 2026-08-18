# Rust Implementation

Noema's production implementation is a Rust 2024 crate at the repository root.
It preserves the established CLI, MCP, Cortex, trace, SQLite, federation, and
plugin contracts while making durability policy explicit.

Build and test it with:

```sh
make build
make test
make release-check
```

The debug binary is `target/debug/noema`; `make build` also copies it to
`./noema`. The optimized binary is `target/release/noema`, and `make release`
copies a host-labelled artifact into `dist/`.

## Durability profiles

`standard` is the default and matches the mutation guarantees of Noema releases
before the Rust cutover. `strong` adds journaled recovery, stable mutation locks,
atomic replacement, and explicit file and directory synchronization:

```sh
NOEMA_DURABILITY=strong noema serve
```

Unknown profile names fail closed. See [Why switch Noema to
Rust?](why-rust.md) for the measured cost and recovery tradeoff.

## Historical comparison

The completed port evaluation, original measurement data, generated charts,
and methodology are retained in `docs/rust-rewrite-*`, `docs/assets/rust-rewrite/`,
and `tests/rust-rewrite/`. They explain and preserve the cutover decision; they
are not the active build path. The signed `rust-cutover-baseline` tag anchors
the final side-by-side Go and Rust state.
