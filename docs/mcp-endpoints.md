# MCP tool endpoints

A Noema HTTP server serves multiple MCP endpoints on the same host and port.
Existing clients can keep using `https://noema.example.com:3000/mcp`: this endpoint
always exposes all tools, including tools added in future versions. Narrower
endpoints are optional views of the same cortex, not separate memory stores.

| Path | Current tools | Intended use |
| --- | ---: | --- |
| `/mcp` | 35 | Complete interface and compatibility with existing clients |
| `/mcp/agent` | 24 | Everyday agent memory work |
| `/mcp/maintainer` | 30 | Everyday work plus diagnosis and maintenance |
| `/mcp/curator` | 26 | Everyday work plus deliberate memory consolidation |
| `/mcp/federation` | 3 | Exchange between Noema instances |

Counts describe the current tool inventory. `tools/list` is authoritative.
All five paths also accept a trailing slash. Unknown paths return 404 rather than
falling back to the full interface.

## Tool membership

The agent endpoint exposes:

- Guidance and identity: `get_instructions`, `cortex_usage`, `cortex_identity`.
- Retrieval: `list_traces`, `get_trace`, `search_traces`, `recall_context`,
  `find_similar_traces`.
- Capture and editing: `create_trace`, `create_traces`, `update_trace`, `append_trace`.
- Tags and engagement: `set_trace_tags`, `append_trace_tags`, `tag_stats`,
  `vote_trace`, `search_activity`.
- Lifecycle and provenance: `archive_trace`, `unarchive_trace`, `delete_trace`,
  `recover_trace`, `trace_history`, `trace_lineage`, `resolve_divergence`.

The maintainer endpoint includes all agent tools plus `tag_doctor`, `rename_tag`,
`delete_tag`, `metrics_summary`, `consolidation_health`, and `federation_status`.

The curator endpoint includes all agent tools plus `list_consolidation_candidates`
and `record_consolidation_result`.

The federation endpoint exposes only `cortex_identity`, `sync_events`, and
`sync_read_signal`. Noema's existing federation client continues to construct
`/mcp` from the configured peer base URL; existing peer configurations need no
changes. The dedicated endpoint is available to clients that select an MCP URL
directly.

`announce_peer` remains available through the full interface only. It acknowledges
an announcement and reports whether a peer is configured; it does not configure
or connect that peer.

## Behavior and access

Each role uses an explicit allowlist. New tools automatically appear at `/mcp`,
but must be deliberately added to narrower roles. Omitted tools are unavailable
through both `tools/list` and `tools/call`. MCP sessions are scoped to the endpoint
that created them: reconnect when changing endpoint URLs.

All endpoints share the cortex connection and background workers. Retrieval keeps
its existing access counters, search-hit tracking, and read hints regardless of
endpoint. Role selection does not make usage tracking a separate operation.
`get_instructions` includes a concise, role-aware directory of the other endpoints
and explains when a different MCP connection is needed. It does not include the
other tools' schemas or imply that the client can automatically switch endpoints.
`cortex_usage` reports `runtime.mcp_tool_profile`, `runtime.mcp_tool_count`, and
`runtime.mcp_endpoints`. The directory provides relative paths, profile purposes,
the current endpoint, and the count of additional tools beyond the current profile.
Stdio guidance makes HTTP availability conditional rather than assuming an HTTP
server is running.

Existing authentication, Host/Origin validation, TLS configuration, and federation
write restrictions apply across the endpoints. A role URL is not an authorization
boundary: a credential accepted by the server can also access `/mcp`. Use these
endpoints to select an appropriate tool set, not to grant different users different
permissions.

## Stdio and existing process profiles

Stdio clients can set `NOEMA_MCP_TOOL_PROFILE` to `agent`, `maintainer`, `curator`,
`federation`, or `full` (the default). The existing `continuity-read` and
`continuity-capture` profiles remain supported for bounded continuity integrations.

For HTTP, choose the endpoint URL instead of a process-wide profile. An unset or
`full` profile is accepted. A restricted or unknown `NOEMA_MCP_TOOL_PROFILE` causes
HTTP startup to fail with migration guidance, rather than silently turning a
previously restricted process into a full-access interface. Remove that environment
setting and configure clients to use the appropriate endpoint.
