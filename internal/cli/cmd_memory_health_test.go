package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// newCortexForHealth builds an on-disk cortex with no traces. Used by
// the health-command tests where the cortex methods do the real work
// and we only care that the CLI dispatch / output formatting wires
// them up correctly.
func newCortexForHealth(t *testing.T, name string) *cortex.Cortex {
	t.Helper()
	parent := t.TempDir()
	if _, err := cortex.Create(name, parent); err != nil {
		t.Fatalf("cortex.Create: %v", err)
	}
	cx, err := cortex.Open(name, filepath.Join(parent, name))
	if err != nil {
		t.Fatalf("cortex.Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx
}

// TestRunMemoryHealth_JSONEnvelope locks in the wire contract: a
// single top-level schema_version per the design doc, plus the three
// expected sections. External tooling will pin to this shape, so
// silently dropping schema_version or renaming a section is a
// breaking change this test should fail on.
func TestRunMemoryHealth_JSONEnvelope(t *testing.T) {
	cx := newCortexForHealth(t, "envtest")
	var buf bytes.Buffer
	if err := runMemoryHealth(&buf, cx, "24h", "json"); err != nil {
		t.Fatalf("runMemoryHealth: %v", err)
	}
	var got struct {
		SchemaVersion int             `json:"schema_version"`
		Activity      json.RawMessage `json:"activity"`
		Latency       json.RawMessage `json:"latency"`
		OneSourceMid  json.RawMessage `json:"one_source_mid"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, buf.String())
	}
	if got.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", got.SchemaVersion)
	}
	if len(got.Activity) == 0 {
		t.Error("activity section missing")
	}
	if len(got.Latency) == 0 {
		t.Error("latency section missing")
	}
	if len(got.OneSourceMid) == 0 {
		t.Error("one_source_mid section missing")
	}
}

// TestRunMemoryHealth_TextRendersAllSections pins that every section
// header appears in the text output even on an empty cortex — the
// operator should always see the layout, with "(no events)" rather
// than a missing block when there's nothing to show.
func TestRunMemoryHealth_TextRendersAllSections(t *testing.T) {
	cx := newCortexForHealth(t, "texttest")
	var buf bytes.Buffer
	if err := runMemoryHealth(&buf, cx, "24h", "text"); err != nil {
		t.Fatalf("runMemoryHealth: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Consolidation activity",
		"(no events in window)",
		"Promotion latency",
		"1-source mid leak detector",
		"gate is holding",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
}

// TestRunMemoryHealth_LeakSignalFlipsWhenPromoteFires is the
// load-bearing test for the leak detector's operator-facing message.
// A clean cortex shows "gate is holding"; the moment a 1-source
// trace is promoted to mid, the message must flip to the warn line.
// If the threshold check ever inverts this would silently mask leaks.
func TestRunMemoryHealth_LeakSignalFlipsWhenPromoteFires(t *testing.T) {
	cx := newCortexForHealth(t, "leaktest")

	src := trace.New("source", "note", "", nil, "body")
	if err := cx.Add(src); err != nil {
		t.Fatalf("Add src: %v", err)
	}
	leaker := trace.New("leaker", "note", "", nil, "body")
	leaker.DerivedFrom = []string{src.ID}
	if err := cx.Add(leaker); err != nil {
		t.Fatalf("Add leaker: %v", err)
	}
	if err := cx.Promote(leaker.ID, trace.TierMid); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	var buf bytes.Buffer
	if err := runMemoryHealth(&buf, cx, "24h", "text"); err != nil {
		t.Fatalf("runMemoryHealth: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "recent leak") {
		t.Errorf("expected leak warning in output, got:\n%s", out)
	}
	if strings.Contains(out, "gate is holding") {
		t.Errorf("'gate is holding' shouldn't appear when PromotedLast7d > 0:\n%s", out)
	}
}

// TestRunMemoryHealth_InvalidOutput surfaces a typo in --output as a
// clear error rather than silently falling through to text rendering.
func TestRunMemoryHealth_InvalidOutput(t *testing.T) {
	cx := newCortexForHealth(t, "errtest")
	var buf bytes.Buffer
	err := runMemoryHealth(&buf, cx, "24h", "yaml")
	if err == nil {
		t.Fatal("expected error for unsupported --output, got nil")
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error should name the bad value: %v", err)
	}
}
