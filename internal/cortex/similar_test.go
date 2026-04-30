package cortex_test

import (
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// TestFindSimilar_TopMatchesAreOnTopic seeds a cortex with a clear
// topic split (Go-related vs. unrelated traces) and asserts that the
// Go-themed traces rank above the unrelated ones for a Go-themed
// source. The exact internal ordering of the on-topic group isn't
// pinned because BM25 score deltas between near-identical texts are
// small and SQLite's BM25 doesn't promise stability across versions —
// what matters is the topic boundary.
func TestFindSimilar_TopMatchesAreOnTopic(t *testing.T) {
	cx := setup(t)

	src := trace.New(
		"Why we chose Go",
		"decision",
		"agent-1",
		[]string{"go", "language"},
		"We chose Go for its tooling, SQLite support via modernc, and goroutines. The static binary makes deployment simple.",
	)
	if err := cx.Add(src); err != nil {
		t.Fatalf("Add src: %v", err)
	}

	// On-topic: traces that share Go-related vocabulary.
	onTopic := []*trace.Trace{
		trace.New("Go tooling notes", "note", "agent-1", []string{"go"},
			"Go's tooling around modules, fmt, and vet is excellent. Static binaries help."),
		trace.New("SQLite and modernc", "fact", "agent-2", []string{"go", "sqlite"},
			"modernc provides a pure-Go SQLite driver, no CGO required for our deployments."),
		trace.New("Goroutines for fan-out", "skill", "agent-2", []string{"go", "concurrency"},
			"Goroutines make fan-out fan-in patterns natural in Go. Channels coordinate cleanly."),
	}

	// Off-topic: traces with no Go vocabulary.
	offTopic := []*trace.Trace{
		trace.New("Coffee preferences", "preference", "agent-3", []string{"coffee"},
			"Pour-over with a medium roast, ground coarse. Burr grinder essential."),
		trace.New("Bird watching log", "observation", "agent-3", []string{"birds"},
			"Cardinal at the feeder this morning. Junco flock arriving for winter."),
		trace.New("Bicycle maintenance", "skill", "agent-4", []string{"cycling"},
			"Chain lubrication every 200 miles. Wax-based lube prefers dry climates."),
	}

	for _, tr := range append(onTopic, offTopic...) {
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	matches, err := cx.FindSimilar(src.ID, cortex.SimilarOpts{Limit: 6})
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("got 0 matches, want at least %d", len(onTopic))
	}

	// The source trace must never appear in its own results.
	for _, m := range matches {
		if m.ID == src.ID {
			t.Fatalf("source trace %q appeared in similarity results", src.ID)
		}
	}

	// All on-topic traces should outrank all off-topic traces. Build
	// sets and check the boundary: every match before the first
	// off-topic hit should be on-topic.
	onTopicIDs := map[string]bool{}
	for _, tr := range onTopic {
		onTopicIDs[tr.ID] = true
	}
	offTopicIDs := map[string]bool{}
	for _, tr := range offTopic {
		offTopicIDs[tr.ID] = true
	}

	sawOffTopic := false
	for i, m := range matches {
		if offTopicIDs[m.ID] {
			sawOffTopic = true
			continue
		}
		if sawOffTopic && onTopicIDs[m.ID] {
			t.Errorf("on-topic match %q at rank %d appeared after an off-topic match", m.ID, i+1)
		}
	}

	// At minimum, the top result must be on-topic — otherwise the
	// document-as-query approach isn't doing its job.
	if !onTopicIDs[matches[0].ID] {
		t.Errorf("top match was %q (off-topic); expected an on-topic trace first", matches[0].ID)
	}
}

func TestFindSimilar_ExcludesSource(t *testing.T) {
	cx := setup(t)

	src := trace.New("solo trace", "note", "agent-1", []string{"alpha"},
		"alpha beta gamma delta epsilon zeta")
	if err := cx.Add(src); err != nil {
		t.Fatalf("Add src: %v", err)
	}
	other := trace.New("alpha beta echo", "note", "agent-1", []string{"alpha"},
		"alpha beta gamma — echo overlaps strongly with the source vocabulary.")
	if err := cx.Add(other); err != nil {
		t.Fatalf("Add other: %v", err)
	}

	matches, err := cx.FindSimilar(src.ID, cortex.SimilarOpts{})
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	for _, m := range matches {
		if m.ID == src.ID {
			t.Fatalf("source trace returned itself")
		}
	}
}

func TestFindSimilar_ExcludesArchivedByDefault(t *testing.T) {
	cx := setup(t)

	src := trace.New("kubernetes deployment notes", "note", "", []string{"k8s"},
		"kubernetes pods, services, and ingress controllers. helm charts simplify deployment.")
	if err := cx.Add(src); err != nil {
		t.Fatalf("Add src: %v", err)
	}
	related := trace.New("helm chart structure", "note", "", []string{"k8s"},
		"helm charts package kubernetes manifests. values.yaml is the customization surface.")
	if err := cx.Add(related); err != nil {
		t.Fatalf("Add related: %v", err)
	}
	if err := cx.Archive(related.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	matches, err := cx.FindSimilar(src.ID, cortex.SimilarOpts{})
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	for _, m := range matches {
		if m.ID == related.ID {
			t.Errorf("archived trace %q surfaced without IncludeArchived", related.ID)
		}
	}

	matches, err = cx.FindSimilar(src.ID, cortex.SimilarOpts{IncludeArchived: true})
	if err != nil {
		t.Fatalf("FindSimilar with IncludeArchived: %v", err)
	}
	found := false
	for _, m := range matches {
		if m.ID == related.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("archived trace %q not surfaced even with IncludeArchived=true", related.ID)
	}
}

func TestFindSimilar_NoMatchesReturnsEmpty(t *testing.T) {
	cx := setup(t)

	src := trace.New("one of a kind", "note", "", nil,
		"vocabulary entirely disjoint from anything else in the corpus.")
	if err := cx.Add(src); err != nil {
		t.Fatalf("Add src: %v", err)
	}

	matches, err := cx.FindSimilar(src.ID, cortex.SimilarOpts{})
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("got %d matches against a single-trace cortex, want 0", len(matches))
	}
}

func TestFindSimilar_RespectsLimit(t *testing.T) {
	cx := setup(t)

	src := trace.New("widget assembly procedure", "skill", "", []string{"widgets"},
		"widget assembly involves fastening the frame, attaching the lever, and calibrating the spring.")
	if err := cx.Add(src); err != nil {
		t.Fatalf("Add src: %v", err)
	}
	for i := range 8 {
		body := "widget frame lever spring calibration variant " + string(rune('a'+i))
		tr := trace.New("widget variant "+string(rune('a'+i)), "note", "", []string{"widgets"}, body)
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add variant %d: %v", i, err)
		}
	}

	matches, err := cx.FindSimilar(src.ID, cortex.SimilarOpts{Limit: 3})
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	if len(matches) != 3 {
		t.Errorf("Limit=3 returned %d matches, want 3", len(matches))
	}
}
