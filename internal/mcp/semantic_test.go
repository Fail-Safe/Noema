package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

func topicVec(s string) [3]float32 {
	v := [3]float32{0.01, 0.01, 0.01}
	for w := range strings.FieldsSeq(strings.ToLower(s)) {
		switch {
		case strings.Contains(w, "alpha"):
			v[0]++
		case strings.Contains(w, "beta"):
			v[1]++
		case strings.Contains(w, "gamma"):
			v[2]++
		}
	}
	return v
}

// topicEmbedServer is an OpenAI-compatible /embeddings endpoint returning a
// deterministic 3-dim topic vector per input, so MCP semantic ranking is
// assertable end-to-end without a real model.
func topicEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"data\":["))
		for i, in := range req.Input {
			if i > 0 {
				w.Write([]byte(","))
			}
			v := topicVec(in)
			fmt.Fprintf(w, `{"index":%d,"embedding":[%v,%v,%v]}`, i, v[0], v[1], v[2])
		}
		w.Write([]byte("]}"))
	}))
}

func writeSearchConfig(t *testing.T, cx *cortex.Cortex, endpoint, model string) {
	t.Helper()
	m, err := cortex.ReadManifest(cx.Dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	m.Search = &cortex.SearchConfig{
		SemanticEnabled:   true,
		EmbeddingEndpoint: endpoint,
		EmbeddingModel:    model,
	}
	if err := cortex.WriteManifest(cx.Dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
}

func TestResolveSearchMode(t *testing.T) {
	cx := newTestCortex(t)

	// Unconfigured: default is lexical; explicit semantic yields no embedder.
	if mode, e, _, _ := resolveSearchMode(cx, ""); mode != cortex.SearchModeLexical || e != nil {
		t.Errorf("unconfigured default = (%s, embedder!=nil:%v), want lexical/nil", mode, e != nil)
	}
	if mode, e, _, _ := resolveSearchMode(cx, "semantic"); mode != cortex.SearchModeSemantic || e != nil {
		t.Errorf("semantic-unconfigured = (%s, embedder!=nil:%v), want semantic/nil-embedder", mode, e != nil)
	}

	// Configured: semantic yields an embedder + model + default weight.
	writeSearchConfig(t, cx, "http://localhost:1", "tm")
	mode, e, model, weight := resolveSearchMode(cx, "semantic")
	if mode != cortex.SearchModeSemantic || e == nil || model != "tm" {
		t.Errorf("configured semantic = (%s, embedder!=nil:%v, model=%q), want semantic/embedder/tm", mode, e != nil, model)
	}
	if weight != 0.5 {
		t.Errorf("default hybrid weight = %v, want 0.5", weight)
	}
}

func TestSearchTraces_SemanticFallsBackToLexical(t *testing.T) {
	cx := newTestCortex(t)
	tr := trace.New("findable thing", "note", "agent", nil, "body about widgets")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s := NewServer(cx, "test", "")

	// Semantic requested but unconfigured -> note + lexical results.
	text, isErr := callTool(t, s, "search_traces", map[string]any{"query": "findable", "mode": "semantic"})
	if isErr {
		t.Fatalf("search_traces errored: %s", text)
	}
	if !strings.Contains(text, "not configured") {
		t.Errorf("expected fallback note, got:\n%s", text)
	}
	if !strings.Contains(text, tr.ID) {
		t.Errorf("expected lexical result for %s, got:\n%s", tr.ID, text)
	}
}

func TestSearchTraces_SemanticEndToEnd(t *testing.T) {
	server := topicEmbedServer(t)
	defer server.Close()

	cx := newTestCortex(t)
	writeSearchConfig(t, cx, server.URL, "tm")
	add := func(title, body string) string {
		tr := trace.New(title, "note", "agent", nil, body)
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
		return tr.ID
	}
	alphaID := add("alpha topic", "alpha alpha content")
	add("beta topic", "beta beta content")
	gammaID := add("gamma topic", "gamma gamma content")

	// Backfill embeddings through the same endpoint the handler will use.
	client, err := consolidation.NewHTTPLLMClient(server.URL, "")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := cx.EmbedBackfill(context.Background(), client, "tm", cortex.EmbedBackfillOpts{}); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	s := NewServer(cx, "test", "")
	text, isErr := callTool(t, s, "search_traces", map[string]any{"query": "alpha alpha alpha", "mode": "semantic"})
	if isErr {
		t.Fatalf("search_traces errored: %s", text)
	}
	if strings.Contains(text, "not configured") || strings.Contains(text, "unavailable") {
		t.Fatalf("unexpected fallback note in semantic result:\n%s", text)
	}
	// The alpha trace should rank above the gamma trace in the output.
	ai := strings.Index(text, alphaID)
	gi := strings.Index(text, gammaID)
	if ai < 0 {
		t.Fatalf("alpha trace %s missing from results:\n%s", alphaID, text)
	}
	if gi >= 0 && ai > gi {
		t.Errorf("alpha (%d) should rank before gamma (%d) for an alpha query:\n%s", ai, gi, text)
	}
}

func TestSearchTraces_SemanticEndpointDownGenericNote(t *testing.T) {
	cx := newTestCortex(t)
	// Configured but unreachable endpoint with a recognizable host:port.
	writeSearchConfig(t, cx, "http://127.0.0.1:1/v1", "tm")
	tr := trace.New("lexical findable widget", "note", "agent", nil, "body about widgets")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s := NewServer(cx, "test", "")
	text, isErr := callTool(t, s, "search_traces", map[string]any{"query": "findable", "mode": "semantic"})
	if isErr {
		t.Fatalf("search_traces errored: %s", text)
	}
	if !strings.Contains(text, "temporarily unavailable") {
		t.Errorf("expected generic degradation note, got:\n%s", text)
	}
	// The endpoint host must NOT leak into client-facing output.
	if strings.Contains(text, "127.0.0.1") {
		t.Errorf("degradation note leaked the endpoint host:\n%s", text)
	}
	// Lexical fallback still returns the matching trace.
	if !strings.Contains(text, tr.ID) {
		t.Errorf("expected lexical fallback result %s, got:\n%s", tr.ID, text)
	}
}

func TestSearchTraces_HybridEndToEnd(t *testing.T) {
	server := topicEmbedServer(t)
	defer server.Close()

	cx := newTestCortex(t)
	writeSearchConfig(t, cx, server.URL, "tm")
	add := func(title, body string) string {
		tr := trace.New(title, "note", "agent", nil, body)
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
		return tr.ID
	}
	alphaID := add("alpha topic", "alpha alpha content")
	add("beta topic", "beta beta content")

	client, _ := consolidation.NewHTTPLLMClient(server.URL, "")
	if _, err := cx.EmbedBackfill(context.Background(), client, "tm", cortex.EmbedBackfillOpts{}); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	s := NewServer(cx, "test", "")
	text, isErr := callTool(t, s, "search_traces", map[string]any{"query": "alpha alpha", "mode": "hybrid"})
	if isErr {
		t.Fatalf("search_traces hybrid errored: %s", text)
	}
	// Real RRF fusion now — no "not yet available" or fallback note.
	if strings.Contains(text, "not yet") || strings.Contains(text, "unavailable") || strings.Contains(text, "not configured") {
		t.Fatalf("unexpected note in hybrid result:\n%s", text)
	}
	if !strings.Contains(text, alphaID) {
		t.Errorf("alpha trace %s missing from hybrid results:\n%s", alphaID, text)
	}
}

func TestFindSimilar_SemanticEndToEnd(t *testing.T) {
	server := topicEmbedServer(t)
	defer server.Close()

	cx := newTestCortex(t)
	writeSearchConfig(t, cx, server.URL, "tm")
	mk := func(title, body string) string {
		tr := trace.New(title, "note", "agent", nil, body)
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
		return tr.ID
	}
	src := mk("alpha source", "alpha alpha alpha")
	mk("beta other", "beta beta")
	mk("alpha cousin", "alpha alpha")

	client, _ := consolidation.NewHTTPLLMClient(server.URL, "")
	if _, err := cx.EmbedBackfill(context.Background(), client, "tm", cortex.EmbedBackfillOpts{}); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	s := NewServer(cx, "test", "")
	text, isErr := callTool(t, s, "find_similar_traces", map[string]any{"trace_id": src, "mode": "semantic"})
	if isErr {
		t.Fatalf("find_similar_traces errored: %s", text)
	}
	if strings.Contains(text, "unavailable") || strings.Contains(text, "not configured") {
		t.Fatalf("unexpected fallback note:\n%s", text)
	}
	if strings.Contains(text, src) {
		t.Errorf("source trace must be excluded from its own similarity:\n%s", text)
	}
}
