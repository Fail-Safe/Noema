package consolidation

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
