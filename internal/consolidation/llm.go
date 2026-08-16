package consolidation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// LLMClient is the interface the consolidation pipeline uses to talk
// to a language model. Kept narrow (one method) so tests can stub it
// without replaying the OpenAI-compatible HTTP dance for every case.
type LLMClient interface {
	Complete(ctx context.Context, req CompletionRequest) (string, error)
}

// Embedder is the narrow interface the semantic-search path uses to turn
// text into vectors. One method, mirroring LLMClient, so the search layer
// and its tests can stub it without the HTTP dance. HTTPLLMClient
// satisfies it, reusing the same endpoint/auth config as completions.
type Embedder interface {
	Embed(ctx context.Context, model string, inputs []string) ([][]float32, error)
}

// Message mirrors the OpenAI chat-completions message shape. All
// providers this client targets (Ollama, LMStudio, llama.cpp server,
// vLLM, OpenAI, Azure OpenAI) accept the same three roles.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CompletionRequest is the provider-neutral shape the pipeline
// constructs. The HTTP client serializes it to the OpenAI
// chat-completions body; model-tier profiles vary the Temperature
// and MaxTokens but the envelope stays the same.
//
// DisableThinking asks reasoning-capable local models (Qwen3, etc.)
// to skip their <think> block and answer directly. On llama.cpp's
// OpenAI-compatible server this sets chat_template_kwargs.enable_thinking=false;
// providers that don't recognize the field silently ignore it.
// Cohesion / template / confidence steps use this because thinking
// consumes the MaxTokens budget before any answer reaches the wire.
type CompletionRequest struct {
	Model           string
	Messages        []Message
	Temperature     float64
	MaxTokens       int
	DisableThinking bool
}

// HTTPLLMClient posts chat-completion requests to an OpenAI-
// compatible HTTP endpoint. Default timeout is 5 minutes which is
// generous for small local models and the 70B-class frontier on
// consumer hardware; tighten via Client.Timeout if needed.
type HTTPLLMClient struct {
	Endpoint   string       // base URL, e.g. "http://localhost:11434/v1"
	APIKey     string       // optional bearer token
	HTTPClient *http.Client // nil means a reasonable default is used
}

// NewHTTPLLMClient constructs a client from a cortex.md-style
// endpoint string and an optional env-var name for the API key. The
// env-var indirection matches the access.shared_key_file pattern for
// the MCP server: the secret itself never lives in cortex.md.
func NewHTTPLLMClient(endpoint, apiKeyEnv string) (*HTTPLLMClient, error) {
	if endpoint == "" {
		return nil, errors.New("llm endpoint is empty")
	}
	if _, err := url.Parse(endpoint); err != nil {
		return nil, fmt.Errorf("invalid llm endpoint %q: %w", endpoint, err)
	}
	return &HTTPLLMClient{
		Endpoint: strings.TrimRight(endpoint, "/"),
		APIKey:   os.Getenv(apiKeyEnv),
		HTTPClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}, nil
}

type openAIRequestBody struct {
	Model              string         `json:"model"`
	Messages           []Message      `json:"messages"`
	Temperature        float64        `json:"temperature"`
	MaxTokens          int            `json:"max_tokens,omitempty"`
	Stream             bool           `json:"stream"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

// openAIResponseMessage accepts both the standard `content` and
// llama.cpp's extended `reasoning_content` so a response from a
// reasoning model is diagnosable even when thinking wasn't disabled
// (content empty, reasoning truncated at MaxTokens).
type openAIResponseMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type openAIResponseBody struct {
	Choices []struct {
		Message      openAIResponseMessage `json:"message"`
		FinishReason string                `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// Complete posts one chat-completion request and returns the first
// choice's content. Non-streaming by design — the pipeline needs the
// full response to validate and parse before deciding what to do.
func (c *HTTPLLMClient) Complete(ctx context.Context, req CompletionRequest) (string, error) {
	reqBody := openAIRequestBody{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      false,
	}
	if req.DisableThinking {
		reqBody.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("encoding llm request: %w", err)
	}

	url := c.Endpoint + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building llm request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("posting llm request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading llm response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Try to surface a structured error message if the provider
		// formatted one; fall back to the raw body truncated so a
		// huge HTML error page doesn't flood the log.
		var parsed openAIResponseBody
		if json.Unmarshal(respBody, &parsed) == nil && parsed.Error != nil {
			return "", fmt.Errorf("llm endpoint %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		snippet := string(respBody)
		if len(snippet) > 256 {
			snippet = snippet[:256] + "..."
		}
		return "", fmt.Errorf("llm endpoint %d: %s", resp.StatusCode, snippet)
	}

	var parsed openAIResponseBody
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("parsing llm response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("llm error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("llm response has no choices")
	}
	choice := parsed.Choices[0]
	if choice.Message.Content == "" {
		// Reasoning models (Qwen3, DeepSeek-R1, etc.) can emit a
		// <think> block into reasoning_content and leave content
		// empty when MaxTokens cuts them off mid-thought. Surface
		// this clearly — a silent empty string would look like a
		// confident "no" to the cohesion parser.
		if choice.Message.ReasoningContent != "" {
			return "", fmt.Errorf("llm produced reasoning only (%d chars, finish=%q); set DisableThinking or raise MaxTokens", len(choice.Message.ReasoningContent), choice.FinishReason)
		}
		if choice.FinishReason == "length" {
			return "", fmt.Errorf("llm response truncated at MaxTokens with empty content (finish=length)")
		}
	}
	return choice.Message.Content, nil
}

// embedRequestBody / embedResponseBody mirror the OpenAI /embeddings
// shape, which Ollama, llama.cpp server, vLLM, LMStudio and OpenAI all
// accept. `input` takes an array; `data` comes back with one entry per
// input, each carrying an `index` aligning it to the request order.
type embedRequestBody struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponseBody struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// defaultEmbedBatch caps how many inputs ride in one /embeddings call.
// Local servers can choke on very large batches; 64 is a safe middle
// ground that still amortizes the per-request overhead.
const defaultEmbedBatch = 64

// Embed returns one vector per input, preserving input order, by POSTing
// to {Endpoint}/embeddings in batches of defaultEmbedBatch. An empty
// inputs slice returns nil without making a request. Reuses the same
// endpoint, API key, and HTTP client as Complete.
func (c *HTTPLLMClient) Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	if model == "" {
		return nil, errors.New("embedding model is empty")
	}
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(inputs))
	for start := 0; start < len(inputs); start += defaultEmbedBatch {
		end := min(start+defaultEmbedBatch, len(inputs))
		vecs, err := c.embedBatch(ctx, model, inputs[start:end])
		if err != nil {
			return nil, fmt.Errorf("embedding batch [%d:%d]: %w", start, end, err)
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (c *HTTPLLMClient) embedBatch(ctx context.Context, model string, batch []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequestBody{Model: model, Input: batch})
	if err != nil {
		return nil, fmt.Errorf("encoding embeddings request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building embeddings request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("posting embeddings request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading embeddings response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var parsed embedResponseBody
		if json.Unmarshal(respBody, &parsed) == nil && parsed.Error != nil {
			return nil, fmt.Errorf("embeddings endpoint %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		snippet := string(respBody)
		if len(snippet) > 256 {
			snippet = snippet[:256] + "..."
		}
		return nil, fmt.Errorf("embeddings endpoint %d: %s", resp.StatusCode, snippet)
	}

	var parsed embedResponseBody
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parsing embeddings response: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("embeddings error: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != len(batch) {
		return nil, fmt.Errorf("embeddings response count %d != input count %d", len(parsed.Data), len(batch))
	}

	// Place by `index` when the provider returns a clean permutation of
	// [0,len); otherwise fall back to response order (some servers omit or
	// zero the index field). Either way the result aligns to the inputs.
	byIndex := indicesArePermutation(parsed.Data, len(batch))
	vecs := make([][]float32, len(batch))
	for i, d := range parsed.Data {
		slot := i
		if byIndex {
			slot = d.Index
		}
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embeddings response has an empty vector at slot %d", slot)
		}
		// Non-finite guard is unnecessary here: encoding/json rejects NaN
		// and out-of-range (±Inf) numbers when decoding into []float32, so
		// vectors that reach this point are already finite. The decode→rank
		// consumer path (Phase 3) validates finiteness of stored vectors,
		// where a corrupted BLOB — not the wire — is the real risk.
		vecs[slot] = d.Embedding
	}
	return vecs, nil
}

// indicesArePermutation reports whether the response's index fields form a
// distinct, in-range covering of [0,n) — i.e. trustworthy for placement.
func indicesArePermutation(data []struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}, n int) bool {
	if len(data) != n {
		return false
	}
	seen := make([]bool, n)
	for _, d := range data {
		if d.Index < 0 || d.Index >= n || seen[d.Index] {
			return false
		}
		seen[d.Index] = true
	}
	return true
}
