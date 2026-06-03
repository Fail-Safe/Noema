package cortex_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

// TestManifest_SearchBlockRoundTrip is the highest-value Phase 1 serialization
// guard: a populated `search:` block must survive WriteManifest -> ReadManifest
// with every field intact. Catches a wrong/missing yaml tag, float drift on
// hybrid_weight, or ValidateSearch (called inside ReadManifest) rejecting a
// valid block.
func TestManifest_SearchBlockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("searchcfg", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cdir := filepath.Join(dir, "searchcfg")

	m, err := cortex.ReadManifest(cdir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	want := &cortex.SearchConfig{
		SemanticEnabled:   true,
		EmbeddingEndpoint: "http://localhost:11434/v1",
		EmbeddingModel:    "nomic-embed-text",
		APIKeyEnv:         "EMB_KEY",
		DefaultMode:       cortex.SearchModeHybrid,
		HybridWeight:      0.7,
		MaxChars:          16000,
	}
	m.Search = want
	if err := cortex.WriteManifest(cdir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	m2, err := cortex.ReadManifest(cdir)
	if err != nil {
		t.Fatalf("ReadManifest (2): %v", err)
	}
	if m2.Search == nil {
		t.Fatal("Search block lost on round-trip")
	}
	got := m2.Search
	if got.SemanticEnabled != want.SemanticEnabled ||
		got.EmbeddingEndpoint != want.EmbeddingEndpoint ||
		got.EmbeddingModel != want.EmbeddingModel ||
		got.APIKeyEnv != want.APIKeyEnv ||
		got.DefaultMode != want.DefaultMode ||
		got.HybridWeight != want.HybridWeight ||
		got.MaxChars != want.MaxChars {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, want)
	}
}

// TestManifest_SearchBlockOmittedWhenNil confirms the back-compat promise:
// a cortex with no search config writes no `search:` key, so older binaries
// and lexical-only cortexes are unaffected.
func TestManifest_SearchBlockOmittedWhenNil(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("plaincfg", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cdir := filepath.Join(dir, "plaincfg")

	m, err := cortex.ReadManifest(cdir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if err := cortex.WriteManifest(cdir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cdir, "cortex.md"))
	if err != nil {
		t.Fatalf("read cortex.md: %v", err)
	}
	if bytes.Contains(data, []byte("search:")) {
		t.Errorf("cortex.md unexpectedly contains a search: block:\n%s", data)
	}
}
