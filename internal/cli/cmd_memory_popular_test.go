package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

func TestRunMemoryPopular_JSONEnvelope(t *testing.T) {
	cx := newCortexForHealth(t, "poptest")
	var buf bytes.Buffer
	if err := runMemoryPopular(&buf, cx, 10, "json"); err != nil {
		t.Fatalf("runMemoryPopular: %v", err)
	}
	var got struct {
		SchemaVersion int             `json:"schema_version"`
		Top           int             `json:"top"`
		Traces        json.RawMessage `json:"traces"`
		Tags          json.RawMessage `json:"tags"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if got.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", got.SchemaVersion)
	}
	if got.Top != 10 {
		t.Errorf("top = %d, want 10", got.Top)
	}
}

// TestRunMemoryPopular_RenderShowsRealData seeds a tagged trace,
// generates a search hit, and asserts the trace title and tag both
// appear in the text output. Locks the CLI rendering against drift
// from the underlying cortex method's column order.
func TestRunMemoryPopular_RenderShowsRealData(t *testing.T) {
	cx := newCortexForHealth(t, "popdata")

	tr := trace.New("findme", "note", "", []string{"sample-tag"}, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := cx.SearchAs("findme", cortex.ListOptions{}, cortex.ActorAgent, cortex.DefaultSearchHitTopN); err != nil {
		t.Fatalf("SearchAs: %v", err)
	}

	var buf bytes.Buffer
	if err := runMemoryPopular(&buf, cx, 5, "text"); err != nil {
		t.Fatalf("runMemoryPopular: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "findme") {
		t.Errorf("text output missing trace title:\n%s", out)
	}
	if !strings.Contains(out, "sample-tag") {
		t.Errorf("text output missing tag:\n%s", out)
	}
}

// TestRunMemoryPopular_EmptyShowsBothPlaceholders pins that even on
// an empty cortex, both leaderboard sections render with explicit
// "no engagement yet" placeholders rather than blank space — keeps
// the layout legible the first time an operator runs the command.
func TestRunMemoryPopular_EmptyShowsBothPlaceholders(t *testing.T) {
	cx := newCortexForHealth(t, "popempty")
	var buf bytes.Buffer
	if err := runMemoryPopular(&buf, cx, 10, "text"); err != nil {
		t.Fatalf("runMemoryPopular: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Top 10 traces") {
		t.Errorf("missing traces section header:\n%s", out)
	}
	if !strings.Contains(out, "Top 10 tags") {
		t.Errorf("missing tags section header:\n%s", out)
	}
	if !strings.Contains(out, "no traces with engagement yet") {
		t.Errorf("missing traces empty placeholder:\n%s", out)
	}
	if !strings.Contains(out, "no tagged traces with engagement yet") {
		t.Errorf("missing tags empty placeholder:\n%s", out)
	}
}

func TestRunMemoryPopular_InvalidOutput(t *testing.T) {
	cx := newCortexForHealth(t, "poperr")
	var buf bytes.Buffer
	err := runMemoryPopular(&buf, cx, 10, "yaml")
	if err == nil {
		t.Fatal("expected error for unsupported --output, got nil")
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error should name the bad value: %v", err)
	}
}
