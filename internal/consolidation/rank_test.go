package consolidation_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/federation"
)

func TestGenerateRank_Bounds(t *testing.T) {
	// 1000 samples is overkill but cheap; rules out a one-off off-by-one
	// at either boundary.
	for range 1000 {
		r, err := consolidation.GenerateRank()
		if err != nil {
			t.Fatalf("GenerateRank: %v", err)
		}
		if r < consolidation.RankMin || r > consolidation.RankMax {
			t.Errorf("rank %d out of [%d, %d]", r, consolidation.RankMin, consolidation.RankMax)
		}
	}
}

func TestElectWinner_Empty(t *testing.T) {
	if got := consolidation.ElectWinner(nil, 0, time.Now()); got != "" {
		t.Errorf("empty entries: got %q, want empty string", got)
	}
}

func TestElectWinner_AllIneligible(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	entries := []consolidation.RankEntry{
		{CortexID: "A", Rank: 0, ObservedAt: old},
		{CortexID: "B", Rank: 0, ObservedAt: old},
	}
	if got := consolidation.ElectWinner(entries, 0, now); got != "" {
		t.Errorf("all ineligible: got %q, want empty string", got)
	}
}

func TestElectWinner_HighestRankWins(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	entries := []consolidation.RankEntry{
		{CortexID: "A", Rank: 10, ObservedAt: old},
		{CortexID: "B", Rank: 50, ObservedAt: old},
		{CortexID: "C", Rank: 30, ObservedAt: old},
	}
	if got := consolidation.ElectWinner(entries, 0, now); got != "B" {
		t.Errorf("got %q, want B (rank 50 > 30 > 10)", got)
	}
}

func TestElectWinner_TiebreakOnCortexID(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	// Three peers all rolled rank 42. Lex-max CortexID wins.
	entries := []consolidation.RankEntry{
		{CortexID: "01ABC", Rank: 42, ObservedAt: old},
		{CortexID: "01XYZ", Rank: 42, ObservedAt: old},
		{CortexID: "01DEF", Rank: 42, ObservedAt: old},
	}
	if got := consolidation.ElectWinner(entries, 0, now); got != "01XYZ" {
		t.Errorf("got %q, want 01XYZ (lex-max tiebreak)", got)
	}
}

func TestElectWinner_QuietPeriodFiltersFresh(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-5 * time.Second).UTC().Format(time.RFC3339)
	stale := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	entries := []consolidation.RankEntry{
		{CortexID: "A", Rank: 99, ObservedAt: fresh}, // fresh, higher rank
		{CortexID: "B", Rank: 50, ObservedAt: stale}, // stale, lower rank
	}
	// With a 1-minute quiet period, A is filtered out and B wins despite
	// its lower rank. This is the core quiet-period guard — a just-
	// advertised entry shouldn't dominate an election before the rest of
	// the ring has had a chance to observe it.
	if got := consolidation.ElectWinner(entries, time.Minute, now); got != "B" {
		t.Errorf("quiet period: got %q, want B (A should be filtered)", got)
	}
}

func TestElectWinner_SkipsMalformedTimestamp(t *testing.T) {
	// A peer advertising garbage is defensively ignored, not elected.
	entries := []consolidation.RankEntry{
		{CortexID: "A", Rank: 99, ObservedAt: "not a timestamp"},
	}
	if got := consolidation.ElectWinner(entries, 0, time.Now()); got != "" {
		t.Errorf("malformed ts: got %q, want empty string", got)
	}
}

func TestElectWinner_SkipsMissingTimestamp(t *testing.T) {
	entries := []consolidation.RankEntry{
		{CortexID: "A", Rank: 99, ObservedAt: ""},
	}
	if got := consolidation.ElectWinner(entries, 0, time.Now()); got != "" {
		t.Errorf("missing ts: got %q, want empty string", got)
	}
}

// newStateForTest mirrors federation's state_health_test.go helper: open
// a real on-disk cortex so rank round-trips exercise the actual
// federation_state table, not a mock.
func newStateForTest(t *testing.T) *federation.State {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("rank", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("rank", filepath.Join(dir, "rank"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return federation.NewState(cx.DB.DB)
}

func TestLocalRank_RoundTrip(t *testing.T) {
	s := newStateForTest(t)

	got, err := consolidation.ReadLocalRank(s)
	if err != nil {
		t.Fatalf("ReadLocalRank: %v", err)
	}
	if got.Rank != consolidation.RankIneligible {
		t.Errorf("fresh cortex local rank = %d, want %d", got.Rank, consolidation.RankIneligible)
	}

	want := consolidation.RankEntry{
		CortexID:   "01KPAA8VQNG3TKMCY1XJ0JJG0Z",
		Rank:       42,
		ObservedAt: "2026-04-21T12:00:00Z",
	}
	if err := consolidation.WriteLocalRank(s, want); err != nil {
		t.Fatalf("WriteLocalRank: %v", err)
	}
	got, err = consolidation.ReadLocalRank(s)
	if err != nil {
		t.Fatalf("ReadLocalRank after write: %v", err)
	}
	if got != want {
		t.Errorf("round-trip: got %+v, want %+v", got, want)
	}
}

func TestPeerRank_RoundTrip(t *testing.T) {
	s := newStateForTest(t)

	got, err := consolidation.ReadPeerRank(s, "never-seen")
	if err != nil {
		t.Fatalf("ReadPeerRank: %v", err)
	}
	if got.Rank != consolidation.RankIneligible {
		t.Errorf("never-seen peer rank = %d, want %d", got.Rank, consolidation.RankIneligible)
	}

	want := consolidation.RankEntry{
		CortexID:   "01KPBB9XR5J5YZ",
		Rank:       77,
		ObservedAt: "2026-04-21T12:01:00Z",
	}
	if err := consolidation.WritePeerRank(s, "ai-2", want); err != nil {
		t.Fatalf("WritePeerRank: %v", err)
	}
	got, err = consolidation.ReadPeerRank(s, "ai-2")
	if err != nil {
		t.Fatalf("ReadPeerRank after write: %v", err)
	}
	if got != want {
		t.Errorf("round-trip: got %+v, want %+v", got, want)
	}
}

func TestPeerRank_Isolation(t *testing.T) {
	// Writing peer A must not bleed into peer B or the local entry.
	s := newStateForTest(t)

	aEntry := consolidation.RankEntry{CortexID: "01A", Rank: 10, ObservedAt: "2026-04-21T12:00:00Z"}
	if err := consolidation.WritePeerRank(s, "ai-2", aEntry); err != nil {
		t.Fatalf("WritePeerRank ai-2: %v", err)
	}

	b, err := consolidation.ReadPeerRank(s, "ai-3")
	if err != nil {
		t.Fatalf("ReadPeerRank ai-3: %v", err)
	}
	if b.Rank != consolidation.RankIneligible {
		t.Errorf("ai-3 rank bled from ai-2: got %d, want %d", b.Rank, consolidation.RankIneligible)
	}

	local, err := consolidation.ReadLocalRank(s)
	if err != nil {
		t.Fatalf("ReadLocalRank: %v", err)
	}
	if local.Rank != consolidation.RankIneligible {
		t.Errorf("local rank bled from peer write: got %d, want %d", local.Rank, consolidation.RankIneligible)
	}
}

func TestReadLocalRank_TolerantOfMalformedJSON(t *testing.T) {
	// A malformed JSON blob (corruption, half-written value) is treated as
	// "no data" rather than a hard error — rank is an advisory signal, not
	// load-bearing, and a parse failure shouldn't block consolidation
	// startup.
	s := newStateForTest(t)
	// Forcibly write garbage under the local rank key via the generic
	// State.Set API.
	if err := s.Set("consolidation:rank", "{not valid json"); err != nil {
		t.Fatalf("Set garbage: %v", err)
	}
	got, err := consolidation.ReadLocalRank(s)
	if err != nil {
		t.Fatalf("ReadLocalRank: %v", err)
	}
	if got.Rank != consolidation.RankIneligible {
		t.Errorf("malformed JSON: got rank %d, want %d", got.Rank, consolidation.RankIneligible)
	}
}
