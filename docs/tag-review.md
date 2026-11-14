# Tag Review

**AI-assisted taxonomy maintenance**, with human approval of every tag change.
Tag Review is on demand: it does not schedule model calls or automatically apply
suggestions. Each proposal affects one trace, not an entire tag across a cortex.

## TUI

Open `noema tui` and press **T** to switch from traces to Tag Review.

| Key | Action |
| --- | --- |
| `h` | Queue deterministic tag-doctor hygiene suggestions, locally |
| `m` | Request model analysis, after confirming transmission of trace excerpts |
| `s` | Model settings: choose Shared or Separate; configure the separate model |
| `[` / `]` | Previous / next model batch offset (40 eligible traces per batch) |
| `j` / `k` | Select proposal |
| Page Up / Page Down | Scroll rationale, preview, and supporting excerpt |
| `a` | Apply selected proposal, after confirming its exact before/after tags |
| `e` | Edit desired tags as a JSON array; Enter queues the revised proposal |
| `x` / `d` / `p` | Reject / defer / resume |
| `u` | Undo an applied proposal, after confirmation |
| `R` | Refresh queue |
| Escape / `T` | Return to traces |

Model analysis runs off the UI thread, so browsing remains available. Rejections
and deferrals survive restarts; repeating an identical proposal against the same
snapshot does not reset its decision. A changed trace can warrant a fresh review.

## Model configuration and privacy

Semantic Tag Review has a per-cortex **Shared / Separate** model setting. Open
Tag Review and press **s**. Select a field with Tab or the arrow keys; Space or
Left/Right toggles the mode. Type/Backspace edits text, Ctrl+U clears a field,
Ctrl+S saves, and Escape cancels. Settings changes do not run analysis.

- **Shared** (the default when no `tag_review` block exists) uses consolidation's
  `llm_enabled`, `local_llm_endpoint`, `model_name`, and `api_key_env` as a unit.
  Neither scheduled consolidation nor automatic distillation needs to be enabled.
- **Separate** uses its own model, endpoint, and optional credential environment
  variable name. Both model and endpoint are required; nothing silently falls
  back to consolidation, including credentials. Consolidation can remain disabled
  or use a different model. For example, consolidation can use a 9B model while
  deliberate semantic reviews use a 35B model.

On the first toggle to Separate, the TUI pre-fills valid shared settings as an
editable starting point. Change the model and endpoint as appropriate. Returning
to Shared preserves saved separate settings for the next time you switch back.
The review screen shows the active model and mode. Consent pins that destination
for the run; settings changes while analysis is running apply only to future runs.

Settings are saved in `cortex.md`, without modifying consolidation settings:

```yaml
tag_review:
  mode: separate # shared | separate
  model_name: my-review-model-35b
  endpoint: http://127.0.0.1:8001/v1
  # api_key_env: REVIEW_API_KEY # variable name only; credentialed URLs require HTTPS
```

To use consolidation again, set `tag_review.mode: shared` (or remove the block).
Hygiene checks never use a model, regardless of this setting.

The endpoint uses the existing OpenAI-compatible `/chat/completions` convention.
Choose a trusted endpoint before requesting analysis. No provider is selected
automatically, and redirects are not followed.
Credentialed model requests require HTTPS, including on loopback endpoints.

Each review window covers up to 40 eligible active/archived trace titles, tag lists,
and body excerpts (up to 3,000 characters each), sent in smaller
requests of at most 10 traces each. Source-locked traces from another
origin are excluded from trace sampling. A vocabulary sample of up to 200 existing
tags is also sent to encourage reuse rather than inventing synonyms.
This is a bounded sample, not a claim to have reviewed the
whole cortex or every trace body. The summary reports coverage and skipped
suggestions. Use the batch offset to inspect further traces.

If the provider reports truncated output, Noema discards that response and
automatically splits its trace group into smaller requests. It never attempts to
salvage incomplete JSON. Retries use the same selected model and credential;
completed proposals remain available even if a later request fails. Only genuinely
changed tags are requested, with concise rationales, to reduce output pressure.

A review window is bounded to 80 request attempts (enough to split all 40 traces
down to individual requests) and a five-minute model-request budget, with a
three-minute timeout per request. If a single-trace response is still truncated,
the provider fails, or a budget is reached, the result explicitly reports partial
completion, completed/unfinished trace counts, and proposals retained. Retry the
same offset (`m` again in the TUI) to finish; identical proposals are deduplicated.
CLI analysis prints its partial-progress JSON and exits unsuccessfully; MCP
returns `model.status: "partial"` with the progress and warning. A completed run
also reports how many split retries were needed. Existing queued proposals are
never erased by a failed request, and no tags are applied automatically.
`analyzed` counts traces in completed request groups, including traces needing no
changes. `skipped` counts returned suggestions that were not queued (for example,
duplicates or invalid changes), not traces omitted by the model as unchanged.

The model can suggest topical renames, merges, additions, or removals. Instructions
inside trace data are untrusted. Model output is validated and only creates local
proposals. Provider error bodies are not printed.

## CLI and MCP

```sh
noema tags review settings                # safe model/mode status, no API key values
noema tags review settings --mode separate --model my-review-model-35b --endpoint http://127.0.0.1:8001/v1
noema tags review settings --mode shared  # retain saved separate settings
noema tags review analyze                 # local hygiene only
noema tags review analyze --model         # explicitly sends excerpts to configured model
noema tags review analyze --model --offset 40
noema tags review list                    # JSON queue, previews, and history
noema tags review decide REVIEW_ID apply  # preview; requires --yes to change tags
noema tags review decide REVIEW_ID apply --yes
noema tags review decide REVIEW_ID defer
noema tags review decide REVIEW_ID reject
noema tags review decide REVIEW_ID resume
noema tags review decide REVIEW_ID edit --tags '["rust","concurrency"]'
noema tags review decide REVIEW_ID undo --yes
```

The full and maintainer MCP profiles expose `tag_review_queue`,
`tag_review_analyze`, and `tag_review_decide`. Analysis and decisions require a
writable server. Apply/undo require `confirmed=true`, which agents must set only
after human approval of the exact preview. This is an approval contract, not an
independent human identity/authentication mechanism.
CLI and MCP analysis use the same per-cortex model selection, and analysis results
include the model name and mode actually used. Newly started analysis reads saved
settings without requiring a server restart; configuration changes alone do not
enable consolidation or make a model request.

## Safety and persistence

- Behavioral/scope/routing tags (`user-preference`, `user-level`, `project-level`,
  `from-*`, and `to-*`) cannot be changed through Tag Review. Use explicit tag
  editing for intentional changes to these selectors.
- Apply and undo verify the non-tag trace snapshot, visibility, and expected tags.
  Changed content or tags make a proposal stale; changes are not forced through.
- Reviewed mutations use the existing recovery journal even under standard
  durability. The queue transition, tag index, FTS index, and normal `tag_update`
  event commit together. Undo refuses to overwrite later edits.
- Review proposals and decisions live in local SQLite storage; they are not
  federated. Accepted tag changes federate normally. Existing source locks apply.
  Purging a trace also removes its local review records and excerpts.
- Per-trace mutation locks serialize reviewed changes with strong-durability
  writers. As with other mutations, arbitrary external filesystem writers do not
  participate in these locks; this is not a distributed compare-and-swap protocol.
- Back up the Cortex directory, including `db/noema.db` and `cortex.md`, before
  upgrading. The review queue and decision history are local database state;
  rebuilding an index from Markdown alone cannot reconstruct them. Restart
  existing Noema processes with the upgraded executable before using Tag Review.

V1 deliberately excludes global model-driven bulk renames, scheduled reviews,
automatic changes, and ontology/relationship modeling.
