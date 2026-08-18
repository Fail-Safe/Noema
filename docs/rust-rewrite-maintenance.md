# Rust cutover evidence maintenance

This document is for Noema maintainers updating the evidence behind the public
[`Why Noema switched to Rust`](why-rust.md) page. It owns benchmark
regeneration, evidence lifecycle rules, operational cautions, and remaining
qualification work. It is not end-user installation or usage guidance.

## Evidence inventory

- Curated chart data:
  [`tests/rust-rewrite/report-data.json`](../tests/rust-rewrite/report-data.json)
- Final comparison source state: signed `rust-cutover-baseline` tag
- Detailed methodology and historical results:
  [`rust-rewrite-experiment-results.md`](rust-rewrite-experiment-results.md)
- Qualification state and handoff notes:
  [`rust-rewrite-status.md`](rust-rewrite-status.md)
- Acceptance criteria:
  [`rust-rewrite-test-plan.md`](rust-rewrite-test-plan.md)

The public page and its accessible tables are the interpretation layer. The
curated JSON and signed tag are the evidence layer. Keep those roles separate.

## Open qualification work

These items refine Noema's guarantees; they are not missing command or protocol
ports:

1. Repeat the passing macOS APFS forced-detach qualification on a hard-powered
   VM or disposable hardware target. The local gate covers restore placement,
   configuration rename, journal cleanup, SQLite recovery, and acknowledged
   standard-mode state, but cannot force a device to lose an already
   acknowledged volatile-cache write.
2. Decide whether corrupt-database salvage belongs in Noema. The current Rust
   implementation fails closed and preserves the original bytes.
3. Complete human TUI checks across the supported terminal emulators.
4. Extend qualitative evaluation to a redacted representative corpus,
   additional models, and multiple reviewers.

## Operational caution

Do not downgrade a cortex while a `strong` pending-mutation record exists.
Complete recovery with the current Noema binary first; releases predating the
strong journal do not understand that record.

## Regenerating the published charts

After updating `tests/rust-rewrite/report-data.json`, regenerate every SVG:

```sh
make historical-report
```

The selected `REPORT_PYTHON` interpreter needs Matplotlib. Matplotlib is report
tooling, not a Noema runtime dependency. Override the interpreter when needed:

```sh
make historical-report REPORT_PYTHON=/path/to/python
```

Set `NOEMA_REPORT_PREVIEW_DIR=/tmp/noema-report-preview` to render uncommitted
PNG copies for visual review alongside the canonical SVGs.

## Adding or replacing a result

1. Require the scenario's correctness gate to pass before measuring it.
2. Use release-optimized binaries and record the candidate commit, host class,
   operating system, architecture, and toolchain.
3. Preserve alternating execution order, independent corpora, fresh processes,
   run count, and exact post-write verification.
4. Keep unlike campaigns separate. Mark superseded measurements as historical
   instead of silently combining them.
5. Update the curated JSON, regenerate every chart, and update the adjacent
   accessible table and prose in `docs/why-rust.md` together.
6. Recheck every percentage and ratio against the curated source data.
7. Run the repository gates before treating a result as release evidence:

   ```sh
   make test
   make release-check
   git diff --check
   ```

8. When a claim targets a release candidate, validate the six-platform
   archives, checksums, plugin bundles, and Homebrew metadata from that exact
   candidate commit before publishing the claim.

Historical Go/Rust reproduction requires checking out the signed
`rust-cutover-baseline` tag. Do not imply that the current Rust-only tree can
recreate the former Go binary without that checkout.

## Public-document rules

- Keep `docs/why-rust.md` focused on the decision, measured evidence,
  limitations, and user-relevant tradeoffs.
- Keep commands, regeneration instructions, open tasks, and release procedures
  here.
- Retain unfavorable results. A new campaign may supersede an older one, but it
  must not silently remove caveats such as broad-output memory use or artifact
  size.
- Use qualified language: measured points are not fitted scaling curves, short
  soaks are not stability claims, and model wall time is not runtime speed.
- Keep chart alt text and accessible tables synchronized with visual changes.
