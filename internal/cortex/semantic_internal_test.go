package cortex

import (
	"math"
	"testing"
)

func TestTopKCosine(t *testing.T) {
	q := []float32{1, 0, 0}
	cands := []vectorCand{
		{row: Row{ID: "a"}, vec: []float32{1, 0, 0}},     // dot 1.0
		{row: Row{ID: "b"}, vec: []float32{0, 1, 0}},     // dot 0.0
		{row: Row{ID: "c"}, vec: []float32{0.7, 0.7, 0}}, // dot 0.7
		{row: Row{ID: "d"}, vec: []float32{1, 0}},        // dim mismatch -> skipped
	}
	res := topKCosine(q, cands, 2)
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2 (limit)", len(res))
	}
	if res[0].ID != "a" || res[1].ID != "c" {
		t.Errorf("order = [%s %s], want [a c]", res[0].ID, res[1].ID)
	}
	// Limit 0 returns all rankable (dim-mismatch still excluded).
	all := topKCosine(q, cands, 0)
	if len(all) != 3 {
		t.Errorf("limit 0 returned %d, want 3", len(all))
	}
}

func TestAllFinite(t *testing.T) {
	if !allFinite([]float32{1, -2, 0.5}) {
		t.Error("finite vector reported non-finite")
	}
	if allFinite([]float32{1, float32(math.Inf(1)), 3}) {
		t.Error("+Inf not detected")
	}
	if allFinite([]float32{float32(math.NaN())}) {
		t.Error("NaN not detected")
	}
}
