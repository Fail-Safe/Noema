package event_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/db"
	"github.com/Fail-Safe/Noema/internal/event"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestAppendAndForTrace(t *testing.T) {
	conn := openTestDB(t)

	tx, err := conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	e := &event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionCreate,
		TraceID:   "20260405-test-trace",
		Origin:    "alpha",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      json.RawMessage(`{"title":"Test Trace"}`),
	}
	if err := event.Append(tx, e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	events, err := event.ForTrace(conn.DB, "20260405-test-trace")
	if err != nil {
		t.Fatalf("ForTrace: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Action != event.ActionCreate {
		t.Errorf("Action = %q, want %q", events[0].Action, event.ActionCreate)
	}
	if events[0].Origin != "alpha" {
		t.Errorf("Origin = %q, want %q", events[0].Origin, "alpha")
	}
}

func TestSince_Pagination(t *testing.T) {
	conn := openTestDB(t)

	// Insert 5 events.
	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		tx, err := conn.Begin()
		if err != nil {
			t.Fatal(err)
		}
		e := &event.Event{
			ID:        event.NewULID(),
			Action:    event.ActionCreate,
			TraceID:   "20260405-trace",
			Origin:    "alpha",
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}
		ids[i] = e.ID
		if err := event.Append(tx, e); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		tx.Commit()
	}

	// Page 1: first 2 events.
	page1, err := event.Since(conn.DB, "", 2)
	if err != nil {
		t.Fatalf("Since (page 1): %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page 1: got %d events, want 2", len(page1))
	}
	if page1[0].ID != ids[0] {
		t.Errorf("page1[0].ID = %s, want %s", page1[0].ID, ids[0])
	}

	// Page 2: next 2 events after page1's last cursor.
	page2, err := event.Since(conn.DB, page1[1].ID, 2)
	if err != nil {
		t.Fatalf("Since (page 2): %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page 2: got %d events, want 2", len(page2))
	}
	if page2[0].ID != ids[2] {
		t.Errorf("page2[0].ID = %s, want %s", page2[0].ID, ids[2])
	}

	// Page 3: last 1 event.
	page3, err := event.Since(conn.DB, page2[1].ID, 2)
	if err != nil {
		t.Fatalf("Since (page 3): %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("page 3: got %d events, want 1", len(page3))
	}
}

func TestForTrace_Empty(t *testing.T) {
	conn := openTestDB(t)
	events, err := event.ForTrace(conn.DB, "nonexistent")
	if err != nil {
		t.Fatalf("ForTrace: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events, want 0", len(events))
	}
}
