package federation_test

import (
	"path/filepath"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/federation"
)

// newStateForTest opens an on-disk SQLite database inside a cortex
// so PeerHealth round-trip tests exercise the real federation_state
// table, not an in-memory stub. Returns the configured state.
func newStateForTest(t *testing.T) *federation.State {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("fed", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("fed", filepath.Join(dir, "fed"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return federation.NewState(cx.DB.DB)
}

func TestPeerHealth_RoundTrip(t *testing.T) {
	s := newStateForTest(t)

	want := federation.PeerHealth{
		Version:             "v0.4.1-19-g1cc3be6",
		VersionObservedAt:   "2026-04-20T12:00:00Z",
		LastSuccess:         "2026-04-20T11:59:00Z",
		ConsecutiveFailures: 3,
		LastError: &federation.PeerError{
			Reason:     federation.ReasonInvalidTraceID,
			EventID:    "01KPGER...",
			TraceID:    "20260418-abc",
			ObservedAt: "2026-04-20T12:00:30Z",
		},
	}

	if err := s.SetPeerHealth("peer-b", want); err != nil {
		t.Fatalf("SetPeerHealth: %v", err)
	}
	got, err := s.GetPeerHealth("peer-b")
	if err != nil {
		t.Fatalf("GetPeerHealth: %v", err)
	}
	if got.Version != want.Version {
		t.Errorf("Version: got %q, want %q", got.Version, want.Version)
	}
	if got.ConsecutiveFailures != want.ConsecutiveFailures {
		t.Errorf("ConsecutiveFailures: got %d, want %d", got.ConsecutiveFailures, want.ConsecutiveFailures)
	}
	if got.LastError == nil {
		t.Fatal("LastError is nil after round-trip")
	}
	if got.LastError.Reason != want.LastError.Reason {
		t.Errorf("Reason: got %q, want %q", got.LastError.Reason, want.LastError.Reason)
	}
	if got.LastError.EventID != want.LastError.EventID {
		t.Errorf("EventID: got %q, want %q", got.LastError.EventID, want.LastError.EventID)
	}
}

func TestPeerHealth_MissingReturnsEmpty(t *testing.T) {
	s := newStateForTest(t)
	got, err := s.GetPeerHealth("never-contacted")
	if err != nil {
		t.Fatalf("GetPeerHealth: %v", err)
	}
	if got.Version != "" || got.LastError != nil || got.ConsecutiveFailures != 0 {
		t.Errorf("expected zero-value PeerHealth for missing peer, got %+v", got)
	}
}

func TestGetPeerState_IncludesHealth(t *testing.T) {
	s := newStateForTest(t)
	err := s.SetPeerHealth("peer-c", federation.PeerHealth{Version: "v0.5.0"})
	if err != nil {
		t.Fatalf("SetPeerHealth: %v", err)
	}
	ps, err := s.GetPeerState("peer-c", "https://peer-c:3000")
	if err != nil {
		t.Fatalf("GetPeerState: %v", err)
	}
	if ps.Health.Version != "v0.5.0" {
		t.Errorf("expected health to be loaded into PeerState, got %+v", ps.Health)
	}
}
