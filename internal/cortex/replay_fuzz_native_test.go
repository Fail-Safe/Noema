package cortex_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// FuzzReplayTierChange explores the data payload of Promote/Demote
// events. The table-driven TestReplay_AdversarialPayloads covers a
// seed set of hostile inputs; this fuzz target extends that coverage
// with corpus-guided exploration. The invariant under test is "no
// panic, DB stays queryable" — a federated peer shouldn't be able to
// DoS the ring by shipping a crafted payload that crashes the
// replay path.
//
// Run with: go test -run '^$' -fuzz=FuzzReplayTierChange -fuzztime=30s ./internal/cortex/
func FuzzReplayTierChange(f *testing.F) {
	cx, tr := fuzzSetup(f, "fuzz-tier")

	f.Add([]byte(`{"from":"short","to":"mid"}`))
	f.Add([]byte(`{"from":"mid","to":"long"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"to":""}`))
	f.Add([]byte(`{"to":null}`))
	f.Add([]byte(`{"to":"SHORT"}`))
	f.Add([]byte(`{"to":"' OR 1=1--"}`))
	f.Add([]byte(`{"to":" "}`))
	f.Add([]byte(`{"to":"mid","extra":{"nested":true}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on payload %q: %v", string(data), r)
			}
		}()
		e := event.Event{
			ID:        event.NewULID(),
			Action:    event.ActionPromote,
			TraceID:   tr.ID,
			CortexID:  "01FUZZ",
			Origin:    "fuzz-peer",
			Timestamp: "2026-04-21T14:00:00Z",
			Data:      json.RawMessage(data),
		}
		_ = cx.ReplayEvent(e)
		if _, err := cx.Get(tr.ID); err != nil {
			t.Fatalf("cx.Get failed after payload %q: %v", string(data), err)
		}
	})
}

// FuzzReplayVote exercises the tier_votes counter path. Adversarial
// deltas (huge positive, huge negative, non-numeric) must not crash
// the UPDATE or leave the counter in an inconsistent state.
func FuzzReplayVote(f *testing.F) {
	cx, tr := fuzzSetup(f, "fuzz-vote")

	f.Add([]byte(`{"delta":1}`))
	f.Add([]byte(`{"delta":-1}`))
	f.Add([]byte(`{"delta":9999999999}`))
	f.Add([]byte(`{"delta":-9999999999}`))
	f.Add([]byte(`{"delta":"string"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on vote payload %q: %v", string(data), r)
			}
		}()
		e := event.Event{
			ID:        event.NewULID(),
			Action:    event.ActionVote,
			TraceID:   tr.ID,
			CortexID:  "01FUZZ",
			Origin:    "fuzz-peer",
			Timestamp: "2026-04-21T14:00:00Z",
			Data:      json.RawMessage(data),
		}
		_ = cx.ReplayEvent(e)
		if _, err := cx.Get(tr.ID); err != nil {
			t.Fatalf("cx.Get failed after vote payload %q: %v", string(data), err)
		}
	})
}

// fuzzSetup opens a real on-disk cortex scoped to the fuzz target's
// lifetime (*testing.F rather than *testing.T — the cortex is reused
// across every fuzz iteration so we don't pay DB setup cost per
// input). Seeds a single trace the fuzz payloads can target.
func fuzzSetup(f *testing.F, name string) (*cortex.Cortex, *trace.Trace) {
	f.Helper()
	dir := f.TempDir()
	if _, err := cortex.Create(name, dir); err != nil {
		f.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open(name, filepath.Join(dir, name))
	if err != nil {
		f.Fatalf("Open: %v", err)
	}
	f.Cleanup(func() { cx.Close() })

	tr := trace.New("fuzz target", "note", "local", nil, "body")
	if err := cx.Add(tr); err != nil {
		f.Fatalf("Add: %v", err)
	}
	return cx, tr
}
