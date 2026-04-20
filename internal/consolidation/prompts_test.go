package consolidation

import (
	"context"
	"strings"
	"testing"
)

// stubLLM returns prescripted responses in order. Used to drive each
// profile's multi-step pipeline deterministically in tests — no HTTP,
// no network, no flakiness. An empty queue makes Complete fail
// loudly so missing-stub bugs surface.
type stubLLM struct {
	responses []string
	calls     []CompletionRequest
}

func (s *stubLLM) Complete(ctx context.Context, req CompletionRequest) (string, error) {
	s.calls = append(s.calls, req)
	if len(s.responses) == 0 {
		return "", errStubExhausted
	}
	r := s.responses[0]
	s.responses = s.responses[1:]
	return r, nil
}

var errStubExhausted = &stubErr{msg: "stub llm queue exhausted"}

type stubErr struct{ msg string }

func (e *stubErr) Error() string { return e.msg }

func sampleCluster() ClusterInput {
	return ClusterInput{
		Traces: []TraceInput{
			{ID: "20260420-a", Title: "Auth kickoff", Tags: []string{"auth"}, Body: "We chose oauth today."},
			{ID: "20260420-b", Title: "Session TTL debate", Tags: []string{"auth", "session"}, Body: "30-min vs 24h options."},
			{ID: "20260420-c", Title: "Token rotation plan", Tags: []string{"auth"}, Body: "Rotate on every login."},
		},
	}
}

// ---- small profile: cohesion + template fill ----

func TestSmallProfile_HappyPath(t *testing.T) {
	llm := &stubLLM{responses: []string{
		"yes\n", // cohesion gate
		"Title: Authentication strategy\nTags: auth, session, oauth\nBody: Consolidated auth decisions.\nOauth chosen with 30-min session TTL.",
	}}
	d, err := smallProfile{}.Run(context.Background(), llm, "llama3.1", sampleCluster())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !d.Cohesive {
		t.Error("expected cohesive=true")
	}
	if d.Title != "Authentication strategy" {
		t.Errorf("title = %q", d.Title)
	}
	if len(d.Tags) != 3 {
		t.Errorf("tags = %v, want 3", d.Tags)
	}
	if !strings.Contains(d.Body, "Oauth chosen") {
		t.Errorf("body missing expected content: %q", d.Body)
	}
	if len(llm.calls) != 2 {
		t.Errorf("expected 2 llm calls (cohesion + template), got %d", len(llm.calls))
	}
}

func TestSmallProfile_NotCohesive_ShortCircuits(t *testing.T) {
	llm := &stubLLM{responses: []string{"no\n"}}
	d, err := smallProfile{}.Run(context.Background(), llm, "llama3.1", sampleCluster())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d.Cohesive {
		t.Error("expected cohesive=false")
	}
	if len(llm.calls) != 1 {
		t.Errorf("expected only cohesion call when not cohesive, got %d", len(llm.calls))
	}
}

// ---- large profile: cohesion + template + confidence ----

func TestLargeProfile_WithConfidence(t *testing.T) {
	llm := &stubLLM{responses: []string{
		"yes",
		"Title: Auth decisions\nTags: auth\nBody: Consolidated body content.",
		"8",
	}}
	d, err := largeProfile{}.Run(context.Background(), llm, "llama3.1:70b", sampleCluster())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d.Confidence != 0.8 {
		t.Errorf("confidence = %f, want 0.8", d.Confidence)
	}
}

func TestLargeProfile_ConfidenceParseFailure_TreatedAsZero(t *testing.T) {
	// If the model answers the confidence question with something
	// that doesn't parse as an integer, we proceed with zero-confidence
	// rather than failing the whole pass.
	llm := &stubLLM{responses: []string{
		"yes",
		"Title: T\nTags: x\nBody: B",
		"not a number",
	}}
	d, err := largeProfile{}.Run(context.Background(), llm, "m", sampleCluster())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d.Confidence != 0 {
		t.Errorf("confidence = %f, want 0", d.Confidence)
	}
}

// ---- frontier profile: single-shot JSON ----

func TestFrontierProfile_ParsesJSON(t *testing.T) {
	llm := &stubLLM{responses: []string{
		`{"cohesive": true, "title": "Auth plan", "tags": ["auth", "session"], "body": "Full consolidated body.", "confidence": 0.9}`,
	}}
	d, err := frontierProfile{}.Run(context.Background(), llm, "claude-opus-4-7", sampleCluster())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d.Title != "Auth plan" || len(d.Tags) != 2 || d.Confidence != 0.9 {
		t.Errorf("got %+v", d)
	}
}

func TestFrontierProfile_StripsFenceWrapping(t *testing.T) {
	// Models sometimes ignore the "JSON only" instruction and wrap
	// in ```json fences. Parser tolerates this so an otherwise-valid
	// response doesn't get rejected.
	llm := &stubLLM{responses: []string{
		"```json\n{\"cohesive\":true,\"title\":\"T\",\"tags\":[],\"body\":\"B\",\"confidence\":0.5}\n```",
	}}
	d, err := frontierProfile{}.Run(context.Background(), llm, "m", sampleCluster())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d.Title != "T" {
		t.Errorf("title = %q after fence strip", d.Title)
	}
}

func TestFrontierProfile_CohesiveFalseNoRequiredFields(t *testing.T) {
	// When the model decides the cluster isn't cohesive, title/body
	// can be empty without raising a parse error.
	llm := &stubLLM{responses: []string{
		`{"cohesive": false}`,
	}}
	d, err := frontierProfile{}.Run(context.Background(), llm, "m", sampleCluster())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d.Cohesive {
		t.Error("expected cohesive=false")
	}
}

// ---- parsers ----

func TestParseConfidenceInt(t *testing.T) {
	cases := map[string]float64{
		"8":            0.8,
		"10":           1.0,
		"  3 ":         0.3,
		"Confidence: 7": 0.7,
		"none":         0,
		"":             0,
	}
	for in, want := range cases {
		if got := parseConfidenceInt(in); got != want {
			t.Errorf("parseConfidenceInt(%q) = %f, want %f", in, got, want)
		}
	}
}

func TestParseCohesion(t *testing.T) {
	yes := []string{"yes", "Yes", "YES", "yes\n", "yes, they're cohesive"}
	no := []string{"no", "No", "nope", "I don't think so", ""}
	for _, s := range yes {
		if !parseCohesion(s) {
			t.Errorf("parseCohesion(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if parseCohesion(s) {
			t.Errorf("parseCohesion(%q) = true, want false", s)
		}
	}
}

func TestParseTemplate_MissingBody(t *testing.T) {
	raw := "Title: only a title\nTags: x\n"
	_, err := parseTemplate(raw)
	if err == nil {
		t.Error("expected parse error when Body section is missing")
	}
}

func TestNormalizeTags(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		// Common LLM-generated shapes from the real cortex run.
		{[]string{"MCP Server"}, []string{"mcp-server"}},
		{[]string{"AI SME", "career goals"}, []string{"ai-sme", "career-goals"}},
		{[]string{"Hugging Face"}, []string{"hugging-face"}},
		{[]string{"memory consolidation"}, []string{"memory-consolidation"}},
		// Already-clean input passes through unchanged.
		{[]string{"mcp-server", "noema"}, []string{"mcp-server", "noema"}},
		// Deduplication across differing-case variants.
		{[]string{"Noema", "noema", "NOEMA"}, []string{"noema"}},
		// Separator characters collapse to a single hyphen.
		{[]string{"foo/bar", "a.b.c"}, []string{"foo-bar", "a-b-c"}},
		// Empty / too-short / all-noise input is dropped.
		{[]string{"", " ", "a", "!!!"}, nil},
		// Leading/trailing whitespace and separators are trimmed.
		{[]string{"  trim-me  ", "leading-hyphen-"}, []string{"trim-me", "leading-hyphen"}},
	}
	for _, tc := range cases {
		got := normalizeTags(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("normalizeTags(%v) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("normalizeTags(%v)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestGetProfile_UnknownFallsBackToLarge(t *testing.T) {
	if GetProfile("bogus").Name() != "large" {
		t.Errorf("unknown profile should fall back to large, got %q", GetProfile("bogus").Name())
	}
	if GetProfile("").Name() != "large" {
		t.Errorf("empty profile should fall back to large, got %q", GetProfile("").Name())
	}
}
