package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Fail-Safe/Noema/internal/trace"
)

// Phase 10 of memory tiering: TUI filter keys and vote keys. See
// docs/plans/consolidation-plan.md §12 in the Noema-design repo.

func keyPress(m model, key string) model {
	next, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	return next
}

func TestTUI_DefaultTierVisibility(t *testing.T) {
	m := initialModel(nil)
	if !m.visibleShort || !m.visibleMid || m.visibleLong {
		t.Errorf("default tiers: short=%v mid=%v long=%v, want true/true/false",
			m.visibleShort, m.visibleMid, m.visibleLong)
	}
	if got := m.currentTiers(); len(got) != 2 {
		t.Errorf("default currentTiers = %v, want 2 entries", got)
	}
}

func TestTUI_CurrentTiers_NilWhenAllVisible(t *testing.T) {
	// Nil return means "no tier filter" — lets the query path skip the
	// IN clause entirely so TUI-all-visible has the same plan as
	// pre-Phase-10 queries.
	m := initialModel(nil)
	m.visibleLong = true
	if got := m.currentTiers(); got != nil {
		t.Errorf("currentTiers with all visible = %v, want nil", got)
	}
}

func TestTUI_TierTogglesAndReset(t *testing.T) {
	m := initialModel(nil)
	m = keyPress(m, "1") // short off
	if m.visibleShort {
		t.Error("'1' did not toggle short off")
	}
	m = keyPress(m, "1") // short back on
	if !m.visibleShort {
		t.Error("'1' second press did not toggle short back on")
	}
	m = keyPress(m, "3") // long on
	if !m.visibleLong {
		t.Error("'3' did not toggle long on")
	}

	// '0' shows all tiers.
	m.visibleShort = false
	m.visibleMid = false
	m.visibleLong = false
	m = keyPress(m, "0")
	if !m.visibleShort || !m.visibleMid || !m.visibleLong {
		t.Errorf("'0' did not restore all tiers: %+v", []bool{m.visibleShort, m.visibleMid, m.visibleLong})
	}
}

func TestTUI_TierToggleResetsCursor(t *testing.T) {
	// Toggling a tier filter can make the current cursor position
	// point at a row that's no longer in the filtered list. The
	// handler resets cursor to 0 to avoid index-out-of-range
	// surprises on the next render.
	m := initialModel(nil)
	m.cursor = 5
	m = keyPress(m, "1")
	if m.cursor != 0 {
		t.Errorf("tier toggle did not reset cursor: %d", m.cursor)
	}
}

func TestTierBadge(t *testing.T) {
	cases := map[string]string{
		trace.TierShort: "s",
		trace.TierMid:   "m",
		trace.TierLong:  "L",
		"":              "?",
		"bogus":         "?",
	}
	for tier, want := range cases {
		if got := tierBadge(tier); got != want {
			t.Errorf("tierBadge(%q) = %q, want %q", tier, got, want)
		}
	}
}

// TestFormatVotes pins the three-state display contract for the
// tier-votes detail-pane line. Zero renders bare rather than as
// "+0" (the unsigned zero looks wrong, and was an early iteration
// bug that confused users into thinking they'd voted). Positive
// values carry an explicit "+" so the sign is always legible at a
// glance next to the tier name.
func TestFormatVotes(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "+1"},
		{7, "+7"},
		{-1, "-1"},
		{-3, "-3"},
	}
	for _, tc := range cases {
		if got := formatVotes(tc.n); got != tc.want {
			t.Errorf("formatVotes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
