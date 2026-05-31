package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// ErrInvalidTraceID is returned when a trace ID contains characters that
// could escape the cortex directory (path traversal). Only IDs matching
// the YYYYMMDD-slug format produced by NewID are accepted.
var ErrInvalidTraceID = errors.New("invalid trace ID")

// validTraceID matches the format produced by NewID: 8 digits, a hyphen,
// then 1–100 characters of lowercase alphanumerics and hyphens. Pre-existing
// traces produced under the old 65-char cap remain valid — the regex is
// strictly a superset of the old pattern.
var validTraceID = regexp.MustCompile(`^[0-9]{8}-[a-z0-9][a-z0-9-]{0,99}$`)

// MaxSlugLen is the maximum length of the slug portion of a trace ID
// (the part after YYYYMMDD-). NewID truncates longer slugs. AGENTS.md
// documents this so agents can aim for short titles rather than
// discovering silent truncation after the fact.
const MaxSlugLen = 100

// IsValidID reports whether id is a well-formed trace ID safe for use in
// filesystem paths. It rejects any ID containing path separators, dot
// segments, or characters outside the NewID slug alphabet.
func IsValidID(id string) bool {
	return validTraceID.MatchString(id)
}

type Type string

const (
	TypeFact        Type = "fact"
	TypeDecision    Type = "decision"
	TypePreference  Type = "preference"
	TypeContext     Type = "context"
	TypeSkill       Type = "skill"
	TypeIntent      Type = "intent"
	TypeObservation Type = "observation"
	TypeNote        Type = "note"
	TypeDivergence  Type = "divergence"
)

var ValidTypes = []Type{
	TypeFact, TypeDecision, TypePreference, TypeContext,
	TypeSkill, TypeIntent, TypeObservation, TypeNote,
	TypeDivergence,
}

func IsValidType(t string) bool {
	for _, v := range ValidTypes {
		if string(v) == t {
			return true
		}
	}
	return false
}

// Tier names the three memory tiers in the consolidation hierarchy. See
// docs/plans/consolidation-plan.md (in the Noema-design repo) for the full
// design. New traces default to TierShort; promotion is short -> mid -> long.
// Long is terminal from routine operation (DB trigger enforces immutability).
const (
	TierShort = "short"
	TierMid   = "mid"
	TierLong  = "long"
)

var validTiers = map[string]struct{}{
	TierShort: {},
	TierMid:   {},
	TierLong:  {},
}

func IsValidTier(s string) bool {
	_, ok := validTiers[s]
	return ok
}

// ErrInvalidFrontmatter is returned by Validate when a Trace's frontmatter
// fails a required-field or enum check. The specific failure is wrapped
// underneath so callers can log it or branch on it.
var ErrInvalidFrontmatter = errors.New("invalid trace frontmatter")

// Validate checks that a parsed Trace satisfies the required-field and
// enum constraints the DB can't enforce on its own. The traces table
// declares id/title/type/created/updated as NOT NULL, but SQLite treats
// empty strings as satisfying NOT NULL, so YAML-missing fields would
// slip through as junk rows. Validate closes that gap for ingest paths
// that parse files written by external tools (the filesystem watcher,
// primarily — Cortex.Add and CLI flows build Traces via trace.New,
// which always fills these fields with sensible values).
//
// All errors wrap ErrInvalidFrontmatter so callers can use errors.Is
// to detect validation failures as a category.
func Validate(t *Trace) error {
	if t == nil {
		return fmt.Errorf("%w: nil trace", ErrInvalidFrontmatter)
	}
	if !IsValidID(t.ID) {
		return fmt.Errorf("%w: id %q does not match YYYYMMDD-slug format", ErrInvalidFrontmatter, t.ID)
	}
	if t.Title == "" {
		return fmt.Errorf("%w: title is required", ErrInvalidFrontmatter)
	}
	if t.Type == "" {
		return fmt.Errorf("%w: type is required", ErrInvalidFrontmatter)
	}
	if !IsValidType(t.Type) {
		return fmt.Errorf("%w: type %q is not one of the recognized types (fact, decision, preference, context, skill, intent, observation, note, divergence)", ErrInvalidFrontmatter, t.Type)
	}
	if t.Created == "" {
		return fmt.Errorf("%w: created timestamp is required", ErrInvalidFrontmatter)
	}
	if t.Updated == "" {
		return fmt.Errorf("%w: updated timestamp is required", ErrInvalidFrontmatter)
	}
	return nil
}

type Frontmatter struct {
	ID           string   `yaml:"id"`
	Title        string   `yaml:"title"`
	Type         string   `yaml:"type"`
	Tier         string   `yaml:"tier,omitempty"` // short|mid|long; empty == short, omitted for cleaner files
	Author       string   `yaml:"author,omitempty"`
	Tags         []string `yaml:"tags,omitempty"`
	DerivedFrom  []string `yaml:"derived_from,omitempty"`
	Origin       string   `yaml:"origin,omitempty"`
	Created      string   `yaml:"created"`
	Updated      string   `yaml:"updated"`
	ContentHash  string   `yaml:"content_hash,omitempty"`
	SourceHash   string   `yaml:"source_hash,omitempty"`
	SourceLocked bool     `yaml:"source_locked,omitempty"`
}

type Trace struct {
	Frontmatter
	Body string
}

func New(title, traceType, author string, tags []string, body string) *Trace {
	now := time.Now().UTC().Format(time.RFC3339)
	id := NewID(title)
	return &Trace{
		Frontmatter: Frontmatter{
			ID:      id,
			Title:   title,
			Type:    traceType,
			Author:  author,
			Tags:    tags,
			Created: now,
			Updated: now,
		},
		Body: body,
	}
}

func Parse(data []byte) (*Trace, error) {
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		return nil, fmt.Errorf("missing frontmatter delimiter")
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, fmt.Errorf("unterminated frontmatter")
	}
	fmStr := rest[:end]
	body := strings.TrimPrefix(rest[end+5:], "\n")

	var fm Frontmatter
	if err := yaml.Unmarshal([]byte(fmStr), &fm); err != nil {
		return nil, fmt.Errorf("parsing frontmatter: %w", err)
	}
	return &Trace{Frontmatter: fm, Body: body}, nil
}

func ParseFile(path string) (*Trace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func (t *Trace) Write(path string) error {
	t.Frontmatter.Updated = time.Now().UTC().Format(time.RFC3339)
	fmData, err := yaml.Marshal(t.Frontmatter)
	if err != nil {
		return fmt.Errorf("encoding frontmatter: %w", err)
	}
	content := fmt.Sprintf("---\n%s---\n\n%s", string(fmData), t.Body)
	return os.WriteFile(path, []byte(content), 0o640)
}

// NewID generates a trace ID from a title: YYYYMMDD-slugified-title.
//
// If the slugified title already begins with a date prefix (either
// `YYYYMMDD-` or `YYYY-MM-DD-`), the leading date is stripped before
// today's date is prepended. This is a defensive measure: agents
// commonly include dates in titles to mark when an event occurred
// (e.g., "20260402 agent-1 heartbeat check"), and without the strip
// the resulting ID would carry two date prefixes
// (`20260402-20260402-agent-1-heartbeat-check`). The strip does not
// validate that the digits form a real calendar date — its only job
// is to prevent visual prefix duplication in the final ID.
func NewID(title string) string {
	date := time.Now().UTC().Format("20060102")
	s := stripLeadingDatePrefix(slug(title))
	if len(s) > MaxSlugLen {
		s = strings.TrimRight(s[:MaxSlugLen], "-")
	}
	return date + "-" + s
}

// stripLeadingDatePrefix removes a leading date prefix from s if one is
// present. It recognises two forms:
//
//	YYYYMMDD-     (8 digits + hyphen)         e.g. "20260402-foo" -> "foo"
//	YYYY-MM-DD-   (4-2-2 digits with hyphens) e.g. "2026-04-02-foo" -> "foo"
//
// Anything else — including titles where digits appear later in the
// string ("divergence-20260402-foo", "2026-was-a-year") — is returned
// unchanged.
func stripLeadingDatePrefix(s string) string {
	// YYYYMMDD- form (9 chars).
	if len(s) >= 9 && s[8] == '-' && allDigits(s[:8]) {
		return s[9:]
	}
	// YYYY-MM-DD- form (11 chars).
	if len(s) >= 11 && s[4] == '-' && s[7] == '-' && s[10] == '-' &&
		allDigits(s[:4]) && allDigits(s[5:7]) && allDigits(s[8:10]) {
		return s[11:]
	}
	return s
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ContentHash returns the SHA-256 hash of a trace body, prefixed with
// "sha256:" for algorithm-explicitness. Used for integrity verification
// and federation sync optimization.
func ContentHash(body string) string {
	h := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(h[:])
}

func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevHyphen := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevHyphen = false
		} else if !prevHyphen {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}
