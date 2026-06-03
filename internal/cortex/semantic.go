package cortex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ScoredRow is one semantic-search result: a trace row plus its cosine
// similarity to the query (higher is closer; vectors are unit-normalized
// so cosine equals the dot product).
type ScoredRow struct {
	Row
	Score float64
}

// SemanticOpts tunes a semantic query.
type SemanticOpts struct {
	Model           string // embedding model whose vectors to search
	Limit           int    // max results (default 10)
	IncludeArchived bool   // include archived traces (default false)
}

// SemanticSearch embeds the query string with e and ranks the cortex's
// stored vectors (for opts.Model) by cosine similarity. Trashed traces are
// always excluded; archived traces are excluded unless opts.IncludeArchived.
// An empty query returns no results. The caller supplies the embedder so
// the cortex package stays free of an import cycle.
func (c *Cortex) SemanticSearch(ctx context.Context, e Embedder, query string, opts SemanticOpts) ([]ScoredRow, error) {
	if e == nil {
		return nil, errors.New("embedder is nil")
	}
	if opts.Model == "" {
		return nil, errors.New("embedding model is empty")
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	vecs, err := e.Embed(ctx, opts.Model, []string{q})
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, errors.New("embedder returned no query vector")
	}
	qv := vecs[0]
	normalizeEmbedding(qv)

	cands, err := c.loadEmbeddedCandidates(opts.Model, opts.IncludeArchived, "")
	if err != nil {
		return nil, err
	}
	return topKCosine(qv, cands, effLimit(opts.Limit)), nil
}

// SemanticSimilar ranks other traces against the stored vector of traceID
// (no query embedding needed — it reuses the source trace's own vector, a
// strictly better signal than the lexical FindSimilar's top-term proxy).
// The source trace must already be embedded for opts.Model.
func (c *Cortex) SemanticSimilar(traceID string, opts SemanticOpts) ([]ScoredRow, error) {
	if opts.Model == "" {
		return nil, errors.New("embedding model is empty")
	}
	var blob []byte
	err := c.DB.QueryRow(
		`SELECT embedding FROM trace_embeddings WHERE trace_id = ? AND embedding_model = ?`,
		traceID, opts.Model,
	).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("trace %s has no %s embedding yet (run: noema embeddings backfill)", traceID, opts.Model)
	}
	if err != nil {
		return nil, fmt.Errorf("loading source vector: %w", err)
	}
	qv, err := decodeEmbedding(blob)
	if err != nil {
		return nil, fmt.Errorf("decoding source vector: %w", err)
	}
	// Stored vectors are already normalized; no need to re-normalize.
	cands, err := c.loadEmbeddedCandidates(opts.Model, opts.IncludeArchived, traceID)
	if err != nil {
		return nil, err
	}
	return topKCosine(qv, cands, effLimit(opts.Limit)), nil
}

func effLimit(l int) int {
	if l <= 0 {
		return 10
	}
	return l
}

// vectorCand pairs a row with its decoded embedding for ranking.
type vectorCand struct {
	row Row
	vec []float32
}

// topKCosine ranks candidates by dot product against query (== cosine for
// unit vectors), descending, and returns the top limit. Candidates whose
// dimension differs from the query (e.g. a stale row from another model
// that slipped through) are skipped rather than mis-scored.
func topKCosine(query []float32, cands []vectorCand, limit int) []ScoredRow {
	out := make([]ScoredRow, 0, len(cands))
	for _, c := range cands {
		if len(c.vec) != len(query) {
			continue
		}
		var dot float64
		for i := range query {
			dot += float64(query[i]) * float64(c.vec[i])
		}
		out = append(out, ScoredRow{Row: c.row, Score: dot})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// loadEmbeddedCandidates returns every embedded, ranking-eligible trace for
// model: non-trashed (archived excluded unless includeArchived), optionally
// excluding excludeID (the source trace in a similarity query). Vectors
// containing non-finite values (a corrupted BLOB) are skipped so they can't
// poison ranking — the finiteness check the Phase-1 review flagged, applied
// here at the consumer where stored vectors are actually read.
func (c *Cortex) loadEmbeddedCandidates(model string, includeArchived bool, excludeID string) ([]vectorCand, error) {
	q := `SELECT t.id, t.title, t.type, t.tier, t.author, t.origin,
	             t.archived_at, t.trashed_at, t.created_at, t.updated_at,
	             t.content_hash, t.source_locked, t.source_hash,
	             te.embedding
	      FROM trace_embeddings te
	      JOIN traces t ON t.id = te.trace_id
	      WHERE te.embedding_model = ? AND t.trashed_at IS NULL`
	args := []any{model}
	if !includeArchived {
		q += ` AND t.archived_at IS NULL`
	}
	if excludeID != "" {
		q += ` AND t.id != ?`
		args = append(args, excludeID)
	}

	rows, err := c.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("loading embedded candidates: %w", err)
	}
	defer rows.Close()

	var out []vectorCand
	for rows.Next() {
		var r Row
		var archivedAt, trashedAt, contentHash, sourceHash *string
		var sourceLocked int
		var blob []byte
		if err := rows.Scan(
			&r.ID, &r.Title, &r.Type, &r.Tier, &r.Author, &r.Origin,
			&archivedAt, &trashedAt, &r.CreatedAt, &r.UpdatedAt,
			&contentHash, &sourceLocked, &sourceHash, &blob,
		); err != nil {
			return nil, err
		}
		if archivedAt != nil {
			r.ArchivedAt = *archivedAt
		}
		if trashedAt != nil {
			r.TrashedAt = *trashedAt
		}
		if contentHash != nil {
			r.ContentHash = *contentHash
		}
		if sourceHash != nil {
			r.SourceHash = *sourceHash
		}
		r.SourceLocked = sourceLocked != 0

		vec, err := decodeEmbedding(blob)
		if err != nil || !allFinite(vec) {
			continue // skip corrupted/non-finite vectors
		}
		out = append(out, vectorCand{row: r, vec: vec})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// allFinite reports whether every element of v is a finite number.
func allFinite(v []float32) bool {
	for _, f := range v {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return false
		}
	}
	return true
}
