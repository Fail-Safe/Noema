package federation

import (
	"errors"
	"testing"

	"github.com/Fail-Safe/Noema/internal/db"
	"github.com/Fail-Safe/Noema/internal/event"
)

// fakeReplayer is an EventReplayer that records every replayed event and
// optionally fails on a specific event ID. Used to drive replayBatch
// without standing up a real cortex.
type fakeReplayer struct {
	failOn       string // ID of the event that should fail; "" means never fail
	replayed     []string
	merged       []VClock
	usageBatches [][]TraceUsage
}

func (f *fakeReplayer) ReplayEvent(e event.Event) error {
	if e.ID == f.failOn {
		return errors.New("synthetic replay failure")
	}
	f.replayed = append(f.replayed, e.ID)
	return nil
}

func (f *fakeReplayer) MergeClock(vc VClock) error {
	f.merged = append(f.merged, vc)
	return nil
}

func (f *fakeReplayer) MergeRemoteUsage(rows []TraceUsage) error {
	f.usageBatches = append(f.usageBatches, rows)
	return nil
}

// newSyncerForTest spins up a minimal Syncer wired to a real federation
// State backed by a fresh on-disk DB. The DB is needed because State
// reads/writes the federation_state table; the fake replayer absorbs the
// cortex-side calls.
func newSyncerForTest(t *testing.T, fr *fakeReplayer) *Syncer {
	t.Helper()
	dir := t.TempDir()
	conn, err := db.Open(dir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	state := NewState(conn.DB)
	return NewSyncer(fr, state, Config{})
}

// TestReplayBatch_PinsCursorOnFailure is the regression test for the
// federation cursor leapfrog bug. The pre-fix behavior was:
//
//	for _, e := range batch {
//	    if err := replay(e); err != nil { log; continue }
//	    advanceCursor(e.ID)
//	}
//
// which silently advanced the cursor past a failed event whenever a
// later event in the same batch succeeded. That dropped real failures
// on the floor and broke causal ordering. The fix in replayBatch is to
// stop processing the batch on the first failure and leave the cursor
// pinned at the previous event so the next poll re-fetches from there.
func TestReplayBatch_PinsCursorOnFailure(t *testing.T) {
	const peerName = "peer-under-test"

	events := []event.Event{
		{ID: "01EVENT0000000000000000001", Action: event.ActionCreate, TraceID: "20260407-a"},
		{ID: "01EVENT0000000000000000002", Action: event.ActionCreate, TraceID: "20260407-b"},
		{ID: "01EVENT0000000000000000003", Action: event.ActionCreate, TraceID: "20260407-c"}, // <-- fails
		{ID: "01EVENT0000000000000000004", Action: event.ActionCreate, TraceID: "20260407-d"},
		{ID: "01EVENT0000000000000000005", Action: event.ActionCreate, TraceID: "20260407-e"},
	}
	fr := &fakeReplayer{failOn: "01EVENT0000000000000000003"}
	s := newSyncerForTest(t, fr)

	err := s.replayBatch(peerName, events)
	var pe *PollError
	if !errors.As(err, &pe) {
		t.Fatalf("replayBatch should return *PollError on replay failure; got %T: %v", err, err)
	}
	if pe.EventID != "01EVENT0000000000000000003" {
		t.Errorf("PollError.EventID = %q, want the failing event's ID", pe.EventID)
	}
	if pe.TraceID != "20260407-c" {
		t.Errorf("PollError.TraceID = %q, want the failing event's trace", pe.TraceID)
	}

	// Only the events strictly before the failure should have been
	// replayed — events after the failure must NOT be touched.
	if len(fr.replayed) != 2 {
		t.Errorf("replayed = %v, want exactly the two events before the failure", fr.replayed)
	}
	for _, id := range fr.replayed {
		if id == "01EVENT0000000000000000004" || id == "01EVENT0000000000000000005" {
			t.Errorf("event %s after the failure should not have been replayed", id)
		}
	}

	// Cursor must point at the last successful event, not past the
	// failure. The next sync poll will re-fetch starting from the
	// failed event.
	cursor, err := s.state.Get(PeerCursorKey(peerName))
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursor != "01EVENT0000000000000000002" {
		t.Errorf("cursor = %q, want %q (last event before the failure)", cursor, "01EVENT0000000000000000002")
	}

	// last_seen should still get bumped: we did successfully reach the
	// peer, the failure is on our local replay side. Without this, a
	// stuck event would also stop the "peer reachable" indicator from
	// updating, hiding the fact that the link itself is healthy.
	seen, err := s.state.Get(PeerSeenKey(peerName))
	if err != nil {
		t.Fatalf("read last_seen: %v", err)
	}
	if seen == "" {
		t.Error("last_seen should be updated even when a replay fails")
	}
}

// TestReplayBatch_AdvancesOnAllSuccess pins the happy path: when every
// event in the batch replays cleanly, the cursor moves to the last
// event's ID and every event is replayed in order.
func TestReplayBatch_AdvancesOnAllSuccess(t *testing.T) {
	const peerName = "happy-peer"
	events := []event.Event{
		{ID: "01OK00000000000000000000A1", Action: event.ActionCreate, TraceID: "t1"},
		{ID: "01OK00000000000000000000A2", Action: event.ActionCreate, TraceID: "t2"},
		{ID: "01OK00000000000000000000A3", Action: event.ActionCreate, TraceID: "t3"},
	}
	fr := &fakeReplayer{}
	s := newSyncerForTest(t, fr)

	if err := s.replayBatch(peerName, events); err != nil {
		t.Fatalf("replayBatch: %v", err)
	}
	if len(fr.replayed) != 3 {
		t.Errorf("replayed %d events, want 3", len(fr.replayed))
	}

	cursor, _ := s.state.Get(PeerCursorKey(peerName))
	if cursor != "01OK00000000000000000000A3" {
		t.Errorf("cursor = %q, want last event id", cursor)
	}
}

// TestSyncer_Start_SkipsPausedPeers verifies that the syncer does not
// spawn a goroutine for peers whose mode is "paused". The test creates
// a syncer with three peers (one paused, two active with unreachable
// endpoints) and checks that the paused peer never gets a state entry.
func TestSyncer_Start_SkipsPausedPeers(t *testing.T) {
	fr := &fakeReplayer{}
	dir := t.TempDir()
	conn, err := db.Open(dir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	state := NewState(conn.DB)
	s := NewSyncer(fr, state, Config{
		Peers: []PeerConfig{
			{Name: "active-1", Endpoint: "http://127.0.0.1:1", Mode: PeerModeSync},
			{Name: "paused-1", Endpoint: "http://127.0.0.1:2", Mode: PeerModePaused},
			{Name: "active-2", Endpoint: "http://127.0.0.1:3"}, // empty = defaults to sync
		},
	})

	s.Start()
	s.Stop()

	// The paused peer should have no state at all — the syncer never
	// even attempted to reach it.
	for _, key := range []string{PeerCursorKey("paused-1"), PeerSeenKey("paused-1")} {
		val, _ := state.Get(key)
		if val != "" {
			t.Errorf("paused peer state %q = %q, want empty", key, val)
		}
	}
}

// TestReplayBatch_FirstEventFailure_LeavesCursorUnset covers the edge
// case where the very first event fails: there is no previous successful
// event to pin the cursor against, so the cursor must remain whatever it
// was before the call (empty in this test). The failed event will be
// re-fetched on the next poll.
func TestReplayBatch_FirstEventFailure_LeavesCursorUnset(t *testing.T) {
	const peerName = "first-fail-peer"
	events := []event.Event{
		{ID: "01FIRSTFAIL0000000000000001", Action: event.ActionCreate, TraceID: "t1"},
		{ID: "01FIRSTFAIL0000000000000002", Action: event.ActionCreate, TraceID: "t2"},
	}
	fr := &fakeReplayer{failOn: "01FIRSTFAIL0000000000000001"}
	s := newSyncerForTest(t, fr)

	err := s.replayBatch(peerName, events)
	var pe *PollError
	if !errors.As(err, &pe) {
		t.Fatalf("replayBatch should return *PollError on replay failure; got %T: %v", err, err)
	}
	if len(fr.replayed) != 0 {
		t.Errorf("replayed = %v, want empty (first event failed)", fr.replayed)
	}
	cursor, _ := s.state.Get(PeerCursorKey(peerName))
	if cursor != "" {
		t.Errorf("cursor = %q, want empty (no successful events before failure)", cursor)
	}
}
