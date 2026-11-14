# November continuity follow-on: reviewed numerical evidence

This is a curated, immutable-intent snapshot of eight November descriptive attempts, not the benchmark's working output directory. It contains 2,448 main-phase rows and 104 separately identified smoke rows. September qualification tables and their interpretation are unchanged.

**Evidence level: analysis-reproducible, not raw-response grading-verifiable.** These are already-scored numerical observations. Readers can recompute aggregates and uncertainty, but cannot independently check each grading decision or replay the exact private client exchanges from this bundle. Inputs, responses, event streams, Cortex contents, reconstructive seeds, local configuration, and private implementation archives are withheld. The public generator supports fresh synthetic replications, not recovery of the withheld instances.

## Files and boundaries

- `manifest.json`: full attempt ledger, model/client versions and hashes, character budgets, explicit projection schema, restoration outcomes, and data checksums. Opaque corpus order is included only to reproduce the frozen bootstrap; it does not include seeds.
- `data/<run-id>/turns.json`: every main-phase scored row for that attempt, including unfavorable outcomes. The two unstarted campaigns have empty arrays, not zero-scoring campaign results.
- `data/<run-id>/smoke-turns.json`: pilot observations, never pooled into main samples. A pre-model execution failure is retained as a failed attempt; its sentinel zero latency is not an observed zero-latency model call.
- `aggregates.json`: reproducible analysis of the projected rows. Incomplete campaigns have no headline. Missing measurements remain null; unavailable monetary-cost comparisons must not be interpreted as free inference.
- `analyze.py`: portable, stdlib-only offline statistical analysis; no model calls.
- `profile_hook.py`, `profile_driver.py`: portable sources for a new explicit 8,000-character replication. These are **not byte-identical to the private tested adapters/launchers**, and are not a new live qualification result.
- `SHA256SUMS`: checksums of this public snapshot, excluding the checksum file itself. Integrity checks do not authenticate private grading evidence.

The projection drops `condition_metadata`, corpus/document fingerprints, machine paths, private configuration and guard details, arbitrary exception text, and unlisted fields. It retains scores, numerical timing/token/safety observations, closed categorical fields, and opaque generated identifiers. Equal-access cycle rows carry explicitly declared derived single-corpus grouping annotations from the 60-record harness design; those annotations are not independently sampled corpora or original observation fields. Scores were not changed during projection.

The working reports remain ignored. Private originals and the publication-review trail remain in a private evidence store; they are not copied wholesale into this snapshot. This bundle deliberately does not include the working directory's September placeholders or every offline diagnostic invocation.

## All November attempts retained

| Run suffix / stage | Main rows | Smoke rows | Disposition |
|---|---:|---:|---|
| `equal-access-20261113T013424Z-c1` | 36 | 6 | Equivalent-token cycle 1; native hierarchy retained |
| `equal-access-20261113T013424Z-c2` | 36 | 6 | Mirrored cycle 2; not an independent new corpus family |
| `independent-20261113T020213Z` | 0 | 18 | Pilot completed mechanically but failed its perfect-accuracy admission rule; no campaign |
| `independent-20261113T024138Z-g2` | 0 | 2 | One completed Markdown model call and one failed Noema seed-control attempt before its model call; no campaign |
| `independent-20261113T024831Z-g3` | 1,080 | 18 | Original negative, natural-cue campaign; retained unchanged |
| `question-scope-20261113T071241Z-s1` | 108 | 18 | Retrieval fixes, original 2,500/8,000-character task budgets; subsequently used for diagnosis |
| `question-budget-20261113T075731Z-s2` | 108 | 18 | New symmetric 8,000-character profile; profile-selection sample |
| `frozen-budget-validation-20261113T172425Z` | 1,080 | 18 | Larger untouched confirmation of S2's frozen profile |

The original pilot required perfect exact/provenance accuracy and clean safety/mechanics. Before the original 1,080-turn campaign, an operator-approved revision admitted mechanics, actual seed counts, direct-retrieval controls, and clean restoration instead. Natural accuracy and safety errors became outcomes for **every arm**, not admission filters. The read-only SQLite seed-control problem was also corrected before that campaign. These changes and the failed attempts are not concealed by the final validation.

The original negative campaign's Noema macro F1 was 15.56%, reflecting abstention/scoping credits with zero factual retrieval in that pinned setup; Markdown was 99.77%. Subsequent native retrieval development changed the implementation. S1 then scored 91.84% Noema macro F1 with three hallucination flags. Its receiving-client cap could truncate needed evidence. S2 used a separately frozen larger budget, not a silent correction to S1 or historical defaults. Because S1 and S2 used different corpora, their model-level difference is not a paired causal estimate of budget alone. Development and profile-selection samples are not pooled with the final validation.

## Final untouched validation

Thirty fresh synthetic corpus draws, ten each at 60, 180 and 600 records; six scenario families, both handoff directions, all three conditions. The 18 pilot turns are excluded from the 1,080 scored turns. All arms within each independent-corpus triple receive identical literal records/questions; the older equivalent-token cycles are identified separately in the manifest.

| Condition | Macro F1 | Descriptive 95% corpus-bootstrap interval | Median total tokens | Median latency |
|---|---:|---:|---:|---:|
| Hierarchical native | 72.13% | 71.94–72.22% | 28,535.0 | 8.78 s |
| Equal-access Markdown | 99.90% | 99.74–100.00% | 39,595.5 | 10.89 s |
| Noema, frozen 8k profile | 99.69% | 99.10–100.00% | 12,600.5 | 10.39 s |

The overall access component is +27.77 percentage points; the remaining retrieval/delivery component is −0.21 points (descriptive interval −0.83 to +0.21). The residual is zero algebraically, not evidence of causal identification. Boundary macro F1 is 16.67% native, 100% Markdown, and 99.90% Noema. The comparison measures the retrieval/delivery package, including bounding and presentation, not isolated ranking quality or workspace permission enforcement.

Noema's pooled-median total-token reduction versus Markdown was 68.18% (descriptive interval 67.83–68.22%); pooled-median latency reduction was 4.62% (3.31–5.75%). These are not monetary-cost savings. Only 180 of 360 turns per final arm reported cost.

This was not perfect: Noema retained one hallucination flag among 660 answers, one failed supersession turn, and one incomplete-provenance outside-work result. Its update-subset macro F1 was 98.33%, versus 100% for both baselines. Markdown had two incomplete-provenance results. All arms had zero stale and scope-leakage flags. All 1,080 turns completed without integration/context-limit failures; restoration and guards passed and pinned executables were removed. Efficiency should not be described as a win at matched quality in the earlier zero-retrieval campaign.

Intervals use 10,000 paired whole-corpus bootstrap draws stratified by size. There are **30 uncertainty clusters, not 1,080 independent replications**, and only ten clusters per size. The two 30-corpus campaigns have cluster intervals; the smaller/equivalent-corpus samples are descriptive without those intervals. The generator/question families and model IDs are fixed; service-level model weights and stochasticity are not independently controlled. Near-parity is observational, not a formal equivalence or superiority result. Zero observed safety errors and zero-width empirical intervals do not establish zero population risk. Real projects and new question families remain untested.

## Offline reanalysis

From this directory:

```sh
shasum -a 256 -c SHA256SUMS
python3 analyze.py --output /tmp/continuity-aggregates.json
```

Select a single run with `--run-id ID`; `--bootstrap-samples N` is a mechanics override, not the published 10,000-draw estimator. No private paths, model credentials, network access, or installed Noema executable are required for offline analysis. Aggregates and cluster/efficiency intervals were compared with the source numerical reports before publication curation. Reanalysis cannot establish whether a private response was graded correctly.

## Fresh operator-authorized replication

The public harness, generator, and [independent-corpus protocol](../../independent-protocol.md) describe the underlying design. Prepare an ignored operator-owned configuration with explicit model IDs, medium reasoning and executable paths as described in the [harness README](../../README.md). From the repository root:

```sh
python3 tests/omarchy-eval/published/continuity-2026-11/profile_driver.py \
  --config /absolute/path/to/local-config.json --run-id public-replication-NEW preflight
python3 tests/omarchy-eval/published/continuity-2026-11/profile_driver.py \
  --config /absolute/path/to/local-config.json --run-id public-replication-NEW smoke --confirm-live
python3 tests/omarchy-eval/published/continuity-2026-11/profile_driver.py \
  --config /absolute/path/to/local-config.json --run-id public-replication-NEW run --confirm-live
python3 tests/omarchy-eval/published/continuity-2026-11/profile_driver.py \
  --config /absolute/path/to/local-config.json --run-id public-replication-NEW restore
```

Preflight performs real executable/model checks; smoke and run incur live model calls. Use a new run ID, retain all outcomes, and always restore even after failure. Fresh plans remain private. The adapter changes only this explicit replication process; the checked-in default qualification hook remains unchanged. New binary/portable-source hashes must be recorded for the new run, not presented as byte-identical to historical private artifacts. This is replication of the public method on fresh instances, not exact replay of withheld instances or qualification of a newly built release.
