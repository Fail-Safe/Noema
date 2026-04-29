package cortex

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/Fail-Safe/Noema/internal/trace"
)

// SimilarMatch is one ranked result from FindSimilar. Score is the raw
// FTS5 BM25 value (lower is closer); callers should treat it as opaque
// ordering, not an absolute confidence number.
type SimilarMatch struct {
	Row
	Score float64
}

// SimilarOpts tunes FindSimilar. All fields zero-default to sane values,
// so passing a zero-value SimilarOpts is fine.
type SimilarOpts struct {
	Limit           int  // max matches returned (default 10)
	IncludeArchived bool // include archived traces in candidate set (default false)
	TopTermsK       int  // distinctive terms to extract from the source (default 25)
}

// FindSimilar returns traces whose token overlap with the source trace
// scores best under FTS5 BM25. The source trace itself is excluded.
// Trashed traces are always excluded; archived traces are excluded by
// default.
//
// The ranking is "document-as-query": tokenize the source's title +
// body + tag text, drop English stopwords and short tokens, take the
// top-K most frequent remaining terms, and submit them as an OR-joined
// FTS5 query. SQLite's BM25 then scores every candidate against that
// query in one shot. This is intentionally simple — no embedding model,
// no external service, no schema change. Quality is bounded by FTS5's
// porter+ASCII tokenizer and the embedded stopword list.
func (c *Cortex) FindSimilar(traceID string, opts SimilarOpts) ([]SimilarMatch, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	topK := opts.TopTermsK
	if topK <= 0 {
		topK = 25
	}

	src, err := c.Get(traceID)
	if err != nil {
		return nil, fmt.Errorf("loading source trace: %w", err)
	}

	path := filepath.Clean(c.TraceFile(traceID, src.ArchivedAt != ""))
	tr, err := trace.ParseFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading source trace body: %w", err)
	}

	terms := extractDistinctiveTerms(tr.Title+" "+tr.Body+" "+strings.Join(tr.Tags, " "), topK)
	if len(terms) == 0 {
		return nil, nil
	}

	query := strings.Join(terms, " OR ")
	if len(query) > MaxSearchQueryLen {
		// Trim from the tail (rarest terms first by frequency-rank
		// position) until we fit. Truncating mid-token is also fine
		// since the OR list is space-separated.
		query = query[:MaxSearchQueryLen]
		query = strings.TrimRight(query, " OR")
	}
	ftsQuery := SanitizeFTS5Query(query)

	q := `
		SELECT t.id, t.title, t.type, t.tier, t.author, t.origin,
		       t.archived_at, t.trashed_at, t.created_at, t.updated_at,
		       t.content_hash, t.source_locked, t.source_hash,
		       bm25(traces_fts) AS score
		FROM traces t
		JOIN traces_fts ON traces_fts.id = t.id
		WHERE traces_fts MATCH ?
		  AND t.id != ?
		  AND t.trashed_at IS NULL`
	args := []any{ftsQuery, traceID}

	if !opts.IncludeArchived {
		q += ` AND t.archived_at IS NULL`
	}
	q += ` ORDER BY score LIMIT ?`
	args = append(args, limit)

	rows, err := c.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("running similarity query: %w", err)
	}
	defer rows.Close()

	var out []SimilarMatch
	for rows.Next() {
		var m SimilarMatch
		var archivedAt, trashedAt, contentHash, sourceHash *string
		var sourceLocked int
		if err := rows.Scan(
			&m.ID, &m.Title, &m.Type, &m.Tier, &m.Author, &m.Origin,
			&archivedAt, &trashedAt, &m.CreatedAt, &m.UpdatedAt,
			&contentHash, &sourceLocked, &sourceHash, &m.Score,
		); err != nil {
			return nil, err
		}
		if archivedAt != nil {
			m.ArchivedAt = *archivedAt
		}
		if trashedAt != nil {
			m.TrashedAt = *trashedAt
		}
		if contentHash != nil {
			m.ContentHash = *contentHash
		}
		if sourceHash != nil {
			m.SourceHash = *sourceHash
		}
		m.SourceLocked = sourceLocked != 0
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// extractDistinctiveTerms tokenizes text, drops stopwords and short
// tokens, frequency-counts the rest, and returns the top-K by count.
// Ties broken by alphabetical order for determinism.
func extractDistinctiveTerms(text string, k int) []string {
	if k <= 0 {
		return nil
	}
	freq := make(map[string]int)
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		tok := strings.ToLower(current.String())
		current.Reset()
		if len(tok) < 3 {
			return
		}
		if _, stop := englishStopwords[tok]; stop {
			return
		}
		// Skip pure-numeric tokens — they rarely carry topical
		// signal and inflate term frequency for date-heavy traces.
		if isAllDigits(tok) {
			return
		}
		freq[tok]++
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()

	if len(freq) == 0 {
		return nil
	}
	type entry struct {
		term  string
		count int
	}
	entries := make([]entry, 0, len(freq))
	for t, n := range freq {
		entries = append(entries, entry{t, n})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].term < entries[j].term
	})
	if len(entries) > k {
		entries = entries[:k]
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.term
	}
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// englishStopwords is a minimal English stopword list. Kept short on
// purpose — long curated lists overfit to news text and silently drop
// terms that carry signal in technical writing (e.g. "system", "data").
// Members were picked from the intersection of common stopword lists
// (NLTK, sklearn, MySQL FTS) restricted to function words.
var englishStopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "are": {}, "but": {}, "not": {},
	"you": {}, "all": {}, "can": {}, "had": {}, "her": {}, "was": {},
	"one": {}, "our": {}, "out": {}, "day": {}, "get": {}, "has": {},
	"him": {}, "his": {}, "how": {}, "man": {}, "new": {}, "now": {},
	"old": {}, "see": {}, "two": {}, "way": {}, "who": {}, "boy": {},
	"did": {}, "its": {}, "let": {}, "put": {}, "say": {}, "she": {},
	"too": {}, "use": {}, "any": {}, "off": {}, "set": {}, "yet": {},
	"that": {}, "with": {}, "this": {}, "from": {}, "they": {}, "have": {},
	"will": {}, "your": {}, "what": {}, "when": {}, "make": {}, "like": {},
	"into": {}, "time": {}, "just": {}, "know": {}, "take": {}, "than": {},
	"them": {}, "well": {}, "were": {}, "been": {}, "more": {}, "some": {},
	"only": {}, "over": {}, "such": {}, "very": {}, "also": {}, "back": {},
	"each": {}, "even": {}, "find": {}, "give": {}, "good": {}, "most": {},
	"much": {}, "must": {}, "name": {}, "need": {}, "next": {}, "open": {},
	"part": {}, "same": {}, "seem": {}, "show": {}, "tell": {}, "then": {},
	"there": {}, "their": {}, "would": {}, "could": {}, "should": {},
	"about": {}, "after": {}, "again": {}, "before": {}, "being": {},
	"these": {}, "those": {}, "where": {}, "which": {}, "while": {},
	"because": {}, "between": {}, "through": {}, "during": {},
}
