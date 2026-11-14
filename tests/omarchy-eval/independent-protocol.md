# Independent-corpus equal-access campaign protocol

## Status and scope

This is a new descriptive experiment, not a qualification checkpoint or a
superiority test. It does not reopen or pool with the September 2026 campaign or
the later two-vocabulary equal-access experiments. Natural capture and additional
models are separate replications, not part of this fixed-model target.

The protocol, generator, driver, existing harness, response schema, private
configuration, plan, selected model, and pinned executables must be hash-locked
before the live smoke. Once holdout collection begins, do not tune against its
prompts, answers, records, or failures. A changed implementation requires a new
run and retention of the previous attempt; it cannot silently replace evidence.

### Admission-gate revision

The initial development pilot completed its integration mechanics but failed a
perfect-accuracy admission rule. Before any holdout collection, the operator
approved replacing that rule with seed-count, transport/delivery, and
direct-retrieval positive controls. The failed pilot and its frozen source remain
separate evidence. Neither the corpus generator nor Noema is changed to improve
the observed natural-query result. Natural-query accuracy and safety errors in
every condition are outcomes, not admission criteria.

## Sampling and allocation

- Generate 30 corpus draws: 10 small (60 records), 10 medium (180), and 10 large
  (600). Randomize corpus order from private entropy; retain the ordered plan and
  all derived corpus seeds privately for reproduction.
- Each corpus contributes all six existing scenario families in both handoff
  directions and all three conditions: 36 retrieval turns, 1,080 campaign turns.
- Draw entities, topics, record selection and placement, distractor assignments,
  supersession chains, and question wording from the frozen generator rather
  than repeating only alpha/beta marker substitutions.
- Use natural entity/topic cues rather than asking the receiver for a literal
  memory ID. Answers still require synthetic value, rationale, and provenance
  markers for deterministic scoring; this is not a semantic prose evaluation.
- Include plausible competing records, both project scopes, missing memories,
  and supersession chains of three to five records. Retain obsolete records.
- Counterbalance alpha/beta vocabulary by corpus/direction. Unlike the older
  two-arm design, all three arms in a matched task use identical literal records,
  questions, and provenance. Vocabulary varies by corpus and direction. Condition
  order rotates across scenarios, directions, and corpus assignments; draws have
  independently assigned content and order.
- Use three separate fixed pilot corpora, one per size, for an 18-turn smoke
  (same-workspace exact, both directions, all three arms). Pilot rows never enter
  the 1,080-turn campaign summary. Development tests and pilot seeds are separate
  from the privately generated holdout seed.

These are independent draws from a fixed synthetic generator, not 30 independent
real-world projects, new scenario families, or independent model providers.
Questions within a corpus and handoff directions remain correlated.

## Conditions and access

`native` retains ordinary isolated hierarchical `AGENTS.md` discovery, including
its sibling-workspace and outside-workspace boundaries. `markdown` makes the full
selected corpus reachable through scope-labelled Markdown in each receiving
workspace. It does not receive only an answer-relevant subset. `noema` seeds that
same record set in its selected isolated Cortex and supplies the existing bounded
prefetch, without Noema MCP retrieval tools in the receiving client.

Wrong-scope records remain accessible in shared Markdown and the selected Cortex;
scoping measures answer selection, not access-control enforcement. Other corpora,
directions, and their client/Cortex state remain physically isolated. This is not
a test of project permissions. Inaccessible projects would require corresponding
separate Cortexes and separate Markdown sets.

For this experiment only, use the same explicit 1 MiB Codex instruction-file byte
limit in every arm's isolated configuration, rather than silently truncating
larger Markdown corpora at an incidental default. Record materialized memory
bytes, record counts, and private content hashes. Provider context-limit errors
are retained as failures. Successful client responses do not attest that every
record was attended to or that no undocumented upstream truncation occurred.
Full Markdown and bounded prefetch intentionally differ in prompt size,
serialization, selection, and delivery. Their contrast is not pure ranking quality.

## Fixed execution controls

Use one explicit model in both clients, with medium reasoning; pin executables
and keep their hashes stable across the campaign. Run sequentially, without
concurrent experimental model turns, to reduce avoidable timing interference.
Preserve isolated client homes, disabled unrelated integrations/autoupdate,
benchmark-owned workspace roots, guard hashes, and owner-only private artifacts.
No live user Codex, OpenCode, Noema, or work-root instructions may be edited.

Preflight must pass. All 18 pilot turns must pass mechanics and
managed-restoration/guard gates before campaign collection. Each of the six
pilot Cortexes must contain the expected number of records and pass a model-free
positive control: prefetch using a known record's literal title must return that
record's exact value marker. The natural question prompts are unchanged.
Seeding CLI writers must have exited before the count check, which reads the
checkpointed SQLite snapshot without creating write-ahead-log sidecars.
Empty natural-query retrieval, wrong answers, and safety errors are measured
outcomes for all arms; they do not fail admission when delivery is valid.
Do not import a smoke from another experiment. A mechanics or positive-control
failure stops collection.

During the campaign, an accuracy or safety error is an observed outcome, not a
reason to discard or retry a row. An integration failure, invalid event/response
stream, failed prefetch, or restoration/guard failure stops live collection and
leaves an explicit incomplete report. Do not retry failed turns or publish a
success-only subset. Persist each completed turn and attempt so an interruption
cannot silently overwrite evidence. Any supported resume may execute only
unattempted work under the same frozen plan and hashes; scored failures remain.

Archive and restore each corpus's managed workspace/Cortex/client state. Finish
with formal restoration of the parent run and removal of pinned executables.
Guard drift must be reported without restoring unrelated live user changes.

## Outcomes and analysis

Use the existing exact-value token-set F1, applicable-dimension macro F1,
rationale/provenance markers, stale answers, hallucinations, scope leakage,
token accounting, tool observations, and latency conventions. Report unavailable
costs as unavailable. Keep failed rows and observation denominators explicit.

For complete matched triples, report in percentage points:

- Total: Noema minus hierarchical native.
- Access/reachability: shared Markdown minus hierarchical native.
- Retrieval/evidence-delivery package: Noema minus shared Markdown.
- Residual: total minus the other two components; this closes algebraically and
  does not estimate an unmeasured causal interaction.

Give overall results and separate size-stratum, direction, scenario, boundary,
update, scoping, and same-workspace summaries. Report corpus and matched-task
counts, attempted/completed/failed/missing turns, and token/latency observation
coverage. The planned complete campaign is required for a complete headline;
interrupted reports must distinguish observed outcomes from missing evidence.

The uncertainty unit is the corpus. Use a deterministic descriptive paired
bootstrap over whole corpus groups, stratified by size (10 corpus draws per
stratum), preserving all scenarios, directions, and conditions in each sampled
group. Aggregate the corpus means with equal weight across the balanced strata.
Do not present a turn-level bootstrap as if 1,080 observations were independent.
For stratum-specific intervals the cluster count is only 10. Report the actual
cluster counts for every subset.

No superiority or equivalence decision is preregistered. Precision is reported,
not manufactured by a success threshold. A zero-width empirical interval at
ceiling describes the sampled corpus effects; it does not establish zero
population uncertainty or universal equivalence. Rule-of-three failure bounds
must not treat correlated turns as independent trials. Findings remain limited
to this generator, sampled tasks, corpus sizes, clients, model, and delivery paths.

## Privacy and reporting

Seeds, full plan material, memory files, source records, prompts, responses,
event streams, client snapshots, Cortexes, and detailed errors stay owner-only
under ignored `.private/`. Reports expose only reviewed aggregate metrics,
opaque corpus/case labels, counts, size labels, hashes where appropriate, and
fixed error codes. Never publish answer keys, retrieved bodies, or secrets.
Sanitized reports remain ignored until explicitly reviewed for publication.
