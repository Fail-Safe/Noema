package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/federation"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// resolveSearchMode determines the effective search mode for a request and,
// for semantic/hybrid modes, builds the embedder from the cortex manifest.
// It returns (mode, embedder, model, hybridWeight): the embedder is nil when
// semantic search isn't configured (no model/endpoint) so callers degrade to
// lexical. reqMode is the caller-supplied "mode" arg ("" means use the
// manifest default, which is lexical unless configured otherwise).
func resolveSearchMode(cx *cortex.Cortex, reqMode string) (string, cortex.Embedder, string, float64) {
	m, err := cortex.ReadManifest(cx.Dir)
	if err != nil {
		return cortex.SearchModeLexical, nil, "", 0
	}
	weight := m.Search.EffectiveHybridWeight()
	mode := reqMode
	if mode == "" {
		mode = m.Search.EffectiveDefaultMode()
	}
	if mode == cortex.SearchModeLexical {
		return cortex.SearchModeLexical, nil, "", weight
	}
	if m.Search == nil || m.Search.EmbeddingModel == "" {
		return mode, nil, "", weight
	}
	endpoint := m.ResolvedEmbeddingEndpoint()
	if endpoint == "" {
		return mode, nil, "", weight
	}
	client, err := consolidation.NewHTTPLLMClient(endpoint, m.ResolvedEmbeddingAPIKeyEnv())
	if err != nil {
		return mode, nil, "", weight
	}
	return mode, client, m.Search.EmbeddingModel, weight
}

func scoredToRows(s []cortex.ScoredRow) []cortex.Row {
	rows := make([]cortex.Row, len(s))
	for i := range s {
		rows[i] = s[i].Row
	}
	return rows
}

func scoredToMatches(s []cortex.ScoredRow) []cortex.SimilarMatch {
	out := make([]cortex.SimilarMatch, len(s))
	for i := range s {
		out[i] = cortex.SimilarMatch{Row: s[i].Row, Score: s[i].Score}
	}
	return out
}

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
		mcp.WithDescription("Returns concise Markdown guidance for agent use of this Cortex. Call this first if you are unfamiliar with Noema; use cortex_usage for structured MCP/client context."),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		m, err := cortex.ReadManifest(cx.Dir)
		if err != nil {
			// Fallback to minimal identity if manifest is unreadable.
			m = cortex.Manifest{Name: cx.Name}
		}
		return mcp.NewToolResultText(renderInstructions(m, noemaVersion)), nil
	})

	s.AddTool(mcp.NewTool("cortex_usage",
		mcp.WithDescription("Returns structured JSON context for MCP clients: active Cortex identity, trace semantics, startup preference pattern, runtime posture, and operational constraints. Tool discovery remains authoritative for callable tools."),
		mcp.WithRawOutputSchema(cortexUsageOutputSchema),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		m, err := cortex.ReadManifest(cx.Dir)
		if err != nil {
			m = cortex.Manifest{Name: cx.Name}
		}
		payload := buildCortexUsage(cx, m, noemaVersion, federationMode)
		data, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshaling cortex usage: %w", err)
		}
		return mcp.NewToolResultStructured(payload, string(data)), nil
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
		tags := parseTagList(req.GetString("tags", ""))
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
			var collision *cortex.ErrTraceIDExists
			if errors.As(err, &collision) {
				return mcp.NewToolResultError(formatTraceIDCollision(collision)), nil
			}
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace created: %s", t.ID)), nil
	}))

	s.AddTool(mcp.NewTool("search_traces",
		mcp.WithDescription("Full-text search across traces"),
		mcp.WithString("query", mcp.Description("Search query"), mcp.Required()),
		mcp.WithBoolean("all", mcp.Description("Include archived traces")),
		mcp.WithString("mode", mcp.Description("Search mode: 'lexical' (FTS5, default), 'semantic' (embedding similarity), or 'hybrid'. Semantic/hybrid need a configured search: block; if unavailable, falls back to lexical.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return nil, err
		}
		includeArchived := req.GetBool("all", false)
		mode, emb, model, weight := resolveSearchMode(cx, req.GetString("mode", ""))

		// Semantic/hybrid path with graceful degradation to lexical: a
		// missing config or an unreachable endpoint should still return
		// useful (lexical) results with a note, never an error.
		note := ""
		if mode == cortex.SearchModeSemantic || mode == cortex.SearchModeHybrid {
			if emb == nil {
				note = "[semantic search not configured; showing lexical results]\n"
			} else {
				opts := cortex.SemanticOpts{Model: model, IncludeArchived: includeArchived}
				var res []cortex.ScoredRow
				var serr error
				if mode == cortex.SearchModeHybrid {
					res, serr = cx.HybridSearchAs(ctx, emb, query, opts, weight, cortex.ActorAgent, cortex.DefaultSearchHitTopN)
				} else {
					res, serr = cx.SemanticSearchAs(ctx, emb, query, opts, cortex.ActorAgent, cortex.DefaultSearchHitTopN)
				}
				if serr != nil {
					// Log the detailed error server-side; surface only a generic
					// note to the client so endpoint URLs / internal hostnames in
					// the error don't leak into MCP output.
					log.Printf("[mcp] %s search failed, falling back to lexical: %v", mode, serr)
					note = "[" + mode + " search temporarily unavailable; showing lexical results]\n"
				} else {
					return mcp.NewToolResultText(formatRows(scoredToRows(res))), nil
				}
			}
		}

		// Lexical (default or fallback). Agent-initiated searches bump
		// search_hit_count on the top-N results — see actor.go SearchAs.
		rows, err := cx.SearchAs(query, cortex.ListOptions{
			All: includeArchived,
		}, cortex.ActorAgent, cortex.DefaultSearchHitTopN)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(note + formatRows(rows)), nil
	})

	s.AddTool(mcp.NewTool("find_similar_traces",
		mcp.WithDescription("Find traces related to a given trace. Default mode ranks by FTS5 BM25 vocabulary overlap; 'semantic' mode ranks by embedding similarity to the source trace's own vector (needs a configured search: block + backfilled embeddings). Useful when you have one trace and want related ones without crafting a query."),
		mcp.WithString("trace_id", mcp.Description("ID of the source trace"), mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("Maximum matches to return (default 10)")),
		mcp.WithBoolean("include_archived", mcp.Description("Include archived traces (default false)")),
		mcp.WithString("mode", mcp.Description("'lexical' (FTS5, default) or 'semantic'/'hybrid' (embedding similarity). Falls back to lexical if semantic isn't configured or the source isn't embedded.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		traceID, err := req.RequireString("trace_id")
		if err != nil {
			return nil, err
		}
		limit := int(req.GetFloat("limit", 0))
		includeArchived := req.GetBool("include_archived", false)
		// Similarity uses the source's stored vector (semantic) and/or
		// FindSimilar (lexical) — no query embedder needed — so we only
		// require the configured model from the manifest.
		mode, _, model, weight := resolveSearchMode(cx, req.GetString("mode", ""))

		note := ""
		if mode == cortex.SearchModeSemantic || mode == cortex.SearchModeHybrid {
			if model == "" {
				note = "[semantic search not configured; showing lexical results]\n"
			} else {
				opts := cortex.SemanticOpts{Model: model, Limit: limit, IncludeArchived: includeArchived}
				var res []cortex.ScoredRow
				var serr error
				if mode == cortex.SearchModeHybrid {
					res, serr = cx.HybridSimilarAs(traceID, opts, weight, cortex.ActorAgent, cortex.DefaultSearchHitTopN)
				} else {
					res, serr = cx.SemanticSimilarAs(traceID, opts, cortex.ActorAgent, cortex.DefaultSearchHitTopN)
				}
				if serr != nil {
					log.Printf("[mcp] %s similar failed, falling back to lexical: %v", mode, serr)
					note = "[" + mode + " similar temporarily unavailable; showing lexical results]\n"
				} else {
					return mcp.NewToolResultText(formatSimilarMatches(scoredToMatches(res))), nil
				}
			}
		}

		// Lexical (default or fallback). Same actor-aware bump as search_traces.
		matches, err := cx.FindSimilarAs(traceID, cortex.SimilarOpts{
			Limit:           limit,
			IncludeArchived: includeArchived,
		}, cortex.ActorAgent, cortex.DefaultSearchHitTopN)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(note + formatSimilarMatches(matches)), nil
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
			t.Tags = parseTagList(v)
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

	s.AddTool(mcp.NewTool("set_trace_tags",
		mcp.WithDescription("Replace a trace's tags with the provided comma-separated list. Use for metadata hygiene; do not use vote_trace as a substitute for tag cleanup."),
		mcp.WithRawOutputSchema(tagMutationOutputSchema),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
		mcp.WithString("tags", mcp.Description("Comma-separated tags. Empty string clears all tags."), mcp.Required()),
	), readOnlyGuard(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		rawTags, err := req.RequireString("tags")
		if err != nil {
			return nil, err
		}
		tags := parseTagList(rawTags)
		if err := setTraceTags(cx, id, tags); err != nil {
			return nil, err
		}
		return tagMutationResult("set", id, tags), nil
	}))

	s.AddTool(mcp.NewTool("append_trace_tags",
		mcp.WithDescription("Add tags to a trace idempotently. Use for retrieval metadata; do not use vote_trace as a substitute for tag cleanup."),
		mcp.WithRawOutputSchema(tagMutationOutputSchema),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
		mcp.WithString("tags", mcp.Description("Comma-separated tags to add"), mcp.Required()),
	), readOnlyGuard(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		rawTags, err := req.RequireString("tags")
		if err != nil {
			return nil, err
		}
		tr, err := traceForMutation(cx, id)
		if err != nil {
			return nil, err
		}
		tags := appendUniqueTags(tr.Tags, parseTagList(rawTags))
		if err := setTraceTags(cx, id, tags); err != nil {
			return nil, err
		}
		return tagMutationResult("append", id, tags), nil
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

	s.AddTool(mcp.NewTool("consolidation_health",
		mcp.WithDescription("Recent consolidation pipeline health: daily success/fail/promote/distill counts within the lookback window, short→mid and mid→long promotion-latency percentiles, and the 1-source mid leak detector. Lets an agent or operator answer 'is consolidation actually happening, and is anything leaking?' without raw SQL against the events table."),
		mcp.WithString("since", mcp.Description("Lookback window for the activity buckets, e.g. \"24h\", \"7d\". Default 24h. Latency percentiles and the leak detector ignore this — they are all-time / fixed-window respectively.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		since, err := cortex.ParseSince(req.GetString("since", "24h"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		activity, err := cx.ConsolidationActivity(since)
		if err != nil {
			return nil, fmt.Errorf("consolidation activity: %w", err)
		}
		latency, err := cx.PromotionLatency()
		if err != nil {
			return nil, fmt.Errorf("promotion latency: %w", err)
		}
		leak, err := cx.OneSourceMidCount()
		if err != nil {
			return nil, fmt.Errorf("one-source mid count: %w", err)
		}
		out := struct {
			SchemaVersion int                          `json:"schema_version"`
			Activity      cortex.ConsolidationActivity `json:"activity"`
			Latency       cortex.PromotionLatency      `json:"latency"`
			OneSourceMid  cortex.OneSourceMidCount     `json:"one_source_mid"`
		}{
			SchemaVersion: 1,
			Activity:      activity,
			Latency:       latency,
			OneSourceMid:  leak,
		}
		buf, _ := json.MarshalIndent(out, "", "  ")
		return mcp.NewToolResultText(string(buf)), nil
	})

	s.AddTool(mcp.NewTool("search_activity",
		mcp.WithDescription("Top-N traces by federation-wide search popularity (search_hit_count then read_count) plus top-N tags by aggregate engagement. Lets an agent answer 'what's worth reading?' or 'which topics are hot?' without scanning every trace. Active traces only; archived/trashed are excluded."),
		mcp.WithNumber("top", mcp.Description("How many top traces and top tags to return. Default 10. Capped at 100.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		top := int(req.GetFloat("top", 10))
		if top <= 0 {
			top = 10
		}
		if top > 100 {
			top = 100
		}
		traces, err := cx.TopSearchedTraces(top)
		if err != nil {
			return nil, fmt.Errorf("top searched traces: %w", err)
		}
		tags, err := cx.TagActivity(top)
		if err != nil {
			return nil, fmt.Errorf("tag activity: %w", err)
		}
		out := struct {
			SchemaVersion int                   `json:"schema_version"`
			Top           int                   `json:"top"`
			Traces        []cortex.PopularTrace `json:"traces"`
			Tags          []cortex.TagSummary   `json:"tags"`
		}{
			SchemaVersion: 1,
			Top:           top,
			Traces:        traces,
			Tags:          tags,
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
		// Advertise the federation signing public key so peers can pin it
		// (trust-on-first-use) and verify this cortex's events. Omitted when
		// the cortex is unsigned; older peers ignore the field. The public
		// key is not a secret.
		if pub := cx.PublicKey(); pub != "" {
			payload["pubkey"] = pub
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

var cortexUsageOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["schema_version", "cortex", "contract", "startup", "trace_model", "runtime"],
  "properties": {
    "schema_version": {"type": "integer"},
    "cortex": {"type": "object"},
    "contract": {
      "type": "object",
      "required": ["tool_discovery_authoritative", "markdown_instructions_tool", "structured_usage_tool"],
      "properties": {
        "tool_discovery_authoritative": {"type": "boolean"},
        "markdown_instructions_tool": {"type": "string"},
        "structured_usage_tool": {"type": "string"},
        "callable_tools_policy": {"type": "string"}
      }
    },
    "startup": {"type": "object"},
    "trace_model": {"type": "object"},
    "search": {"type": "object"},
    "workflows": {"type": "object"},
    "runtime": {"type": "object"},
    "authoring_tips": {"type": "array", "items": {"type": "string"}}
  },
  "additionalProperties": true
}`)

var tagMutationOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["id", "action", "tags"],
  "properties": {
    "id": {"type": "string"},
    "action": {"type": "string", "enum": ["set", "append"]},
    "tags": {"type": "array", "items": {"type": "string"}}
  },
  "additionalProperties": false
}`)

func buildCortexUsage(cx *cortex.Cortex, m cortex.Manifest, noemaVersion string, federationMode string) map[string]any {
	name := m.Name
	if name == "" {
		name = cx.Name
	}
	id := m.ID
	if id == "" {
		id = cx.ID
	}
	mode := federationMode
	if mode == "" {
		mode = m.Federation.EffectiveMode()
	}

	access := map[string]any{
		"mode":                    "open",
		"tls_required_when_keyed": true,
	}
	if key, err := cortex.LoadAccessKey(cx.Dir, m.Access); err != nil {
		access["mode"] = "error"
		access["error"] = err.Error()
	} else if key.Keyed() {
		access["mode"] = "keyed"
		access["source"] = key.Source
		access["fingerprint"] = key.Fingerprint
	}

	watchEnabled := true
	if m.Watch != nil && m.Watch.Enabled != nil {
		watchEnabled = *m.Watch.Enabled
	}

	traceTypes := make([]map[string]string, 0, len(trace.ValidTypes))
	for _, tt := range trace.ValidTypes {
		traceTypes = append(traceTypes, map[string]string{
			"name":        string(tt),
			"description": traceTypeDescription(tt),
		})
	}

	return map[string]any{
		"schema_version": 1,
		"cortex": map[string]any{
			"id":               id,
			"name":             name,
			"purpose":          m.Purpose,
			"owner":            m.Owner,
			"created":          m.Created,
			"manifest_version": m.Version,
			"noema_version":    noemaVersion,
		},
		"contract": map[string]any{
			"tool_discovery_authoritative": true,
			"markdown_instructions_tool":   "get_instructions",
			"structured_usage_tool":        "cortex_usage",
			"callable_tools_policy":        "Use MCP tool discovery and each tool schema as the source of truth for callable operations in this client session.",
		},
		"startup": map[string]any{
			"preference_sequence": []map[string]any{
				{"tool": "list_traces", "arguments": map[string]any{"tag": "user-preference"}},
				{"tool": "get_trace", "for_each_result": true, "body_policy": "binding durable preference content"},
				{"tool": "list_traces", "arguments": map[string]any{"type": "preference"}, "optional": true, "purpose": "find untagged preferences"},
			},
			"failure_policy": "If preference retrieval fails because of transport, auth, or schema issues, surface the failure explicitly and proceed with ordinary defaults.",
		},
		"trace_model": map[string]any{
			"types": traceTypes,
			"id": map[string]any{
				"format":       "YYYYMMDD-slugified-title",
				"slug_max_len": trace.MaxSlugLen,
				"generated_by": "create_trace",
			},
			"title_rules": []string{
				"Aim for titles under 80 characters.",
				"Do not include a date in the title; create_trace prepends today's date automatically.",
				"Leading YYYYMMDD- and YYYY-MM-DD- prefixes are stripped, but mid-title date fragments survive.",
				"If a trace is about a specific date, put it in a tag such as event-2026-04-02 or in the body.",
			},
			"required_fields":  []string{"id", "title", "type", "created", "updated"},
			"optional_fields":  []string{"author", "tags", "derived_from", "origin", "source_hash", "source_locked"},
			"generated_fields": []string{"content_hash"},
			"tier_glyphs": map[string]string{
				"s": trace.TierShort,
				"m": trace.TierMid,
				"L": trace.TierLong,
			},
		},
		"search": map[string]any{
			"modes":                         []string{cortex.SearchModeLexical, cortex.SearchModeSemantic, cortex.SearchModeHybrid},
			"default_mode":                  m.Search.EffectiveDefaultMode(),
			"semantic_enabled":              m.Search.SemanticOn(),
			"embedding_endpoint_configured": m.ResolvedEmbeddingEndpoint() != "",
			"embedding_model_configured":    m.Search != nil && m.Search.EmbeddingModel != "",
			"hybrid_weight":                 m.Search.EffectiveHybridWeight(),
			"max_chars":                     m.Search.EffectiveMaxChars(),
		},
		"workflows": map[string]any{
			"read": []string{
				"list_traces lists active traces by default; archived=true shows archived only; all=true shows active and archived.",
				"get_trace returns full body and metadata.",
				"search_traces searches by text and may support semantic or hybrid ranking when configured.",
				"find_similar_traces starts from an existing trace ID.",
			},
			"write": []string{
				"create_trace creates a new trace when exposed.",
				"update_trace changes selected fields when exposed.",
				"append_trace appends content without reading the full trace first.",
				"set_trace_tags replaces retrieval tags without touching title, body, type, or lineage.",
				"append_trace_tags adds retrieval tags idempotently without touching title, body, type, or lineage.",
				"archive_trace hides without deleting; delete_trace moves to trash; recover_trace restores from trash.",
			},
			"audit_federation": []string{
				"trace_history shows immutable mutation history.",
				"trace_lineage shows derived_from and derived_by relationships.",
				"resolve_divergence resolves federation conflicts by accepting a peer version or supplying a merge.",
			},
		},
		"runtime": map[string]any{
			"federation_mode":            mode,
			"federation_verify":          m.Federation.EffectiveVerify(),
			"access":                     access,
			"filesystem_watch_enabled":   watchEnabled,
			"long_tier_content_mutable":  false,
			"trash_visible_through_mcp":  false,
			"source_locking_description": "Source-locked foreign traces refuse update, delete, and remove outside their origin; archive and unarchive remain local visibility choices.",
		},
		"authoring_tips": []string{
			"Prefer specific types over note.",
			"Use tags for cross-cutting retrieval.",
			"Use set_trace_tags or append_trace_tags for tag cleanup; vote_trace is only a tier-preference signal.",
			"Set author to the human or agent responsible for the memory.",
			"Keep public-facing content free of private hostnames, personal identifiers, cortex names, and secret-bearing output unless explicitly approved.",
		},
	}
}

func traceTypeDescription(tt trace.Type) string {
	switch tt {
	case trace.TypeFact:
		return "discrete thing that is true"
	case trace.TypeDecision:
		return "choice made and why"
	case trace.TypePreference:
		return "behavioral or stylistic lean"
	case trace.TypeContext:
		return "situational background"
	case trace.TypeSkill:
		return "learned capability or procedure"
	case trace.TypeIntent:
		return "something that needs to happen"
	case trace.TypeObservation:
		return "witnessed but not yet verified"
	case trace.TypeDivergence:
		return "concurrent edit conflict, created by federation"
	default:
		return "fallback for anything else"
	}
}

func parseTagList(raw string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		tag := strings.TrimSpace(part)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
}

func appendUniqueTags(current, additions []string) []string {
	out := append([]string{}, current...)
	seen := map[string]struct{}{}
	for _, tag := range out {
		seen[tag] = struct{}{}
	}
	for _, tag := range additions {
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
}

func traceForMutation(cx *cortex.Cortex, id string) (*trace.Trace, error) {
	row, err := cx.Get(id)
	if err != nil {
		return nil, fmt.Errorf("trace %q not found", id)
	}
	path := cx.TraceFile(id, row.ArchivedAt != "")
	return trace.ParseFile(path)
}

func setTraceTags(cx *cortex.Cortex, id string, tags []string) error {
	if _, err := cx.Get(id); err != nil {
		return fmt.Errorf("trace %q not found", id)
	}
	return cx.SetTraceTagsAs(id, tags, cortex.ActorAgent)
}

func tagMutationResult(action, id string, tags []string) *mcp.CallToolResult {
	payload := map[string]any{
		"id":     id,
		"action": action,
		"tags":   tags,
	}
	return mcp.NewToolResultStructured(payload, fmt.Sprintf("Trace %s tags: %s", id, strings.Join(tags, ", ")))
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
	return fmt.Sprintf(`# Noema Agent Instructions

## Active Cortex
Name:     %s
Version:  noema %s (manifest v%d)
%s%s
## Agent Startup
Before establishing user or project defaults, fetch durable preferences from
this Cortex:

1. list_traces with tag="user-preference".
2. get_trace for each relevant result; the body is the binding content.
3. Optionally list_traces with type="preference" to find untagged preferences.

If preference retrieval fails because of transport, auth, or schema issues,
surface that failure explicitly and proceed with ordinary defaults.

## MCP Usage
Use MCP tool discovery and each tool's input schema as the source of truth for
what this client can call right now. Some Noema deployments expose read-only,
federated, or client-filtered tool sets.

Call cortex_usage when you need structured JSON context for MCP clients:
runtime posture, trace semantics, startup sequence, search configuration, and
operational constraints.

## Memory Semantics
A Cortex is a named collection of Traces; this instance is %q. A Trace is one
memory unit with a markdown body, YAML frontmatter, and SQLite index row.

Choose the most specific trace type:

- fact: discrete thing that is true.
- decision: choice made and why.
- preference: behavioral or stylistic lean.
- context: situational background.
- skill: learned capability or procedure.
- intent: something that needs to happen.
- observation: witnessed but not yet verified.
- note: fallback for anything else.
- divergence: concurrent edit conflict, created by federation.

## Creating Traces
When create_trace is exposed, pass title, type, and body plus optional author,
tags, derived_from, origin, source_locked, and source_hash per the tool schema.

Aim for titles under 80 characters. Do NOT include a date in the title — the ID
generator prepends today's date automatically, and leading YYYYMMDD- or
YYYY-MM-DD- prefixes in the title are stripped to prevent doubled IDs like
20260402-20260402-foo. Avoid mid-title dates too, such as "session 20260416
142000"; only leading prefixes are stripped, so mid-title date fragments survive.
If a trace is about a specific date, put it in a tag such as event-2026-04-02 or
in the body.

Search before creating a durable trace when duplication would matter. Use
derived_from when synthesizing conclusions from existing traces.

append_trace is useful for running logs and fire-and-forget writes because it
appends content without reading the full trace first.

Use set_trace_tags or append_trace_tags for retrieval metadata hygiene. Do not
use vote_trace to compensate for missing or excessive tags; voting is only a
tier-preference signal.

## Guardrails
- Prefer specific types over note.
- Use tags for cross-cutting retrieval.
- Set author to the human or agent responsible for the memory.
- Keep public-facing content free of private hostnames, personal identifiers,
  cortex names, and secret-bearing output unless explicitly approved.
`, m.Name, noemaVersion, m.Version, purposeLine, ownerLine, m.Name)
}

// formatTraceIDCollision renders an ErrTraceIDExists into a JSON
// envelope with a human-readable summary. Agents that parse the body
// can branch on `kind == "trace_id_collision"` and `existing_state`;
// agents (or LLMs) that read the text directly still get the
// human-readable lines under `summary`. The MCP error result still
// carries the same payload as text — clients see `isError: true` plus
// this JSON body.
func formatTraceIDCollision(c *cortex.ErrTraceIDExists) string {
	fix := []string{"vary the title (different slug → different id)"}
	switch c.State {
	case "trashed":
		fix = append(fix,
			fmt.Sprintf("noema recover %s   (restore the trashed trace)", c.ID),
			fmt.Sprintf("noema memory purge %s   (free the slot, irreversible)", c.ID),
		)
	case "archived":
		fix = append(fix,
			fmt.Sprintf("noema unarchive %s (restore the archived trace)", c.ID),
			fmt.Sprintf("noema memory purge %s (free the slot, irreversible)", c.ID),
		)
	case "purged":
		fix = append(fix,
			fmt.Sprintf("noema memory purge --hard %s (only a hard purge frees the slot)", c.ID),
		)
	default:
		fix = append(fix, fmt.Sprintf("noema get %s (read the existing trace before deciding)", c.ID))
	}
	payload := map[string]any{
		"kind":           "trace_id_collision",
		"id":             c.ID,
		"existing_state": c.State,
		"archived_at":    c.ArchivedAt,
		"trashed_at":     c.TrashedAt,
		"purged_at":      c.PurgedAt,
		"fix":            fix,
		"summary":        c.Error(),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return c.Error()
	}
	return string(b)
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
