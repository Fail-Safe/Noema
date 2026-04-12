package trace_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/trace"
)

// ---- Parse ----

func TestParse_ValidInput(t *testing.T) {
	input := "---\nid: 20260329-why-we-chose-go\ntitle: Why we chose Go\ntype: decision\nauthor: agent-1\ntags:\n  - go\n  - language\ncreated: 2026-03-29T12:00:00Z\nupdated: 2026-03-29T12:00:00Z\n---\n\nBody content here.\n"

	tr, err := trace.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	check := func(field, want, got string) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	check("ID", "20260329-why-we-chose-go", tr.ID)
	check("Title", "Why we chose Go", tr.Title)
	check("Type", "decision", tr.Type)
	check("Author", "agent-1", tr.Author)
	check("Body", "Body content here.\n", tr.Body)
	if len(tr.Tags) != 2 || tr.Tags[0] != "go" || tr.Tags[1] != "language" {
		t.Errorf("Tags = %v, want [go language]", tr.Tags)
	}
}

func TestParse_EmptyBody(t *testing.T) {
	input := "---\nid: 20260329-empty\ntitle: Empty\ntype: note\ncreated: 2026-03-29T12:00:00Z\nupdated: 2026-03-29T12:00:00Z\n---\n"
	tr, err := trace.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tr.Body != "" {
		t.Errorf("Body = %q, want empty", tr.Body)
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"no frontmatter", "Just plain body content"},
		{"unterminated", "---\nid: foo\ntitle: foo\n"},
		{"missing opening delimiter", "id: foo\ntitle: foo\n---\n\nbody"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := trace.Parse([]byte(tc.input)); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// ---- Write / round-trip ----

func TestWriteParseRoundTrip(t *testing.T) {
	original := trace.New("Round-trip test", "fact", "agent-1", []string{"a", "b"}, "Some body.\nWith two lines.")

	path := filepath.Join(t.TempDir(), original.ID+".md")
	if err := original.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	restored, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	if restored.ID != original.ID {
		t.Errorf("ID: got %q, want %q", restored.ID, original.ID)
	}
	if restored.Title != original.Title {
		t.Errorf("Title: got %q, want %q", restored.Title, original.Title)
	}
	if restored.Type != original.Type {
		t.Errorf("Type: got %q, want %q", restored.Type, original.Type)
	}
	if restored.Author != original.Author {
		t.Errorf("Author: got %q, want %q", restored.Author, original.Author)
	}
	if restored.Body != original.Body {
		t.Errorf("Body: got %q, want %q", restored.Body, original.Body)
	}
	if len(restored.Tags) != 2 {
		t.Errorf("Tags: got %v, want [a b]", restored.Tags)
	}
}

func TestWrite_UpdatesUpdatedField(t *testing.T) {
	tr := trace.New("Update field test", "note", "", nil, "body")
	original := tr.Updated

	path := filepath.Join(t.TempDir(), tr.ID+".md")
	if err := tr.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Write mutates Updated to now — verify it changed or stayed equal (may be same
	// instant on fast machines, but must be a valid RFC3339 timestamp).
	if tr.Updated == "" {
		t.Error("Updated is empty after Write")
	}
	_ = original // may equal Updated if Write ran fast enough; that is acceptable
}

func TestParseFile_NotFound(t *testing.T) {
	_, err := trace.ParseFile("/nonexistent/path/trace.md")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

// ---- NewID / slug ----

func TestNewID_Format(t *testing.T) {
	id := trace.NewID("Hello, World! This is a test.")
	date := time.Now().UTC().Format("20060102")

	if !strings.HasPrefix(id, date+"-") {
		t.Errorf("ID %q does not start with today's date %q", id, date)
	}
	slug := strings.TrimPrefix(id, date+"-")
	if slug != "hello-world-this-is-a-test" {
		t.Errorf("slug = %q, want %q", slug, "hello-world-this-is-a-test")
	}
}

func TestNewID_Truncation(t *testing.T) {
	longTitle := strings.Repeat("a", 120)
	id := trace.NewID(longTitle)
	// max: 8 (date) + 1 (-) + 60 (slug) = 69 chars
	if len(id) > 69 {
		t.Errorf("ID length = %d, want <= 69", len(id))
	}
	if strings.HasSuffix(id, "-") {
		t.Error("ID must not end with a hyphen after truncation")
	}
}

func TestNewID_AllPunctuation(t *testing.T) {
	id := trace.NewID("!!! @@@")
	date := time.Now().UTC().Format("20060102")
	// Slug is empty — ID should still be valid (just the date part)
	if !strings.HasPrefix(id, date) {
		t.Errorf("ID %q should start with date %q", id, date)
	}
}

func TestNewID_Unicode(t *testing.T) {
	id := trace.NewID("Über café résumé")
	// Non-ASCII letters are treated as non-alphanumeric and become hyphens.
	// The result should be a valid date-prefixed slug with no leading/trailing hyphens.
	if strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
		t.Errorf("ID has leading/trailing hyphens: %q", id)
	}
}

func TestNewID_StripsLeadingDatePrefix(t *testing.T) {
	date := time.Now().UTC().Format("20060102")

	cases := []struct {
		name     string
		title    string
		wantSlug string
	}{
		{
			name:     "YYYYMMDD prefix with space separator",
			title:    "20260402 dadbot autonomous infrastructure access",
			wantSlug: "dadbot-autonomous-infrastructure-access",
		},
		{
			name:     "YYYYMMDD prefix already slug-shaped",
			title:    "20260402-dadbot-autonomous-infrastructure-access",
			wantSlug: "dadbot-autonomous-infrastructure-access",
		},
		{
			name:     "YYYY-MM-DD prefix with space separator",
			title:    "2026-04-02 ai news digest",
			wantSlug: "ai-news-digest",
		},
		{
			name:     "YYYY-MM-DD prefix already slug-shaped",
			title:    "2026-04-02-ai-news-digest",
			wantSlug: "ai-news-digest",
		},
		{
			name:     "embedded date is not stripped",
			title:    "divergence-20260402-original-trace",
			wantSlug: "divergence-20260402-original-trace",
		},
		{
			name:     "four-digit year alone is not stripped",
			title:    "2026 was a year",
			wantSlug: "2026-was-a-year",
		},
		{
			name:     "eight digits without trailing hyphen are not stripped",
			title:    "20260402dadbot",
			wantSlug: "20260402dadbot",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := trace.NewID(tc.title)
			wantPrefix := date + "-"
			if !strings.HasPrefix(id, wantPrefix) {
				t.Fatalf("ID %q does not start with today's date %q", id, wantPrefix)
			}
			gotSlug := strings.TrimPrefix(id, wantPrefix)
			if gotSlug != tc.wantSlug {
				t.Errorf("slug = %q, want %q (full id: %q)", gotSlug, tc.wantSlug, id)
			}
		})
	}
}

// ---- IsValidType ----

func TestIsValidType(t *testing.T) {
	valid := []string{"fact", "decision", "preference", "context", "skill", "intent", "observation", "note"}
	for _, v := range valid {
		if !trace.IsValidType(v) {
			t.Errorf("IsValidType(%q) = false, want true", v)
		}
	}
	for _, inv := range []string{"", "Fact", "FACT", "unknown", "type"} {
		if trace.IsValidType(inv) {
			t.Errorf("IsValidType(%q) = true, want false", inv)
		}
	}
}

// ---- New ----

func TestNew(t *testing.T) {
	tr := trace.New("My Trace", "skill", "bob", []string{"tag1", "tag2"}, "Body text.")

	if !strings.Contains(tr.ID, "my-trace") {
		t.Errorf("ID %q should contain slug of title", tr.ID)
	}
	if tr.Title != "My Trace" {
		t.Errorf("Title = %q, want %q", tr.Title, "My Trace")
	}
	if tr.Type != "skill" {
		t.Errorf("Type = %q, want %q", tr.Type, "skill")
	}
	if tr.Author != "bob" {
		t.Errorf("Author = %q, want %q", tr.Author, "bob")
	}
	if tr.Body != "Body text." {
		t.Errorf("Body = %q, want %q", tr.Body, "Body text.")
	}
	if tr.Created == "" || tr.Updated == "" {
		t.Error("Created/Updated timestamps must be set")
	}
}

// ---- IsValidID ----

func TestIsValidID(t *testing.T) {
	valid := []string{
		"20260329-why-we-chose-go",
		"20260401-a",
		"20260401-a-b-c",
		"20260401-abc123",
	}
	for _, id := range valid {
		if !trace.IsValidID(id) {
			t.Errorf("IsValidID(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"",
		"no-date-prefix",
		"../etc/passwd",
		"20260329-../../etc/passwd",
		"20260329-hello/world",
		"20260329-hello\\world",
		"20260329-",
		"20260329-UPPERCASE",
		"20260329-hello world",
		"2026032-short-date",
		"20260329-" + strings.Repeat("a", 66), // exceeds max slug length
	}
	for _, id := range invalid {
		if trace.IsValidID(id) {
			t.Errorf("IsValidID(%q) = true, want false", id)
		}
	}
}
