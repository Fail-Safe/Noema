package cortex_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// Phase 4 of memory tiering: tier mutation methods (Promote, Demote,
// Vote) with event emission. See docs/plans/consolidation-plan.md §9
// and §12 in the Noema-design repo.

func tierOf(t *testing.T, cx *cortex.Cortex, id string) string {
	t.Helper()
	row, err := cx.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return row.Tier
}

func voteSumOf(t *testing.T, cx *cortex.Cortex, id string) int {
	t.Helper()
	var votes int
	if err := cx.DB.QueryRow(`SELECT tier_votes FROM traces WHERE id = ?`, id).Scan(&votes); err != nil {
		t.Fatalf("reading tier_votes: %v", err)
	}
	return votes
}

// lastEvent returns the most recent event for a trace, useful for
// asserting that Promote/Demote/Vote emitted the right action with
// the right payload.
func lastEvent(t *testing.T, cx *cortex.Cortex, id string) (event.Action, map[string]any) {
	t.Helper()
	var action string
	var data string
	err := cx.DB.QueryRow(
		`SELECT action, data FROM events WHERE trace_id = ? ORDER BY id DESC LIMIT 1`, id,
	).Scan(&action, &data)
	if err != nil {
		t.Fatalf("reading latest event: %v", err)
	}
	var payload map[string]any
	if data != "" {
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshaling event data %q: %v", data, err)
		}
	}
	return event.Action(action), payload
}

// seed creates a trace at the given starting tier. Uses Cortex.Add (which
// accepts explicit tier per Phase 1 policy) for short/mid seeds, and
// promotes step-by-step for long (Promote enforces short->mid->long).
func seed(t *testing.T, cx *cortex.Cortex, title, startTier string) string {
	t.Helper()
	tr := trace.New(title, "note", "", nil, "body")
	switch startTier {
	case trace.TierShort, "":
		tr.Tier = trace.TierShort
	case trace.TierMid:
		tr.Tier = trace.TierMid
	case trace.TierLong:
		tr.Tier = trace.TierMid
	}
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if startTier == trace.TierLong {
		if err := cx.Promote(tr.ID, trace.TierLong); err != nil {
			t.Fatalf("Promote to long: %v", err)
		}
	}
	return tr.ID
}

// ---- Promote ----

func TestPromote_ShortToMid(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "short to mid", trace.TierShort)
	if err := cx.Promote(id, trace.TierMid); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if got := tierOf(t, cx, id); got != trace.TierMid {
		t.Errorf("tier after promote = %q, want %q", got, trace.TierMid)
	}
	action, payload := lastEvent(t, cx, id)
	if action != event.ActionPromote {
		t.Errorf("last event = %q, want %q", action, event.ActionPromote)
	}
	if payload["from"] != trace.TierShort || payload["to"] != trace.TierMid {
		t.Errorf("event payload = %+v, want from=short,to=mid", payload)
	}
}

func TestPromote_MidToLong(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "mid to long", trace.TierMid)
	if err := cx.Promote(id, trace.TierLong); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if got := tierOf(t, cx, id); got != trace.TierLong {
		t.Errorf("tier after promote = %q, want %q", got, trace.TierLong)
	}
}

func TestPromote_RefusesCrossSkipAndReverse(t *testing.T) {
	cx := setup(t)
	cases := []struct {
		name      string
		startTier string
		target    string
	}{
		{"short to long (cross-skip)", trace.TierShort, trace.TierLong},
		{"mid to short (reverse)", trace.TierMid, trace.TierShort},
		{"long to mid (reverse)", trace.TierLong, trace.TierMid},
		{"short to short (no-op)", trace.TierShort, trace.TierShort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := seed(t, cx, tc.name, tc.startTier)
			err := cx.Promote(id, tc.target)
			if err == nil {
				t.Errorf("Promote %s -> %s should have failed", tc.startTier, tc.target)
			} else if !strings.Contains(err.Error(), "invalid promotion") {
				t.Errorf("error = %v, want 'invalid promotion'", err)
			}
		})
	}
}

// ---- Demote ----

func TestDemote_MidToShort(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "mid to short", trace.TierMid)
	if err := cx.Demote(id, trace.TierShort); err != nil {
		t.Fatalf("Demote: %v", err)
	}
	if got := tierOf(t, cx, id); got != trace.TierShort {
		t.Errorf("tier after demote = %q, want %q", got, trace.TierShort)
	}
	action, payload := lastEvent(t, cx, id)
	if action != event.ActionDemote {
		t.Errorf("last event = %q, want %q", action, event.ActionDemote)
	}
	if payload["from"] != trace.TierMid || payload["to"] != trace.TierShort {
		t.Errorf("event payload = %+v, want from=mid,to=short", payload)
	}
}

func TestDemote_RefusesLongDemotion(t *testing.T) {
	// Long demotion is the admin-purge path's responsibility (Phase 6).
	// Demote deliberately refuses it here so the "long is terminal in
	// routine operation" invariant stays intact.
	cx := setup(t)
	id := seed(t, cx, "long demote attempt", trace.TierLong)
	err := cx.Demote(id, trace.TierShort)
	if err == nil {
		t.Error("Demote long -> short should have been refused")
	}
}

func TestDemote_RefusesShortDemote(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "short demote", trace.TierShort)
	err := cx.Demote(id, trace.TierShort)
	if err == nil {
		t.Error("Demote short -> short should have been refused")
	}
}

// ---- Vote ----

func TestVote_AccumulatesBothDirections(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "vote accumulation", trace.TierShort)
	if err := cx.Vote(id, 1, cortex.ActorHuman); err != nil {
		t.Fatalf("Vote +1: %v", err)
	}
	if err := cx.Vote(id, 1, cortex.ActorAgent); err != nil {
		t.Fatalf("Vote +1 agent: %v", err)
	}
	if err := cx.Vote(id, -1, cortex.ActorHuman); err != nil {
		t.Fatalf("Vote -1: %v", err)
	}
	if got := voteSumOf(t, cx, id); got != 1 {
		t.Errorf("tier_votes sum = %d, want 1 (two +1 one -1)", got)
	}

	action, payload := lastEvent(t, cx, id)
	if action != event.ActionVote {
		t.Errorf("last event = %q, want %q", action, event.ActionVote)
	}
	// Event payload stores numbers as float64 after JSON round-trip.
	if delta, _ := payload["delta"].(float64); delta != -1 {
		t.Errorf("last event delta = %v, want -1", payload["delta"])
	}
	if payload["actor"] != "human" {
		t.Errorf("last event actor = %v, want human", payload["actor"])
	}
}

func TestVote_RejectsBadDelta(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "bad delta", trace.TierShort)
	// ±1 and ±2 are valid (±2 supports the TUI session-toggle flip
	// case: going from -1 intent to +1 intent in one keypress). 0
	// and anything beyond ±2 is rejected so the signal can't be
	// amplified arbitrarily.
	for _, bad := range []int{0, 3, -3, 10} {
		if err := cx.Vote(id, bad, cortex.ActorAgent); err == nil {
			t.Errorf("Vote with delta %d should have failed", bad)
		}
	}
	if got := voteSumOf(t, cx, id); got != 0 {
		t.Errorf("tier_votes polluted by rejected votes = %d, want 0", got)
	}
}

func TestVote_AcceptsFlipDelta(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "flip delta", trace.TierShort)
	// -2 flips a -1 intent straight to +1 (or a +1 to -1) in the
	// TUI session-toggle handler. The DB accumulator just adds.
	if err := cx.Vote(id, -1, cortex.ActorHuman); err != nil {
		t.Fatalf("Vote -1: %v", err)
	}
	if err := cx.Vote(id, 2, cortex.ActorHuman); err != nil {
		t.Fatalf("Vote +2 (flip): %v", err)
	}
	if got := voteSumOf(t, cx, id); got != 1 {
		t.Errorf("tier_votes after -1 then +2 = %d, want 1", got)
	}
	if err := cx.Vote(id, -2, cortex.ActorHuman); err != nil {
		t.Fatalf("Vote -2 (flip): %v", err)
	}
	if got := voteSumOf(t, cx, id); got != -1 {
		t.Errorf("tier_votes after flip back = %d, want -1", got)
	}
}

func TestVote_RejectsSystemActor(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "system vote", trace.TierShort)
	if err := cx.Vote(id, 1, cortex.ActorSystem); err == nil {
		t.Error("Vote from ActorSystem should have been refused")
	}
	if got := voteSumOf(t, cx, id); got != 0 {
		t.Errorf("tier_votes polluted by system vote = %d, want 0", got)
	}
}

func TestVote_OnLongTerm_PermittedByRefinedTrigger(t *testing.T) {
	// Pins the guarantee from migration 009: the immutability trigger
	// must permit tier_votes changes on tier='long' rows. A break here
	// signals the refined WHEN clause has drifted into over-blocking.
	cx := setup(t)
	id := seed(t, cx, "long vote", trace.TierLong)
	if err := cx.Vote(id, 1, cortex.ActorHuman); err != nil {
		t.Fatalf("Vote on long-term: %v", err)
	}
	if got := voteSumOf(t, cx, id); got != 1 {
		t.Errorf("tier_votes = %d, want 1", got)
	}
}

func TestLongTerm_ContentStillImmutable(t *testing.T) {
	// Counterpoint to the previous test: even after migration 009
	// relaxes the trigger for counter/vote updates, editing content
	// or identity on a long-term row must still abort.
	cx := setup(t)
	id := seed(t, cx, "long content lock", trace.TierLong)
	_, err := cx.DB.Exec(`UPDATE traces SET title = ? WHERE id = ?`, "rewrite", id)
	if err == nil {
		t.Error("refined trigger let title change slip through on long-term")
	}
}
