# Rust rewrite experiment results

Date: 2026-08-16. Host: Apple Silicon macOS. Both implementations were built
with release optimizations. Results are local comparisons, not portable
performance guarantees.

## Persistent MCP connection

The first Rust MCP implementation reopened the cortex for every tool call. At
100 traces in a single preliminary run it delivered 698 searches/second versus
791 for Go. The server now retains one Tokio-mutex-protected cortex connection
for its lifetime.

Five alternating-order runs after that change produced:

| Corpus | Metric | Go median | Rust median | Rust relative result |
| ---: | --- | ---: | ---: | ---: |
| 250 | requests/second | 386.91 | 409.83 | 5.9% higher |
| 250 | median latency | 2.537 ms | 2.425 ms | 4.4% lower |
| 250 | p95 latency | 3.078 ms | 2.645 ms | 14.1% lower |
| 250 | maximum RSS | 33,200 KiB | 16,944 KiB | 49.0% lower |
| 250 | sampled CPU utilization | 115.7% | 86.1% | 25.6% lower |
| 1,000 | requests/second | 105.31 | 101.01 | 4.1% lower |
| 1,000 | median latency | 9.455 ms | 9.857 ms | 4.3% higher |
| 1,000 | p95 latency | 10.109 ms | 10.287 ms | 1.8% higher |

The throughput crossover suggests that result construction/serialization at
larger result sets matters more than connection setup. Rust has a clear memory
and CPU advantage at the measured 250-trace workload, but no general
steady-state throughput win has been established.

## Signed federation and mixed-process sync

The Rust cortex now has direct replay, a one-shot Streamable HTTP peer pull, and
a cancellable background scheduler with per-peer exponential backoff. The unit
and mixed-process tests cover:

- Ed25519 verification before any state mutation;
- trust-on-first-use key pinning under the Go-compatible `cortexkey:<id>` key;
- stable peer-identity pinning and endpoint-replacement rejection;
- monotonic event ULIDs for lexical federation cursors;
- 205-event pagination across three batches;
- per-event cursor advancement, unavailable-peer pinning, and retry recovery;
- vector-clock merge and causal update convergence;
- duplicate-event idempotency;
- update-before-create materialization without rolling state backward;
- concurrent-update divergence traces containing both versions;
- tampered signed-payload rejection; and
- foreign source-lock enforcement;
- metadata, archive/unarchive, trash/recover, tier, vote, and purge replay; and
- signed Go-to-Rust and Rust-to-Go convergence under `verify: enforce`;
- independent per-peer usage cursors and monotonic `MAX` counter merges;
- three-node background convergence with transitive duplicate delivery;
- live pause/resume, outage classification and recovery, and graceful SIGTERM;
- concurrent edits across two disconnected replicas; and
- fail-closed signing-key rotation with event cursors left unchanged, followed
  by explicit hard-pin recovery without replaying old signatures;
- shared bearer-key authentication with environment-over-sidecar precedence;
- Rustls HTTPS serving and TLS 1.2-or-newer outbound federation;
- private-CA trust for both Go-to-Rust and Rust-to-Go federation; and
- missing/wrong bearer rejection, missing-CA rejection, live outbound access-key
  reload, plaintext-auth refusal, and secret-redacted failure output;
- expired and not-yet-valid leaf refusal, seven-day startup warning,
  CLI-over-manifest certificate precedence, and explicit bypass parity; and
- an hourly single-owner certificate monitor with immediate, transition-only
  90/30/7-day, expired, and unreadable observations; and
- live peer-set reconciliation under one supervisor, including zero-peer
  startup, worker addition/removal/restart, and cursor-preserving re-addition;
- four-process same-cortex contention in both builds, including MCP-only losers,
  SIGKILL owner release, replacement acquisition, and continued exclusion; and
- safe pre-mutation I/O refusal for trace overwrite, lifecycle rename, and
  manifest replacement, with file, database, event, and integrity invariants;
  and
- Rust-only returned-error rollback after a successful file write or rename but
  an aborted SQLite trace transaction. Local and replay tests require exact
  original bytes and permissions, inverse lifecycle moves, cleanup of newly
  materialized files, preservation of orphan files, unchanged rows, and no
  partial event; and
- Rust atomic trace replacement plus durable pending records for transactional
  create, replace, move, and delete paths. Five subprocess scenarios kill the
  writer after the filesystem mutation and prove the next Rust open restores
  the old row/file/event state, including hard delete and watcher trash
  reconstruction. Owner locks skip live mutations; stable per-trace locks reject
  concurrent duplicate materialization before file replacement.
- Mixed-runtime takeover now fails closed: the branch's Go build detects a
  pending Rust mutation without parsing its value, Rust performs recovery, and
  Go then opens normally. Corrupt SQLite bytes, malformed recovery JSON, and
  pending path traversal also fail without rewriting protected data.

The network experiment found two interoperability defects that in-process tests
did not expose. Rust's first identity response did not use Go's `version` and
`pubkey` fields. More subtly, Noema signatures cover the exact event payload
bytes: decoding JSON into a sorted map or pretty-printing `sync_events` changed
those bytes and invalidated an otherwise correct signature. The Rust build now
preserves insertion order, omits fields exactly like Go's event snapshot, emits
compact federation JSON, and has a Go-side signed Rust wire fixture.

The three-node test found another wire edge: Go serializes an empty nil event
slice as `null`, while the original Rust client accepted only `[]`. Rust now
normalizes either form to an empty batch. The same tolerance applies to empty
usage-signal results.

Final report validation also exposed a rotation-boundary race in the fixture.
It took its pre-rotation cursor snapshot while the origin could still receive
and relay an old-key event, which could strand that event behind the new pin.
The gate now requires convergence of the rotating origin's signed event set
before capturing the boundary. Three consecutive mixed-ring runs and the full
differential suite passed after the correction.

## Consolidation election foundation

The Rust server now implements the coordination layer below the actual
distillation pass:

- Go-compatible eligibility gates for feature enablement, scheduling triggers,
  federation mode, and an OpenAI-compatible `/models` health probe;
- cryptographically random ranks from 1 through 99, with rank zero reserved for
  ineligible nodes;
- persisted local and per-peer rank advertisements exchanged through the
  existing federation identity handshake;
- quiet-period filtering, highest-rank selection, and lexical cortex-ID
  tie-breaking;
- operator-visible winner and local-run decisions in federation status;
- signed claim, success, and failure coordination events that replay into the
  audit log without materializing synthetic traces; and
- a cross-runtime background lock so only one server process per cortex runs
  federation and eligibility workers.

The next coordination layer is also present. A reusable Rust pass gate now
matches Go's initial election, signed claim, quiet-period wait, re-election,
preemption classification, in-flight tracking, inner-pass error closure, and
success behavior. The live Rust server runs a stale-claim watchdog with a strict
local timeout, extra remote propagation grace, in-flight suppression, and
event-log-based deduplication.

Rust now also runs a real deterministic pass from the threshold trigger. Its
candidate query matches Go's active-state and rolling-window filters and sums
usage across peers. Its score matches the Go weights for reads/search hits,
modifications, inbound lineage, and votes, including the two-inbound-reference
minimum and the single-source passive-search guard. Qualifying traces move from
short to mid in both SQLite and Markdown, emit the same promotion event data,
and leave the candidate pool so a restart cannot promote them twice. Federated
cortexes execute this pass through the existing election gate; single-node
cortexes avoid unnecessary claim/success events.

The mixed Go/Rust fixture exercises deterministic winner selection, endpoint
loss and rank-zero failover, endpoint recovery, and replay of a real Go
threshold pass's signed claim/success pair. Both the election and three-node
ring scenarios passed five consecutive repetitions after their timing
preconditions were made explicit. The fixture now also removes a terminal event
from the Rust replica to model an orphaned Go claim, then verifies Rust emits a
signed `watchdog_expired` closure that Go accepts under enforced verification.

The independent Go/Rust promotion fixture gives both implementations identical
read, search, edit, vote, lineage, age, and archive inputs. It checks exact
candidate choices, event data, frontmatter mutation, and restart idempotency;
the scenario passed five consecutive repetitions.

The rest of deterministic tier maintenance is now present as one cadence owner,
preserving Go's cron-before-threshold-before-idle priority. Cron fires once per
local day; federated runs wait for a success event and retry three times at
five-minute intervals, while single-node runs mark the day immediately. Idle
requires a real event-history timestamp and uses any prior pass as its cooldown
boundary. Graduation runs after the heuristic pass and uses the same AND-gates:
minimum age, reads plus search hits, optional unmodified requirement, no active
downvote, and no automatic promotion for preference traces.

A second independent Go/Rust fixture drives midnight cron and a backdated idle
log, checking old/young, read/search, modified, downvoted, preference, and
archived cases. It verifies exact `mid` to `long` event data, Markdown updates,
and restart idempotency. The scenario passed five consecutive repetitions.

Model-driven consolidation is now wired ahead of those maintenance passes. The
Rust candidate query excludes source IDs already recorded by a prior
`consolidate` event without promoting the sources themselves. Deterministic
type-and-day buckets honor the small/large/frontier cluster ceilings, call an
OpenAI-compatible endpoint with bounded retries, normalize returned tags, and
materialize a mid-tier observation with exact `derived_from` lineage plus a
separate telemetry event. Invalid or unreachable model responses fall back to
the same score-gated heuristic, while cadence still reaches graduation.

A controlled-endpoint fixture now compares Go and Rust success output, request
shape, malformed-response retries, offline behavior, source-tier preservation,
lineage, telemetry, and restart idempotency. The signed federation fixture also
creates distilled traces through each runtime's MCP tool and proves the other
runtime replays both the create snapshot and consolidate event exactly once.
The same fixture now drives each operator CLI with manifest values deliberately
overridden by flags. It compares summary text and emitted JSON, and proves both
successful distillation and malformed-response fallback remain write-free under
`--dry-run`.

## Real-model consolidation

Seven alternating-order runs drove the release Go and Rust CLIs against the
same trusted LAN OpenAI-compatible endpoint and `qwen3.5-9b` model. Each run
created fresh cortexes containing nine synthetic traces: one cohesive decision
bucket, one deliberately unrelated fact bucket, and one cohesive observation
bucket. Both CLIs used `--dry-run`, so the model saw only synthetic fixture data
and neither cortex was mutated by consolidation.

| Metric | Go | Rust | Rust relative result |
| --- | ---: | ---: | ---: |
| Bucket decisions | 21/21 | 21/21 | equal |
| Planted-term retention | 111/112 | 112/112 | one additional term retained |
| Median wall time | 17.19 s | 13.68 s | 20.4% lower |
| Wall-time range | 16.55-18.45 s | 11.85-18.10 s | wider model variance |
| Median peak RSS | 23.06 MiB | 14.75 MiB | 36.0% lower |
| Peak-RSS range | 22.77-23.33 MiB | 14.69-14.88 MiB | consistently lower |

Every run made five successful HTTP requests per implementation. The final two
runs added request accounting: Go sent 2,488 prompt tokens and 11,071 request
bytes per run, while Rust sent 1,220 prompt tokens and 5,206 request bytes. Rust
therefore used 51.0% fewer prompt tokens and 53.0% fewer request bytes on this
fixture. Completion-token counts varied with model output, as expected.

The lower Rust wall time is principally a model-workload result, not evidence
that its HTTP client executes inference faster: nearly the entire measured wall
time was spent awaiting the endpoint, while each CLI consumed only about 0.00
to 0.02 seconds of user plus system CPU. The experiment does establish that the
shorter Rust prompt retained the tested decisions, values, and unrelated-bucket
rejection while materially reducing prompt cost. It does not establish general
distillation-quality parity; larger, ambiguous, and adversarial corpora remain
useful follow-up work. The reusable runner is
`tests/rust-rewrite/real_model_distillation.py`; it records only counts,
timings, and synthetic result text, never endpoint response bodies.

### Expanded blinded qualitative comparison

The final release artifacts from commit `8cd9719` were then exercised in four
fresh, alternating-order runs against the same model. Each implementation saw
30 synthetic traces split into eight independent type-and-day buckets: five
cohesive cases and three rejection cases. The corpus added corrected root-cause
history, negative preferences, chronology, unresolved contradictory evidence,
superficially similar context, and three unrelated meanings of "Mercury". All
runs used the small profile and `--dry-run`.

Automated scoring was re-derived from the saved output text after collection,
rather than trusting cached matches. A semantic stem treats "rejection" as
retaining "rejected", while date fragments copied from source trace IDs are
reported as provenance leakage instead of invented numeric facts.

| Metric | Go | Rust | Interpretation |
| --- | ---: | ---: | --- |
| Bucket decisions | 29/32 | 28/32 | Both struggled with lexical polysemy; Go rejected it once |
| Planted-term retention | 136/136 | 136/136 | Equal on accepted-case factual content |
| Forbidden claims | 0 | 0 | No planted contradiction was introduced |
| Novel numeric claims | 0 | 0 | No numeric hallucination after separating source-ID leakage |
| Median wall time | 49.941 s | 51.447 s | Rust was 3.0% slower; run order showed no consistent advantage |
| Median peak RSS | 23.625 MiB | 14.836 MiB | Rust used 37.2% less |
| Prompt tokens | 29,800 | 15,916 | Rust used 46.6% fewer |
| Request bytes | 128,968 | 64,720 | Rust used 49.8% fewer |
| Source-reference leaks | 1 | 3 | Go copied one date fragment; Rust copied nine full IDs across three outputs |
| Evaluation-framing mentions | 3 | 1 | Both sometimes exposed the synthetic-test context |
| Structural tag/title degradations | 0 | 10 | Rust more often collapsed tags or placed them in the title |

The 32 output pairs were deterministically labeled A/B and reviewed before the
implementation key was revealed. The ten-point rubric assigned four points to
fidelity, three to clarity, two to calibration, and one to concision.

| Blind result | Go | Rust |
| --- | ---: | ---: |
| Mean score | 9.219 | 8.438 |
| Pair wins | 14 | 3 |
| Ties | 15 | 15 |

The factual core is encouraging: when either build accepted a cohesive bucket,
both retained every planted requirement, avoided the forbidden claims, and did
not invent numbers. The qualitative gap is nevertheless material. Rust more
often emitted a single oversized tag, moved a tag list into the title, or
included trace identifiers and awkward date/hour phrasing. Go generally
produced cleaner titles, tags, and ambiguity prose. Both builds need a stronger
cohesion test for lexical overlap: Rust accepted the Mercury bucket in all four
runs, while Go rejected it in only one.

At that checkpoint, the result supported continuing the Rust implementation
but not selecting it for production solely on performance. It motivated the
prompt/parser and polysemy remediation below. The evidence was bounded to four
runs, one local model, synthetic data, and a single blind reviewer.

The reusable runner is
`tests/rust-rewrite/qualitative_distillation.py`; focused scoring and blinding
tests are in `tests/rust-rewrite/qualitative_distillation_test.py`.

### Qualitative parity remediation

The first comparison exposed a concrete implementation difference rather than
a Rust-language limitation. Rust had abbreviated all four Go prompt families:
cohesion, template generation, confidence, and frontier JSON. The shorter
prompts explained both the token reduction and the weaker structure. The two
clients also disagreed on the zero-temperature wire shape because Go's JSON
encoder omitted the zero value.

The remediation:

- ports the complete Go prompt guidance to Rust and pins semantic request
  equality for all three profiles in the controlled-endpoint fixture;
- sends `temperature: 0` explicitly from both clients;
- adds a general polysemy warning to the cohesion gate;
- removes internal trace IDs from model input while retaining lineage directly
  from candidate rows;
- rejects missing tags, inline field labels, titles over 100 characters, and
  tag counts outside 1-8; and
- prevents source metadata and evaluation mechanics from becoming durable
  title/body prose unless they are the actual subject.

An intermediate four-run checkpoint using only prompt/request parity removed
all ten Rust schema degradations and all three exact source-ID leaks from the
original sample. Rust improved from 28/32 to 31/32 decisions; Go made 32/32.
That checkpoint also revealed that an ID date could be paraphrased rather than
copied, motivating complete ID removal instead of output regex filtering.

The definitive qualification removed evaluator-only tags from the source
traces so the model received neither an artificial cohesion hint nor
synthetic-test framing. A first six-run batch showed a narrow Rust lead within
ordinary model variation. After the copied prompt examples were normalized to
public-safe placeholders, a fresh exact-artifact batch rebuilt both release
binaries and produced 48 new blinded pairs in alternating execution order:

| Metric | Go | Rust | Rust relative result |
| --- | ---: | ---: | ---: |
| Bucket decisions | 48/48 | 48/48 | equal |
| Automated requirement retention | 202/204 | 203/204 | one more literal match |
| Adjudicated semantic retention | 203/204 | 203/204 | equal |
| Forbidden claims | 0 | 0 | equal |
| Novel numeric claims | 1 | 1 | equal |
| Source-reference leaks | 0 | 0 | equal |
| Evaluation-framing mentions | 0 | 0 | equal |
| Structural tag/title degradations | 0 | 0 | equal |
| Median wall time | 39.637 s | 39.124 s | 1.3% faster |
| Median peak RSS | 23.508 MiB | 14.820 MiB | 37.0% lower |
| Prompt tokens | 36,864 | 36,864 | equal |
| Request bytes | 176,016 | 175,062 | 0.5% fewer |

Blind review used the same ten-point rubric and wrote the score file before the
implementation key was opened. Go scored 473/480 (mean 9.854) and Rust scored
467/480 (mean 9.729): Go won eight pairs, Rust won four, and 36 tied. Each
runtime made the same isolated numeric substitution, changing a planted row
count from 18,442 to 18,444 once. Go's other automated retention miss used
"disproved" where the scorer expected a rejection synonym, so manual
adjudication raised Go to the same 203/204 semantic retention as Rust.

The previous independent six-run batch had favored Rust by two rubric points
with 44 ties. The direction reversal, exact request equality, equal decision
accuracy, and symmetric factual error support bounded qualitative parity rather
than a durable winner. The final batch also exposed occasional process-oriented
filler in both implementations that the automatic marker list did not flag;
blind scoring penalized it directly.

Prompt cost is now intentionally equal. The earlier Rust token advantage was
mostly under-prompting and is no longer counted as a performance benefit. Rust
retains its process-memory advantage, while model-dominated wall time is
effectively tied. The remaining quality qualification is broader rather than a
known parity defect: use a redacted representative corpus, additional models,
and multiple independent reviewers before release replacement.

Profile-specific follow-up then exercised the other two model paths for three
alternating-order runs each. The `large` profile produced equal 24/24 decisions
and 102/102 requirement retention, with no forbidden claims, novel numbers,
leaks, or schema defects. Median wall time was 55.808 seconds for Go and 55.916
seconds for Rust; median peak RSS was 23.828 MiB and 14.797 MiB respectively.

The first `frontier` pass made equal 24/24 decisions but only 96/102 literal
retention in both runtimes. Every miss replaced the exact identifier
`accounts_shadow` and the literal zero mismatch count with understandable but
less precise paraphrases. Unlike the multi-step profiles, the single-shot
frontier prompt lacked their grounding and verbatim-preservation rules.
Adding those rules symmetrically raised both runtimes to 102/102 on a fresh
three-run pass, with no forbidden claims, novel numbers, leaks, schema defects,
or pair disagreements. Blind review scored Go 239/240 and Rust 238/240: one win
each and 22 ties. Median wall time was 50.800 seconds for Go and 48.256 seconds
for Rust; median peak RSS was 23.375 MiB and 14.844 MiB respectively.

## Filesystem watcher parity

The original Rust watcher slept once per raw notification and called whole-
cortex `sync`. That approach reindexed files but did not emit mutation events,
distinguish moves from deletes, preserve source locks, rescue malformed files,
or guarantee true per-path debounce.

The replacement owns one Cortex connection under the existing per-cortex
background lock and reconciles each settled path against its SQLite state. A
mixed-process fixture now gives Go and Rust identical external edits and checks:

- unchanged editor bytes after body/frontmatter reindex;
- one update for a five-write burst;
- valid create plus archive/unarchive transitions without transient trash;
- atomic remove-and-replace without delete misclassification;
- frontmatter reconstruction with user body and indexed title preserved;
- raw Markdown onboarding with canonical rename and provenance;
- recoverable active-file deletion followed by external trash purge; and
- refusal to ingest edits to foreign source-locked traces.

The complete fixture passes in `make compare-rust`, and three earlier
repetitions passed concurrently as a timing-stress check. Watcher startup is
also synchronous with server readiness, so a server cannot begin accepting MCP
traffic before directory registration succeeds.

On this macOS host, `notify` 8.2's default FSEvents backend registered without
error but did not deliver events for the temporary-cortex fixture. Its polling
fallback also missed same-second writes unless content hashing was enabled,
which would impose repeated full-file reads. The Rust watcher therefore keeps
the existing native backend for low-latency delivery and adds a dependency-free
metadata rescan using full `SystemTime` precision and file length. The rescan
interval is five debounce windows bounded to 250 milliseconds through two
seconds; a focused test pins same-second detection. A post-reconcile snapshot
also records files created internally during recoverable delete or onboarding,
so an immediate follow-up purge or edit remains observable.

## Semantic-search parity

The Rust build now uses the existing Reqwest/Rustls client for the same
OpenAI-compatible `/embeddings` contract as Go. A deterministic three-topic
endpoint reverses every response batch while retaining valid `index` fields,
which proves both clients restore input order rather than trusting wire order.
The fixture also verifies environment-based bearer authentication and the
configured Unicode-safe 40-character input cap without recording the token.

Both builds produce byte-identical version-1 little-endian float32 BLOBs after
normalization. The lifecycle fixture covers initial missing rows, idempotent
reruns, body-edit staleness, model-change invalidation, force and limit controls,
source/content-hash equality, and automatic maintenance under `noema serve`.
Malformed stored candidates with the wrong dimension or non-finite elements are
skipped rather than allowed to poison a result set.

Cosine search, stored-vector similarity, and weighted reciprocal-rank fusion
return the same topic ordering across Go and Rust. The fixture also pins source
exclusion and archived visibility. CLI flags and MCP modes use the same core;
when an MCP embedding endpoint is unavailable, both return lexical results with
a generic note that does not expose the endpoint address.

This is a deterministic contract test, not a semantic-quality benchmark. A
real-model comparison would be useful only after selecting a representative
embedding model and query corpus; model quality and index throughput remain
separate questions from the wire/storage/ranking parity established here.

## MCP contract parity

The MCP comparison now snapshots both servers' discovered tool contracts and
normalizes only protocol-equivalent schema dialect differences. The 28 tool
names, parameter sets, required fields, advertised enums, field descriptions,
and output-schema presence match. The fixture also exercises structured cortex
usage and tag results, startup policy reads, explicit usage recording, tag and
append mutations, archive transitions, lexical search, history, invalid votes,
and instruction sections.

Three previously shallow Rust tools now use the shared Cortex storage and event
contracts. `search_activity` aggregates federation-wide usage rows for traces
and tags. `consolidation_health` reports daily activity, separates election
preemption from pipeline failure, derives short-to-mid and mid-to-long latency,
and exposes the one-source-mid leak detector. `resolve_divergence` applies a
selected or custom body through the guarded update path and trashes the resolved
conflict. Synthetic fixed-time rows make these outputs deterministic across Go
and Rust.

The fixture found one invisible side-effect mismatch: Rust's MCP append path
incremented `modify_count`, while Go's fire-and-forget append does not. Aligning
that policy restored identical tag-activity output. Rust deliberately retains
stricter runtime rejection for trace types outside the enum that both servers
advertise; Go currently accepts such input despite its discovery schema.

The advanced pass now compares rolling consolidation candidates and their
usage/lineage signals, one-source rejection, distilled trace tier/source/event
telemetry, human-readable lineage, identity and federation status, sync cursor
and usage rows, invalid and self announcements, and missing-resource/error
behavior. It also proves announcements do not rewrite `cortex.md`.

For HTTP, every mutating tool is rejected on a publish-mode cortex, reads and
event publication remain available, and local stdio writes still succeed.
Subscribe mode rejects both event and usage-signal publication. These checks
close the known MCP behavior gaps; they do not claim exhaustive combinatorial
coverage of every argument permutation across all 28 tools.

## Embedded-plugin lifecycle parity

The Rust binary now compile-time embeds the same six built runtime files as Go:
three for the Hermes memory provider and three for the Obsidian vault plugin.
Plugin commands dispatch before cortex resolution, which makes installation and
inspection usable on a fresh machine without a Noema cortex. Inventory text,
default and explicit target resolution, and SHA-256 drift reports match Go.

The differential fixture covers missing targets, parent-directory guardrails,
non-mutating `--check`, first install, idempotent reinstall, changed-file
refusal, `--check --force`, forced replacement, and final status checks for both
plugin families. It also proves unmanaged operator files survive and every
installed artifact is byte-identical across builds.

The symlink case is deliberately adversarial: a managed path points to a file
outside the plugin directory. Both builds refuse it without force and replace
the symlink directory entry when forced, leaving the external file unchanged.
The fixture also checks that no atomic-write temporary files remain.

## Functional TUI parity

The original Rust TUI was a static full-screen list with only quit handling.
It now uses a testable state model for the same operational workflow as Go:
list/detail focus, bounded detail scrolling, search, tier and visibility
filters, help, confirmations, archive/trash/recover/purge, session-scoped vote
cycling, manual/live refresh, sticky selection, and two-tick arrival
highlights. New and existing traces can round-trip through the configured
external editor while raw mode and the alternate screen are suspended, and a
drop guard restores the terminal on every exit path.

Eight Rust TUI tests exercise the state transitions and SQLite/file mutations.
A user-supplied same-trace screenshot comparison exposed visual differences
that text-only PTY checks could not detect: an inline rather than right-aligned
trace count, uncolored tier glyphs, clipped titles without an ellipsis, flat
type/tag metadata, missing label punctuation, and a fixed detail separator that
scrolled with the metadata.

The Rust renderer now follows the Go hierarchy: the trace count is pinned to
the right edge; short/mid/long glyphs use the same warm-to-cool colors; titles
truncate explicitly; type and individually wrapped tags use inverted `#`
chips; metadata labels retain colons; and the full-width separator shows body
scroll direction and position while metadata stays pinned. Fixed 120x28 dark
and light buffer tests assert content placement and actual cell colors. An
80x14 long-body test asserts pinned metadata and bottom-scroll state.

An identical-size post-change capture then found one residual geometry defect:
Rust's right border consumed the last cell of the 34% list allocation while Go
placed its divider after that allocation, and Rust reserved one extra prefix
cell before each title. The Rust pane now includes a dedicated border cell and
uses the same three-cell cursor/tier prefix as Go. The fixed-size renderer test
pins the divider and title-start columns. A final identical-size capture of the
release binary visually matches Go in the original terminal. Multi-emulator
review remains human portability qualification, not an automated claim.

A dependency-free PTY fixture now drives the actual Crossterm backend. It
checks initial rendering, the external-editor leave-alternate/show-cursor/
re-enter ordering, help, a real 24-to-32-row resize redraw, keyboard exit,
canonical/echo mode restoration, alternate-screen exit, and visible-cursor
restoration. Thirty consecutive focused runs passed during the earlier backend
qualification, and the post-visual-parity fixture passes as part of the complete
differential target.

## Backup and restore parity

The Rust CLI now writes and restores Go-compatible cortex archives without
invoking the platform `tar` command. The direct gzip/tar writer produces one
sorted root, supports long pending-mutation paths, strips owner and extended
metadata, refuses non-regular source entries, and prevents an output archive
from being placed inside the cortex it is archiving. Forced archive replacement
keeps the previous output until the new archive is complete and synced.

Restore extracts into a private staging directory and accepts only safe,
relative directories and regular files beneath exactly one root. Traversal,
links, duplicate paths, multiple roots, unsafe names, and invalid identities
fail before placement. Registered names, IDs, and destination paths are also
checked before `--force` can displace an existing directory. A returned
configuration-save failure restores both the in-memory configuration and the
previous destination.

The differential fixture passed Go archive to Rust restore and Rust archive to
Go restore while preserving trace IDs and bodies. It also passed duplicate-ID
refusal, non-destructive refusal, forced replacement without leftover staging
artifacts, traversal refusal, and symlink refusal. This establishes archive
interoperability and returned-error behavior; real power loss remains
unqualified.

A separate read-only recovery-status path classifies a configured cortex as
clean, pending, malformed-journal, or unreadable-database without opening it or
printing journal values, trace identifiers, paths, or decoder errors. Five
safety tests prove the probe leaves corrupt bytes, pending records, and trace
files unchanged and that the executable output omits sensitive journal fields.

Restore now writes an owner-only, content-free transaction journal before
moving either tree. Its five phases bind the incoming and previous trees by
SHA-256 and record only the cortex identity, destination parent, prior default,
and expected filesystem state. A stable per-destination kernel lock rejects
concurrent replacement while allowing recovery after a killed owner.

Configuration persistence no longer truncates the live YAML. Rust serializes
saves with a stable kernel lock, writes an owner-private same-directory temp,
syncs its bytes and permissions, atomically renames it, and syncs the directory.
The next locked writer safely removes a stale regular temp left by process
death; read-only targets and non-file temp artifacts still fail closed.

`cortex restore-status` lists transaction IDs, labels, phases, and coarse
recovery states without paths or hashes. `cortex restore-recover` performs an
explicit resume or rollback only after the current tree and configuration
match the recorded state. Tampered or ambiguous state remains untouched.

Nine restore subprocess scenarios now cover `SIGKILL` after preservation, placement,
and configuration save; resume and rollback of the resulting states; committed
cleanup; rollback when no destination previously existed; tamper refusal;
malformed-journal redaction; owner-only permissions; concurrent-target
rejection; killed-owner lock release; and death after config-temp sync but
before atomic rename. An independent config scenario proves the prior YAML is
byte-identical after that kill and a retry commits the new complete YAML while
cleaning the stale temp. These establish process-death reconciliation, not
automatic startup recovery or power-loss durability.

## Bounded federation soak

Equivalent homogeneous three-node clusters ran 80 paced create mutations over
eight seconds, with one node gracefully restarted halfway through. Both builds
converged to exactly 80 unique event rows per node and shut down gracefully.

| Metric (three-server aggregate) | Go | Rust | Rust/Go |
| --- | ---: | ---: | ---: |
| Wall time | 9.586 s | 9.112 s | 0.950 |
| Peak sampled RSS | 90.266 MiB | 56.469 MiB | 0.626 |
| Median sampled RSS | 86.594 MiB | 56.172 MiB | 0.649 |
| Mean sampled CPU | 7.913% | 6.429% | 0.812 |
| Peak sampled CPU | 12.7% | 9.1% | 0.717 |

This is a short local soak, not a stability claim. It does establish that the
Rust scheduler retained its earlier memory advantage under active replication
and recovery rather than only under a single-process search benchmark.

This remains a complexity probe, not complete federation parity. The Rust path
now dynamically reconciles workers without restart and rolls filesystem state
back when a non-destructive trace transaction returns an error. Atomic writes
and durable recovery records now cover process death for transactional create,
replace, move, delete, purge replay, and watcher reconstruction paths on the
next Rust open. The branch Go build now refuses mixed-runtime takeover until
Rust recovery completes, and database corruption fails without rewriting data.
Explicit restore from a prior archive and hash-bound recovery of interrupted
restore transactions plus atomic configuration persistence are now available,
while corrupt-database salvage/status, compatibility with older unaware Go
binaries, and power-loss qualification remain open. Its consolidation election and
coordination wire, pass gate, watchdog recovery, full cadence, heuristic pass,
graduation, and model-driven distillation are now present. A bounded real-model
comparison also passes, but broader distillation-quality evaluation remains
advisable.

## Large-corpus and concurrent MCP comparison

The scale harness uses stripped release binaries, alternating implementation
order, a warmed stdio connection, exact trace-count checks, and `verify` after
seeding and every mixed workload. Each measured scenario starts fresh server
processes so a broad result cannot contaminate later RSS samples. Rust's
`list_traces` and `search_traces` output was also aligned to Go's compact row
format after the first campaign exposed that Rust's pretty full-row JSON made
the response and retained allocations substantially larger.

The final 10,000-trace campaigns used five runs and four clients. The final
100,000-scale campaign used five runs, four clients, and independent
byte-identical copies of one verified 100,150-trace corpus. All campaigns
passed graceful shutdown, database verification, and exact post-write counts.

Two Rust durability profiles were measured. `standard` is the selected
default and matches Go's mutation posture: direct trace-file writes and one SQLite
transaction, without per-mutation recovery records, trace/recovery locks,
atomic temporary-file replacement, or file/directory fsync. The selected
profile is exposed in `cortex_usage.runtime.durability_profile`; unknown values
fail closed. `NOEMA_DURABILITY=strong` opts into the recovery protocol
described above.

The final standard-profile read and mixed results were:

| Scale and scenario | Go ops/s | Rust ops/s | Go RSS | Rust RSS |
| --- | ---: | ---: | ---: | ---: |
| 10k serial broad | 9.77 | 14.58 | 64.3 MiB | 81.2 MiB |
| 10k serial selective | 6,100.91 | 8,834.44 | 36.5 MiB | 25.8 MiB |
| 10k four-client broad | 33.08 | 37.63 | 196.4 MiB | 188.0 MiB |
| 10k four-client selective | 6,099.24 | 7,020.06 | 102.5 MiB | 84.9 MiB |
| 10k mixed read/write | 3,412.50 | 4,311.13 | 109.6 MiB | 85.8 MiB |
| 100k serial broad | 0.73 | 1.39 | 298.2 MiB | 642.1 MiB |
| 100k serial selective | 5,587.46 | 8,154.33 | 98.6 MiB | 44.0 MiB |
| 100k four-client broad | 2.38 | 3.69 | 956.4 MiB | 957.2 MiB |
| 100k four-client selective | 5,265.01 | 7,992.39 | 168.0 MiB | 103.0 MiB |
| 100k mixed read/write | 2,977.24 | 4,203.50 | 175.6 MiB | 104.0 MiB |

Rust is faster in every final throughput median. It uses materially less RSS
for selective and mixed workloads. Full-corpus broad output is the exception:
one Rust process retained about 642 MiB versus Go's 298 MiB at 100,000 traces,
while four-process aggregate broad RSS was effectively tied near 957 MiB.

The final mutation comparison separates the like-for-like runtime result from
the cost of Rust's additional durability option:

| Runtime and profile | 10k seed | 10k mixed throughput | Median write latency |
| --- | ---: | ---: | ---: |
| Go current | 6.596 s | 3,412.50 ops/s | 0.354 ms |
| Rust standard | 3.096 s | 4,311.13 ops/s | 0.301 ms |
| Rust strong | 92.987 s | 252.75 ops/s | 9.128 ms |

Rust standard is the Go-equivalent comparison: it seeded 2.13x faster,
sustained 1.26x the mixed throughput, and had 15% lower median write latency at
10,000 traces. At 100,000 traces it sustained 1.41x the mixed throughput with
23% lower median write latency.

The Rust-strong campaign also recorded an ordinary Go repeat at 6.491 seconds,
3,299.16 mixed ops/s, and 0.348 ms median write latency. That repeat confirms
the baseline was stable; Go does not implement the recovery protocol and the
repeat is not a `strong` profile.

Inspection confirmed a policy difference rather than a language-runtime limit.
For each mutation Rust durably inserts a recovery record, acquires trace and
recovery locks, writes and fsyncs an atomic temporary trace, renames it, fsyncs
the trace directory, and commits the database while clearing the recovery
record. Go writes the trace file and performs one SQLite transaction without
per-trace file or directory fsyncs. Activity Monitor qualitatively showed more
than 12 GiB of cumulative writes during the Rust 100,000-trace seed, but that
observation had no controlled Go child counter and is not used as a numeric
cross-runtime result.

The profile experiment also exposed an independent Rust create-path defect:
new traces used update-style `DELETE FROM traces_fts WHERE id=?` before every
FTS insert. Because `id` is a stored FTS column rather than its indexed rowid,
the delete scanned an increasingly large virtual table. Create now uses
insert-only tag, lineage, and FTS helpers; update paths retain delete-and-insert
replacement. This removed the corpus-size regression and moved standard mode
from below Go to ahead of it at both measured scales.

Standard mode deliberately does not pass the strong process-death guarantee:
a killed or power-lost writer can leave a truncated/updated trace file without
its matching database transaction, or vice versa. The opt-in strong profile
continues to pass the subprocess crash-recovery suite. Standard is the
release default so migration from Go does not introduce an immediate
performance regression; users who prioritize the enhanced recovery contract
can select strong explicitly.

## Current interpretation

The additional work further weakens the case for stopping immediately: Rust can
interoperate with the Go federation wire protocol in both directions, express
the critical signing/vector-clock rules cleanly, use materially less memory,
remain competitive for smaller cortexes, and now sustain lower measured RSS and
CPU during a short replicated workload. Exact consolidation prompt/request
parity and blinded real-model evaluation now establish bounded quality parity
rather than a Rust prompt-cost advantage. Watcher behavior also matches the
tested Go outcomes, as do deterministic semantic indexing/ranking, the advanced
MCP contract, and the functional TUI state model. TLS certificate lifecycle checks
now also match the deterministic Go oracle, while returned database failures
and tested process death have stronger filesystem recovery in the Rust
experiment. Cross-runtime backup restore now supplies an explicit recovery
path from a known-good archive, and non-mutating status plus explicit
resume/rollback reconciles interrupted restore transactions without exposing
journal contents. Corrected 10,000- and 100,000-scale testing now shows that
large-result read throughput and isolated-scenario memory are strengths, not
remaining objections. Standard mode also meets the measured mutation
performance gate, while strong mode preserves the enhanced recovery contract
at a large write cost. The production default is now resolved as standard.
It still does not justify an unconditional replacement until multi-emulator
human TUI validation, broader distillation-quality evaluation, power-loss fault
injection, older-Go recovery, and corrupt-database salvage also remain.
