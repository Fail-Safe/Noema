package cortex_test

import (
	"encoding/json"
	"testing"

	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// TestReplay_AdversarialPayloads checks that ReplayEvent never panics
// or corrupts the DB on adversarial event payloads for the new
// replay handlers. A malicious peer (or a bug in another cortex's
// event emitter) could otherwise ship a crafted event that either
// crashes the process or pins the federation cursor indefinitely.
func TestReplay_AdversarialPayloads(t *testing.T) {
	cx := setup(t)
	tr := trace.New("victim", "note", "local", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	cases := []struct {
		name   string
		action event.Action
		data   []byte
	}{
		{"promote/empty", event.ActionPromote, []byte(`{}`)},
		{"promote/missing-to", event.ActionPromote, []byte(`{"from":"short"}`)},
		{"promote/malformed-json", event.ActionPromote, []byte(`{not json`)},
		{"promote/null-to", event.ActionPromote, []byte(`{"from":"short","to":null}`)},
		{"promote/sqli-attempt", event.ActionPromote, []byte(`{"from":"short","to":"mid'; DROP TABLE traces; --"}`)},
		{"demote/missing-to", event.ActionDemote, []byte(`{"from":"mid"}`)},
		{"vote/missing-delta", event.ActionVote, []byte(`{"actor":"human"}`)},
		{"vote/huge-delta", event.ActionVote, []byte(`{"delta":9999999999}`)},
		{"vote/negative-delta", event.ActionVote, []byte(`{"delta":-1}`)},
		{"vote/malformed", event.ActionVote, []byte(`not valid at all`)},
		{"consolidate/empty", event.ActionConsolidate, []byte(`{}`)},
		{"consolidate/garbage", event.ActionConsolidate, []byte(`{{{`)},
		{"purge-long/missing-reason", event.ActionPurgeLongTerm, []byte(`{}`)},
		{"purge-hard/missing-fields", event.ActionPurgeHard, []byte(`{}`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic on %s: %v", tc.name, r)
				}
			}()
			e := event.Event{
				ID:        event.NewULID(),
				Action:    tc.action,
				TraceID:   tr.ID,
				CortexID:  "01REMOTE",
				Origin:    "hostile-peer",
				Timestamp: "2026-04-21T14:00:00Z",
				Data:      json.RawMessage(tc.data),
			}
			// Success or error doesn't matter — the contract is "don't
			// panic, don't corrupt the DB." A later Get on the seeded
			// trace below confirms the DB is still queryable.
			_ = cx.ReplayEvent(e)

			// Post-check: trace still retrievable, DB still queryable.
			// Exception: purge-hard with missing fields could have
			// hard-deleted the seeded trace. Re-seed if the row is gone.
			if _, err := cx.Get(tr.ID); err != nil {
				if tc.action == event.ActionPurgeHard {
					// Purge-hard is allowed to remove the row; skip the
					// post-check for that case.
					return
				}
				t.Errorf("cx.Get failed after %s: %v", tc.name, err)
			}
		})
	}
}
