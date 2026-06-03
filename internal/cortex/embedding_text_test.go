package cortex

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEmbeddingText(t *testing.T) {
	if got := embeddingText("Title", "Body", 0); got != "Title\n\nBody" {
		t.Errorf("title+body = %q", got)
	}
	if got := embeddingText("  Title  ", "  Body  ", 0); got != "Title\n\nBody" {
		t.Errorf("trim = %q", got)
	}
	if got := embeddingText("Only title", "   ", 0); got != "Only title" {
		t.Errorf("empty body => title only, got %q", got)
	}
	if got := embeddingText("", "Only body", 0); got != "Only body" {
		t.Errorf("empty title => body only, got %q", got)
	}
	if got := embeddingText("  ", "  ", 0); got != "" {
		t.Errorf("both empty => empty, got %q", got)
	}

	// Truncation is rune-bounded and never splits a multibyte character.
	multi := strings.Repeat("héllo ", 100) // 'é' is 2 bytes
	got := embeddingText(multi, "", 10)
	if utf8.RuneCountInString(got) != 10 {
		t.Errorf("truncated rune count = %d, want 10", utf8.RuneCountInString(got))
	}
	if !utf8.ValidString(got) {
		t.Error("truncation produced invalid UTF-8")
	}
}
