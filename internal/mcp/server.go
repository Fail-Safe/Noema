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
		out := fmt.Sprintf("ID: %s\nTitle: %s\nType: %s\nTier: %s\nAuthor: %s\nTags: %s\nCreated: %s\nUpdated: %s",
			row.ID, row.Title, row.Type, row.Tier, row.Author,
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
		// Agent-initiated searches bump search_hit_count on the top-N
		// results. Auto-injection providers (Hermes, etc.) consume search
		// output without ever calling get_trace, so without this bump the
		// graduation gate never sees signal from those reads. See
		// internal/cortex/actor.go SearchAs for the rationale.
		rows, err := cx.SearchAs(query, cortex.ListOptions{
			All: req.GetBool("all", false),
		}, cortex.ActorAgent, cortex.DefaultSearchHitTopN)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(formatRows(rows)), nil
	})

	s.AddTool(mcp.NewTool("find_similar_traces",
		mcp.WithDescription("Find traces with overlapping vocabulary to a given trace, ranked by FTS5 BM25. Useful when you have one trace and want to surface related ones without crafting a search query."),
		mcp.WithString("trace_id", mcp.Description("ID of the source trace"), mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("Maximum matches to return (default 10)")),
		mcp.WithBoolean("include_archived", mcp.Description("Include archived traces (default false)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		traceID, err := req.RequireString("trace_id")
		if err != nil {
			return nil, err
		}
		opts := cortex.SimilarOpts{
			Limit:           int(req.GetFloat("limit", 0)),
			IncludeArchived: req.GetBool("include_archived", false),
		}
		// Same actor-aware bump as search_traces; see SearchAs for why.
		matches, err := cx.FindSimilarAs(traceID, opts, cortex.ActorAgent, cortex.DefaultSearchHitTopN)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(formatSimilarMatches(matches)), nil
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

	s.AddTool(mcp.NewTool("vote_trace",
		mcp.WithDescription("Cast a tier-preference vote on a trace. Use sparingly: only when the user has clearly indicated preference (\"this really matters\", \"forget this one\"). 'up' nudges the consolidation agent toward promoting the trace to a higher memory tier; 'down' nudges toward demoting or keeping it low. Votes accumulate across calls and are preferences, not overrides — the consolidation agent still makes the final decision."),
		mcp.WithString("id", mcp.Description("Trace ID to vote on"), mcp.Required()),
		mcp.WithString("direction", mcp.Description("'up' for promotion preference, 'down' for demotion preference"), mcp.Required(),
			mcp.Enum("up", "down")),
	), readOnlyGuard(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		direction, err := req.RequireString("direction")
		if err != nil {
			return nil, err
		}
		delta := 0
		switch direction {
		case "up":
			delta = 1
		case "down":
			delta = -1
		default:
			return nil, fmt.Errorf("direction must be 'up' or 'down', got %q", direction)
		}
		if err := cx.Vote(id, delta, cortex.ActorAgent); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Vote recorded: %s %s.", direction, id)), nil
	}))

	// ---- Memory consolidation tools (internal-only) --------------------
	//
	// Two tools that together support an LLM-driven consolidation
	// flow: list_consolidation_candidates surfaces short-term traces
	// with their usage signals, and record_consolidation_result
	// materialises a distilled mid-tier trace linked via derived_from
	// to the sources it consolidates.
	//
	// Internal-only per the pattern in docs/plans/hermes-plugin-plan.md:74:
	// these are not exposed to the Hermes agent surface (or equivalents)
	// — they're called by whatever component is designated as the
	// consolidator (an LLM via `noema consolidate`, or a custom agent
	// told via get_instructions that it's the consolidator for the
	// current cortex).

	s.AddTool(mcp.NewTool("list_consolidation_candidates",
		mcp.WithDescription("Internal tool. Returns short-term traces within the rolling consolidation window along with their usage signals (read_count, modify_count, tier_votes, derived_from_count). Consumer scores these and submits distilled mid-tier traces via record_consolidation_result."),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		m, err := cortex.ReadManifest(cx.Dir)
		if err != nil {
			return nil, fmt.Errorf("reading manifest for window config: %w", err)
		}
		window := m.Consolidation.EffectiveWindowHours()
		cands, err := cx.PromotionCandidates(trace.TierShort, window)
		if err != nil {
			return nil, err
		}
		out := struct {
			WindowHours int                         `json:"window_hours"`
			Candidates  []cortex.PromotionCandidate `json:"candidates"`
		}{
			WindowHours: int(window.Hours()),
			Candidates:  cands,
		}
		buf, _ := json.MarshalIndent(out, "", "  ")
		return mcp.NewToolResultText(string(buf)), nil
	})

	s.AddTool(mcp.NewTool("record_consolidation_result",
		mcp.WithDescription("Internal tool. Materialises a distilled mid-tier trace from a set of short-term sources. Validates the source IDs exist (>=2 required), creates the new trace with derived_from lineage pointing at the sources, and emits an ActionConsolidate event carrying model/profile/confidence telemetry for the quality dashboard."),
		mcp.WithString("title", mcp.Description("Title for the distilled trace"), mcp.Required()),
		mcp.WithString("body", mcp.Description("Body of the distilled trace — the consolidated memory"), mcp.Required()),
		mcp.WithString("source_ids", mcp.Description("Comma-separated source trace IDs (at least 2)"), mcp.Required()),
		mcp.WithString("tags", mcp.Description("Comma-separated tags (optional)")),
		mcp.WithString("author", mcp.Description("Author identifier for the distilled trace (optional)")),
		mcp.WithString("model_name", mcp.Description("Model that produced the distillation (optional; e.g. claude-opus-4-7)")),
		mcp.WithString("model_tier_profile", mcp.Description("Prompt profile used (optional)"),
			mcp.Enum("small", "large", "frontier")),
		mcp.WithNumber("cohesion_confidence", mcp.Description("Confidence 0.0-1.0 that the cluster was cohesive (optional)")),
	), readOnlyGuard(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		title, err := req.RequireString("title")
		if err != nil {
			return nil, err
		}
		body, err := req.RequireString("body")
		if err != nil {
			return nil, err
		}
		sourcesRaw, err := req.RequireString("source_ids")
		if err != nil {
			return nil, err
		}
		var sources []string
		for _, id := range strings.Split(sourcesRaw, ",") {
			if id := strings.TrimSpace(id); id != "" {
				sources = append(sources, id)
			}
		}

		var tags []string
		if raw := req.GetString("tags", ""); raw != "" {
			for _, t := range strings.Split(raw, ",") {
				if tag := strings.TrimSpace(t); tag != "" {
					tags = append(tags, tag)
				}
			}
		}

		spec := cortex.DistilledTraceSpec{
			Title:              title,
			Body:               body,
			Tags:               tags,
			Author:             req.GetString("author", ""),
			SourceIDs:          sources,
			ModelName:          req.GetString("model_name", ""),
			ModelTierProfile:   req.GetString("model_tier_profile", ""),
			CohesionConfidence: req.GetFloat("cohesion_confidence", 0),
		}
		id, err := cx.CreateDistilledTrace(spec)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Distilled trace created: %s (from %d sources)", id, len(sources))), nil
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
		// Piggyback the current consolidation rank on the identity
		// response so federated peers can observe eligibility without a
		// separate round-trip. Missing state (feature off, eligibility
		// loop not yet running, malformed kv row) is surfaced as
		// rank=0 — the coordination layer treats zero as "not
		// participating". Older peers that don't know about the field
		// simply ignore it.
		state := federation.NewState(cx.DB.DB)
		if rank, rerr := state.GetLocalRank(); rerr == nil && rank.CortexID != "" {
			payload["rank"] = rank
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

	s.AddTool(mcp.NewTool("sync_read_signal",
		mcp.WithDescription("Returns per-peer tier-usage deltas (read_count, modify_count, search_hit_count, last_read_at) for federation sync. Each peer publishes only its own rows — the ring aggregates by SUMing over every peer's contribution, so consolidation decisions operate on a federation-wide signal rather than the local slice. Returns a JSON array of trace_usage rows owned by this cortex with updated_at > since. search_hit_count is omitted when zero for wire compatibility with pre-migration-015 peers."),
		mcp.WithString("since", mcp.Description("RFC3339 cursor — return only rows with updated_at > this value. Empty returns everything this peer owns.")),
		mcp.WithNumber("limit", mcp.Description("Max rows to return (default 100, max 1000)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if federationMode == cortex.FederationModeSubscribe {
			return mcp.NewToolResultError(
				"this cortex is in subscribe mode and does not serve read signal"), nil
		}
		since := req.GetString("since", "")
		limit := req.GetInt("limit", 100)
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		rows, err := cx.LocalUsageSince(since, limit)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(rows)
		if err != nil {
			return nil, fmt.Errorf("marshaling usage rows: %w", err)
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

			state := federation.NewState(cx.DB.DB)
			// Surface the local consolidation rank (plan §14). A missing
			// entry or Rank=0 is rendered plainly as "(ineligible)" so
			// operators can see at a glance whether coordination is
			// armed for this cortex.
			if localRank, rerr := state.GetLocalRank(); rerr == nil {
				sb.WriteString(fmt.Sprintf("Consolidation Rank: %s\n", formatRank(localRank)))
			}
			sb.WriteByte('\n')

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
				peerRank := "(none)"
				if pr, perr := state.GetPeerRank(p.Name); perr == nil && pr.ObservedAt != "" {
					peerRank = formatRank(pr)
				}
				sb.WriteString(fmt.Sprintf("  %s\n    endpoint:   %s\n    mode:       %s\n    cortex_id:  %s\n    rank:       %s\n    last_seen:  %s\n    last_event: %s\n",
					p.Name, p.Endpoint, peerMode, cortexID, peerRank, lastSeen, lastEvent))
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

// formatRank renders a federation.RankEntry for federation_status
// output. Empty / ineligible entries become "(ineligible)"; live
// entries show the numeric rank plus the observation timestamp so
// operators can see whether the advertisement is fresh.
func formatRank(r federation.RankEntry) string {
	if r.Rank == 0 || r.ObservedAt == "" {
		return "(ineligible)"
	}
	return fmt.Sprintf("%d (observed %s)", r.Rank, r.ObservedAt)
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
  find_similar_traces trace_id [limit] [include_archived]
                                 → traces with overlapping vocabulary,
                                   ranked by BM25 (lower score = closer
                                   match). Useful when you have one
                                   trace in hand and want to surface
                                   related ones without crafting a query
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

## Tier Graduation (short → mid → long)
The long tier accumulates base truths — traces that have proven durable via stable,
repeat usage. An automatic graduation pass runs on the consolidation schedule and
promotes every mid-tier trace that clears four AND-gated criteria:

  - age >= 14 days (configurable via consolidation.graduation.min_age_days)
  - (read_count + search_hit_count) >= 3 (configurable via consolidation.graduation.min_read_count)
  - modify_count == 0 (unless require_unmodified is false)
  - tier_votes >= 0 (no active downvotes)

read_count bumps on get_trace; search_hit_count bumps on search_traces /
find_similar_traces top-N hits. Both contribute equally to the read gate so
auto-injection providers (Hermes, etc.) that consume search output without
ever calling get_trace can still drive traces to long tier.

Traces at the long tier are DB-level immutable: content / identity fields are
frozen, tier cannot change except via the admin-purge ceremony, and routine
Update/Delete is refused. Archive / trash / vote still work — visibility and
preference signals remain mutable.

Operators promote manually via 'noema memory promote <id>' / 'demote <id>'.
Agents don't directly promote — vote_trace is the signal to influence the
automatic pass. The ceremony for removing a long trace is 'noema memory
purge --tier long' (add '--hard' for full deletion; default tombstones).

## Consolidation Coordination
When multiple federated peers have consolidation.enabled + consolidation.llm_enabled
with a reachable local_llm_endpoint, exactly one peer runs each consolidation cycle:

  - Every peer advertises a random rank (1..99) via cortex_identity on each sync.
  - At trigger time, the peer with the highest rank (cortex_id breaks ties) runs
    the pass; the rest silently skip. Subscribe-mode cortexes advertise rank=0
    and never win.
  - The winner emits consolidation_claim → runs → consolidation_success|fail
    events that replicate through the standard event log.

federation_status displays each peer's current rank. No configuration required
beyond the existing consolidation block — coordination is automatic when peers
are present.

Setting consolidation.auto_distillation_enabled=true folds the LLM distillation
pipeline into every scheduled trigger (distillation → heuristic → graduation on
the elected peer). Requires llm_enabled + local_llm_endpoint + model_name, and
degrades to heuristic+graduation when the LLM endpoint is unreachable.

## Source-Locking
Traces can be source-locked by setting source_locked=true on creation. A source-locked
trace refuses update, delete, and remove when the local cortex is not the trace's origin.
This enforces publisher authority: consumers receive the trace via federation but cannot
modify it. archive/unarchive remain allowed (non-destructive).

content_hash is auto-computed (SHA-256 of the body) on every write and carried through
federation events. source_hash records the origin's content_hash at publish time so
consumers can detect drift.

get_trace shows Source Locked, Source Hash, and Content Hash fields when present.
Both list_traces and search_traces prefix each row with a tier glyph (s=short,
m=mid, L=long) so you can see the tier without a second lookup. get_trace surfaces
the full tier name on a dedicated "Tier:" line in the metadata block.

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
		sb.WriteString(fmt.Sprintf("[%s] [%s] %s (%s)", tierGlyph(r.Tier), typeLabel, r.ID, r.CreatedAt[:10]))
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

// formatSimilarMatches renders FindSimilar results in the same shape as
// formatRows but with a leading score column. Score is FTS5 BM25, where
// lower = closer match; we surface it so an agent or operator can tell
// "strong match" from "marginal match" rather than treating every result
// as equally relevant.
func formatSimilarMatches(matches []cortex.SimilarMatch) string {
	if len(matches) == 0 {
		return "No similar traces found."
	}
	var sb strings.Builder
	for _, m := range matches {
		typeLabel := m.Type
		if m.Type == string(trace.TypeDivergence) {
			typeLabel = "DIVERGENCE"
		}
		sb.WriteString(fmt.Sprintf("[%s] [%s] [score=%.2f] %s (%s)",
			tierGlyph(m.Tier), typeLabel, m.Score, m.ID, m.CreatedAt[:10]))
		if m.Author != "" {
			sb.WriteString(fmt.Sprintf(" — %s", m.Author))
		}
		if len(m.Tags) > 0 {
			sb.WriteString(fmt.Sprintf(" [%s]", strings.Join(m.Tags, ", ")))
		}
		sb.WriteByte('\n')
		sb.WriteString(fmt.Sprintf("  %s\n", m.Title))
	}
	return sb.String()
}

// tierGlyph returns the one-letter tier indicator used in MCP list/search
// output, matching the TUI convention (lowercase for short/mid,
// uppercase L for long to make long-term traces easy to spot). Unknown
// tiers render as "?" so a missing tier column surfaces visibly rather
// than blending in.
func tierGlyph(tier string) string {
	switch tier {
	case trace.TierShort:
		return "s"
	case trace.TierMid:
		return "m"
	case trace.TierLong:
		return "L"
	default:
		return "?"
	}
}
