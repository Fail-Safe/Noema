package cortex_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// insertRawEvent writes an event row directly so tests can backdate
// timestamps and exercise the time-window filters. Uses the same
// columns the production emitter writes via internal/cortex/cortex.go
// emitEvent (id, action, trace_id, origin, timestamp, data, vclock,
// cortex_id). Production callers go through emitEvent inside a
// transaction; tests are the only legitimate direct-insert path.
func insertRawEvent(t *testing.T, cx *cortex.Cortex, action event.Action, traceID string, ts time.Time, data map[string]any) {
	t.Helper()
	payload := []byte("{}")
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			t.Fatalf("marshal event data: %v", err)
		}
		payload = b
	}
	// Synthesize a unique id so we can insert many events in one test.
	id := fmt.Sprintf("test-%d-%s-%s", ts.UnixNano(), action, traceID)
	_, err := cx.DB.Exec(
		`INSERT INTO events (id, action, trace_id, origin, timestamp, data, vclock, cortex_id)
		 VALUES (?, ?, ?, ?, ?, ?, '{}', ?)`,
		id, string(action), traceID, cx.Name, ts.UTC().Format(time.RFC3339), string(payload), cx.ID,
	)
	if err != nil {
		t.Fatalf("insert raw event: %v", err)
	}
}

// TestParseSince pins the duration syntax accepted by every --since
// surface. Go's time.ParseDuration handles "24h" / "90m" natively;
// the "d" (days) and "w" (weeks) suffixes are added by ParseSince so
// operators don't have to compute hours for week-scale lookbacks.
func TestParseSince(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"", 0, false},
		{"24h", 24 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"2w", 2 * 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"garbage", 0, true},
		{"5x", 0, true},
	}
	for _, tc := range cases {
		got, err := cortex.ParseSince(tc.in)
		if tc.err && err == nil {
			t.Errorf("ParseSince(%q): want error, got %v", tc.in, got)
			continue
		}
		if !tc.err && err != nil {
			t.Errorf("ParseSince(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSince(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestConsolidationActivity_EmptyLog(t *testing.T) {
	cx := setup(t)
	got, err := cx.ConsolidationActivity(0)
	if err != nil {
		t.Fatalf("ConsolidationActivity: %v", err)
	}
	if len(got.Daily) != 0 {
		t.Errorf("Daily = %v, want empty", got.Daily)
	}
	if (got.Totals != cortex.ConsolidationTotals{}) {
		t.Errorf("Totals = %+v, want zero", got.Totals)
	}
}

// TestConsolidationActivity_BucketsAndTotals pins that the day-bucket
// keying is UTC-date (substr of RFC3339 timestamp) and that every
// observed action increments both its day bucket and the rollup total.
// Inserts a mix of action types across two days so a regression that
// swaps action keys or off-by-ones a date boundary is visible.
func TestConsolidationActivity_BucketsAndTotals(t *testing.T) {
	cx := setup(t)
	tr := trace.New("seed", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	day1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	insertRawEvent(t, cx, event.ActionConsolidationClaim, tr.ID, day1, nil)
	insertRawEvent(t, cx, event.ActionConsolidationSuccess, tr.ID, day1, nil)
	insertRawEvent(t, cx, event.ActionConsolidationSuccess, tr.ID, day2, nil)
	insertRawEvent(t, cx, event.ActionConsolidationFail, tr.ID, day2, nil)
	insertRawEvent(t, cx, event.ActionPromote, tr.ID, day2, map[string]any{"from": "short", "to": "mid"})
	insertRawEvent(t, cx, event.ActionConsolidate, tr.ID, day2, nil)

	got, err := cx.ConsolidationActivity(0)
	if err != nil {
		t.Fatalf("ConsolidationActivity: %v", err)
	}
	if len(got.Daily) != 2 {
		t.Fatalf("Daily entries = %d, want 2: %+v", len(got.Daily), got.Daily)
	}
	if got.Daily[0].Date != "2026-05-01" || got.Daily[0].Claim != 1 || got.Daily[0].Success != 1 {
		t.Errorf("Day 1 = %+v, want date=2026-05-01 claim=1 success=1", got.Daily[0])
	}
	if got.Daily[1].Date != "2026-05-02" || got.Daily[1].Success != 1 || got.Daily[1].Fail != 1 || got.Daily[1].Promote != 1 || got.Daily[1].Distill != 1 {
		t.Errorf("Day 2 = %+v, want date=2026-05-02 success=1 fail=1 promote=1 distill=1", got.Daily[1])
	}
	want := cortex.ConsolidationTotals{Success: 2, Fail: 1, Claim: 1, Promote: 1, Distill: 1}
	if got.Totals != want {
		t.Errorf("Totals = %+v, want %+v", got.Totals, want)
	}
}

// TestConsolidationActivity_SinceFilter pins that the time window
// drops events older than the cutoff. Without this filter the
// `noema memory health --since 24h` command would always show
// all-time totals, defeating the point.
func TestConsolidationActivity_SinceFilter(t *testing.T) {
	cx := setup(t)
	tr := trace.New("seed", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	old := time.Now().UTC().Add(-72 * time.Hour)
	recent := time.Now().UTC().Add(-1 * time.Hour)
	insertRawEvent(t, cx, event.ActionConsolidationSuccess, tr.ID, old, nil)
	insertRawEvent(t, cx, event.ActionConsolidationSuccess, tr.ID, recent, nil)

	got, err := cx.ConsolidationActivity(24 * time.Hour)
	if err != nil {
		t.Fatalf("ConsolidationActivity: %v", err)
	}
	if got.Totals.Success != 1 {
		t.Errorf("Totals.Success = %d, want 1 (since-window should drop the 72h-old event)", got.Totals.Success)
	}
}

func TestPromotionLatency_EmptyLog(t *testing.T) {
	cx := setup(t)
	got, err := cx.PromotionLatency()
	if err != nil {
		t.Fatalf("PromotionLatency: %v", err)
	}
	if got.ShortToMid.Count != 0 || got.MidToLong.Count != 0 {
		t.Errorf("expected zero counts, got %+v", got)
	}
}

// TestPromotionLatency_ShortToMid_FromCreated pins that short→mid
// measures elapsed time from the trace's created_at to the first
// short→mid promote event. Uses a real Promote call so the event
// payload (`from`/`to` fields) matches what production emits — a
// regression that drops those JSON fields would silently break the
// latency calculation here.
func TestPromotionLatency_ShortToMid_FromCreated(t *testing.T) {
	cx := setup(t)
	tr := trace.New("subject", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := cx.Promote(tr.ID, trace.TierMid); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	got, err := cx.PromotionLatency()
	if err != nil {
		t.Fatalf("PromotionLatency: %v", err)
	}
	if got.ShortToMid.Count != 1 {
		t.Errorf("ShortToMid.Count = %d, want 1", got.ShortToMid.Count)
	}
	if got.ShortToMid.P50 < 0 {
		t.Errorf("ShortToMid.P50 = %v, want >= 0", got.ShortToMid.P50)
	}
}

// TestPromotionLatency_MidToLong_UsesMidEntry is the load-bearing test
// for the two-stage promotion case. A trace created at short, promoted
// to mid 10 days later, then promoted to long 5 days after that should
// report mid→long latency as 5 days, not 15. If the code mistakenly
// used created_at as the start for the mid→long bucket, this test
// would catch it.
func TestPromotionLatency_MidToLong_UsesMidEntry(t *testing.T) {
	cx := setup(t)
	tr := trace.New("two-stage", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Backdate created_at so the elapsed deltas are meaningful.
	created := time.Now().UTC().Add(-20 * 24 * time.Hour)
	if _, err := cx.DB.Exec(`UPDATE traces SET created_at = ? WHERE id = ?`, created.Format(time.RFC3339), tr.ID); err != nil {
		t.Fatalf("backdate created_at: %v", err)
	}
	midEntry := created.Add(10 * 24 * time.Hour)
	longEntry := midEntry.Add(5 * 24 * time.Hour)
	insertRawEvent(t, cx, event.ActionPromote, tr.ID, midEntry, map[string]any{"from": "short", "to": "mid"})
	insertRawEvent(t, cx, event.ActionPromote, tr.ID, longEntry, map[string]any{"from": "mid", "to": "long"})

	got, err := cx.PromotionLatency()
	if err != nil {
		t.Fatalf("PromotionLatency: %v", err)
	}
	if got.MidToLong.Count != 1 {
		t.Fatalf("MidToLong.Count = %d, want 1", got.MidToLong.Count)
	}
	wantApprox := 5 * 24 * time.Hour
	diff := got.MidToLong.P50 - wantApprox
	if diff < -time.Hour || diff > time.Hour {
		t.Errorf("MidToLong.P50 = %v, want ~%v (mid→long should measure from mid entry, not created_at)", got.MidToLong.P50, wantApprox)
	}
}

func TestOneSourceMidCount_Empty(t *testing.T) {
	cx := setup(t)
	got, err := cx.OneSourceMidCount()
	if err != nil {
		t.Fatalf("OneSourceMidCount: %v", err)
	}
	if got.Current != 0 || got.PromotedLast7d != 0 {
		t.Errorf("expected zero, got %+v", got)
	}
}

// TestOneSourceMidCount_CountsActive1Source pins that the current
// count includes only ACTIVE mid traces with exactly one derived_from
// — archived/trashed are excluded (matches the engagement-stats
// convention) and traces with 0 or 2+ sources don't count.
func TestOneSourceMidCount_CountsActive1Source(t *testing.T) {
	cx := setup(t)
	src := trace.New("source", "note", "", nil, "body")
	if err := cx.Add(src); err != nil {
		t.Fatalf("Add src: %v", err)
	}
	// 1-source mid (counts).
	single := trace.New("single", "note", "", nil, "body")
	single.Tier = trace.TierMid
	single.DerivedFrom = []string{src.ID}
	if err := cx.Add(single); err != nil {
		t.Fatalf("Add single: %v", err)
	}
	// 0-source mid (doesn't count).
	zero := trace.New("zero", "note", "", nil, "body")
	zero.Tier = trace.TierMid
	if err := cx.Add(zero); err != nil {
		t.Fatalf("Add zero: %v", err)
	}
	// Archived 1-source mid (doesn't count).
	src2 := trace.New("source2", "note", "", nil, "body")
	if err := cx.Add(src2); err != nil {
		t.Fatalf("Add src2: %v", err)
	}
	hidden := trace.New("hidden", "note", "", nil, "body")
	hidden.Tier = trace.TierMid
	hidden.DerivedFrom = []string{src2.ID}
	if err := cx.Add(hidden); err != nil {
		t.Fatalf("Add hidden: %v", err)
	}
	if err := cx.Archive(hidden.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	got, err := cx.OneSourceMidCount()
	if err != nil {
		t.Fatalf("OneSourceMidCount: %v", err)
	}
	if got.Current != 1 {
		t.Errorf("Current = %d, want 1 (only the active 1-source mid)", got.Current)
	}
}

// TestOneSourceMidCount_PromotedLast7d pins the leak-detection
// signal — a 1-source trace promoted to mid within the last 7 days
// should appear in PromotedLast7d. This is the metric that should be
// zero if PRs #86 and #90's gates are holding.
func TestOneSourceMidCount_PromotedLast7d(t *testing.T) {
	cx := setup(t)
	src := trace.New("source", "note", "", nil, "body")
	if err := cx.Add(src); err != nil {
		t.Fatalf("Add src: %v", err)
	}
	leaker := trace.New("leaker", "note", "", nil, "body")
	leaker.DerivedFrom = []string{src.ID}
	if err := cx.Add(leaker); err != nil {
		t.Fatalf("Add leaker: %v", err)
	}
	// Use a real Promote so the event payload shape matches production.
	if err := cx.Promote(leaker.ID, trace.TierMid); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	got, err := cx.OneSourceMidCount()
	if err != nil {
		t.Fatalf("OneSourceMidCount: %v", err)
	}
	if got.PromotedLast7d != 1 {
		t.Errorf("PromotedLast7d = %d, want 1", got.PromotedLast7d)
	}
	if got.Current != 1 {
		t.Errorf("Current = %d, want 1", got.Current)
	}
}

// TestOneSourceMidCount_OldPromoteExcluded pins that promotes older
// than the 7-day window don't inflate PromotedLast7d — otherwise the
// leak signal stays "on" forever once it fires once.
func TestOneSourceMidCount_OldPromoteExcluded(t *testing.T) {
	cx := setup(t)
	src := trace.New("source", "note", "", nil, "body")
	if err := cx.Add(src); err != nil {
		t.Fatalf("Add src: %v", err)
	}
	old := trace.New("old", "note", "", nil, "body")
	old.Tier = trace.TierMid
	old.DerivedFrom = []string{src.ID}
	if err := cx.Add(old); err != nil {
		t.Fatalf("Add old: %v", err)
	}
	insertRawEvent(t, cx, event.ActionPromote, old.ID,
		time.Now().UTC().Add(-30*24*time.Hour),
		map[string]any{"from": "short", "to": "mid"})

	got, err := cx.OneSourceMidCount()
	if err != nil {
		t.Fatalf("OneSourceMidCount: %v", err)
	}
	if got.PromotedLast7d != 0 {
		t.Errorf("PromotedLast7d = %d, want 0 (30d-old promote should be outside the 7d window)", got.PromotedLast7d)
	}
	if got.Current != 1 {
		t.Errorf("Current = %d, want 1", got.Current)
	}
}
