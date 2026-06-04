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

func TestRRFFuse(t *testing.T) {
	lex := []Row{{ID: "a"}, {ID: "b"}, {ID: "c"}}                                     // lexical order a,b,c
	sem := []ScoredRow{{Row: Row{ID: "c"}}, {Row: Row{ID: "d"}}, {Row: Row{ID: "a"}}} // semantic order c,d,a
	ids := func(rs []ScoredRow) []string {
		out := make([]string, len(rs))
		for i := range rs {
			out[i] = rs[i].ID
		}
		return out
	}

	// weight 0 => pure lexical order (items only in semantic get score 0, sink).
	if got := ids(rrfFuse(lex, sem, 0, 0)); got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("weight 0 top3 = %v, want [a b c]", got[:3])
	}
	// weight 1 => pure semantic order.
	if got := ids(rrfFuse(lex, sem, 1, 0)); got[0] != "c" || got[1] != "d" || got[2] != "a" {
		t.Errorf("weight 1 top3 = %v, want [c d a]", got[:3])
	}
	// weight .5 => a & c tie (symmetric), b & d tie; ID breaks ties.
	if got := ids(rrfFuse(lex, sem, 0.5, 0)); got[0] != "a" || got[1] != "c" || got[2] != "b" || got[3] != "d" {
		t.Errorf("weight .5 = %v, want [a c b d]", got)
	}
	// limit truncates.
	if l := rrfFuse(lex, sem, 0.5, 2); len(l) != 2 {
		t.Errorf("limit 2 returned %d", len(l))
	}
	// weight > 1 clamps to 1 (same as semantic order).
	if got := ids(rrfFuse(lex, sem, 5, 0)); got[0] != "c" {
		t.Errorf("weight 5 should clamp to 1; top = %s, want c", got[0])
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
