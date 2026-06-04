package cortex_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

// topicEmbedder maps text to a 3-dim "topic" vector by counting occurrences
// of alpha/beta/gamma, so rankings are deterministic and assertable. (The
// shared seedTraces helper makes trace i about alpha/beta/gamma by title.)
type topicEmbedder struct{}

func (topicEmbedder) Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		v := []float32{0.01, 0.01, 0.01}
		for w := range strings.FieldsSeq(strings.ToLower(in)) {
			switch {
			case strings.Contains(w, "alpha"):
				v[0]++
			case strings.Contains(w, "beta"):
				v[1]++
			case strings.Contains(w, "gamma"):
				v[2]++
			}
		}
		out[i] = v
	}
	return out, nil
}

func setupSemantic(t *testing.T) (*cortex.Cortex, []string) {
	t.Helper()
	cx := setup(t)
	ids := seedTraces(t, cx, 3) // 0=alpha, 1=beta, 2=gamma (by title)
	if _, err := cx.EmbedBackfill(context.Background(), topicEmbedder{}, "tm", cortex.EmbedBackfillOpts{}); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	return cx, ids
}

func TestSemanticSearch_RanksByTopic(t *testing.T) {
	cx, ids := setupSemantic(t)
	ctx := context.Background()

	res, err := cx.SemanticSearch(ctx, topicEmbedder{}, "alpha alpha alpha", cortex.SemanticOpts{Model: "tm"})
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("got %d results, want 3", len(res))
	}
	if res[0].ID != ids[0] {
		t.Errorf("top result = %s, want alpha trace %s", res[0].ID, ids[0])
	}

	res, _ = cx.SemanticSearch(ctx, topicEmbedder{}, "gamma gamma", cortex.SemanticOpts{Model: "tm"})
	if res[0].ID != ids[2] {
		t.Errorf("gamma query top = %s, want %s", res[0].ID, ids[2])
	}
}

func TestSemanticSearch_LimitAndArchived(t *testing.T) {
	cx, ids := setupSemantic(t)
	ctx := context.Background()

	res, _ := cx.SemanticSearch(ctx, topicEmbedder{}, "alpha", cortex.SemanticOpts{Model: "tm", Limit: 1})
	if len(res) != 1 {
		t.Fatalf("limit=1 returned %d", len(res))
	}

	// Archive the beta trace; default search must exclude it.
	if err := cx.Archive(ids[1]); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	res, _ = cx.SemanticSearch(ctx, topicEmbedder{}, "beta", cortex.SemanticOpts{Model: "tm"})
	for _, r := range res {
		if r.ID == ids[1] {
			t.Error("archived trace returned without IncludeArchived")
		}
	}
	res, _ = cx.SemanticSearch(ctx, topicEmbedder{}, "beta", cortex.SemanticOpts{Model: "tm", IncludeArchived: true})
	found := false
	for _, r := range res {
		if r.ID == ids[1] {
			found = true
		}
	}
	if !found {
		t.Error("archived trace missing with IncludeArchived=true")
	}
}

func TestSemanticSimilar_ExcludesSource(t *testing.T) {
	cx, ids := setupSemantic(t)

	res, err := cx.SemanticSimilar(ids[0], cortex.SemanticOpts{Model: "tm"})
	if err != nil {
		t.Fatalf("SemanticSimilar: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d, want 2 (source excluded)", len(res))
	}
	for _, r := range res {
		if r.ID == ids[0] {
			t.Error("source trace must be excluded from its own similarity")
		}
	}

	// A trace with no embedding for the model errors clearly.
	if _, err := cx.SemanticSimilar(ids[0], cortex.SemanticOpts{Model: "other-model"}); err == nil {
		t.Error("expected error for un-embedded model")
	}
}

func TestSemanticSearch_SkipsNonFiniteVectors(t *testing.T) {
	cx, ids := setupSemantic(t)
	ctx := context.Background()

	// Corrupt the gamma trace's stored vector to all +Inf (codec v1: 1-byte
	// version + little-endian float32 bits; +Inf = 0x7F800000).
	infBlob := []byte{1, 0x00, 0x00, 0x80, 0x7f, 0x00, 0x00, 0x80, 0x7f, 0x00, 0x00, 0x80, 0x7f}
	if _, err := cx.DB.Exec(
		`UPDATE trace_embeddings SET embedding=?, dim=3 WHERE trace_id=?`, infBlob, ids[2],
	); err != nil {
		t.Fatalf("corrupt vector: %v", err)
	}

	res, err := cx.SemanticSearch(ctx, topicEmbedder{}, "gamma", cortex.SemanticOpts{Model: "tm"})
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	for _, r := range res {
		if r.ID == ids[2] {
			t.Error("non-finite (corrupted) vector must be skipped, not ranked")
		}
	}
}

func TestHybridSearch_FusesAndRanks(t *testing.T) {
	cx, ids := setupSemantic(t)
	ctx := context.Background()

	// "alpha" matches the alpha trace both lexically (title token) and
	// semantically (topic vector), so it must rank first; semantic
	// contributes the other topics, so all 3 appear.
	res, err := cx.HybridSearch(ctx, topicEmbedder{}, "alpha alpha", cortex.SemanticOpts{Model: "tm"}, 0.5)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("got %d fused results, want 3", len(res))
	}
	if res[0].ID != ids[0] {
		t.Errorf("top fused result = %s, want alpha trace %s", res[0].ID, ids[0])
	}

	// Blank query short-circuits; nil embedder errors.
	if r, err := cx.HybridSearch(ctx, topicEmbedder{}, "  ", cortex.SemanticOpts{Model: "tm"}, 0.5); err != nil || r != nil {
		t.Errorf("blank query => (nil,nil); got (%v,%v)", r, err)
	}
	if _, err := cx.HybridSearch(ctx, nil, "x", cortex.SemanticOpts{Model: "tm"}, 0.5); err == nil {
		t.Error("nil embedder should error")
	}
}

func TestHybridSimilar_ExcludesSource(t *testing.T) {
	cx, ids := setupSemantic(t)
	res, err := cx.HybridSimilar(ids[0], cortex.SemanticOpts{Model: "tm"}, 0.5)
	if err != nil {
		t.Fatalf("HybridSimilar: %v", err)
	}
	for _, r := range res {
		if r.ID == ids[0] {
			t.Error("source trace must be excluded from its own hybrid similarity")
		}
	}
	if len(res) == 0 {
		t.Error("expected fused similar results")
	}
}

func TestSemanticSearch_QueryTooLong(t *testing.T) {
	cx, _ := setupSemantic(t)
	ctx := context.Background()
	long := strings.Repeat("x", 1001) // > MaxSearchQueryLen (1000)
	if _, err := cx.SemanticSearch(ctx, topicEmbedder{}, long, cortex.SemanticOpts{Model: "tm"}); err == nil {
		t.Error("over-long semantic query should be rejected (DoS guard)")
	}
	if _, err := cx.HybridSearch(ctx, topicEmbedder{}, long, cortex.SemanticOpts{Model: "tm"}, 0.5); err == nil {
		t.Error("over-long hybrid query should be rejected (DoS guard)")
	}
}

func TestSemanticSearch_Guards(t *testing.T) {
	cx := setup(t)
	ctx := context.Background()
	if _, err := cx.SemanticSearch(ctx, nil, "q", cortex.SemanticOpts{Model: "tm"}); err == nil {
		t.Error("nil embedder should error")
	}
	if _, err := cx.SemanticSearch(ctx, topicEmbedder{}, "q", cortex.SemanticOpts{}); err == nil {
		t.Error("empty model should error")
	}
	if res, err := cx.SemanticSearch(ctx, topicEmbedder{}, "   ", cortex.SemanticOpts{Model: "tm"}); err != nil || res != nil {
		t.Errorf("blank query => (nil,nil); got (%v,%v)", res, err)
	}
}
