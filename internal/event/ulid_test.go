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

func TestIsValidULID(t *testing.T) {
	valid := []string{
		NewULID(),
		"01ARZ3NDEKTSV4RRFFQ69G5FAV", // canonical example
		"00000000000000000000000000", // all zeros
		"7ZZZZZZZZZZZZZZZZZZZZZZZZZ", // max value
	}
	for _, id := range valid {
		if !IsValidULID(id) {
			t.Errorf("IsValidULID(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"",
		"too-short",
		"01ARZ3NDEKTSV4RRFFQ69G5FA",  // 25 chars
		"01ARZ3NDEKTSV4RRFFQ69G5FAVX", // 27 chars
		"01ARZ3NDEKTSV4RRFFQ69G5FAi",  // lowercase i
		"01ARZ3NDEKTSV4RRFFQ69G5FAO",  // excluded O
		"01ARZ3NDEKTSV4RRFFQ69G5FAL",  // excluded L
		"01ARZ3NDEKTSV4RRFFQ69G5FAU",  // excluded U
		"01ARZ3NDEKTSV4RRFFQ69G5FAI",  // excluded I
		"../../../etc/passwd/000000",   // path traversal attempt
	}
	for _, id := range invalid {
		if IsValidULID(id) {
			t.Errorf("IsValidULID(%q) = true, want false", id)
		}
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
