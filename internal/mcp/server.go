package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/federation"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// NewServer builds an MCP server exposing all Cortex operations. The
// version string is plumbed through to the MCP protocol's serverInfo
// (visible to every client in the initialize handshake) and to the
// get_instructions output, so any agent or operator can identify which
// noema build they're talking to without grepping logs. Callers should
// pass cli.version() so the value matches `noema --version`. An empty
// string is normalized to "dev" so the protocol field is never blank.
// NewServer creates a new MCP server for the given cortex. federationMode
// controls tool access: "publish" blocks mutating tools (source of truth),
// "subscribe" blocks sync_events (consumer only), "sync"/empty allows all.
func NewServer(cx *cortex.Cortex, noemaVersion string, federationMode string) *server.MCPServer {
	if noemaVersion == "" {
		noemaVersion = "dev"
	}
	s := server.NewMCPServer("noema", noemaVersion,
		server.WithToolCapabilities(true),
	)

	// readOnlyGuard wraps a tool handler to reject calls when the cortex is
	// in publish mode. Publish-mode cortexes are read-only for remote peers;
	// local writes go through a separate stdio transport process.
	type toolHandler = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
	readOnlyGuard := func(next toolHandler) toolHandler {
		if federationMode != cortex.FederationModePublish {
			return next
		}
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError(
				"this cortex is in publish mode (read-only for remote peers); " +
					"use a local stdio transport for writes"), nil
		}
	}

	s.AddTool(mcp.NewTool("get_instructions",
		mcp.WithDescription("Returns a reference guide for working with this Cortex: terminology, trace types, field definitions, filtering options, and tool usage. Call this first if you are unfamiliar with Noema."),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		m, err := cortex.ReadManifest(cx.Dir)
		if err != nil {
			// Fallback to minimal identity if manifest is unreadable.
			m = cortex.Manifest{Name: cx.Name}
		}
		return mcp.NewToolResultText(renderInstructions(m, noemaVersion)), nil
	})

	s.AddTool(mcp.NewTool("list_traces",
		mcp.WithDescription("List traces in the cortex"),
		mcp.WithString("type", mcp.Description("Filter by trace type")),
		mcp.WithString("author", mcp.Description("Filter by author")),
		mcp.WithString("tag", mcp.Description("Filter by tag")),
		mcp.WithString("origin", mcp.Description("Filter by origin cortex name")),
		mcp.WithBoolean("archived", mcp.Description("Show only archived traces")),
		mcp.WithBoolean("all", mcp.Description("Show active and archived traces")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rows, err := cx.List(cortex.ListOptions{
			Type:     req.GetString("type", ""),
			Author:   req.GetString("author", ""),
			Tag:      req.GetString("tag", ""),
			Origin:   req.GetString("origin", ""),
			Archived: req.GetBool("archived", false),
			All:      req.GetBool("all", false),
		})
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(formatRows(rows)), nil
	})

	s.AddTool(mcp.NewTool("get_trace",
		mcp.WithDescription("Get a trace by ID, including its full body"),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		row, err := cx.GetAs(id, cortex.ActorAgent)
		if err != nil {
			return nil, fmt.Errorf("trace %q not found", id)
		}
		t, err := trace.ParseFile(cx.TraceFile(id, row.ArchivedAt != ""))
		if err != nil {
			return nil, err
		}
		out := fmt.Sprintf("ID: %s\nTitle: %s\nType: %s\nAuthor: %s\nTags: %s\nCreated: %s\nUpdated: %s",
			row.ID, row.Title, row.Type, row.Author,
			strings.Join(row.Tags, ", "),
			row.CreatedAt, row.UpdatedAt,
		)
		if row.Origin != "" {
			out += fmt.Sprintf("\nOrigin: %s", row.Origin)
		}
		if len(row.DerivedFrom) > 0 {
			out += fmt.Sprintf("\nDerived From: %s", strings.Join(row.DerivedFrom, ", "))
		}
		if row.SourceLocked {
			out += "\nSource Locked: yes"
		}
		if row.SourceHash != "" {
			out += fmt.Sprintf("\nSource Hash: %s", row.SourceHash)
		}
		if row.ContentHash != "" {
			out += fmt.Sprintf("\nContent Hash: %s", row.ContentHash)
		}
		out += fmt.Sprintf("\n\n%s", t.Body)
		return mcp.NewToolResultText(out), nil
	})

	s.AddTool(mcp.NewTool("create_trace",
		mcp.WithDescription("Create a new trace"),
		mcp.WithString("title", mcp.Description("Trace title"), mcp.Required()),
		mcp.WithString("type", mcp.Description("Trace type"), mcp.Required(),
			mcp.Enum("fact", "decision", "preference", "context", "skill", "intent", "observation", "note", "divergence")),
		mcp.WithString("body", mcp.Description("Trace body content"), mcp.Required()),
		mcp.WithString("author", mcp.Description("Author name or agent identifier")),
		mcp.WithString("tags", mcp.Description("Comma-separated tags")),
		mcp.WithString("derived_from", mcp.Description("Comma-separated trace IDs this trace was derived from")),
		mcp.WithString("origin", mcp.Description("Origin cortex name (defaults to current cortex)")),
		mcp.WithBoolean("source_locked", mcp.Description("Mark trace as source-locked (immutable on consumer side)")),
		mcp.WithString("source_hash", mcp.Description("Content hash from the source/publisher cortex")),
	), readOnlyGuard(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		title, err := req.RequireString("title")
		if err != nil {
			return nil, err
		}
		traceType, err := req.RequireString("type")
		if err != nil {
			return nil, err
		}
		body, err := req.RequireString("body")
		if err != nil {
			return nil, err
		}
		author := req.GetString("author", "")
		var tags []string
		if raw := req.GetString("tags", ""); raw != "" {
			for _, t := range strings.Split(raw, ",") {
				if tag := strings.TrimSpace(t); tag != "" {
					tags = append(tags, tag)
				}
			}
		}
		t := trace.New(title, traceType, author, tags, body)
		if raw := req.GetString("derived_from", ""); raw != "" {
			for _, id := range strings.Split(raw, ",") {
				if id := strings.TrimSpace(id); id != "" {
					t.DerivedFrom = append(t.DerivedFrom, id)
				}
			}
		}
		if origin := req.GetString("origin", ""); origin != "" {
			t.Origin = origin
		}
		if req.GetBool("source_locked", false) {
			t.SourceLocked = true
		}
		if sh := req.GetString("source_hash", ""); sh != "" {
			t.SourceHash = sh
		}
		if err := cx.Add(t); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace created: %s", t.ID)), nil
	}))

	s.AddTool(mcp.NewTool("search_traces",
		mcp.WithDescription("Full-text search across traces"),
		mcp.WithString("query", mcp.Description("Search query"), mcp.Required()),
		mcp.WithBoolean("all", mcp.Description("Include archived traces")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return nil, err
		}
		rows, err := cx.Search(query, cortex.ListOptions{
			All: req.GetBool("all", false),
		})
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(formatRows(rows)), nil
	})

	s.AddTool(mcp.NewTool("delete_trace",
		mcp.WithDescription("Move a trace to trash (soft-delete, recoverable for 30 days). Use recover_trace to restore it."),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
	), readOnlyGuard(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		if err := cx.Trash(id); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace %s moved to trash.", id)), nil
	}))

	s.AddTool(mcp.NewTool("recover_trace",
		mcp.WithDescription("Restore a trace from trash back to active"),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
	), readOnlyGuard(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		if err := cx.Recover(id); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace %s recovered.", id)), nil
	}))

	s.AddTool(mcp.NewTool("archive_trace",
		mcp.WithDescription("Archive a trace"),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
	), readOnlyGuard(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		if err := cx.Archive(id); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace %s archived.", id)), nil
	}))

	s.AddTool(mcp.NewTool("unarchive_trace",
		mcp.WithDescription("Restore an archived trace"),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
	), readOnlyGuard(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		if err := cx.Unarchive(id); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace %s restored.", id)), nil
	}))

	s.AddTool(mcp.NewTool("update_trace",
		mcp.WithDescription("Update fields of an existing trace. Only provided fields are changed."),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
		mcp.WithString("title", mcp.Description("New title")),
		mcp.WithString("type", mcp.Description("New type"),
			mcp.Enum("fact", "decision", "preference", "context", "skill", "intent", "observation", "note", "divergence")),
		mcp.WithString("author", mcp.Description("New author")),
		mcp.WithString("tags", mcp.Description("New tags, comma-separated (replaces existing tags)")),
		mcp.WithString("derived_from", mcp.Description("New derived_from, comma-separated trace IDs (replaces existing lineage)")),
		mcp.WithString("body", mcp.Description("New body content")),
	), readOnlyGuard(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		row, err := cx.Get(id)
		if err != nil {
			return nil, fmt.Errorf("trace %q not found", id)
		}
		path := cx.TraceFile(id, row.ArchivedAt != "")
		t, err := trace.ParseFile(path)
		if err != nil {
			return nil, err
		}
		if v := req.GetString("title", ""); v != "" {
			t.Title = v
		}
		if v := req.GetString("type", ""); v != "" {
			t.Type = v
		}
		if v := req.GetString("author", ""); v != "" {
			t.Author = v
		}
		if v := req.GetString("tags", ""); v != "" {
			var tags []string
			for _, tag := range strings.Split(v, ",") {
				if tag := strings.TrimSpace(tag); tag != "" {
					tags = append(tags, tag)
				}
			}
			t.Tags = tags
		}
		if v := req.GetString("body", ""); v != "" {
			t.Body = v
		}
		if v := req.GetString("derived_from", ""); v != "" {
			var sources []string
			for _, id := range strings.Split(v, ",") {
				if id := strings.TrimSpace(id); id != "" {
					sources = append(sources, id)
				}
			}
			t.DerivedFrom = sources
		}
		if err := cx.CheckSourceLock(id); err != nil {
			return nil, err
		}
		if err := t.Write(path); err != nil {
			return nil, err
		}
		if err := cx.UpdateAs(id, cortex.ActorAgent); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace %s updated.", id)), nil
	}))

	s.AddTool(mcp.NewTool("append_trace",
		mcp.WithDescription("Append content to an existing trace's body without reading the full trace first. Ideal for fire-and-forget logging, running journals, or any case where an agent needs to add to a trace without consuming its current content."),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
		mcp.WithString("content", mcp.Description("Content to append to the trace body"), mcp.Required()),
	), readOnlyGuard(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		content, err := req.RequireString("content")
		if err != nil {
			return nil, err
		}
		if err := cx.Append(id, content); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Content appended to trace %s.", id)), nil
	}))

	// ---- Event Log & Lineage tools ----

	s.AddTool(mcp.NewTool("trace_history",
		mcp.WithDescription("Show the event log (audit trail) for a trace: all mutations in chronological order."),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		events, err := cx.Events(id)
		if err != nil {
			return nil, err
		}
		if len(events) == 0 {
			return mcp.NewToolResultText("No events found for trace " + id), nil
		}
		var sb strings.Builder
		for _, e := range events {
			sb.WriteString(fmt.Sprintf("%s  %-10s  %s  origin=%s\n", e.ID, e.Action, e.Timestamp, e.Origin))
		}
		return mcp.NewToolResultText(sb.String()), nil
	})

	s.AddTool(mcp.NewTool("trace_lineage",
		mcp.WithDescription("Show the derivation graph for a trace: what it was derived from and what was derived from it."),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		row, err := cx.Get(id)
		if err != nil {
			return nil, fmt.Errorf("trace %q not found", id)
		}
		derivedBy, err := cx.DerivedBy(id)
		if err != nil {
			return nil, err
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Trace: %s\n", id))
		if len(row.DerivedFrom) > 0 {
			sb.WriteString(fmt.Sprintf("Derived from: %s\n", strings.Join(row.DerivedFrom, ", ")))
		} else {
			sb.WriteString("Derived from: (none)\n")
		}
		if len(derivedBy) > 0 {
			sb.WriteString(fmt.Sprintf("Derived by:   %s\n", strings.Join(derivedBy, ", ")))
		} else {
			sb.WriteString("Derived by:   (none)\n")
		}
		return mcp.NewToolResultText(sb.String()), nil
	})

	// ---- Conflict Resolution ----

	s.AddTool(mcp.NewTool("resolve_divergence",
		mcp.WithDescription("Resolve a divergence (concurrent edit conflict). Either accept one of the versions by origin name, or supply a custom merged body."),
		mcp.WithString("id", mcp.Description("Divergence trace ID"), mcp.Required()),
		mcp.WithString("accept", mcp.Description("Origin name whose version to accept. See the '**Conflicting origins:**' line in the divergence body for valid values.")),
		mcp.WithString("body", mcp.Description("Custom merged body content. Mutually exclusive with 'accept'.")),
	), readOnlyGuard(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		accept := req.GetString("accept", "")
		body := req.GetString("body", "")
		if err := cx.ResolveDivergence(id, accept, body); err != nil {
			return nil, err
		}
		if accept != "" {
			return mcp.NewToolResultText(fmt.Sprintf("Divergence %s resolved (accepted %s).", id, accept)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Divergence %s resolved (custom merge).", id)), nil
	}))

	// ---- Federation tools ----

	s.AddTool(mcp.NewTool("cortex_identity",
		mcp.WithDescription("Returns this cortex's stable identity (ULID, name, manifest version). Federation peers call this on every sync to verify the remote endpoint still belongs to the cortex they originally paired with."),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		m, err := cortex.ReadManifest(cx.Dir)
		if err != nil {
			return nil, fmt.Errorf("reading manifest: %w", err)
		}
		payload := map[string]any{
			"id":      cx.ID,
			"name":    m.Name,
			"version": m.Version,
			"mode":    m.Federation.EffectiveMode(),
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshaling identity: %w", err)
		}
		return mcp.NewToolResultText(string(data)), nil
	})

	s.AddTool(mcp.NewTool("sync_events",
		mcp.WithDescription("Returns events from this cortex for federation sync. Remote peers call this to pull new events. Returns a JSON array of event objects."),
		mcp.WithString("since", mcp.Description("ULID cursor — return only events after this ID")),
		mcp.WithNumber("limit", mcp.Description("Max events to return (default 100, max 1000)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if federationMode == cortex.FederationModeSubscribe {
			return mcp.NewToolResultError(
				"this cortex is in subscribe mode and does not serve events"), nil
		}
		since := req.GetString("since", "")
		if since != "" && !event.IsValidULID(since) {
			return mcp.NewToolResultError(
				"since must be a valid ULID cursor (26-char Crockford base32)"), nil
		}
		limit := req.GetInt("limit", 100)
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		events, err := cx.EventsSince(since, limit)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(events)
		if err != nil {
			return nil, fmt.Errorf("marshaling events: %w", err)
		}
		return mcp.NewToolResultText(string(data)), nil
	})

	s.AddTool(mcp.NewTool("federation_status",
		mcp.WithDescription("Show federation configuration, peer sync state, and local vector clock."),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		m, err := cortex.ReadManifest(cx.Dir)
		if err != nil {
			m = cortex.Manifest{Name: cx.Name}
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Cortex: %s\n", m.Name))
		sb.WriteString(fmt.Sprintf("Mode: %s\n", m.Federation.EffectiveMode()))

		// Surface the MCP access posture alongside federation state so a
		// peer calling this tool sees exactly what the middleware is
		// enforcing locally. Resolution errors are non-fatal: print them
		// and keep going rather than blanking the rest of the status.
		if key, kerr := cortex.LoadAccessKey(cx.Dir, m.Access); kerr != nil {
			sb.WriteString(fmt.Sprintf("Access: error loading key: %v\n", kerr))
		} else if key.Keyed() {
			sb.WriteString(fmt.Sprintf("Access: keyed (source=%s, fingerprint=%s)\n", key.Source, key.Fingerprint))
		} else {
			sb.WriteString("Access: open\n")
		}
		sb.WriteByte('\n')

		if m.Federation == nil || len(m.Federation.Peers) == 0 {
			sb.WriteString("Federation: not configured (no peers in cortex.md)\n")
		} else {
			sb.WriteString(fmt.Sprintf("Peers: %d\n", len(m.Federation.Peers)))
			if m.Federation.Interval != "" {
				sb.WriteString(fmt.Sprintf("Interval: %s\n", m.Federation.Interval))
			}
			sb.WriteByte('\n')

			state := federation.NewState(cx.DB.DB)
			for _, p := range m.Federation.Peers {
				ps, err := state.GetPeerState(p.Name, p.Endpoint)
				if err != nil {
					sb.WriteString(fmt.Sprintf("  %s (%s): error loading state\n", p.Name, p.Endpoint))
					continue
				}
				lastSeen := "(never)"
				if ps.LastSeen != "" {
					lastSeen = ps.LastSeen
				}
				lastEvent := "(none)"
				if ps.LastEvent != "" {
					lastEvent = ps.LastEvent
				}
				cortexID := "(unverified)"
				if ps.CortexID != "" {
					cortexID = ps.CortexID
				}
				peerMode := p.EffectiveMode()
				sb.WriteString(fmt.Sprintf("  %s\n    endpoint:   %s\n    mode:       %s\n    cortex_id:  %s\n    last_seen:  %s\n    last_event: %s\n",
					p.Name, p.Endpoint, peerMode, cortexID, lastSeen, lastEvent))
			}
		}

		// Show local vector clock. Vector clocks are keyed on cortex IDs
		// (ULIDs); annotate the local cortex's own bucket with its name and
		// each peer's bucket with its display name when we know it.
		vc, err := cx.GetClock()
		if err == nil && len(vc) > 0 {
			sb.WriteString("\nVector Clock:\n")
			peerNames := make(map[string]string)
			peerNames[cx.ID] = m.Name + " (local)"
			if m.Federation != nil {
				state := federation.NewState(cx.DB.DB)
				for _, p := range m.Federation.Peers {
					if id, _ := state.Get(federation.PeerCortexIDKey(p.Name)); id != "" {
						peerNames[id] = p.Name
					}
				}
			}
			for cortexID, tick := range vc {
				label := peerNames[cortexID]
				if label == "" {
					label = "(unknown peer)"
				}
				sb.WriteString(fmt.Sprintf("  %s [%s]: %d\n", cortexID, label, tick))
			}
		}

		// Show unresolved divergences.
		if n, err := cx.DivergenceCount(); err == nil && n > 0 {
			sb.WriteString(fmt.Sprintf("\nUnresolved Divergences: %d\n", n))
			sb.WriteString("  Use resolve_divergence or `noema resolve` to resolve them.\n")
		}

		return mcp.NewToolResultText(sb.String()), nil
	})

	s.AddTool(mcp.NewTool("announce_peer",
		mcp.WithDescription("Accept a peer announcement from a remote cortex. Returns this cortex's identity for mutual discovery."),
		mcp.WithString("name", mcp.Description("Name of the announcing cortex"), mcp.Required()),
		mcp.WithString("endpoint", mcp.Description("Streamable HTTP base URL of the announcing cortex (the /mcp path is appended automatically)"), mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, err := req.RequireString("name")
		if err != nil {
			return nil, err
		}
		endpoint, err := req.RequireString("endpoint")
		if err != nil {
			return nil, err
		}

		// Validate that the endpoint is a reachable HTTP(S) URL.
		// Accepting arbitrary schemes (file://, javascript:, etc.)
		// risks confusion if the value is later copy-pasted into
		// federation config or rendered in status output.
		u, parseErr := url.ParseRequestURI(endpoint)
		if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return mcp.NewToolResultError(
				"endpoint must be a valid http:// or https:// URL"), nil
		}

		m, err := cortex.ReadManifest(cx.Dir)
		if err != nil {
			m = cortex.Manifest{Name: cx.Name}
		}

		// Reject any announcement that uses our own cortex name. Two
		// participants in a federation cannot share an identity — vector
		// clocks would merge into one bucket and concurrent-edit detection
		// would silently break. See docs/design/cortex-uuid-plan.md.
		if m.PeerLabelCollidesWithSelf(name) {
			return mcp.NewToolResultError(fmt.Sprintf(
				"refusing announcement: name %q matches this cortex's own name. The announcing peer must rename its cortex (in its cortex.md) to a unique value before federation can proceed.",
				name,
			)), nil
		}

		// Check if this peer is already known.
		known := false
		if m.Federation != nil {
			for _, p := range m.Federation.Peers {
				if p.Name == name {
					known = true
					break
				}
			}
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Acknowledged. This cortex is %q.\n", m.Name))
		if known {
			sb.WriteString(fmt.Sprintf("Peer %q is already configured.\n", name))
		} else {
			sb.WriteString(fmt.Sprintf("Peer %q (%s) is not yet configured. Add it to cortex.md to enable sync.\n", name, endpoint))
		}

		return mcp.NewToolResultText(sb.String()), nil
	})

	return s
}

// renderInstructions builds the agent reference guide returned by the
// get_instructions tool. It is split out from the tool closure so the
// (large, easy-to-regress) template can be unit-tested directly without
// standing up a full MCP server. The version parameter is the noema
// binary version (set via -ldflags); the manifest version comes from
// the cortex.md file and bumps when the cortex schema changes.
func renderInstructions(m cortex.Manifest, noemaVersion string) string {
	purposeLine := ""
	if m.Purpose != "" {
		purposeLine = fmt.Sprintf("Purpose:  %s\n", m.Purpose)
	}
	ownerLine := ""
	if m.Owner != "" {
		ownerLine = fmt.Sprintf("Owner:    %s\n", m.Owner)
	}
	return fmt.Sprintf(`# Noema — Agent Reference

## Active Cortex
Name:     %s
Version:  noema %s (manifest v%d)
%s%s
## Terminology
- Cortex: a named collection of Traces (this instance: %q)
- Trace:  a single memory unit — one markdown file with YAML frontmatter +
          a corresponding row in the SQLite index

## Trace Types
Choose the type that best reflects the intent of the memory:

  fact        — a discrete thing that is true
  decision    — a choice made and why
  preference  — a behavioral or stylistic lean
  context     — situational background
  skill       — a learned capability or procedure
  intent      — something that needs to happen (autonomous pickup)
  observation — something witnessed but not yet verified
  note        — anything else
  divergence  — concurrent edit conflict (auto-created by federation sync)

## Trace Fields
  id            required  YYYYMMDD-slugified-title  (e.g. 20260330-why-we-chose-go)
  title         required  short, descriptive
  type          required  one of the types above
  created       required  RFC3339 UTC timestamp (set on creation, never changed)
  updated       required  RFC3339 UTC timestamp (update on every edit)
  author        optional  human username or agent name
  tags          optional  list of keyword strings
  derived_from  optional  list of trace IDs this trace was derived from
  origin        optional  cortex name that created this trace (auto-set)
  content_hash  auto      SHA-256 of body (sha256:<hex>), recomputed on every write
  source_hash   optional  origin's content_hash at publish time (immutable on consumer)
  source_locked optional  when true, consumer-side mutations are refused

## Tools

  get_instructions               → this document
  list_traces [type] [author] [tag] [origin] [archived] [all]
                                 → list traces; defaults to active only
  get_trace id                   → full content including body, origin, lineage
  create_trace title type body [author] [tags] [derived_from] [origin] [source_locked] [source_hash]
                                 → create a new trace; tags/derived_from = comma-separated.
                                   Aim for short descriptive titles (under 80 chars); the ID
                                   slug is capped at 100 chars and longer titles are silently
                                   truncated. Do NOT include a date in the title — the ID
                                   generator prepends today's date automatically, and leading
                                   YYYYMMDD- or YYYY-MM-DD- prefixes in the title are stripped
                                   to prevent doubled IDs like 20260402-20260402-foo. Avoid
                                   mid-title dates too (e.g. "session 20260416 142000") —
                                   only leading prefixes are stripped, so mid-title date
                                   fragments survive. If a trace is *about* a specific date,
                                   put it in a tag (e.g. tags=event-2026-04-02) or the body
  update_trace id [title] [type] [author] [tags] [derived_from] [body]
                                 → update any subset of fields
  append_trace id content        → append content to an existing trace's body
                                   without reading the full trace first; ideal
                                   for running logs and fire-and-forget writes
  search_traces query [all]      → FTS5 full-text search
  archive_trace id               → move to archive (reversible)
  unarchive_trace id             → restore from archive
  delete_trace id                → move to trash (soft-delete, recoverable)
  recover_trace id               → restore a trashed trace to active
  trace_history id               → event log (audit trail) for a trace
  trace_lineage id               → derivation graph: derived_from + derived_by
  resolve_divergence id [accept] [body]
                                 → resolve a concurrent edit conflict by
                                   accepting an origin's version, or by
                                   supplying a custom merged body
  cortex_identity                → return this cortex's stable ULID + name +
                                   manifest version (used by federation peers
                                   to verify identity on every sync)
  sync_events [since] [limit]    → pull events for federation (JSON array)
  federation_status              → MCP access posture, federation config,
                                   peer states, vector clock
  announce_peer name endpoint    → accept a peer announcement for discovery

## Filtering
  default              active traces only (not archived, not trashed)
  archived=true        archived traces only
  all=true             active + archived (excludes trash)
  origin=<name>        traces from a specific cortex
  (no trashed filter via MCP — use the CLI for trash operations)

## Provenance
- origin is auto-set to the current cortex name on creation. Override it when
  replaying events from a remote peer.
- derived_from records which traces informed this one — use it when synthesizing
  conclusions from multiple sources. trace_lineage shows both directions.
- trace_history shows every mutation (create, update, archive, etc.) as an
  immutable event log with timestamps and origin attribution.

## Conflict Resolution
- When federated peers edit the same trace concurrently, a "divergence" trace is
  auto-created containing every conflicting version. It has type=divergence,
  tags=[divergence, needs-resolution], and derived_from=[original-trace-id].
- The divergence body lists "**Conflicting origins:** <labels>" and one
  "### Version from <name> (<id-prefix>)" section per version, where id-prefix
  is the first 8 chars of the peer's cortex ULID. Sections are sorted by
  cortex id (not name) so every replica produces byte-identical content.
- Use resolve_divergence to resolve: pass accept=<peer-name> or accept=<id-prefix>
  to apply that version cluster-wide, or pass body=<merged content> to apply a
  custom merge.
- Use list_traces with type filter or tag=needs-resolution to find unresolved divergences.
- federation_status shows the count of unresolved divergences.

## Access Posture
- The HTTP MCP endpoint runs either "open" (no auth) or "keyed" (bearer key required).
  federation_status reports the active posture as "Access: open" or
  "Access: keyed (source=env|file, fingerprint=SHA256:...)".
- Keyed mode is mandatory for any non-loopback deployment and for federation rings.
  In keyed mode the server also requires TLS — it refuses to start over plaintext HTTP.
- The fingerprint is a non-secret SHA-256 of the key; every host in a federation ring
  must report the same fingerprint or peers will 401 each other on sync.
- Key configuration is operator-side: NOEMA_MCP_KEY environment variable, or an
  access.shared_key_file sidecar path in cortex.md pointing at a 0600 file. Env wins
  if both are set. As an agent you do not read, write, or rotate the key yourself —
  your client either has it configured and every tool call works, or it doesn't and
  you'll see a 401 from the transport layer.
- Stdio is unaffected by this posture: stdio implies local-process trust.

## Federation Modes
The cortex-level federation mode controls how this instance participates in a ring:

  sync        (default) Bidirectional: pull events from peers and serve events to them.
  publish     Outbound only: serve events via sync_events, but never pull. Write tools
              (create/update/delete/archive/recover/resolve) are blocked on HTTP — use
              a local stdio transport for content management.
  subscribe   Inbound only: pull events from peers normally. sync_events refuses to
              serve — remote peers cannot pull from this cortex.

Each peer can also be paused individually (mode=paused in cortex.md) without affecting
the rest of the ring. A paused peer keeps its cursor and identity pin intact.

cortex_identity reports the active mode. federation_status shows both the cortex mode
and per-peer modes.

## Source-Locking
Traces can be source-locked by setting source_locked=true on creation. A source-locked
trace refuses update, delete, and remove when the local cortex is not the trace's origin.
This enforces publisher authority: consumers receive the trace via federation but cannot
modify it. archive/unarchive remain allowed (non-destructive).

content_hash is auto-computed (SHA-256 of the body) on every write and carried through
federation events. source_hash records the origin's content_hash at publish time so
consumers can detect drift.

get_trace shows Source Locked, Source Hash, and Content Hash fields when present.

## External Filesystem Edits
Whenever noema serve is running (stdio OR http), a background watcher
observes the traces/, archive/traces/, and trash/traces/ directories.
External changes (Obsidian, VS Code, Finder drags, rm, iCloud sync from
another device, a second noema process on the same cortex) are treated
as first-class mutations:

  edit a trace's .md file      → update event
  drop a new valid .md in      → create event (frontmatter must be complete)
  move traces/ → archive/      → archive event
  move archive/ → traces/      → unarchive event
  move to trash/               → trash event
  move out of trash/           → recover event
  delete from traces/          → trash event; body is restored to trash/
                                 from the event log so recover_trace works
  delete from trash/           → purge event; trace is permanently removed

Source-locked foreign traces are refused on external edit (watcher logs a
warning and skips). Malformed frontmatter is skipped with a warning —
drop-in files must carry a valid id, title, type, created, updated.

The watcher is on by default; opt out by setting watch.enabled=false in
cortex.md. A short per-path debounce (default 300ms) collapses editor
save bursts into a single event. Federation propagation is HTTP-only
(peers need a network endpoint), but external-edit events land in the
local log under stdio too and flow outward the next time an HTTP serve
runs.

## Tips
- Prefer specific types over "note" — it helps retrieval and reasoning later.
- Use tags to group related traces across types.
- author should be your agent name so traces are attributable in multi-agent systems.
- Use derived_from when creating traces based on other traces — it builds a knowledge graph.
- search_traces supports FTS5 syntax: quoted phrases, AND/OR/NOT, prefix* matching.
`, m.Name, noemaVersion, m.Version, purposeLine, ownerLine, m.Name)
}

func formatRows(rows []cortex.Row) string {
	if len(rows) == 0 {
		return "No traces found."
	}
	var sb strings.Builder
	for _, r := range rows {
		typeLabel := r.Type
		if r.Type == string(trace.TypeDivergence) {
			typeLabel = "DIVERGENCE"
		}
		sb.WriteString(fmt.Sprintf("[%s] %s (%s)", typeLabel, r.ID, r.CreatedAt[:10]))
		if r.Author != "" {
			sb.WriteString(fmt.Sprintf(" — %s", r.Author))
		}
		if len(r.Tags) > 0 {
			sb.WriteString(fmt.Sprintf(" [%s]", strings.Join(r.Tags, ", ")))
		}
		sb.WriteByte('\n')
		sb.WriteString(fmt.Sprintf("  %s\n", r.Title))
	}
	return sb.String()
}
