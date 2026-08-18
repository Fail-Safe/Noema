# Why Noema switched to Rust

Noema's production implementation switched to Rust in v0.20.0. The case was
not that Rust is universally faster, nor that a language rewrite automatically
improves a system. The case was that this specific implementation matched
Noema's tested behavior, improved the workloads most representative of a
long-lived memory service, and made an enhanced recovery protocol available
without imposing its cost on every user.

The final side-by-side state is preserved by the signed
`rust-cutover-baseline` tag. The repository root, build, CI, and release paths
now use the Rust implementation; the measurements below remain the evidence
for that decision.

> **★ Insight**
>
> The decision changed when the comparison stopped treating durability policy
> as implementation performance. Under the default `standard` profile, Rust
> and Go perform equivalent mutation work. Under `strong`, Rust deliberately
> performs more work to buy a stronger recovery guarantee.

## Decision scorecard

| Dimension | Current result | Interpretation |
| --- | --- | --- |
| Public behavior | Differential suites pass across storage, CLI, MCP, federation, consolidation, watcher, plugins, restore, semantic search, and TUI state | No known public command or protocol port remains |
| Mixed throughput | Rust is 26.3% faster at 10k traces and 41.2% faster at 100k | Favors Rust |
| Mixed-workload memory | Rust uses 21.7% less RSS at 10k and 40.8% less at 100k | Favors Rust |
| Selective retrieval | Rust is faster and uses less RSS at both final scales | Favors Rust |
| Full-corpus output | Rust is faster, but one 100k process retains 2.15 times Go's RSS | Important Rust caveat |
| Federation soak | Rust uses 37.4% less peak aggregate RSS and 18.8% less mean sampled CPU | Favors Rust in the bounded test |
| Consolidation quality | Exact-artifact and frontier blind reviews are effectively tied | Parity, not a language advantage |
| Default durability | `standard` matches Go's mutation and crash posture | Migration does not impose an immediate performance regression |
| Enhanced durability | `strong` passes process-death and APFS detach/remount recovery, with a large write cost | Useful opt-in; hard power-cut/device-cache qualification remains |
| Release artifact | Rust is 27.8% larger in the current local release build | Favors Go, but operational impact is small |
| Production source | Rust uses 10.7% fewer code-only lines and 30.3% fewer physical lines | Smaller implementation; not proof of correctness or a language-wide result |
| Release delivery | v0.20.0 shipped Rust-only source, CI, six-platform archives, checksums, plugins, and Homebrew packages | Production cutover complete |

## Performance at scale

The final scale campaign used release-optimized binaries, five
alternating-order runs, fresh MCP server processes per scenario, four clients,
exact post-write trace counts, and `verify` after mutation. The 100k campaign
used independent byte-identical clones of one verified 100,150-trace corpus.

![Two grouped bar charts show Rust 26 and 41 percent faster than Go at 10k and 100k traces, while using 22 and 41 percent less peak memory.](assets/rust-rewrite/mixed-scale.svg)

| Mixed workload | Go throughput | Rust throughput | Go RSS | Rust RSS |
| --- | ---: | ---: | ---: | ---: |
| 10k traces | 3,412.50 ops/s | 4,311.13 ops/s | 109.6 MiB | 85.8 MiB |
| 100k traces | 2,977.24 ops/s | 4,203.50 ops/s | 175.6 MiB | 104.0 MiB |

The mixed workload matters because Noema is not only a search index. A running
server serves selective reads while agents create and update traces. Rust's
advantage grows rather than disappears between the two measured corpus sizes.
These are two measured points, not a fitted scaling curve.

### The workload shape matters

Rust led every final throughput median. Memory was more nuanced: selective and
mixed workloads favored Rust, four-client broad output was nearly tied at
100k, and single-process broad output favored Go substantially.

![Diverging horizontal bars show Rust faster in all ten final scenarios. Rust uses less peak memory in seven scenarios, approximately the same memory for 100k four-client broad output, and more memory for the two broad serial scans.](assets/rust-rewrite/workload-ratios.svg)

| Scale and scenario | Throughput vs. Go | Peak memory vs. Go |
| --- | ---: | ---: |
| 10k serial broad | 49% faster | 26% more |
| 10k serial selective | 45% faster | 29% less |
| 10k four-client broad | 14% faster | 4% less |
| 10k four-client selective | 15% faster | 17% less |
| 10k mixed read/write | 26% faster | 22% less |
| 100k serial broad | 90% faster | 115% more |
| 100k serial selective | 46% faster | 55% less |
| 100k four-client broad | 55% faster | approximately equal |
| 100k four-client selective | 52% faster | 39% less |
| 100k mixed read/write | 41% faster | 41% less |

The 100k serial-broad result is the clearest performance caveat. It
does not erase the gains elsewhere, but it prevents a claim that Rust always
uses less memory. Users relying on repeated full-corpus MCP responses should
budget memory accordingly; selective retrieval and mixed traffic use less.

## Long-lived federation uses fewer resources

A bounded homogeneous three-node soak applied 80 paced mutations over eight
seconds, restarted one node, verified exact event convergence, and measured the
three-server aggregate. Both implementations converged to exactly 80 unique
events per node.

![Horizontal baseline bars show Rust using 37.4 percent less peak memory and 18.8 percent less mean sampled CPU than Go in the bounded federation soak.](assets/rust-rewrite/federation-resources.svg)

| Three-server aggregate | Go | Rust | Rust/Go |
| --- | ---: | ---: | ---: |
| Wall time | 9.586 s | 9.112 s | 0.950x |
| Peak sampled RSS | 90.266 MiB | 56.469 MiB | 0.626x |
| Mean sampled CPU | 7.913% | 6.429% | 0.812x |

This is a short resource comparison, not a stability claim. It nevertheless
shows that Rust's memory advantage survives active replication, restart, and
convergence rather than appearing only in a single-process microbenchmark.

## Model-driven quality reached bounded parity

The hardest qualitative comparison originally favored Go. That was useful: it
exposed abbreviated Rust prompts, weaker output-shape validation, source-ID
leakage, and an insufficient polysemy rule. Porting the complete prompt
contracts and applying the same grounding changes to both implementations
closed those defects.

![Two score cards show Go and Rust within 1.25 percentage points in both blind-review evaluations, with equal decision accuracy and near-identical retention.](assets/rust-rewrite/quality-parity.svg)

| Qualification | Go | Rust | Shared correctness result |
| --- | ---: | ---: | --- |
| Small exact-artifact blind review | 473/480 | 467/480 | 48/48 decisions and 203/204 adjudicated retention each |
| Frontier blind review | 239/240 | 238/240 | 24/24 decisions and 102/102 retention each |
| Large profile | Not blind-scored | Not blind-scored | 24/24 decisions and 102/102 retention each |

In the exact-artifact batch, 36 of 48 pairs tied. The preceding independent
batch narrowly favored Rust, while the final batch narrowly favored Go. With
equal prompts, symmetric factual error, and equal decision accuracy, the
defensible conclusion is bounded quality parity rather than a winner.

Rust retained a process-memory advantage during model-driven work. The final
small-profile median peak RSS was 14.820 MiB versus Go's 23.508 MiB. Model wall
time was effectively tied and should not be presented as client-runtime speed.

> **★ Insight**
>
> The failed early quality comparison strengthened the port. It demonstrated
> that the gap came from incomplete behavioral translation, not from Rust, and
> it left both runtimes with stricter prompts and validation.

## Durability is an explicit product choice

The canonical profiles are:

- `standard` — the default. It matches Go's direct trace-file write and single
  SQLite transaction, without a per-mutation recovery journal or file/directory
  fsync.
- `strong` — opt-in recovery records, stable mutation locks, atomic temporary
  files, rename, file and directory fsync, and recovery reconciliation.

![Three bar charts compare current Go, Rust standard, and Rust strong at 10k traces. Rust standard is the like-for-like comparison; Rust strong shows the cost of recovery guarantees unavailable in Go.](assets/rust-rewrite/durability-tradeoff.svg)

| Runtime and profile | Seed time | Mixed throughput | Median write latency | Durability posture |
| --- | ---: | ---: | ---: | --- |
| Go current | 6.596 s | 3,412.50 ops/s | 0.354 ms | Current/default posture |
| Rust standard | 3.096 s | 4,311.13 ops/s | 0.301 ms | Matches Go's posture |
| Rust strong | 92.987 s | 252.75 ops/s | 9.128 ms | Adds journaled recovery and explicit flushes |

A second ordinary Go control was recorded during the Rust-strong campaign
(6.491 seconds seed, 3,299.16 mixed ops/s, and 0.348 ms median write latency).
It confirms that the Go baseline was stable; it is not a Go strong profile and
is therefore not plotted as one.

Strong mode is not a fair drop-in performance comparison with Go because it
performs a different durability protocol. Its cost is still operationally
important: users selecting it should understand the write amplification. The
strong subprocess suite validates process-death recovery. A separate macOS
APFS gate forcibly detaches and remounts disposable images at restore,
configuration, and mutation boundaries; four complete runs preserved explicit
restore recovery, cleared the strong journal, and retained SQLite integrity.
Forced detach may still honor completed flushes, so this does not prove
behavior when hardware loses an already-acknowledged volatile write cache.

The stronger protocol is also not an intrinsic Rust feature. It could be
implemented in Go. Rust made the protocol practical to express and test within
this rewrite, while the profile boundary lets Noema ship it without redefining
the default behavior.

## Artifact size and operational shape

![Horizontal bars show the current Go release binary at 14.2 MiB and Rust at 18.1 MiB, an increase of 3.9 MiB or 27.8 percent.](assets/rust-rewrite/artifact-size.svg)

| Release artifact | Bytes | MiB |
| --- | ---: | ---: |
| Go | 14,887,442 | 14.2 |
| Rust | 19,019,360 | 18.1 |

The Rust artifact is currently 27.8% larger. Both remain single local binaries,
so the practical distribution cost is modest, but this is a real Go advantage.
Rust also changes the build and cross-compilation toolchain. The v0.20.0
release validated bundled SQLite and the macOS, Linux, and Windows packaging
matrix on x86-64 and ARM64.

Rust's language-level contribution is compile-time memory and thread safety for
a server that combines SQLite access, background federation, watchers,
consolidation, TLS, and concurrent MCP transports. That is a maintainability
and defect-prevention argument, not a measured performance percentage.

## A smaller production implementation

At the signed final comparison baseline, Noema's production implementation
decreased from 19,775 code-only lines across 97 Go files to 17,664 lines across
25 Rust source files. That is 2,111 fewer code lines, a 10.7% reduction. The
physical source decreased from 26,966 to 18,784 lines, a 30.3% reduction.

![Two horizontal bar charts compare the production implementations. Code-only LOC falls from 19,775 in Go to 17,664 in Rust, or 10.7 percent. Physical LOC falls from 26,966 to 18,784, or 30.3 percent.](assets/rust-rewrite/source-loc.svg)

| Production implementation | Files | Code-only | Blank | Comment-only | Physical |
| --- | ---: | ---: | ---: | ---: | ---: |
| Go | 97 | 19,775 | 2,218 | 4,973 | 26,966 |
| Rust | 25 | 17,664 | 1,012 | 108 | 18,784 |
| Rust change | — | 10.7% fewer | 54.4% fewer | 97.8% fewer | 30.3% fewer |

The count comes from `rust-cutover-baseline` using cloc 2.10. Code-only LOC
excludes blank and comment-only lines; physical LOC includes both. The scope
excludes Go `*_test.go` files, top-level Rust `#[cfg(test)]` modules, Rust
integration tests, migrations, plugins, and shared benchmark harnesses. This
makes it a comparison of the maintained runtime implementations rather than
their differently sized test suites.

> **★ Insight**
>
> The useful conclusion is not that Rust is inherently more concise. Noema's
> production behavior occupies 10.7% fewer code lines in Rust; the larger
> physical-line reduction mainly reflects Go's much heavier commentary. Both
> support a smaller maintenance surface, while compatibility and resilience
> suites remain the evidence for correctness.

## Why the final results differ from the first results

The experiment found and corrected several implementation effects:

1. The first Rust MCP server reopened the cortex for every request. Retaining
   one connection removed that avoidable overhead.
2. Rust initially emitted pretty full-row MCP results where Go emitted compact
   rows. Matching the wire representation removed response and allocation bias.
3. New Rust traces used an update-style FTS delete before insert. Because the
   stored `id` column was not the FTS rowid, each delete scanned an increasingly
   large virtual table. Insert-only create helpers removed the corpus-size
   regression.
4. Strong durability initially looked like a general Rust mutation problem.
   Separating it from the standard profile showed that most of the difference
   was extra recovery work, not a language-runtime limit.

Only the corrected final campaigns are used in the headline scale charts.
Earlier results remain valuable as an optimization history, not as final
Go-versus-Rust evidence.

## Evidence and methodology

The charts and accessible tables above are generated from the curated
[`report-data.json`](../tests/rust-rewrite/report-data.json) dataset. The signed
`rust-cutover-baseline` tag preserves the final Go and Rust comparison state.
Detailed methodology, campaign definitions, and historical results remain in
the [`Rust rewrite experiment results`](rust-rewrite-experiment-results.md).

Contributor procedures, evidence-update rules, and open qualification work are
kept separately in the
[`Rust cutover evidence maintenance guide`](rust-rewrite-maintenance.md), so
this page can remain a stable explanation of the public decision.
