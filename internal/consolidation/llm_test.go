package consolidation

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestHTTPLLMClient_HappyPath verifies the OpenAI-compatible HTTP
// envelope end-to-end: request body format, auth header, response
// parsing. Runs against httptest so no real network is touched.
func TestHTTPLLMClient_HappyPath(t *testing.T) {
	const wantModel = "llama3.1"
	const wantToken = "secret-token-xyz"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("endpoint path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			t.Errorf("auth header = %q, want Bearer token", got)
		}
		body, _ := io.ReadAll(r.Body)
		var envelope map[string]any
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatalf("decoding request envelope: %v", err)
		}
		if temperature, ok := envelope["temperature"]; !ok || temperature != float64(0) {
			t.Errorf("temperature = %v (present=%v), want explicit zero", temperature, ok)
		}
		var req openAIRequestBody
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		if req.Model != wantModel {
			t.Errorf("model = %q, want %q", req.Model, wantModel)
		}
		if req.Stream {
			t.Error("stream should be false")
		}
		if len(req.Messages) == 0 {
			t.Error("messages empty")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello back"}}]}`))
	}))
	defer server.Close()

	t.Setenv("TEST_LLM_KEY", wantToken)
	client, err := NewHTTPLLMClient(server.URL, "TEST_LLM_KEY")
	if err != nil {
		t.Fatalf("NewHTTPLLMClient: %v", err)
	}

	got, err := client.Complete(context.Background(), CompletionRequest{
		Model:    wantModel,
		Messages: []Message{{Role: "user", Content: "say hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "hello back" {
		t.Errorf("content = %q, want %q", got, "hello back")
	}
}

func TestHTTPLLMClient_NoAuthWhenKeyEnvUnset(t *testing.T) {
	os.Unsetenv("UNSET_LLM_KEY")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("auth header = %q, want empty (no api-key env)", got)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	client, err := NewHTTPLLMClient(server.URL, "UNSET_LLM_KEY")
	if err != nil {
		t.Fatalf("NewHTTPLLMClient: %v", err)
	}
	if _, err := client.Complete(context.Background(), CompletionRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "x"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestHTTPLLMClient_StructuredErrorSurface(t *testing.T) {
	// OpenAI-style error bodies should be parsed into a readable
	// error rather than dumped as raw HTML.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found","type":"invalid_request"}}`))
	}))
	defer server.Close()

	client, _ := NewHTTPLLMClient(server.URL, "")
	_, err := client.Complete(context.Background(), CompletionRequest{Model: "bogus", Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("expected error from 400 response")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("error should include provider's message; got %v", err)
	}
}

func TestHTTPLLMClient_EmptyChoicesTreatedAsError(t *testing.T) {
	// A 200 response with no choices array is malformed — calling
	// code would panic trying to index the first choice, so the
	// client should return a clear error instead.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	client, _ := NewHTTPLLMClient(server.URL, "")
	_, err := client.Complete(context.Background(), CompletionRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("expected error on empty choices array")
	}
}

func TestNewHTTPLLMClient_ValidatesEndpoint(t *testing.T) {
	if _, err := NewHTTPLLMClient("", ""); err == nil {
		t.Error("empty endpoint should error")
	}
}

// TestHTTPLLMClient_Embed verifies the /embeddings envelope: path, auth,
// model, input-order preservation, and batching across the
// defaultEmbedBatch boundary. The fake server encodes each input's numeric
// suffix into the returned vector so alignment is checkable.
func TestHTTPLLMClient_Embed(t *testing.T) {
	const wantModel = "nomic-embed-text"
	const wantToken = "embed-token"
	batches := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("endpoint path = %q, want /embeddings", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			t.Errorf("auth header = %q, want Bearer token", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req embedRequestBody
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		if req.Model != wantModel {
			t.Errorf("model = %q, want %q", req.Model, wantModel)
		}
		batches++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"data\":["))
		for i, in := range req.Input {
			n := strings.TrimPrefix(in, "t")
			if i > 0 {
				w.Write([]byte(","))
			}
			// vector = [suffix, 0.0]; index aligns to request order.
			w.Write([]byte(`{"index":` + itoa(i) + `,"embedding":[` + n + `,0.0]}`))
		}
		w.Write([]byte("]}"))
	}))
	defer server.Close()

	t.Setenv("TEST_EMBED_KEY", wantToken)
	client, err := NewHTTPLLMClient(server.URL, "TEST_EMBED_KEY")
	if err != nil {
		t.Fatalf("NewHTTPLLMClient: %v", err)
	}

	// 150 inputs forces 3 batches (64+64+22).
	inputs := make([]string, 150)
	for i := range inputs {
		inputs[i] = "t" + itoa(i)
	}
	got, err := client.Embed(context.Background(), wantModel, inputs)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != len(inputs) {
		t.Fatalf("got %d vectors, want %d", len(got), len(inputs))
	}
	if batches != 3 {
		t.Errorf("server saw %d batches, want 3 (chunking by defaultEmbedBatch)", batches)
	}
	for i, v := range got {
		if len(v) != 2 {
			t.Fatalf("vector %d has dim %d, want 2", i, len(v))
		}
		if int(v[0]) != i {
			t.Errorf("vector %d carries suffix %v, want %d (order not preserved across batches)", i, v[0], i)
		}
	}
}

// TestHTTPLLMClient_EmbedReindexes verifies that an out-of-order response
// (data shuffled but carrying correct index fields) is realigned.
func TestHTTPLLMClient_EmbedReindexes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Two inputs returned in reverse, but index fields are correct.
		w.Write([]byte(`{"data":[{"index":1,"embedding":[1.0]},{"index":0,"embedding":[0.0]}]}`))
	}))
	defer server.Close()

	client, err := NewHTTPLLMClient(server.URL, "")
	if err != nil {
		t.Fatalf("NewHTTPLLMClient: %v", err)
	}
	got, err := client.Embed(context.Background(), "m", []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got[0][0] != 0.0 || got[1][0] != 1.0 {
		t.Errorf("reindex failed: got[0]=%v got[1]=%v, want [0] then [1]", got[0], got[1])
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// TestHTTPLLMClient_EmbedValidation covers the early-return guards: an
// empty model errors without a request, and empty/nil inputs return
// (nil, nil) without hitting the server.
func TestHTTPLLMClient_EmbedValidation(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	client, err := NewHTTPLLMClient(server.URL, "")
	if err != nil {
		t.Fatalf("NewHTTPLLMClient: %v", err)
	}

	if _, err := client.Embed(context.Background(), "", []string{"a"}); err == nil {
		t.Error("empty model should error")
	}
	if got, err := client.Embed(context.Background(), "m", nil); err != nil || got != nil {
		t.Errorf("nil inputs => (nil,nil); got (%v,%v)", got, err)
	}
	if got, err := client.Embed(context.Background(), "m", []string{}); err != nil || got != nil {
		t.Errorf("empty inputs => (nil,nil); got (%v,%v)", got, err)
	}
	if hit {
		t.Error("server must not be hit for invalid/empty input")
	}
}

// TestHTTPLLMClient_EmbedCountMismatch: a short response (fewer vectors
// than inputs) must error rather than silently mis-aligning vectors to
// traces.
func TestHTTPLLMClient_EmbedCountMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1.0]}]}`)) // 1 vector for 2 inputs
	}))
	defer server.Close()
	client, _ := NewHTTPLLMClient(server.URL, "")
	_, err := client.Embed(context.Background(), "m", []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "count") {
		t.Errorf("expected count-mismatch error, got %v", err)
	}
}

// TestHTTPLLMClient_EmbedErrorStatus covers the non-200 branch in
// embedBatch (separate from Complete's): structured error message and the
// truncated raw-body fallback.
func TestHTTPLLMClient_EmbedErrorStatus(t *testing.T) {
	structured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad model name"}}`))
	}))
	defer structured.Close()
	c1, _ := NewHTTPLLMClient(structured.URL, "")
	if _, err := c1.Embed(context.Background(), "m", []string{"a"}); err == nil || !strings.Contains(err.Error(), "bad model name") {
		t.Errorf("expected structured error surfaced, got %v", err)
	}

	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(strings.Repeat("X", 600))) // > 256, must be truncated
	}))
	defer raw.Close()
	c2, _ := NewHTTPLLMClient(raw.URL, "")
	_, err := c2.Embed(context.Background(), "m", []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "...") {
		t.Errorf("expected truncated raw-body error, got %v", err)
	}
}

// TestHTTPLLMClient_EmbedEmptyVectorSlot: a datum with an empty embedding
// array must error.
func TestHTTPLLMClient_EmbedEmptyVectorSlot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[]}]}`))
	}))
	defer server.Close()
	client, _ := NewHTTPLLMClient(server.URL, "")
	_, err := client.Embed(context.Background(), "m", []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "empty vector") {
		t.Errorf("expected empty-vector error, got %v", err)
	}
}

// TestHTTPLLMClient_EmbedNonFiniteRejectedByJSON documents the actual
// safeguard: encoding/json rejects out-of-range (±Inf) and NaN numbers when
// decoding into []float32, so a non-finite vector can never arrive over the
// wire — it surfaces as a parse error rather than a poisoned vector.
func TestHTTPLLMClient_EmbedNonFiniteRejectedByJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1e999, 0.5]}]}`))
	}))
	defer server.Close()
	client, _ := NewHTTPLLMClient(server.URL, "")
	if _, err := client.Embed(context.Background(), "m", []string{"a"}); err == nil {
		t.Error("expected the JSON decoder to reject an out-of-range (±Inf) number")
	}
}

// TestHTTPLLMClient_EmbedMultiBatchOneFails: when a later batch fails, the
// whole call aborts with a wrapped error rather than returning a short
// slice.
func TestHTTPLLMClient_EmbedMultiBatchOneFails(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls >= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req embedRequestBody
		_ = json.Unmarshal(body, &req)
		w.Write([]byte("{\"data\":["))
		for i := range req.Input {
			if i > 0 {
				w.Write([]byte(","))
			}
			w.Write([]byte(`{"index":` + itoa(i) + `,"embedding":[0.1]}`))
		}
		w.Write([]byte("]}"))
	}))
	defer server.Close()
	client, _ := NewHTTPLLMClient(server.URL, "")
	inputs := make([]string, 100) // forces 2 batches
	for i := range inputs {
		inputs[i] = "t" + itoa(i)
	}
	got, err := client.Embed(context.Background(), "m", inputs)
	if err == nil || !strings.Contains(err.Error(), "[64:100]") {
		t.Errorf("expected wrapped second-batch error, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result on batch failure, got len %d", len(got))
	}
}
