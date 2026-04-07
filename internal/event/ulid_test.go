package event

import (
	"strings"
	"testing"
)

func TestNewULID_Format(t *testing.T) {
	id := NewULID()
	if len(id) != 26 {
		t.Fatalf("ULID length = %d, want 26", len(id))
	}
	for i, c := range id {
		if !strings.ContainsRune(crockford, c) {
			t.Errorf("char %d (%c) is not valid Crockford base32", i, c)
		}
	}
}

func TestNewULID_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := NewULID()
		if seen[id] {
			t.Fatalf("duplicate ULID on iteration %d: %s", i, id)
		}
		seen[id] = true
	}
}

func TestNewULID_LexicographicOrder(t *testing.T) {
	// Generate two ULIDs in sequence; the second must be >= the first.
	// They share the same millisecond most of the time, so we check >=.
	a := NewULID()
	b := NewULID()
	if b < a {
		t.Errorf("ULIDs not in order: %s > %s", a, b)
	}
}
