package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// --------- tier visibility in MCP output ---------
//
// Pins the contract that list_traces / search_traces / get_trace must
// expose the tier of every trace to MCP consumers. Agents reason about
// immutability and curation based on tier; the TUI and CLI both surface
// it, so MCP parity is required. A regression here would make agents
// tier-blind again.

func TestFormatRows_IncludesTierGlyph(t *testing.T) {
	rows := []cortex.Row{
		{ID: "20260424-a", Title: "short one", Type: "note", CreatedAt: "2026-04-24T00:00:00Z", Tier: trace.TierShort},
		{ID: "20260424-b", Title: "mid one", Type: "note", CreatedAt: "2026-04-24T00:00:00Z", Tier: trace.TierMid},
		{ID: "20260424-c", Title: "long one", Type: "note", CreatedAt: "2026-04-24T00:00:00Z", Tier: trace.TierLong},
	}
	out := formatRows(rows)
	for _, want := range []string{"[s]", "[m]", "[L]"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatRows output missing tier glyph %q; got:\n%s", want, out)
		}
	}
}

func TestGetTrace_IncludesTierLine(t *testing.T) {
	// Integration-level: seed a trace at mid tier through the Cortex,
	// invoke get_trace over MCP, and assert the output carries a
	// "Tier: mid" line. Pre-fix this failed because the metadata block
	// only surfaced ID/Title/Type/Author/Tags/Created/Updated with no
	// tier slot at all.
	cx := newTestCortex(t)
	tr := trace.New("tier-vis", "note", "agent", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := cx.Promote(tr.ID, trace.TierMid); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	s := NewServer(cx, "test-version", "")
	text, isErr := callTool(t, s, "get_trace", map[string]any{"id": tr.ID})
	if isErr {
		t.Fatalf("get_trace returned error: %s", text)
	}
	if !strings.Contains(text, "Tier: mid") {
		t.Errorf("get_trace output missing 'Tier: mid' line; got:\n%s", text)
	}
}

func TestTierGlyph(t *testing.T) {
	cases := map[string]string{
		trace.TierShort: "s",
		trace.TierMid:   "m",
		trace.TierLong:  "L",
		"":              "?",
		"bogus":         "?",
	}
	for in, want := range cases {
		if got := tierGlyph(in); got != want {
			t.Errorf("tierGlyph(%q) = %q, want %q", in, got, want)
		}
	}
}

// --------- renderInstructions (pure helper) ---------

// TestRenderInstructions_IncludesNoemaVersion pins the most important
// outcome of plumbing the version through: the agent reference guide
// must show the build version on the very line agents read first. The
// whole reason for this change was that operators couldn't tell which
// noema binary an MCP client was talking to — this assertion is what
// keeps that fix from regressing.
func TestRenderInstructions_IncludesNoemaVersion(t *testing.T) {
	out := renderInstructions(cortex.Manifest{
		Name:    "agentbrain",
		Version: 2,
	}, "v0.2.5-test")

	want := "Version:  noema v0.2.5-test (manifest v2)"
	if !strings.Contains(out, want) {
		t.Errorf("instructions missing version line %q\nfull output:\n%s", want, out)
	}
}

// TestRenderInstructions_HandlesMissingOptionalFields pins the empty
// purpose/owner branches. The original code only emitted those lines
// when the manifest set them; the refactor must preserve that —
// otherwise empty cortex.md files would render with stray "Purpose: \n"
// noise that looks like a bug.
func TestRenderInstructions_HandlesMissingOptionalFields(t *testing.T) {
	out := renderInstructions(cortex.Manifest{
		Name:    "minimal",
		Version: 2,
	}, "dev")

	if strings.Contains(out, "Purpose:") {
		t.Errorf("instructions should omit Purpose line when manifest has none:\n%s", out)
	}
	if strings.Contains(out, "Owner:") {
		t.Errorf("instructions should omit Owner line when manifest has none:\n%s", out)
	}
	// And the cortex name still has to appear in both spots that
	// reference it (Name: header and the Terminology line).
	if strings.Count(out, "minimal") < 2 {
		t.Errorf("expected cortex name in both header and terminology section:\n%s", out)
	}
}

// TestRenderInstructions_RendersPurposeAndOwner pins the populated
// branches: when the manifest carries Purpose/Owner, both lines must
// appear with the same column alignment as Name/Version. Misaligned
// fields would make the reference guide look broken to humans reading
// it via `noema serve --print-config` or in agent transcripts.
func TestRenderInstructions_RendersPurposeAndOwner(t *testing.T) {
	out := renderInstructions(cortex.Manifest{
		Name:    "research",
		Purpose: "Primary research cortex",
		Owner:   "mark",
		Version: 2,
	}, "v0.2.5")

	for _, want := range []string{
		"Name:     research",
		"Version:  noema v0.2.5 (manifest v2)",
		"Purpose:  Primary research cortex",
		"Owner:    mark",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("instructions missing line %q\nfull output:\n%s", want, out)
		}
	}
}

// TestRenderInstructions_ManifestVersionReflectsInput pins that the
// manifest version comes from the manifest field, not a hardcoded
// constant. When the cortex schema bumps, an old cortex still on
// manifest v1 will be visible as such — and that distinction matters
// for federation peers who need to know whether the remote end has
// migrated.
func TestRenderInstructions_ManifestVersionReflectsInput(t *testing.T) {
	for _, v := range []int{1, 2, 99} {
		out := renderInstructions(cortex.Manifest{Name: "test", Version: v}, "dev")
		want := "(manifest v" + itoa(v) + ")"
		if !strings.Contains(out, want) {
			t.Errorf("manifest v%d not reflected:\nlooking for %q\nfull:\n%s", v, want, out)
		}
	}
}

// TestRenderInstructions_WarnsAgainstDateInTitle pins the warning text
// added after a fleet of agents on the live federation ring was found
// creating traces with IDs like 20260402-20260402-dadbot-foo. The agents
// were dutifully putting dates in titles to mark when an event occurred,
// and trace.NewID was prepending today's date on top — producing the
// doubled prefix. NewID now strips leading YYYYMMDD- and YYYY-MM-DD-
// prefixes defensively, but the get_instructions document is the
// canonical place to teach agents the cleaner pattern up front so the
// underlying habit shifts. Without this assertion, a future refactor of
// the Tools section could quietly drop the warning and the doubled-
// prefix bug would re-emerge from agent habit even though the code-
// level defense would still catch it.
func TestRenderInstructions_IncludesAppendTrace(t *testing.T) {
	out := renderInstructions(cortex.Manifest{Name: "test", Version: 2}, "dev")
	if !strings.Contains(out, "append_trace") {
		t.Errorf("instructions should mention append_trace\nfull output:\n%s", out)
	}
}

func TestRenderInstructions_WarnsAgainstDateInTitle(t *testing.T) {
	out := renderInstructions(cortex.Manifest{Name: "test", Version: 2}, "dev")

	for _, want := range []string{
		"Do NOT include a date in the title",
		"YYYYMMDD-",
		"YYYY-MM-DD-",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("instructions missing date-in-title warning fragment %q\nfull output:\n%s", want, out)
		}
	}
}

// itoa avoids pulling in strconv for a single call. Tests are the place
// for tiny helpers like this — keeps the imports minimal and signals
// "this isn't a hot path".
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// --------- NewServer protocol-level wiring ---------

// newTestCortex spins up a real cortex in a temp directory. The MCP
// server needs a working cortex to back its tools, and the cortex
// constructor expects a real on-disk layout (db, traces dir, manifest),
// so there's no faking this.
func newTestCortex(t *testing.T) *cortex.Cortex {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("test", dir); err != nil {
		t.Fatalf("cortex.Create: %v", err)
	}
	cx, err := cortex.Open("test", filepath.Join(dir, "test"))
	if err != nil {
		t.Fatalf("cortex.Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx
}

// initializeServer drives the MCP `initialize` handshake against a
// freshly built server and returns the parsed result. Any failure here
// is a fatal: a server that can't initialize is a server that no client
// can ever talk to, so a failed handshake is never a "soft" condition.
func initializeServer(t *testing.T, cx *cortex.Cortex, version string) mcp.InitializeResult {
	t.Helper()
	s := NewServer(cx, version, "")

	initReq := mcp.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(int64(1)),
		Request: mcp.Request{Method: "initialize"},
		Params: struct {
			ProtocolVersion string                 `json:"protocolVersion"`
			ClientInfo      mcp.Implementation     `json:"clientInfo"`
			Capabilities    mcp.ClientCapabilities `json:"capabilities"`
		}{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "noema-test-client",
				Version: "1.0.0",
			},
		},
	}

	body, err := json.Marshal(initReq)
	if err != nil {
		t.Fatalf("marshal initialize request: %v", err)
	}

	resp := s.HandleMessage(context.Background(), body)
	if resp == nil {
		t.Fatal("HandleMessage returned nil response")
	}
	jr, ok := resp.(mcp.JSONRPCResponse)
	if !ok {
		t.Fatalf("expected JSONRPCResponse, got %T", resp)
	}
	result, ok := jr.Result.(mcp.InitializeResult)
	if !ok {
		t.Fatalf("expected InitializeResult, got %T", jr.Result)
	}
	return result
}

// TestNewServer_ServerInfoCarriesVersion is the protocol-level
// counterpart to TestRenderInstructions_IncludesNoemaVersion. Even an
// MCP client that never calls get_instructions (e.g. one that only
// inspects the initialize handshake) must be able to identify the
// noema build. This test pins serverInfo.Name and serverInfo.Version
// against the same value the build script injects via -ldflags.
func TestNewServer_ServerInfoCarriesVersion(t *testing.T) {
	cx := newTestCortex(t)
	result := initializeServer(t, cx, "v0.2.5-protocol-test")

	if result.ServerInfo.Name != "noema" {
		t.Errorf("ServerInfo.Name = %q, want %q", result.ServerInfo.Name, "noema")
	}
	if result.ServerInfo.Version != "v0.2.5-protocol-test" {
		t.Errorf("ServerInfo.Version = %q, want %q", result.ServerInfo.Version, "v0.2.5-protocol-test")
	}
}

// TestNewServer_EmptyVersionNormalizesToDev pins the empty-string
// guard. An MCP server that advertised an empty version would render
// as `noema ` in client UIs — which looks like a bug and breaks any
// client that key-by-version. We normalize to "dev" so the field is
// always non-empty AND signals "you're talking to a development
// build, expect surprises".
func TestNewServer_EmptyVersionNormalizesToDev(t *testing.T) {
	cx := newTestCortex(t)
	result := initializeServer(t, cx, "")

	if result.ServerInfo.Version != "dev" {
		t.Errorf("ServerInfo.Version = %q, want %q (empty should normalize)", result.ServerInfo.Version, "dev")
	}
}

// --------- Federation mode guards ---------

// callTool invokes a named tool on an already-initialized MCP server
// and returns the result text + whether it was an error result.
func callTool(t *testing.T, s *server.MCPServer, toolName string, args map[string]any) (string, bool) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	params, _ := json.Marshal(map[string]any{
		"name":      toolName,
		"arguments": args,
	})
	req := mcp.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(int64(42)),
		Request: mcp.Request{Method: "tools/call"},
	}
	reqBytes, _ := json.Marshal(req)
	// Splice params into the raw JSON.
	reqBytes = []byte(strings.TrimSuffix(string(reqBytes), "}") + `,"params":` + string(params) + "}")

	result := s.HandleMessage(context.Background(), reqBytes)
	if result == nil {
		t.Fatalf("HandleMessage returned nil for %s", toolName)
	}
	switch r := result.(type) {
	case mcp.JSONRPCResponse:
		data, _ := json.Marshal(r.Result)
		var toolResult mcp.CallToolResult
		if err := json.Unmarshal(data, &toolResult); err != nil {
			t.Fatalf("unmarshal CallToolResult for %s: %v", toolName, err)
		}
		text := ""
		if len(toolResult.Content) > 0 {
			if tc, ok := toolResult.Content[0].(mcp.TextContent); ok {
				text = tc.Text
			}
		}
		return text, toolResult.IsError
	case mcp.JSONRPCError:
		// Handlers that return `nil, err` surface as protocol-level
		// JSON-RPC errors rather than tool-result errors. Treat both
		// as error outcomes so tests can assert on failure modes
		// uniformly regardless of which path the handler took.
		return r.Error.Message, true
	default:
		t.Fatalf("unexpected response type for %s: %T", toolName, result)
		return "", false
	}
}

// initServer drives the MCP initialize handshake so tools can be called.
func initServer(t *testing.T, s *server.MCPServer) {
	t.Helper()
	initReq := mcp.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(int64(1)),
		Request: mcp.Request{Method: "initialize"},
		Params: struct {
			ProtocolVersion string                 `json:"protocolVersion"`
			ClientInfo      mcp.Implementation     `json:"clientInfo"`
			Capabilities    mcp.ClientCapabilities `json:"capabilities"`
		}{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "test", Version: "1.0"},
		},
	}
	body, _ := json.Marshal(initReq)
	resp := s.HandleMessage(context.Background(), body)
	if resp == nil {
		t.Fatal("initialize returned nil")
	}
}

func TestPublishMode_BlocksMutatingTools(t *testing.T) {
	cx := newTestCortex(t)
	s := NewServer(cx, "test", "publish")
	initServer(t, s)

	mutating := []struct {
		name string
		args map[string]any
	}{
		{"create_trace", map[string]any{"title": "x", "type": "note", "body": "y"}},
		{"update_trace", map[string]any{"id": "nonexistent", "title": "x"}},
		{"append_trace", map[string]any{"id": "nonexistent", "content": "x"}},
		{"delete_trace", map[string]any{"id": "nonexistent"}},
		{"recover_trace", map[string]any{"id": "nonexistent"}},
		{"archive_trace", map[string]any{"id": "nonexistent"}},
		{"unarchive_trace", map[string]any{"id": "nonexistent"}},
		{"resolve_divergence", map[string]any{"id": "nonexistent"}},
		{"vote_trace", map[string]any{"id": "nonexistent", "direction": "up"}},
		{"record_consolidation_result", map[string]any{"title": "x", "body": "y", "source_ids": "a,b"}},
	}

	for _, tc := range mutating {
		text, isErr := callTool(t, s, tc.name, tc.args)
		if !isErr {
			t.Errorf("%s: expected error result in publish mode, got success: %s", tc.name, text)
		}
		if !strings.Contains(text, "publish mode") {
			t.Errorf("%s: error should mention publish mode, got: %s", tc.name, text)
		}
	}
}

func TestPublishMode_AllowsReadTools(t *testing.T) {
	cx := newTestCortex(t)
	s := NewServer(cx, "test", "publish")
	initServer(t, s)

	// These should all succeed (not return publish-mode errors).
	readTools := []struct {
		name string
		args map[string]any
	}{
		{"list_traces", nil},
		{"search_traces", map[string]any{"query": "test"}},
		{"sync_events", nil},
		{"federation_status", nil},
	}

	for _, tc := range readTools {
		text, isErr := callTool(t, s, tc.name, tc.args)
		if isErr && strings.Contains(text, "publish mode") {
			t.Errorf("%s: should not be blocked in publish mode, got: %s", tc.name, text)
		}
	}
}

func TestSubscribeMode_BlocksSyncEvents(t *testing.T) {
	cx := newTestCortex(t)
	s := NewServer(cx, "test", "subscribe")
	initServer(t, s)

	text, isErr := callTool(t, s, "sync_events", nil)
	if !isErr {
		t.Errorf("sync_events should be blocked in subscribe mode, got: %s", text)
	}
	if !strings.Contains(text, "subscribe mode") {
		t.Errorf("error should mention subscribe mode, got: %s", text)
	}
}

func TestSubscribeMode_AllowsMutatingTools(t *testing.T) {
	cx := newTestCortex(t)
	s := NewServer(cx, "test", "subscribe")
	initServer(t, s)

	// create_trace should work in subscribe mode.
	text, isErr := callTool(t, s, "create_trace", map[string]any{
		"title": "sub-test", "type": "note", "body": "hello",
	})
	if isErr {
		t.Errorf("create_trace should work in subscribe mode, got error: %s", text)
	}
}

func TestSyncMode_AllowsEverything(t *testing.T) {
	cx := newTestCortex(t)
	s := NewServer(cx, "test", "")
	initServer(t, s)

	// create_trace should work.
	text, isErr := callTool(t, s, "create_trace", map[string]any{
		"title": "sync-test", "type": "note", "body": "hello",
	})
	if isErr {
		t.Errorf("create_trace should work in sync mode, got error: %s", text)
	}

	// sync_events should work.
	text, isErr = callTool(t, s, "sync_events", nil)
	if isErr && strings.Contains(text, "subscribe mode") {
		t.Errorf("sync_events should work in sync mode, got: %s", text)
	}
}

func TestSubscribeMode_BlocksSyncReadSignal(t *testing.T) {
	cx := newTestCortex(t)
	s := NewServer(cx, "test", "subscribe")
	initServer(t, s)

	text, isErr := callTool(t, s, "sync_read_signal", nil)
	if !isErr {
		t.Errorf("sync_read_signal should be blocked in subscribe mode, got: %s", text)
	}
	if !strings.Contains(text, "subscribe mode") {
		t.Errorf("error should mention subscribe mode, got: %s", text)
	}
}

func TestPublishMode_AllowsSyncReadSignal(t *testing.T) {
	cx := newTestCortex(t)
	s := NewServer(cx, "test", "publish")
	initServer(t, s)

	text, isErr := callTool(t, s, "sync_read_signal", nil)
	if isErr {
		t.Errorf("sync_read_signal should be served in publish mode, got error: %s", text)
	}
}
