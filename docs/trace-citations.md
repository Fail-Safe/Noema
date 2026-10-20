# Trace citations

Durable trace identity remains `YYYYMMDD-slug` on disk and in JSON `id` fields.

For chat, copy/paste, and agent-facing prose, cite traces as:

```text
noema:20261019-adopt-noema-citation-scheme-without-renaming-trace-ids
```

Also accepted on tool/CLI input:

```text
noe://trace/20261019-adopt-noema-citation-scheme-without-renaming-trace-ids
```

Noema normalizes those forms to the bare ID before lookup. Agents that see a
`noema:` token should call `get_trace` immediately. This scheme does not rename
existing files or rewrite historical IDs.

## Tags and lineage inputs

As part of the same naming-schema work, MCP inputs for `tags` and
`derived_from` accept either:

- a JSON string array: `["from-assistant", "project-noema"]`
- a comma/semicolon-separated string: `from-assistant,project-noema`

Noema normalizes those inputs to a string list and persists them as YAML lists
for Obsidian Properties. It does not write CSV onto disk. Stringified JSON
arrays (a JSON array encoded inside a string) are rejected.
