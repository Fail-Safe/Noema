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

// ScoredRow is one ranked result: a trace row plus a score. For semantic
// search the score is cosine similarity (higher is closer); for hybrid it
// is the fused reciprocal-rank score (also higher-is-better, but on a
// different scale — treat it as opaque ordering).
type ScoredRow struct {
	Row
	Score float64
}

// SemanticOpts tunes a semantic or hybrid query.
type SemanticOpts struct {
	Model           string // embedding model whose vectors to search
	Limit           int    // max results (default 10)
	IncludeArchived bool   // include archived traces (default false)
}

// rrfK is the Reciprocal Rank Fusion constant. 60 is the value from the
// original RRF paper and the de-facto default; it damps the contribution
// of low-ranked items without a tuning knob.
const rrfK = 60.0

// hybridLexicalPool bounds how many lexical (FindSimilar) candidates feed
// hybrid similarity fusion. Semantic contributes all embedded candidates;
// the lexical BM25 side is naturally top-k, so we request a generous pool.
const hybridLexicalPool = 50

// SemanticSearch embeds the query with e and ranks the cortex's stored
// vectors (for opts.Model) by cosine similarity. Trashed traces are always
// excluded; archived traces are excluded unless opts.IncludeArchived. An
// empty query returns no results.
func (c *Cortex) SemanticSearch(ctx context.Context, e Embedder, query string, opts SemanticOpts) ([]ScoredRow, error) {
	qv, err := c.embedQuery(ctx, e, opts.Model, query)
	if err != nil || qv == nil {
		return nil, err
	}
	cands, err := c.loadEmbeddedCandidates(opts.Model, opts.IncludeArchived, "")
	if err != nil {
		return nil, err
	}
	return topKCosine(qv, cands, effLimit(opts.Limit)), nil
}

// SemanticSimilar ranks other traces against the stored vector of traceID
// (no query embedding — it reuses the source trace's own vector). The
// source trace must already be embedded for opts.Model.
func (c *Cortex) SemanticSimilar(traceID string, opts SemanticOpts) ([]ScoredRow, error) {
	qv, err := c.sourceVector(traceID, opts.Model)
	if err != nil {
		return nil, err
	}
	cands, err := c.loadEmbeddedCandidates(opts.Model, opts.IncludeArchived, traceID)
	if err != nil {
		return nil, err
	}
	return topKCosine(qv, cands, effLimit(opts.Limit)), nil
}

// HybridSearch fuses lexical (FTS5 BM25) and semantic (embedding cosine)
// rankings of the query via Reciprocal Rank Fusion, weighted by weight
// (0 = pure lexical, 1 = pure semantic; clamped). Both rankers contribute
// their full ranked lists; the fused top opts.Limit is returned.
func (c *Cortex) HybridSearch(ctx context.Context, e Embedder, query string, opts SemanticOpts, weight float64) ([]ScoredRow, error) {
	qv, err := c.embedQuery(ctx, e, opts.Model, query)
	if err != nil || qv == nil {
		return nil, err
	}
	cands, err := c.loadEmbeddedCandidates(opts.Model, opts.IncludeArchived, "")
	if err != nil {
		return nil, err
	}
	sem := topKCosine(qv, cands, 0)
	lex, err := c.lexicalRanked(query, opts.IncludeArchived)
	if err != nil {
		return nil, err
	}
	return rrfFuse(lex, sem, weight, effLimit(opts.Limit)), nil
}

// HybridSimilar fuses lexical FindSimilar and SemanticSimilar rankings for
// a source trace. Like HybridSearch, weight blends the two (0 = lexical,
// 1 = semantic).
func (c *Cortex) HybridSimilar(traceID string, opts SemanticOpts, weight float64) ([]ScoredRow, error) {
	qv, err := c.sourceVector(traceID, opts.Model)
	if err != nil {
		return nil, err
	}
	cands, err := c.loadEmbeddedCandidates(opts.Model, opts.IncludeArchived, traceID)
	if err != nil {
		return nil, err
	}
	sem := topKCosine(qv, cands, 0)
	lexMatches, err := c.FindSimilar(traceID, SimilarOpts{Limit: hybridLexicalPool, IncludeArchived: opts.IncludeArchived})
	if err != nil {
		return nil, err
	}
	lex := make([]Row, len(lexMatches))
	for i := range lexMatches {
		lex[i] = lexMatches[i].Row
	}
	return rrfFuse(lex, sem, weight, effLimit(opts.Limit)), nil
}

// embedQuery embeds and unit-normalizes a single query string. Returns
// (nil, nil) for a blank query so callers can short-circuit to empty
// results. The embedder/model guards live here so every query path shares
// them.
func (c *Cortex) embedQuery(ctx context.Context, e Embedder, model, query string) ([]float32, error) {
	if e == nil {
		return nil, errors.New("embedder is nil")
	}
	if model == "" {
		return nil, errors.New("embedding model is empty")
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	if len(q) > MaxSearchQueryLen {
		return nil, fmt.Errorf("query too long (%d chars, max %d)", len(q), MaxSearchQueryLen)
	}
	vecs, err := e.Embed(ctx, model, []string{q})
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, errors.New("embedder returned no query vector")
	}
	qv := vecs[0]
	normalizeEmbedding(qv)
	return qv, nil
}

// sourceVector loads and decodes the stored embedding for traceID under
// model (already normalized at store time). Returns a clear error if the
// trace isn't embedded yet.
func (c *Cortex) sourceVector(traceID, model string) ([]float32, error) {
	if model == "" {
		return nil, errors.New("embedding model is empty")
	}
	var blob []byte
	err := c.DB.QueryRow(
		`SELECT embedding FROM trace_embeddings WHERE trace_id = ? AND embedding_model = ?`,
		traceID, model,
	).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("trace %s has no %s embedding yet (run: noema embeddings backfill)", traceID, model)
	}
	if err != nil {
		return nil, fmt.Errorf("loading source vector: %w", err)
	}
	return decodeEmbedding(blob)
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
// unit vectors), descending, and returns the top limit (0 = all).
// Candidates whose dimension differs from the query are skipped.
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

// rrfFuse combines a lexical ranking (Rows in relevance order) and a
// semantic ranking (ScoredRows in cosine order) by Reciprocal Rank Fusion:
// each item gets sum over rankers of w_r / (rrfK + rank), with rank 1-based
// and w split by weight (semantic) vs 1-weight (lexical). Items appearing
// in both rankers accumulate from both. Ties break on ID for deterministic
// output regardless of map/scan order. Returns the fused top limit.
func rrfFuse(lex []Row, sem []ScoredRow, weight float64, limit int) []ScoredRow {
	if weight < 0 {
		weight = 0
	}
	if weight > 1 {
		weight = 1
	}
	pos := make(map[string]int, len(lex)+len(sem))
	var out []ScoredRow
	add := func(r Row, contrib float64) {
		if p, ok := pos[r.ID]; ok {
			out[p].Score += contrib
			return
		}
		pos[r.ID] = len(out)
		out = append(out, ScoredRow{Row: r, Score: contrib})
	}
	for i, r := range lex {
		add(r, (1-weight)/(rrfK+float64(i+1)))
	}
	for i, s := range sem {
		add(s.Row, weight/(rrfK+float64(i+1)))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// lexicalRanked runs the query through FTS5 and returns matching rows in
// BM25 relevance order (best first) — the lexical input to hybrid fusion.
// Trashed excluded; archived excluded unless includeArchived.
func (c *Cortex) lexicalRanked(query string, includeArchived bool) ([]Row, error) {
	if len(query) > MaxSearchQueryLen {
		return nil, fmt.Errorf("query too long (%d chars, max %d)", len(query), MaxSearchQueryLen)
	}
	ftsQuery := SanitizeFTS5Query(query)
	if strings.TrimSpace(ftsQuery) == "" {
		return nil, nil
	}
	q := `SELECT t.id, t.title, t.type, t.tier, t.author, t.origin,
	             t.archived_at, t.trashed_at, t.created_at, t.updated_at,
	             t.content_hash, t.source_locked, t.source_hash
	      FROM traces t
	      JOIN traces_fts ON traces_fts.id = t.id
	      WHERE traces_fts MATCH ? AND t.trashed_at IS NULL`
	if !includeArchived {
		q += ` AND t.archived_at IS NULL`
	}
	q += ` ORDER BY bm25(traces_fts)`

	rows, err := c.DB.Query(q, ftsQuery)
	if err != nil {
		return nil, fmt.Errorf("lexical ranking query: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRowWithNullable(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadEmbeddedCandidates returns every embedded, ranking-eligible trace for
// model: non-trashed (archived excluded unless includeArchived), optionally
// excluding excludeID (the source trace in a similarity query). Vectors
// containing non-finite values (a corrupted BLOB) are skipped so they can't
// poison ranking — the finiteness check applied at the consumer where
// stored vectors are read.
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
		applyNullable(&r, archivedAt, trashedAt, contentHash, sourceHash, sourceLocked)
		vec, err := decodeEmbedding(blob)
		if err != nil || !allFinite(vec) {
			continue // skip corrupted/non-finite vectors
		}
		out = append(out, vectorCand{row: r, vec: vec})
	}
	return out, rows.Err()
}

// scanRowWithNullable scans the standard 13-column trace projection (no
// embedding) into a Row, handling the nullable columns.
func scanRowWithNullable(rows *sql.Rows) (Row, error) {
	var r Row
	var archivedAt, trashedAt, contentHash, sourceHash *string
	var sourceLocked int
	if err := rows.Scan(
		&r.ID, &r.Title, &r.Type, &r.Tier, &r.Author, &r.Origin,
		&archivedAt, &trashedAt, &r.CreatedAt, &r.UpdatedAt,
		&contentHash, &sourceLocked, &sourceHash,
	); err != nil {
		return r, err
	}
	applyNullable(&r, archivedAt, trashedAt, contentHash, sourceHash, sourceLocked)
	return r, nil
}

func applyNullable(r *Row, archivedAt, trashedAt, contentHash, sourceHash *string, sourceLocked int) {
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
