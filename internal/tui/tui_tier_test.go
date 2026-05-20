package tui

import (
	"strings"
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

// TestTUI_HelpToggle pins the discoverability fix: '?' from the list
// enters the help overlay; '?' or 'esc' exits back to the list. The
// gap this closes is that tier-toggle keys (1/2/3/0) had been
// undocumented in any UI surface, so users couldn't find the long-tier
// rows hidden behind the default filter.
func TestTUI_HelpToggle(t *testing.T) {
	m := initialModel(nil)
	if m.state != stateList {
		t.Fatalf("starting state = %v, want stateList", m.state)
	}
	m = keyPress(m, "?")
	if m.state != stateHelp {
		t.Errorf("'?' from list did not enter stateHelp, got %v", m.state)
	}
	next, _ := m.updateHelp(tea.KeyMsg{Type: tea.KeyEsc})
	if next.state != stateList {
		t.Errorf("'esc' from help did not return to stateList, got %v", next.state)
	}
}

// TestFooter_AdvertisesTierKeys locks in that the previously-hidden
// tier toggle keys and the help overlay are advertised in the default-
// mode footer hint. The whole point of this work is that these keys
// are no longer invisible to a user who hasn't read the source.
func TestFooter_AdvertisesTierKeys(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "first", "note")
	m := loadedModel(t, cx)

	hint := m.renderFooter()
	for _, want := range []string{"1/2/3:tier", "0:all-tiers", "?:help"} {
		if !strings.Contains(hint, want) {
			t.Errorf("footer missing %q hint:\n%s", want, hint)
		}
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

// TestRenderTierGlyph_DimDropsANSI pins the pane-dim-backdrop
// interaction. When the list pane is acting as a dim backdrop (the
// detail pane owns focus), the tier glyph must render as a plain
// character without any embedded ANSI color — otherwise the heat-
// chart styling's reset terminates the outer styleRowDim mid-line
// and everything after the glyph (title, badge, date) fails to dim.
// This test pins that contract so a future "let's always colorize the
// tier glyph" refactor has to reckon with the dim case.
func TestRenderTierGlyph_DimDropsANSI(t *testing.T) {
	loadPalette("dark")

	cases := []struct {
		tier string
	}{{"short"}, {"mid"}, {"long"}}

	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			// Non-dim: ANSI color present so the heat chart reads.
			colored := renderTierGlyph(tc.tier, false)
			if !strings.Contains(colored, "\x1b[") {
				t.Errorf("renderTierGlyph(%q, false) should embed ANSI, got %q", tc.tier, colored)
			}
			// Dim: must be a plain character so outer dim style can
			// wrap the whole row uniformly.
			plain := renderTierGlyph(tc.tier, true)
			if strings.Contains(plain, "\x1b[") {
				t.Errorf("renderTierGlyph(%q, true) leaked ANSI, got %q — outer dim will break at the glyph", tc.tier, plain)
			}
		})
	}
}
