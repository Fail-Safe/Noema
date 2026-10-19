# Decision-assisted retrieval

Noema can optionally use a local or hosted decision provider to filter and
score retrieval candidates before they are returned to an agent. This is an
additive evaluation path over native lexical/vector retrieval. When the
provider is disabled, unavailable, or returns an invalid response, Noema keeps
native retrieval results.

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
