package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// NewServer builds an MCP server exposing all Cortex operations.
func NewServer(cx *cortex.Cortex) *server.MCPServer {
	s := server.NewMCPServer("noema", "0.1.0",
		server.WithToolCapabilities(true),
	)

	s.AddTool(mcp.NewTool("get_instructions",
		mcp.WithDescription("Returns a reference guide for working with this Cortex: terminology, trace types, field definitions, filtering options, and tool usage. Call this first if you are unfamiliar with Noema."),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		m, err := cortex.ReadManifest(cx.Dir)
		if err != nil {
			// Fallback to minimal identity if manifest is unreadable.
			m = cortex.Manifest{Name: cx.Name}
		}

		purposeLine := ""
		if m.Purpose != "" {
			purposeLine = fmt.Sprintf("Purpose: %s\n", m.Purpose)
		}
		ownerLine := ""
		if m.Owner != "" {
			ownerLine = fmt.Sprintf("Owner:   %s\n", m.Owner)
		}

		out := fmt.Sprintf(`# Noema — Agent Reference

## Active Cortex
Name:    %s
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

## Trace Fields
  id       required  YYYYMMDD-slugified-title  (e.g. 20260330-why-we-chose-go)
  title    required  short, descriptive
  type     required  one of the types above
  created  required  RFC3339 UTC timestamp (set on creation, never changed)
  updated  required  RFC3339 UTC timestamp (update on every edit)
  author   optional  human username or agent name
  tags     optional  list of keyword strings

## Tools

  get_instructions               → this document
  list_traces [type] [author] [tag] [archived] [all]
                                 → list traces; defaults to active only
  get_trace id                   → full content including body
  create_trace title type body [author] [tags]
                                 → create a new trace; tags = comma-separated
  update_trace id [title] [type] [author] [tags] [body]
                                 → update any subset of fields
  search_traces query [all]      → FTS5 full-text search
  archive_trace id               → move to archive (reversible)
  unarchive_trace id             → restore from archive
  delete_trace id                → move to trash (soft-delete, recoverable)
  recover_trace id               → restore a trashed trace to active

## Filtering
  default              active traces only (not archived, not trashed)
  archived=true        archived traces only
  all=true             active + archived (excludes trash)
  (no trashed filter via MCP — use the CLI for trash operations)

## Tips
- Prefer specific types over "note" — it helps retrieval and reasoning later.
- Use tags to group related traces across types.
- author should be your agent name so traces are attributable in multi-agent systems.
- search_traces supports FTS5 syntax: quoted phrases, AND/OR/NOT, prefix* matching.
`, m.Name, purposeLine, ownerLine, m.Name)

		return mcp.NewToolResultText(out), nil
	})

	s.AddTool(mcp.NewTool("list_traces",
		mcp.WithDescription("List traces in the cortex"),
		mcp.WithString("type", mcp.Description("Filter by trace type")),
		mcp.WithString("author", mcp.Description("Filter by author")),
		mcp.WithString("tag", mcp.Description("Filter by tag")),
		mcp.WithBoolean("archived", mcp.Description("Show only archived traces")),
		mcp.WithBoolean("all", mcp.Description("Show active and archived traces")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rows, err := cx.List(cortex.ListOptions{
			Type:     req.GetString("type", ""),
			Author:   req.GetString("author", ""),
			Tag:      req.GetString("tag", ""),
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
		row, err := cx.Get(id)
		if err != nil {
			return nil, fmt.Errorf("trace %q not found", id)
		}
		t, err := trace.ParseFile(cx.TraceFile(id, row.ArchivedAt != ""))
		if err != nil {
			return nil, err
		}
		out := fmt.Sprintf("ID: %s\nTitle: %s\nType: %s\nAuthor: %s\nTags: %s\nCreated: %s\nUpdated: %s\n\n%s",
			row.ID, row.Title, row.Type, row.Author,
			strings.Join(row.Tags, ", "),
			row.CreatedAt, row.UpdatedAt,
			t.Body,
		)
		return mcp.NewToolResultText(out), nil
	})

	s.AddTool(mcp.NewTool("create_trace",
		mcp.WithDescription("Create a new trace"),
		mcp.WithString("title", mcp.Description("Trace title"), mcp.Required()),
		mcp.WithString("type", mcp.Description("Trace type"), mcp.Required(),
			mcp.Enum("fact", "decision", "preference", "context", "skill", "intent", "observation", "note")),
		mcp.WithString("body", mcp.Description("Trace body content"), mcp.Required()),
		mcp.WithString("author", mcp.Description("Author name or agent identifier")),
		mcp.WithString("tags", mcp.Description("Comma-separated tags")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		if err := cx.Add(t); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace created: %s", t.ID)), nil
	})

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
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		if err := cx.Trash(id); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace %s moved to trash.", id)), nil
	})

	s.AddTool(mcp.NewTool("recover_trace",
		mcp.WithDescription("Restore a trace from trash back to active"),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		if err := cx.Recover(id); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace %s recovered.", id)), nil
	})

	s.AddTool(mcp.NewTool("archive_trace",
		mcp.WithDescription("Archive a trace"),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		if err := cx.Archive(id); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace %s archived.", id)), nil
	})

	s.AddTool(mcp.NewTool("unarchive_trace",
		mcp.WithDescription("Restore an archived trace"),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return nil, err
		}
		if err := cx.Unarchive(id); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace %s restored.", id)), nil
	})

	s.AddTool(mcp.NewTool("update_trace",
		mcp.WithDescription("Update fields of an existing trace. Only provided fields are changed."),
		mcp.WithString("id", mcp.Description("Trace ID"), mcp.Required()),
		mcp.WithString("title", mcp.Description("New title")),
		mcp.WithString("type", mcp.Description("New type"),
			mcp.Enum("fact", "decision", "preference", "context", "skill", "intent", "observation", "note")),
		mcp.WithString("author", mcp.Description("New author")),
		mcp.WithString("tags", mcp.Description("New tags, comma-separated (replaces existing tags)")),
		mcp.WithString("body", mcp.Description("New body content")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		if err := t.Write(path); err != nil {
			return nil, err
		}
		if err := cx.Update(id); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Trace %s updated.", id)), nil
	})

	return s
}

func formatRows(rows []cortex.Row) string {
	if len(rows) == 0 {
		return "No traces found."
	}
	var sb strings.Builder
	for _, r := range rows {
		sb.WriteString(fmt.Sprintf("[%s] %s (%s)", r.Type, r.ID, r.CreatedAt[:10]))
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
