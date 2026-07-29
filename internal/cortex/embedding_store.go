package cortex

import (
	"context"
	"fmt"
	"time"

	"github.com/Fail-Safe/Noema/internal/trace"
)

// EmbeddingStatus summarizes semantic-search embedding coverage for a given
// embedding model. "Embeddable" is every non-trashed trace (active +
// archived); trashed traces are excluded by design. A trace is "embedded"
// only when it has a row for the active model whose source_hash still
// matches the trace's content_hash — otherwise it is "stale" (body changed
// or a different model) or "missing" (no row at all).
type EmbeddingStatus struct {
	Model      string
	Embeddable int
	Embedded   int
	Stale      int
	Missing    int
}

// EmbeddingStatus computes coverage counts for model. A model of "" reports
// everything as stale/missing (nothing matches an empty model), which is
// the correct signal for a cortex that hasn't configured semantic search.
func (c *Cortex) EmbeddingStatus(model string) (EmbeddingStatus, error) {
	st := EmbeddingStatus{Model: model}
	if err := c.DB.QueryRow(`SELECT COUNT(*) FROM traces WHERE trashed_at IS NULL`).Scan(&st.Embeddable); err != nil {
		return st, fmt.Errorf("count embeddable: %w", err)
	}
	var withRow int
	if err := c.DB.QueryRow(
		`SELECT COUNT(*) FROM traces t JOIN trace_embeddings te ON te.trace_id = t.id WHERE t.trashed_at IS NULL`,
	).Scan(&withRow); err != nil {
		return st, fmt.Errorf("count embedding rows: %w", err)
	}
	if err := c.DB.QueryRow(
		`SELECT COUNT(*) FROM traces t JOIN trace_embeddings te ON te.trace_id = t.id
		 WHERE t.trashed_at IS NULL AND te.embedding_model = ? AND te.source_hash = t.content_hash`,
		model,
	).Scan(&st.Embedded); err != nil {
		return st, fmt.Errorf("count embedded: %w", err)
	}
	st.Missing = st.Embeddable - withRow
	st.Stale = withRow - st.Embedded
	return st, nil
}

// EmbedBackfillOpts tunes a backfill run.
type EmbedBackfillOpts struct {
	Force     bool // re-embed every embeddable trace, ignoring staleness
	Limit     int  // cap traces processed this run (0 = no cap)
	MaxChars  int  // per-trace text budget (0 = default)
	BatchSize int  // traces per Embed call (0 = 64)
}

// EmbedBackfillResult reports what a run did.
type EmbedBackfillResult struct {
	Considered int // candidates selected
	Embedded   int // vectors computed and stored
}

// EmbedBackfill computes and stores embeddings for traces that are missing
// or stale for model (or all embeddable traces when Force). It is
// idempotent (a second run with no changes does nothing), resumable (each
// batch is committed before the next, so a mid-run failure keeps prior
// progress), and safe to run while serving (WAL). It never blocks a
// mutation — callers invoke it explicitly (CLI) or on a schedule.
//
// Body text is read from each trace's markdown file (the source of truth);
// the stored source_hash is the trace's content_hash at selection time, so
// a later body edit re-marks the row stale.
func (c *Cortex) EmbedBackfill(ctx context.Context, e Embedder, model string, opts EmbedBackfillOpts) (EmbedBackfillResult, error) {
	var res EmbedBackfillResult
	if e == nil {
		return res, fmt.Errorf("embedder is nil")
	}
	if model == "" {
		return res, fmt.Errorf("embedding model is empty")
	}
	batch := opts.BatchSize
	if batch <= 0 {
		batch = 64
	}
	maxChars := opts.MaxChars
	if maxChars <= 0 {
		maxChars = defaultEmbedMaxChars
	}

	q := `SELECT t.id, t.content_hash FROM traces t
	      LEFT JOIN trace_embeddings te ON te.trace_id = t.id
	      WHERE t.trashed_at IS NULL`
	var args []any
	if !opts.Force {
		q += ` AND (te.trace_id IS NULL OR te.embedding_model != ? OR te.source_hash != t.content_hash OR t.content_hash IS NULL OR t.content_hash = '')`
		args = append(args, model)
	}
	q += ` ORDER BY t.created_at`
	if opts.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := c.DB.Query(q, args...)
	if err != nil {
		return res, fmt.Errorf("query candidates: %w", err)
	}
	type cand struct{ id, contentHash string }
	var cands []cand
	for rows.Next() {
		var ce cand
		var contentHash *string
		if err := rows.Scan(&ce.id, &contentHash); err != nil {
			rows.Close()
			return res, fmt.Errorf("scan candidate: %w", err)
		}
		if contentHash != nil {
			ce.contentHash = *contentHash
		}
		cands = append(cands, ce)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("iterate candidates: %w", err)
	}
	res.Considered = len(cands)

	now := time.Now().UTC().Format(time.RFC3339)
	for start := 0; start < len(cands); start += batch {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		end := min(start+batch, len(cands))
		chunk := cands[start:end]

		texts := make([]string, 0, len(chunk))
		ids := make([]string, 0, len(chunk))
		hashes := make([]string, 0, len(chunk))
		for _, ce := range chunk {
			r, err := c.Get(ce.id)
			if err != nil {
				continue // vanished mid-run; skip
			}
			t, err := trace.ParseFile(c.filePath(r))
			if err != nil {
				continue // unreadable/malformed; skip
			}
			sourceHash := ce.contentHash
			if sourceHash == "" {
				// The markdown body is authoritative. Older indexes could
				// contain a NULL content_hash, which previously aborted the
				// entire backfill scan. Reconstruct the derived index value
				// so this trace becomes stably up-to-date after embedding.
				sourceHash = trace.ContentHash(t.Body)
				if _, err := c.DB.Exec(
					`UPDATE traces SET content_hash = ? WHERE id = ? AND (content_hash IS NULL OR content_hash = '')`,
					sourceHash, ce.id,
				); err != nil {
					return res, fmt.Errorf("repair content hash for %s: %w", ce.id, err)
				}
			}
			texts = append(texts, embeddingText(r.Title, t.Body, maxChars))
			ids = append(ids, ce.id)
			hashes = append(hashes, sourceHash)
		}
		if len(texts) == 0 {
			continue
		}

		vecs, err := e.Embed(ctx, model, texts)
		if err != nil {
			return res, fmt.Errorf("embed batch: %w", err)
		}
		if len(vecs) != len(texts) {
			return res, fmt.Errorf("embedder returned %d vectors for %d inputs", len(vecs), len(texts))
		}
		for i := range ids {
			v := vecs[i]
			normalizeEmbedding(v)
			if err := c.upsertEmbedding(ids[i], model, len(v), encodeEmbedding(v), hashes[i], now); err != nil {
				return res, fmt.Errorf("store embedding for %s: %w", ids[i], err)
			}
			res.Embedded++
		}
	}
	return res, nil
}

// upsertEmbedding inserts or replaces the embedding row for a trace.
func (c *Cortex) upsertEmbedding(id, model string, dim int, blob []byte, sourceHash, updatedAt string) error {
	_, err := c.DB.Exec(
		`INSERT INTO trace_embeddings (trace_id, embedding_model, dim, embedding, source_hash, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(trace_id) DO UPDATE SET
		   embedding_model = excluded.embedding_model,
		   dim             = excluded.dim,
		   embedding       = excluded.embedding,
		   source_hash     = excluded.source_hash,
		   updated_at      = excluded.updated_at`,
		id, model, dim, blob, sourceHash, updatedAt,
	)
	return err
}
