# Decision-assisted retrieval

Noema can optionally use a local or hosted decision provider to filter and
score retrieval candidates before they are returned to an agent. This is an
additive evaluation path over native lexical/vector retrieval. When the
provider is disabled, unavailable, or returns an invalid response, Noema keeps
native retrieval results.

The default local endpoint is `http://127.0.0.1:18788/v1/decision`. Providers
that still expose the older TypeSafe-compatible path `/v1/systemone` can be
selected with an explicit `endpoint:`.

## Native prefetch planning

`noema prefetch` searches and qualifies ordinary questions independently before
merging their evidence. It recognizes question marks, question lines, and common
list/label framing; unrecognized imperative requests retain whole-prompt lookup.
Unrelated procedural sentences do not consume a recognized question's term budget.

Inside an explicit Questions section, compact colon-prefixed identifiers are
question labels, not fact constraints. Put requested record identifiers in the
question text. Recognized coordinated subject clauses can be searched separately;
other conjunctions retain whole-task lookup. Generic requested-value framing can
move the evidence anchor to its subject without removing words from the coverage
denominator. Compound constraints count once, not as separate spelling variants.

Planning is bounded to eight tasks and sixteen normalized terms per task. Candidate
selection round-robins across searches, deduplicates records, and retains the shared
limit of `min(max_results * 2, 32)`. The result, preference, rescue, and rendered-character
limits remain separate and unchanged. `query_term_count` counts distinct planned
terms across tasks, so it can exceed sixteen.

Rendered-character limits include the preamble and provenance metadata. A successful
search can still return incomplete evidence: inspect `truncated`, and do not treat
partial values or provenance as complete. Client-hook budgets may be smaller than
the CLI default; pin those budgets when comparing retrieval conditions.

Explicit identifiers, versions, and named scope constraints are reserved before
term truncation. Overconstrained tasks fail closed. Matching an identifier does not
override a contradictory topic or a missing identifier in the same question;
independent questions can retrieve different records. Hyphenated and compact
structured-identifier spellings are searched together.

This remains conservative lexical retrieval, not a general semantic question parser.
Some natural-language framing can still prevent a match. Qualification thresholds
have not been lowered. Scope labels are lexical constraints, **not access control**:
search still uses the selected Cortex. Use separate Cortexes when records must be
inaccessible across projects.

## Provider configuration

Configure under `decision` in the Noema config file:

```yaml
decision:
  enabled: auto
  provider: auto   # auto | local | hosted
  model: clef-flash
  timeout_ms: 30000
  token_env: TYPESAFE_API_KEY
```

| `provider` | Behavior |
|---|---|
| `auto` (default) | Infer from model: `clef-flash` uses local activation; other models use hosted credentials |
| `local` | Local decision endpoint only; hosted credentials are rejected |
| `hosted` | Hosted TypeSafe credentials required |

Local mode never auto-falls back to hosted evaluation.

## CLI

`noema decision status|metrics|probe|gate|pair` inspects activation and runs
advisory checks. See [native decision evaluation](testing/native-decision.md).

## Retrieval metrics

Builds from this release record retrieval evaluation attempts under
`decision_retrieval` in local operation reports. Successes and native-result
fallbacks use the same provider-independent name. Migration 024 renames existing
`jev_retrieval` rows without changing timestamps, durations, or outcomes, keeping
historical and new samples in one reporting series.

These rows count retrieval evaluation passes, not all memory operations or
individual provider batches. Operation success does not measure decision quality.

## Related

The trace-body cache and conditional preference revision protocol are separate
from evaluator selection. See [conditional preference retrieval](preference-revisions.md)
for safe context reuse. Revision reuse does not authorize a different processing
destination or prove that preference bodies survived client compaction.
