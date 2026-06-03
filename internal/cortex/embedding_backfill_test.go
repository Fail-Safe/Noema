package cortex_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// stubEmbedder is a deterministic cortex.Embedder for tests: no network,
// returns one fixed-dim vector per input and records call/input counts.
type stubEmbedder struct {
	calls  int
	inputs int
	model  string
	dim    int
}

func (s *stubEmbedder) Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	s.calls++
	s.model = model
	s.inputs += len(inputs)
	d := s.dim
	if d == 0 {
		d = 4
	}
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		v := make([]float32, d)
		for j := range v {
			v[j] = float32(len(in)%7 + j + 1)
		}
		out[i] = v
	}
	return out, nil
}

func seedTraces(t *testing.T, cx *cortex.Cortex, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	titles := []string{"alpha doc", "beta note", "gamma idea", "delta plan", "epsilon log"}
	for i := range n {
		tr := trace.New(titles[i%len(titles)], "note", "tester", nil, fmt.Sprintf("body number %d", i))
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
		ids = append(ids, tr.ID)
	}
	return ids
}

func TestEmbedBackfill_Lifecycle(t *testing.T) {
	cx := setup(t)
	ctx := context.Background()
	ids := seedTraces(t, cx, 3)

	// Initially everything is missing.
	st, err := cx.EmbeddingStatus("m1")
	if err != nil {
		t.Fatalf("EmbeddingStatus: %v", err)
	}
	if st.Embeddable != 3 || st.Missing != 3 || st.Embedded != 0 {
		t.Fatalf("initial status = %+v, want embeddable=3 missing=3 embedded=0", st)
	}

	emb := &stubEmbedder{dim: 4}
	res, err := cx.EmbedBackfill(ctx, emb, "m1", cortex.EmbedBackfillOpts{})
	if err != nil {
		t.Fatalf("EmbedBackfill: %v", err)
	}
	if res.Considered != 3 || res.Embedded != 3 {
		t.Fatalf("backfill result = %+v, want considered=3 embedded=3", res)
	}
	if emb.model != "m1" {
		t.Errorf("embedder saw model %q, want m1", emb.model)
	}

	st, _ = cx.EmbeddingStatus("m1")
	if st.Embedded != 3 || st.Missing != 0 || st.Stale != 0 {
		t.Fatalf("post-backfill status = %+v, want embedded=3", st)
	}

	// Idempotent: nothing stale, so a second run does no work.
	res2, _ := cx.EmbedBackfill(ctx, emb, "m1", cortex.EmbedBackfillOpts{})
	if res2.Considered != 0 || res2.Embedded != 0 {
		t.Errorf("second run = %+v, want no work (idempotent)", res2)
	}

	// Stored row: dim correct and source_hash == content_hash.
	var dim int
	var sh, ch string
	if err := cx.DB.QueryRow(`SELECT dim, source_hash FROM trace_embeddings WHERE trace_id=?`, ids[0]).Scan(&dim, &sh); err != nil {
		t.Fatalf("read embedding row: %v", err)
	}
	if err := cx.DB.QueryRow(`SELECT content_hash FROM traces WHERE id=?`, ids[0]).Scan(&ch); err != nil {
		t.Fatalf("read content_hash: %v", err)
	}
	if dim != 4 {
		t.Errorf("stored dim = %d, want 4", dim)
	}
	if sh != ch || sh == "" {
		t.Errorf("source_hash %q != content_hash %q", sh, ch)
	}

	// A body edit makes that trace stale again.
	if err := cx.Append(ids[0], "appended content changes the body hash"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	st, _ = cx.EmbeddingStatus("m1")
	if st.Stale != 1 {
		t.Fatalf("after body edit, stale = %d, want 1", st.Stale)
	}
	res3, _ := cx.EmbedBackfill(ctx, emb, "m1", cortex.EmbedBackfillOpts{})
	if res3.Considered != 1 || res3.Embedded != 1 {
		t.Errorf("re-embed after edit = %+v, want considered=1 embedded=1", res3)
	}

	// Switching models invalidates every row for the new model.
	res4, _ := cx.EmbedBackfill(ctx, emb, "m2", cortex.EmbedBackfillOpts{})
	if res4.Considered != 3 {
		t.Errorf("model change considered = %d, want 3", res4.Considered)
	}
	if newSt, _ := cx.EmbeddingStatus("m2"); newSt.Embedded != 3 {
		t.Errorf("m2 embedded = %d, want 3", newSt.Embedded)
	}
	if oldSt, _ := cx.EmbeddingStatus("m1"); oldSt.Embedded != 0 || oldSt.Stale != 3 {
		t.Errorf("m1 status after switch = %+v, want embedded=0 stale=3", oldSt)
	}
}

func TestEmbedBackfill_ForceAndLimit(t *testing.T) {
	cx := setup(t)
	ctx := context.Background()
	seedTraces(t, cx, 3)
	emb := &stubEmbedder{dim: 4}

	if _, err := cx.EmbedBackfill(ctx, emb, "m", cortex.EmbedBackfillOpts{}); err != nil {
		t.Fatalf("initial backfill: %v", err)
	}
	// Force re-embeds even up-to-date rows.
	r, _ := cx.EmbedBackfill(ctx, emb, "m", cortex.EmbedBackfillOpts{Force: true})
	if r.Considered != 3 || r.Embedded != 3 {
		t.Errorf("force = %+v, want considered=3 embedded=3", r)
	}
	// Limit caps the run.
	r2, _ := cx.EmbedBackfill(ctx, emb, "m", cortex.EmbedBackfillOpts{Force: true, Limit: 2})
	if r2.Considered != 2 || r2.Embedded != 2 {
		t.Errorf("limit=2 => %+v, want considered=2 embedded=2", r2)
	}
}

// TestEmbedBackfill_WithRealHTTPClient runs the backfill through the actual
// consolidation.HTTPLLMClient against an httptest /embeddings server,
// proving the real client satisfies cortex.Embedder and the end-to-end
// wiring stores vectors of the served dimension.
func TestEmbedBackfill_WithRealHTTPClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"data\":["))
		for i := range req.Input {
			if i > 0 {
				w.Write([]byte(","))
			}
			// 3-dim vector per input
			w.Write([]byte(`{"index":` + strconv.Itoa(i) + `,"embedding":[0.1,0.2,0.3]}`))
		}
		w.Write([]byte("]}"))
	}))
	defer server.Close()

	cx := setup(t)
	ids := seedTraces(t, cx, 2)
	client, err := consolidation.NewHTTPLLMClient(server.URL, "")
	if err != nil {
		t.Fatalf("NewHTTPLLMClient: %v", err)
	}
	res, err := cx.EmbedBackfill(context.Background(), client, "real-model", cortex.EmbedBackfillOpts{})
	if err != nil {
		t.Fatalf("EmbedBackfill: %v", err)
	}
	if res.Embedded != 2 {
		t.Fatalf("embedded = %d, want 2", res.Embedded)
	}
	var dim int
	if err := cx.DB.QueryRow(`SELECT dim FROM trace_embeddings WHERE trace_id=?`, ids[0]).Scan(&dim); err != nil {
		t.Fatalf("read dim: %v", err)
	}
	if dim != 3 {
		t.Errorf("stored dim = %d, want 3 (from server)", dim)
	}
	if st, _ := cx.EmbeddingStatus("real-model"); st.Embedded != 2 {
		t.Errorf("status embedded = %d, want 2", st.Embedded)
	}
}

func TestEmbedBackfill_Guards(t *testing.T) {
	cx := setup(t)
	ctx := context.Background()
	if _, err := cx.EmbedBackfill(ctx, nil, "m", cortex.EmbedBackfillOpts{}); err == nil {
		t.Error("nil embedder should error")
	}
	if _, err := cx.EmbedBackfill(ctx, &stubEmbedder{}, "", cortex.EmbedBackfillOpts{}); err == nil {
		t.Error("empty model should error")
	}
}
