# Conditional preference retrieval

Status: unreleased protocol extension, implemented for private testing. This
document describes the extension's contract; it does not imply that a published
release already supports these fields. Clients must accept older servers that
return full preference bodies without a revision.

`recall_context` can omit preference bodies that a client still has in its current
context. This is optional: callers that do not send a revision continue receiving
the preference bodies on every request.

First load preferences normally:

```json
{"queries": [], "include_preferences": true}
```

The response includes `preferences`, an opaque `preferences_revision`, and
`preferences_unchanged: false`. Save the revision alongside the returned
preference view. While those bodies remain available in the same context, send:

```json
{
  "queries": ["Which deployment procedure applies?"],
  "include_preferences": true,
  "known_preferences_revision": "<revision from the previous response>"
}
```

If the revision matches, `preferences_unchanged` is true and `preferences` is
empty. Retain the previously loaded preference view. Task-query matches are
still retrieved and returned normally.

If `preferences_unchanged` is false and `preferences_revision` is present,
replace the saved preference view with the returned `preferences`. This includes
an empty array: it can mean that all active preferences were removed. The
revision covers the cortex, active set, metadata, indexed body hashes, requested
body limit, and file-state checks. A tag removal, archive, body or metadata edit,
new preference, or changed body limit causes a refresh. The existing
`preference_results_truncated` flag remains meaningful when the revision matches.

When `include_preferences` is false, the revision is null; leave the preference
cache alone. Servers without this extension return full preference bodies and
no revision. Clients should accept those responses without conditional reuse.

Clear the saved revision when switching cortex/server or changing the body
limit. Clear it after a new session, context reset, or compaction that
may have removed the bodies. An application retaining a copy in its own memory
does not establish that the agent model still has the instructions. Either
reinject the cached bodies into the new context or request a full response.

Preference startup reads default to `record_usage: false`. If explicitly enabled,
usage recording also counts preference IDs reused through a matching revision.

Conditional reuse currently uses Unix file identity/change-time checks. Other
platforms conservatively return full bodies. This revision is a local view token,
not a portable trace identity or a global federation version.

For a quantitative test over a real isolated MCP stdio connection, run:

```sh
python3 scripts/bench-preference-revisions.py \
  --binary target/release/noema \
  --output /tmp/preference-revision-results.json
```

The benchmark uses disposable configuration and storage, verifies client context
reset and preference addition, alternates full and conditional requests, and
reports round-trip latency plus actual request/response bytes. An optional
`--bodies` JSON array supplies a local preference-body corpus. Bodies are omitted
from the report. The benchmark accepts up to 23 bodies to leave one slot for its
addition check. Response-byte savings are not a measured billable-token count.

## Agent and integration adoption

`get_instructions`, structured `cortex_usage` startup guidance, and managed
bootstrap text explain this workflow. Managed bootstrap changes take effect
after reinstalling or refreshing that integration. Agents should retain the
opaque revision with its corresponding view and use it on subsequent calls;
it is not a permanent preference identity.

An integration may automate reuse only when it can track the model context's
lifecycle. Reset/compaction events must clear the revision or explicitly restore
the matching bodies. Integrations that do not request preferences need no
preference cache. Clients that omit the revision remain compatible.

## Lifecycle-aware client component

`plugins/hermes/preference_context.py` provides `PreferenceContext`,
`PreferenceScope`, and `RecallContextClient` using the Python standard library.
The component is bundled by the Hermes plugin installer, but Hermes' existing
preference-free bounded search does not automatically use it.

The host supplies a cortex ID, server endpoint identity, and body limit. The
wrapper removes caller-supplied revision tokens and attaches one only after
`mark_delivered(delivery)` confirms that the matching view is available in the
model context. Call that method after delivering a full view, not just after
receiving it over the network. A false return means the context/request changed;
do not deliver a response whose generation is stale.

```python
scope = PreferenceScope(cortex_id, server_endpoint, max_body_chars=4000)
client = RecallContextClient(call_decoded_recall_context, observer=log_call_shape)
delivery = client.recall({"queries": [question], "record_usage": False}, scope)
if delivery is not None:
    deliver_to_model_context(delivery.output)
    client.context.mark_delivered(delivery)
# On a lifecycle event, before accepting further responses:
client.context.invalidate("compaction")
```

The host must serialize context delivery with reset events. The wrapper rejects
responses from older context generations and older preference requests, but it
cannot stop a host from injecting a response after checking it and racing a
reset. It never persists preference bodies or tokens across sessions. Observer
events contain only call count, outcome, and a fixed reason; no bodies, tokens,
or scope identifiers are logged. Preference-free calls preserve the view; older
servers without revisions receive full-response handling.

## Cursor startup bridge

The managed Cursor hook uses `noema preference-snapshot --max-body-chars 12000`.
This read-only CLI command calls the same snapshot implementation as MCP and
returns the preference view, opaque revision, cortex identity/directory, and body
limit. It defaults to no usage recording and performs no task search or decision
evaluation. The hook injects the bodies and revision together for a new context.

Reuse that startup revision only with a local stdio MCP connection serving the
same cortex and the same body limit. Unknown or remote connections must start
with a full MCP load. A truncated snapshot falls back to full CLI preference
bodies without a revision; older binaries also use that fallback. A malformed or
failed snapshot emits a redacted failure instead of an invented revision.

This bridge enables an agent to make its first matching mid-session call
conditional. It does not rewrite Cursor MCP arguments or automatically track
compaction for Cursor. Agents must discard the token after context loss; lifecycle-
aware hosts should use the component above. The benchmark script uses that
component against a real MCP server with explicit simulated-context delivery.
