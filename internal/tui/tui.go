package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// ---- styles ----------------------------------------------------------------

// Brand palette — locked to BRAND.md (three values, no supporting
// palettes). We use CompleteColor rather than bare hex so 256-color
// terminals get hand-picked indices instead of termenv's automatic
// downmatch (which picks color 232 — near-black — for the brand red,
// rendering the period invisible on dark backgrounds).
var (
	// brandCream — warm cream `#ece4d4`. Foreground in dark mode,
	// background in light mode.
	brandCream = lipgloss.CompleteColor{
		TrueColor: "#ece4d4",
		ANSI256:   "223", // #ffd7af — warm beige, closest to cream
		ANSI:      "7",   // white
	}

	// brandInk — near-black `#1a1a1a`. Background in dark mode,
	// foreground in light mode.
	brandInk = lipgloss.CompleteColor{
		TrueColor: "#1a1a1a",
		ANSI256:   "234", // very dark gray
		ANSI:      "0",   // black
	}

	// brandRed — rubricated accent `#e10032`. Period only; never
	// recolored. One value across both themes per BRAND.md.
	brandRed = lipgloss.CompleteColor{
		TrueColor: "#e10032",
		ANSI256:   "161", // #d7005f — closest saturated red
		ANSI:      "1",   // red
	}
)

// palette holds every color the TUI uses for a single theme variant.
// Two instances exist — darkPalette() and lightPalette() — selected
// at startup by loadPalette(). All style vars below are assigned from
// the active palette, so a theme swap rebuilds every style without
// touching the render paths.
type palette struct {
	bg       lipgloss.TerminalColor // surface background
	fg       lipgloss.TerminalColor // brand accent — wordmark + chip fill ONLY
	body     lipgloss.TerminalColor // default body text (row titles, detail values)
	red      lipgloss.TerminalColor // accent (period only)
	label    lipgloss.TerminalColor // dimmer than body — row labels, metadata keys
	value    lipgloss.TerminalColor // metadata values; matches body in current themes
	dim      lipgloss.TerminalColor // header decoration, "no rows" placeholder
	divider  lipgloss.TerminalColor // pane divider + separator lines
	footer   lipgloss.TerminalColor // footer hint text
	selBg    lipgloss.TerminalColor // selected row backdrop
	selFg    lipgloss.TerminalColor // selected row text
	dimRow   lipgloss.TerminalColor // background-pane rows when detail has focus
	selDim   lipgloss.TerminalColor // selected row when detail has focus
	newRow   lipgloss.TerminalColor // highlight for just-arrived rows
	newDim   lipgloss.TerminalColor // newRow shade while detail has focus
	live     lipgloss.TerminalColor // live-mode badge
	errorFg  lipgloss.TerminalColor // footer error text
	statusFg lipgloss.TerminalColor // footer status text

	// Tier glyphs render as a heat chart: short is warm (volatile,
	// just-arrived), long is cold (frozen, archival). Mid sits between
	// as an amber/gold transition. "?" (unknown tier, a migration bug
	// signal) reuses the dim color so it reads as an anomaly, not a
	// fourth legitimate tier.
	tierShort lipgloss.TerminalColor // short-term: warm orange
	tierMid   lipgloss.TerminalColor // mid-term: amber/gold
	tierLong  lipgloss.TerminalColor // long-term: cool blue
}

// darkPalette is the primary brand surface: near-black background,
// cream foreground, rubricated red accent.
func darkPalette() palette {
	return palette{
		bg:       brandInk,
		fg:       brandCream,
		body:     lipgloss.Color("250"),
		red:      brandRed,
		label:    lipgloss.Color("244"),
		value:    lipgloss.Color("250"),
		dim:      lipgloss.Color("240"),
		divider:  lipgloss.Color("238"),
		footer:   lipgloss.Color("241"),
		selBg:    lipgloss.Color("236"),
		selFg:    lipgloss.Color("255"),
		dimRow:   lipgloss.Color("238"),
		selDim:   lipgloss.Color("244"),
		newRow:   lipgloss.Color("71"),
		newDim:   lipgloss.Color("65"),
		live:     lipgloss.Color("71"),
		errorFg:  lipgloss.Color("196"),
		statusFg: lipgloss.Color("71"),
		// Heat-chart tier glyphs on a near-black surface: bright
		// orange for short (volatile), warm gold for mid, sky blue
		// for long (archival). All three are bright enough to pop
		// from the 250-grey body without competing with brandRed.
		tierShort: lipgloss.Color("208"),
		tierMid:   lipgloss.Color("221"),
		tierLong:  lipgloss.Color("39"),
	}
}

// lightPalette uses a cool off-white background (ANSI 230) so the
// warm brand cream can serve as an accent (selected row, chips)
// rather than flooding the entire surface. Ink stays the wordmark
// accent, mirroring cream's role in dark mode.
func lightPalette() palette {
	return palette{
		bg:       lipgloss.Color("255"), // near-white, no color cast — cream is the accent
		fg:       brandInk,
		body:     lipgloss.Color("238"),
		red:      brandRed,
		label:    lipgloss.Color("243"),
		value:    lipgloss.Color("238"),
		dim:      lipgloss.Color("248"),
		divider:  lipgloss.Color("249"),
		footer:   lipgloss.Color("244"),
		selBg:    brandCream, // cream as accent — the cursor row is the warm band
		selFg:    lipgloss.Color("232"),
		dimRow:   lipgloss.Color("249"),
		selDim:   lipgloss.Color("244"),
		newRow:   lipgloss.Color("22"),
		newDim:   lipgloss.Color("28"),
		live:     lipgloss.Color("22"),
		errorFg:  lipgloss.Color("124"),
		statusFg: lipgloss.Color("22"),
		// Heat-chart tiers on a near-white surface need darker
		// variants of the same progression — bright 208 reads as
		// pastel against white. Terracotta (166) keeps the "warm"
		// feel with enough contrast; amber 136 is the gold mid;
		// steel blue 25 is the cool/frozen endpoint.
		tierShort: lipgloss.Color("166"),
		tierMid:   lipgloss.Color("136"),
		tierLong:  lipgloss.Color("25"),
	}
}

// activePalette is set by loadPalette() at startup. Tests that need
// deterministic styling can call loadPalette("dark") explicitly.
var activePalette = darkPalette()

var (
	// styleHeader renders the "Noema" portion of the wordmark in the
	// brand cream. styleHeaderAccent renders the trailing period in
	// brand red. Splitting them keeps the two colors independent.
	styleHeader       lipgloss.Style
	styleHeaderAccent lipgloss.Style

	styleSelected lipgloss.Style
	styleDim      lipgloss.Style

	// styleChip renders tag values (and the type value) as inline
	// pills — inverted brand palette with 1-cell padding. See
	// renderTagChips for the wrap logic.
	styleChip lipgloss.Style

	styleDivider lipgloss.Style
	styleLabel   lipgloss.Style
	styleValue   lipgloss.Style
	styleFooter  lipgloss.Style
	styleError   lipgloss.Style
	styleStatus  lipgloss.Style

	// styleNewRow tints rows that arrived on the most recent refresh —
	// the visible "pop in" effect when watching live updates.
	styleNewRow lipgloss.Style

	// styleLive is the header badge shown while follow mode is active.
	styleLive lipgloss.Style

	// When the detail pane owns focus, the list pane gets a "modal
	// backdrop" treatment — every row renders through a dim palette
	// so it visibly recedes. The selected row stays a notch brighter
	// than the rest so the cursor position is still legible.
	styleSelectedDim lipgloss.Style
	styleRowDim      lipgloss.Style
	styleNewRowDim   lipgloss.Style

	// styleSurface paints the active palette's background on any
	// region passed through it. Used for header, footer, and the
	// implicit fill behind the list/detail/divider block so the
	// whole TUI reads as one brand surface.
	styleSurface lipgloss.Style

	// Heat-chart tier glyphs: short/mid/long get warm→cold colors
	// so a scan of the list reads the tier distribution the same
	// way the eye reads a heatmap. Used by tierBadge.
	styleTierShort lipgloss.Style
	styleTierMid   lipgloss.Style
	styleTierLong  lipgloss.Style
)

func init() {
	loadPalette("dark")
}

// loadPalette reassigns every style variable from the named theme
// ("dark", "light", or "auto" — auto consults lipgloss' terminal
// background detection). Safe to call multiple times; call once at
// startup before Run().
func loadPalette(theme string) {
	switch resolveTheme(theme) {
	case "light":
		activePalette = lightPalette()
	default:
		activePalette = darkPalette()
	}
	p := activePalette

	styleSurface = lipgloss.NewStyle().Background(p.bg).Foreground(p.body)

	styleHeader = lipgloss.NewStyle().
		Bold(true).
		Background(p.bg).
		Foreground(p.fg)

	styleHeaderAccent = lipgloss.NewStyle().
		Bold(true).
		Background(p.bg).
		Foreground(p.red)

	styleSelected = lipgloss.NewStyle().
		Bold(true).
		Background(p.selBg).
		Foreground(p.selFg)

	styleDim = lipgloss.NewStyle().
		Background(p.bg).
		Foreground(p.dim)

	// Chips use the inverted palette — a small "patch of light mode"
	// in dark mode, and vice versa — so they read as a distinct
	// labeled object without introducing a fourth brand color.
	styleChip = lipgloss.NewStyle().
		Background(p.fg).
		Foreground(p.bg).
		Padding(0, 1)

	styleDivider = lipgloss.NewStyle().
		Background(p.bg).
		Foreground(p.divider)

	styleLabel = lipgloss.NewStyle().
		Background(p.bg).
		Foreground(p.label)

	styleValue = lipgloss.NewStyle().
		Background(p.bg).
		Foreground(p.value)

	styleFooter = lipgloss.NewStyle().
		Background(p.bg).
		Foreground(p.footer)

	styleError = lipgloss.NewStyle().
		Background(p.bg).
		Foreground(p.errorFg)

	styleStatus = lipgloss.NewStyle().
		Background(p.bg).
		Foreground(p.statusFg)

	styleNewRow = lipgloss.NewStyle().
		Background(p.bg).
		Foreground(p.newRow)

	styleLive = lipgloss.NewStyle().
		Background(p.bg).
		Foreground(p.live).
		Bold(true)

	styleSelectedDim = lipgloss.NewStyle().
		Background(p.bg).
		Foreground(p.selDim)

	styleRowDim = lipgloss.NewStyle().
		Background(p.bg).
		Foreground(p.dimRow)

	styleNewRowDim = lipgloss.NewStyle().
		Background(p.bg).
		Foreground(p.newDim)

	// Tier glyphs are bold so the heat color registers at a glance
	// even at the 1-character width. Background isn't set here — the
	// outer row style (styleSurface/styleSelected/styleRowDim) paints
	// the row background underneath, and lipgloss preserves the
	// inner foreground when rendering a nested pre-styled substring.
	styleTierShort = lipgloss.NewStyle().Foreground(p.tierShort).Bold(true)
	styleTierMid = lipgloss.NewStyle().Foreground(p.tierMid).Bold(true)
	styleTierLong = lipgloss.NewStyle().Foreground(p.tierLong).Bold(true)
}

// resolveTheme picks a concrete theme ("dark" or "light") from an
// input that may be "auto", empty, or already concrete. Empty and
// "auto" consult the terminal background via lipgloss.
func resolveTheme(in string) string {
	switch in {
	case "dark", "light":
		return in
	default:
		if lipgloss.HasDarkBackground() {
			return "dark"
		}
		return "light"
	}
}

// followInterval is how often the TUI polls the cortex when follow mode
// is on. 1s is fast enough to feel live when an agent is writing into
// the cortex in the background, but slow enough to be invisible CPU load
// (List is sub-millisecond on a local SQLite file).
const followInterval = 1 * time.Second

// newRowHighlightTicks is how many consecutive refresh ticks a newly-
// arrived trace stays highlighted. With followInterval=1s this gives
// ~2 seconds of green tint before the row fades back to normal.
const newRowHighlightTicks = 2

// ---- state machine ---------------------------------------------------------

type viewState int

const (
	stateList    viewState = iota
	stateSearch
	stateConfirm
)

// focusPane tracks which pane (list or detail) receives navigation keys.
// Only the list/detail split is focus-aware — modal states like search
// and confirm pre-empt focus while active.
type focusPane int

const (
	focusList focusPane = iota
	focusDetail
)

// ---- messages --------------------------------------------------------------

type rowsLoadedMsg []cortex.Row

type editorDoneMsg struct {
	err   error
	id    string // empty when adding a new trace
	isNew bool
	path  string // temp file path for new traces
}

type errMsg struct{ err error }

// tickMsg drives the follow-mode polling loop. `gen` is a generation
// counter used to discard stale ticks left over from a previous
// follow-mode session — without it, rapid f-on → f-off → f-on toggling
// could leave multiple tick chains running in parallel.
type tickMsg struct{ gen int }

// ---- model -----------------------------------------------------------------

type confirmAction struct {
	action string // "archive" or "delete"
	id     string
}

type model struct {
	cx           *cortex.Cortex
	rows         []cortex.Row
	cursor       int
	current      *trace.Trace // cached detail for selected row
	currentVotes int          // tier_votes for m.current, refreshed whenever m.current reloads

	// sessionVotes tracks this session's vote intent per trace ID: -1,
	// 0, or +1. The DB column tier_votes is an accumulator that any
	// actor can contribute to; this map is the TUI's local "what have
	// I cast in this session" so a single user can't pile up +6 by
	// holding the + key. Reddit-style three-state cycle: + on a fresh
	// trace → +1; + again → 0 (clears); + when at -1 → +1 (flip).
	sessionVotes map[string]int
	width       int
	height      int
	state       viewState
	search      textinput.Model
	searchQuery string
	showAll     bool
	showTrashed bool
	confirm     confirmAction
	err         error
	status      string

	// Memory-tier visibility filters. Long-term is hidden by default
	// — those are stable "base truths" that would clutter day-to-day
	// browsing of recent cortex activity. Keys 1/2/3 toggle each tier,
	// 0 shows all. See docs/plans/consolidation-plan.md §12 in the
	// Noema-design repo.
	visibleShort bool
	visibleMid   bool
	visibleLong  bool

	// Follow-mode state (auto-refresh).
	follow    bool           // true when auto-refresh is on
	followGen int            // bumped each time follow turns on; stale ticks discarded
	newRowTTL map[string]int // trace ID → ticks of highlight remaining

	// Pane focus + detail-pane scroll position (body lines only —
	// metadata always stays pinned). detailScroll is reset whenever the
	// trace under the cursor changes, but preserved across tab toggles
	// so you can glance at the list and come back to where you were.
	focus        focusPane
	detailScroll int

	// Snapshot of the filter context at the time of the last accepted
	// rowsLoadedMsg. Used to detect when a refresh crosses a context
	// boundary (e.g. user toggled `a`/`t`/`/`) — in that case the new
	// rowset isn't comparable to the previous one, so we skip the
	// new-row diff and the sticky-cursor reseat.
	prevSeen        bool
	lastShowAll     bool
	lastShowTrashed bool
	lastQuery       string
}

func initialModel(cx *cortex.Cortex) model {
	ti := textinput.New()
	ti.Placeholder = "search traces…"
	ti.CharLimit = 120

	return model{
		cx:           cx,
		search:       ti,
		state:        stateList,
		newRowTTL:    map[string]int{},
		sessionVotes: map[string]int{},
		visibleShort: true,
		visibleMid:   true,
		visibleLong:  false,
	}
}

// tierBadge returns the single-character glyph shown next to each row
// in the list. S/M/L correspond to short/mid/long. Empty tier renders
// as "?" to make schema drift visible rather than silent (a trace
// without a tier at this point is a migration bug).
func tierBadge(tier string) string {
	switch tier {
	case trace.TierShort:
		return "s"
	case trace.TierMid:
		return "m"
	case trace.TierLong:
		return "L"
	default:
		return "?"
	}
}

// renderTierGlyph returns the tier badge pre-styled with its heat-chart
// color (warm orange → amber → cool blue for short → mid → long) when
// the pane is in focus. When dim=true (list acting as backdrop while
// the detail pane has focus), the glyph renders as a plain character
// so the outer styleRowDim can apply uniform dimming across the whole
// row — the heat-chart ANSI embeds a reset that would otherwise
// terminate the outer dim mid-line, leaving the title/badge/date
// rendered at default terminal color.
//
// Unknown tiers render without color in both states to keep the "?"
// reading as a migration-anomaly signal rather than a fourth tier.
func renderTierGlyph(tier string, dim bool) string {
	glyph := tierBadge(tier)
	if dim {
		return glyph
	}
	switch tier {
	case trace.TierShort:
		return styleTierShort.Render(glyph)
	case trace.TierMid:
		return styleTierMid.Render(glyph)
	case trace.TierLong:
		return styleTierLong.Render(glyph)
	default:
		return glyph
	}
}

// currentTiers returns the list of tier names the model wants the
// backend to filter on, in a stable order so the resulting SQL plan is
// cacheable and tests can assert on the slice contents. An all-
// visible configuration returns nil so `noema list` and the MCP tools
// behave identically to pre-Phase-10 (no tier filter) when every
// tier is turned on.
func (m model) currentTiers() []string {
	if m.visibleShort && m.visibleMid && m.visibleLong {
		return nil
	}
	var out []string
	if m.visibleShort {
		out = append(out, trace.TierShort)
	}
	if m.visibleMid {
		out = append(out, trace.TierMid)
	}
	if m.visibleLong {
		out = append(out, trace.TierLong)
	}
	return out
}

// ---- commands --------------------------------------------------------------

func loadRows(cx *cortex.Cortex, query string, all, trashed bool, tiers []string) tea.Cmd {
	return func() tea.Msg {
		opts := cortex.ListOptions{All: all, Trashed: trashed, Tiers: tiers}
		var (
			rows []cortex.Row
			err  error
		)
		if query != "" {
			rows, err = cx.Search(query, opts)
		} else {
			rows, err = cx.List(opts)
		}
		if err != nil {
			return errMsg{err}
		}
		return rowsLoadedMsg(rows)
	}
}

// reload is a convenience wrapper so call sites don't have to re-
// assemble the filter every time. Every explicit reload triggered
// from an Update handler flows through here.
func (m model) reload() tea.Cmd {
	return loadRows(m.cx, m.searchQuery, m.showAll, m.showTrashed, m.currentTiers())
}

// tickCmd schedules the next follow-mode poll. The generation is
// carried inside the message so that Update can reject ticks left over
// from a stopped-and-restarted follow session.
func tickCmd(gen int) tea.Cmd {
	return tea.Tick(followInterval, func(_ time.Time) tea.Msg {
		return tickMsg{gen: gen}
	})
}

func editorCmd(path, id string, isNew bool) tea.Cmd {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}
	c := exec.Command(editor, path)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return editorDoneMsg{err: err, id: id, isNew: isNew, path: path}
	})
}

// ---- lifecycle -------------------------------------------------------------

func (m model) Init() tea.Cmd {
	return loadRows(m.cx, "", false, false, m.currentTiers())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case rowsLoadedMsg:
		return m.handleRowsLoaded([]cortex.Row(msg)), nil

	case tickMsg:
		// Discard stale ticks from a prior follow-mode session.
		if !m.follow || msg.gen != m.followGen {
			return m, nil
		}
		// Always re-queue the next tick — we want the chain to keep
		// spinning even when we suppress the actual refresh (search
		// focus, confirm modal), so the loop resumes automatically
		// when the user returns to the list.
		cmds := []tea.Cmd{tickCmd(msg.gen)}
		if m.state == stateList {
			cmds = append(cmds, m.reload())
		}
		return m, tea.Batch(cmds...)

	case editorDoneMsg:
		return m.handleEditorDone(msg)

	case errMsg:
		m.err = msg.err
		m.status = ""
		return m, nil

	case tea.KeyMsg:
		m.err = nil
		m.status = ""
		switch m.state {
		case stateList:
			return m.updateList(msg)
		case stateSearch:
			return m.updateSearch(msg)
		case stateConfirm:
			return m.updateConfirm(msg)
		}
	}
	return m, nil
}

func (m model) updateList(msg tea.KeyMsg) (model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "tab":
		// Cycle focus between the list and detail panes. Modal states
		// (search/confirm) can't be reached from here, so there are
		// only two stops.
		if m.focus == focusList {
			m.focus = focusDetail
		} else {
			m.focus = focusList
		}
		return m, nil

	case "left":
		// Absolute focus: left arrow always lands on the list pane,
		// regardless of current focus. Pairs with "right" for
		// one-handed browsing — you can scroll a trace with j/k on the
		// right side and jump back to the list without thinking about
		// which pane you're currently in.
		m.focus = focusList
		return m, nil

	case "right":
		m.focus = focusDetail
		return m, nil

	case "j", "down":
		if m.focus == focusDetail {
			return m.scrollDetail(1), nil
		}
		if m.cursor < len(m.rows)-1 {
			m = m.selectCursor(m.cursor + 1)
		}

	case "k", "up":
		if m.focus == focusDetail {
			return m.scrollDetail(-1), nil
		}
		if m.cursor > 0 {
			m = m.selectCursor(m.cursor - 1)
		}

	case "g":
		if m.focus == focusDetail {
			m.detailScroll = 0
			return m, nil
		}
		m = m.selectCursor(0)

	case "G":
		if m.focus == focusDetail {
			m.detailScroll = m.detailMaxScroll()
			return m, nil
		}
		if len(m.rows) > 0 {
			m = m.selectCursor(len(m.rows) - 1)
		}

	case "pgdown", "ctrl+d":
		// Half-page scroll is detail-only. In list focus these keys
		// are no-ops (existing behavior).
		if m.focus == focusDetail {
			step := m.detailVisibleBodyH() / 2
			if step < 1 {
				step = 1
			}
			return m.scrollDetail(step), nil
		}

	case "pgup", "ctrl+u":
		if m.focus == focusDetail {
			step := m.detailVisibleBodyH() / 2
			if step < 1 {
				step = 1
			}
			return m.scrollDetail(-step), nil
		}

	case "n":
		if m.showTrashed {
			return m, nil // no new traces from trash view
		}
		path, err := newTraceTempFile()
		if err != nil {
			m.err = err
			return m, nil
		}
		return m, editorCmd(path, "", true)

	case "e":
		if len(m.rows) == 0 || m.showTrashed {
			return m, nil
		}
		row := m.rows[m.cursor]
		if row.SourceLocked && row.Origin != m.cx.Name {
			m.status = "Trace is source-locked by " + row.Origin
			return m, nil
		}
		path := m.cx.TraceFile(row.ID, row.ArchivedAt != "")
		return m, editorCmd(path, row.ID, false)

	case "d":
		if len(m.rows) == 0 || m.showTrashed {
			return m, nil
		}
		m.confirm = confirmAction{action: "archive", id: m.rows[m.cursor].ID}
		m.state = stateConfirm

	case "D":
		if len(m.rows) == 0 {
			return m, nil
		}
		if m.showTrashed {
			// Hard-delete from trash view.
			m.confirm = confirmAction{action: "purge", id: m.rows[m.cursor].ID}
		} else {
			// Soft-delete to trash.
			m.confirm = confirmAction{action: "trash", id: m.rows[m.cursor].ID}
		}
		m.state = stateConfirm

	case "r":
		if len(m.rows) == 0 || !m.showTrashed {
			return m, nil
		}
		row := m.rows[m.cursor]
		if err := m.cx.Recover(row.ID); err != nil {
			m.err = err
			return m, nil
		}
		m.status = "Recovered " + row.ID
		return m, m.reload()

	case "u":
		if len(m.rows) == 0 {
			return m, nil
		}
		row := m.rows[m.cursor]
		if row.ArchivedAt == "" {
			return m, nil
		}
		if err := m.cx.Unarchive(row.ID); err != nil {
			m.err = err
			return m, nil
		}
		m.status = "Unarchived " + row.ID
		return m, m.reload()

	case "a":
		m.showAll = !m.showAll
		m.showTrashed = false
		m.cursor = 0
		return m, loadRows(m.cx, m.searchQuery, m.showAll, false, m.currentTiers())

	case "t":
		m.showTrashed = !m.showTrashed
		m.showAll = false
		m.cursor = 0
		return m, loadRows(m.cx, m.searchQuery, false, m.showTrashed, m.currentTiers())

	case "1":
		m.visibleShort = !m.visibleShort
		m.cursor = 0
		return m, m.reload()

	case "2":
		m.visibleMid = !m.visibleMid
		m.cursor = 0
		return m, m.reload()

	case "3":
		m.visibleLong = !m.visibleLong
		m.cursor = 0
		return m, m.reload()

	case "0":
		m.visibleShort, m.visibleMid, m.visibleLong = true, true, true
		m.cursor = 0
		return m, m.reload()

	case "+", "=":
		// Accept "=" too so users on layouts where "+" requires Shift
		// can upvote without the modifier. "-" is unambiguous.
		//
		// Three-state cycle per trace per TUI session:
		//   0 → +1   (cast upvote)
		//  +1 → 0    (clear my upvote — same key cycles off)
		//  -1 → +1   (flip downvote to upvote)
		// sessionVotes tracks the intent; the DB delta is new - prev.
		if len(m.rows) == 0 {
			return m, nil
		}
		return m.castVote(m.rows[m.cursor].ID, +1, "▲", "cleared ▲ on")

	case "-":
		if len(m.rows) == 0 {
			return m, nil
		}
		return m.castVote(m.rows[m.cursor].ID, -1, "▼", "cleared ▼ on")

	case "/":
		m.state = stateSearch
		m.search.SetValue("")
		m.search.Focus()
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(nil)
		return m, cmd

	case "f":
		// Toggle follow (auto-refresh) mode. A fresh generation is
		// minted on every on-transition so stale ticks from a prior
		// on→off→on cycle are discarded on arrival.
		m.follow = !m.follow
		if m.follow {
			m.followGen++
			m.status = "Live mode on"
			return m, tickCmd(m.followGen)
		}
		m.status = "Live mode off"
		return m, nil

	case "R":
		// Manual refresh — useful when follow is off, or as a
		// "refresh now, don't wait for the next tick" escape hatch.
		return m, m.reload()

	case "esc":
		// First esc pops detail focus back to the list; second esc
		// (or esc with list already focused) clears any active search.
		if m.focus == focusDetail {
			m.focus = focusList
			return m, nil
		}
		if m.searchQuery != "" {
			m.searchQuery = ""
			m.cursor = 0
			return m, loadRows(m.cx, "", m.showAll, m.showTrashed, m.currentTiers())
		}
	}
	return m, nil
}

func (m model) updateSearch(msg tea.KeyMsg) (model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.searchQuery = m.search.Value()
		m.state = stateList
		m.search.Blur()
		m.cursor = 0
		return m, m.reload()

	case "esc":
		m.state = stateList
		m.search.Blur()
		m.search.SetValue(m.searchQuery)
		return m, nil
	}

	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	return m, cmd
}

func (m model) updateConfirm(msg tea.KeyMsg) (model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		action, id := m.confirm.action, m.confirm.id
		m.state = stateList
		m.confirm = confirmAction{}

		var err error
		switch action {
		case "archive":
			err = m.cx.Archive(id)
			if err == nil {
				m.status = "Archived " + id
			}
		case "trash":
			err = m.cx.Trash(id)
			if err == nil {
				m.status = "Moved to trash: " + id
			}
		case "purge":
			err = m.cx.Remove(id)
			if err == nil {
				m.status = "Permanently deleted " + id
			}
		}
		if err != nil {
			m.err = err
			return m, nil
		}
		return m, m.reload()

	case "n", "N", "esc":
		m.state = stateList
		m.confirm = confirmAction{}
	}
	return m, nil
}

func (m model) handleEditorDone(msg editorDoneMsg) (model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
		if msg.isNew && msg.path != "" {
			os.Remove(msg.path)
		}
		return m, nil
	}

	if msg.isNew {
		t, err := trace.ParseFile(msg.path)
		os.Remove(msg.path)
		if err != nil || t.Title == "" {
			// No title → user cancelled; silently discard.
			return m, nil
		}
		traceType := t.Type
		if !trace.IsValidType(traceType) {
			traceType = "note"
		}
		newT := trace.New(t.Title, traceType, t.Author, t.Tags, t.Body)
		if err := m.cx.Add(newT); err != nil {
			m.err = err
			return m, nil
		}
		m.status = "Added " + newT.ID
	} else {
		if err := m.cx.Update(msg.id); err != nil {
			m.err = err
			return m, nil
		}
		m.status = "Updated " + msg.id
	}

	return m, m.reload()
}

// handleRowsLoaded folds a fresh rowset into the model, preserving the
// cursor's trace ID across the refresh and tracking which rows arrived
// on this tick so they can be highlighted briefly. Context changes
// (user toggled a/t/search) skip the sticky + diff logic because the
// new rowset isn't comparable to the previous one. Detail scroll is
// reset if the refresh lands on a different trace than before.
func (m model) handleRowsLoaded(newRows []cortex.Row) model {
	var prevCurrentID string
	if m.current != nil {
		prevCurrentID = m.current.ID
	}
	sameContext := m.prevSeen &&
		m.lastShowAll == m.showAll &&
		m.lastShowTrashed == m.showTrashed &&
		m.lastQuery == m.searchQuery

	if sameContext {
		// Fade the highlight on rows that were flagged previously.
		// Delete-during-iterate is safe for Go maps.
		for id, ttl := range m.newRowTTL {
			if ttl-1 <= 0 {
				delete(m.newRowTTL, id)
			} else {
				m.newRowTTL[id] = ttl - 1
			}
		}
		// Flag rows that weren't in the previous snapshot as new.
		prev := make(map[string]bool, len(m.rows))
		for _, r := range m.rows {
			prev[r.ID] = true
		}
		for _, r := range newRows {
			if !prev[r.ID] {
				m.newRowTTL[r.ID] = newRowHighlightTicks
			}
		}
	} else {
		// Context change — the old highlights belong to a different
		// view. Drop them to avoid a misleading green flash.
		m.newRowTTL = map[string]int{}
	}

	// Sticky cursor by trace ID. Capture what's selected *before*
	// swapping the rows out.
	var selectedID string
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		selectedID = m.rows[m.cursor].ID
	}
	m.rows = newRows

	if sameContext && selectedID != "" {
		for i, r := range m.rows {
			if r.ID == selectedID {
				m.cursor = i
				break
			}
		}
	}
	if m.cursor >= len(m.rows) {
		m.cursor = max(0, len(m.rows)-1)
	}

	m.prevSeen = true
	m.lastShowAll = m.showAll
	m.lastShowTrashed = m.showTrashed
	m.lastQuery = m.searchQuery

	m.current = m.loadCurrent()
	m.currentVotes = m.loadCurrentVotes()

	// If the refresh landed on a different trace (sticky-cursor
	// failed because the row was archived/deleted, or context
	// changed), reset the detail scroll — the old offset belongs
	// to a different body.
	var newCurrentID string
	if m.current != nil {
		newCurrentID = m.current.ID
	}
	if newCurrentID != prevCurrentID {
		m.detailScroll = 0
	}

	return m
}

func (m model) loadCurrent() *trace.Trace {
	if len(m.rows) == 0 {
		return nil
	}
	row := m.rows[m.cursor]
	var path string
	if row.TrashedAt != "" {
		path = m.cx.TrashFile(row.ID)
	} else {
		path = m.cx.TraceFile(row.ID, row.ArchivedAt != "")
	}
	t, _ := trace.ParseFile(path)
	return t
}

// formatVotes renders the tier_votes count for the detail pane. Zero
// is shown bare ("0") rather than "+0" — the signed format %+d stamps
// a sign on every number including zero, which looks wrong. Positive
// counts get an explicit + so the sign is always legible at a glance.
func formatVotes(n int) string {
	switch {
	case n > 0:
		return fmt.Sprintf("+%d", n)
	case n < 0:
		return fmt.Sprintf("%d", n) // negative values already carry the sign
	default:
		return "0"
	}
}

// loadCurrentVotes returns the tier_votes count for the row under the
// cursor. Renders "0" for an empty list or a row that can't be
// resolved — the detail pane is blanked in those cases anyway, so the
// value isn't displayed.
func (m model) loadCurrentVotes() int {
	if len(m.rows) == 0 {
		return 0
	}
	votes, err := m.cx.TierVotes(m.rows[m.cursor].ID)
	if err != nil {
		return 0
	}
	return votes
}

// castVote applies the session-toggle cycle for a ±1 keypress. target
// is the caller's desired direction (+1 for '+'/'=', -1 for '-').
// The new session intent is:
//
//	prev == target → 0        (same key twice clears the vote)
//	prev == 0      → target   (fresh cast)
//	prev == -target → target  (flip)
//
// The DB delta sent to cx.Vote is (newIntent - prev) and falls in
// {-2, -1, 0, 1, 2}. Zero is a no-op (shouldn't happen with the logic
// above but defended for safety). Any non-zero value hits the
// accumulator and emits one ActionVote event per keypress.
//
// castKind / clearKind are the status-line glyphs shown after the
// cast depending on whether this was a set or a clear.
func (m model) castVote(traceID string, target int, castKind, clearKind string) (model, tea.Cmd) {
	prev := m.sessionVotes[traceID]
	var newIntent int
	if prev == target {
		newIntent = 0
	} else {
		newIntent = target
	}
	delta := newIntent - prev
	if delta == 0 {
		return m, nil
	}
	if err := m.cx.Vote(traceID, delta, cortex.ActorHuman); err != nil {
		m.err = err
		return m, nil
	}
	m.sessionVotes[traceID] = newIntent
	if newIntent == 0 {
		m.status = clearKind + " " + traceID
	} else {
		m.status = castKind + " " + traceID
	}
	return m, m.reload()
}

// paneWidths returns the widths of the list pane and the detail pane
// given the current terminal width. Single source of truth so the
// scroll math stays consistent with what View() actually renders.
func (m model) paneWidths() (listW, detailW int) {
	listW = m.width * listPct / 100
	detailW = m.width - listW - 1 // -1 for the divider column
	return
}

// bodyHeight returns the height of the body region between the header
// and footer rows.
func (m model) bodyHeight() int {
	return m.height - 2
}

// detailMetaLineCount returns how many metadata rows the detail pane
// will render for the current trace (excluding the separator). This
// matches the append order in renderDetail — keep them in sync.
func (m model) detailMetaLineCount() int {
	if m.current == nil {
		return 0
	}
	// id, title, type, created are always present.
	n := 4
	if m.current.Author != "" {
		n++
	}
	if len(m.current.Tags) > 0 {
		// Tags render as chip rows — potentially more than one line
		// when the chips wrap. Query renderTagChips directly with the
		// same width inputs renderDetail uses, so the body budget
		// stays honest even when a trace has a lot of tags.
		_, detailW := m.paneWidths()
		labelW := 10
		metaValW := detailW - labelW - 2
		if metaValW < 4 {
			metaValW = 4
		}
		n += len(renderTagChips(m.current.Tags, labelW, metaValW))
	}
	return n
}

// detailBodyLines returns the body of the current trace, pre-wrapped
// to the detail pane's body width. Returns an empty slice if there is
// no current trace or no body.
func (m model) detailBodyLines() []string {
	if m.current == nil || m.current.Body == "" {
		return nil
	}
	_, detailW := m.paneWidths()
	bodyW := detailW - 4
	if bodyW < 10 {
		bodyW = 10
	}
	var out []string
	for _, raw := range strings.Split(m.current.Body, "\n") {
		for _, wrapped := range wrapLine(raw, bodyW) {
			out = append(out, "  "+wrapped)
		}
	}
	return out
}

// detailVisibleBodyH returns how many body rows fit in the detail pane
// after the metadata block and separator.
func (m model) detailVisibleBodyH() int {
	h := m.bodyHeight() - m.detailMetaLineCount() - 1 // -1 for the separator
	if h < 0 {
		return 0
	}
	return h
}

// detailMaxScroll returns the largest valid detailScroll offset — i.e.
// the offset where the last body line sits on the last visible row.
func (m model) detailMaxScroll() int {
	total := len(m.detailBodyLines())
	visible := m.detailVisibleBodyH()
	if total <= visible {
		return 0
	}
	return total - visible
}

// scrollDetail advances (or rewinds) detailScroll by delta lines,
// clamped to the valid range for the current trace and pane size.
func (m model) scrollDetail(delta int) model {
	m.detailScroll += delta
	if m.detailScroll < 0 {
		m.detailScroll = 0
	}
	if max := m.detailMaxScroll(); m.detailScroll > max {
		m.detailScroll = max
	}
	return m
}

// selectCursor moves the row cursor, reloads the current trace, and
// resets the detail scroll only if the trace actually changed. Used
// by every list-focus navigation handler so we don't drop scroll state
// on no-op moves (e.g. pressing `k` at the top of the list).
func (m model) selectCursor(idx int) model {
	var prevID string
	if m.current != nil {
		prevID = m.current.ID
	}
	m.cursor = idx
	m.current = m.loadCurrent()
	m.currentVotes = m.loadCurrentVotes()
	var newID string
	if m.current != nil {
		newID = m.current.ID
	}
	if newID != prevID {
		m.detailScroll = 0
	}
	return m
}

// newTraceTempFile writes a blank trace template to a temp file.
func newTraceTempFile() (string, error) {
	f, err := os.CreateTemp("", "noema-new-*.md")
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = f.WriteString("---\ntitle: \"\"\ntype: note\nauthor: \"\"\ntags: []\n---\n\n")
	return f.Name(), err
}

// ---- rendering -------------------------------------------------------------

const listPct = 34 // percent of width for the list pane

func (m model) View() string {
	if m.width == 0 {
		return "Loading…\n"
	}

	listW, detailW := m.paneWidths()
	bodyH := m.bodyHeight()

	list := m.renderList(listW, bodyH)
	detail := m.renderDetail(detailW, bodyH)

	// Single-column divider
	divLines := make([]string, bodyH)
	for i := range divLines {
		divLines[i] = styleDivider.Render("│")
	}
	divider := strings.Join(divLines, "\n")

	body := lipgloss.JoinHorizontal(lipgloss.Top, list, divider, detail)

	// Header and footer are single-row strings that only stretch as
	// far as their content goes; right-pad them with surface cells so
	// the entire row renders on the same brand background rather than
	// leaking the terminal default at the right edge.
	header := padLineToWidth(m.renderHeader(), m.width)
	footer := padLineToWidth(m.renderFooter(), m.width)

	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m model) renderHeader() string {
	left := styleHeader.Render("Noema") + styleHeaderAccent.Render(".") + styleDim.Render("  "+m.cx.Name)
	if m.searchQuery != "" {
		left += styleDim.Render(`  search:"` + m.searchQuery + `"`)
	}
	switch {
	case m.showTrashed:
		left += styleDim.Render("  [trash]")
	case m.showAll:
		left += styleDim.Render("  [all]")
	}
	if m.follow {
		left += styleLive.Render("  ● live")
	}
	right := styleDim.Render(fmt.Sprintf("%d traces", len(m.rows)))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	// Gap spacer is painted with the surface background so the raw
	// space characters don't leak the terminal default through the
	// middle of the header row.
	return left + styleSurface.Render(strings.Repeat(" ", gap)) + right
}

func (m model) renderList(width, height int) string {
	if len(m.rows) == 0 {
		empty := styleDim.Render("  No traces.")
		return padBlock(empty, width, height)
	}

	// Scroll: keep cursor visible
	start := 0
	if m.cursor >= height {
		start = m.cursor - height + 1
	}
	end := min(start+height, len(m.rows))

	var sb strings.Builder
	for i := start; i < end; i++ {
		r := m.rows[i]

		date := r.CreatedAt
		if len(date) > 10 {
			date = date[:10]
		}
		badge := "[" + r.Type + "]"
		// badge is at most 14 chars (longest type: "observation"=11 + 2 = 13)
		badgeW := 14
		dateW := 10
		// When the detail pane has focus, the list acts as a dim
		// backdrop — pass that through so the tier glyph skips its
		// heat-chart ANSI (whose embedded reset would otherwise
		// terminate the outer dim style mid-line).
		dim := m.focus == focusDetail
		tierGlyph := renderTierGlyph(r.Tier, dim)
		// 1 cursor + 1 space + tier(1) + 1 space + title + 1 space +
		// badge(14) + 1 space + date(10)
		titleW := width - 2 - 2 - badgeW - 1 - dateW - 1
		if titleW < 4 {
			titleW = 4
		}

		title := r.Title
		switch {
		case r.TrashedAt != "":
			title = "~" + title
		case r.ArchivedAt != "":
			title = "~" + title
		}
		title = truncRune(title, titleW)

		// Build plain-text line with rune-padding for alignment.
		// Cursor glyph is "▸" when the list pane owns focus, "·" when the
		// detail pane owns it — visible feedback that arrow keys will
		// drive the other side.
		cursor := " "
		if i == m.cursor {
			if m.focus == focusList {
				cursor = "▸"
			} else {
				cursor = "·"
			}
		}
		line := fmt.Sprintf("%s %s %-*s %-*s %s",
			cursor,
			tierGlyph,
			titleW, padRunes(title, titleW),
			badgeW, badge,
			date,
		)

		// dim was computed above (alongside tierGlyph) so the tier
		// glyph could opt out of its heat-chart ANSI when the pane
		// is a backdrop; now it gates the row styles too. The entire
		// pane fades like a modal backdrop, not just the selected
		// row — the cursor row still sits one brightness step above
		// the rest so the selection is findable at a glance when
		// tabbing back.
		switch {
		case i == m.cursor:
			if dim {
				sb.WriteString(styleSelectedDim.Width(width).Render(line))
			} else {
				sb.WriteString(styleSelected.Width(width).Render(line))
			}
		case m.newRowTTL[r.ID] > 0:
			if dim {
				sb.WriteString(styleNewRowDim.Width(width).Render(line))
			} else {
				sb.WriteString(styleNewRow.Width(width).Render(line))
			}
		default:
			if dim {
				sb.WriteString(styleRowDim.Width(width).Render(line))
			} else {
				sb.WriteString(styleSurface.Width(width).Render(line))
			}
		}
		if i < end-1 {
			sb.WriteByte('\n')
		}
	}

	// Pad to height
	rendered := sb.String()
	lines := strings.Count(rendered, "\n") + 1
	for lines < height {
		rendered += "\n" + styleSurface.Width(width).Render("")
		lines++
	}
	return rendered
}

func (m model) renderDetail(width, height int) string {
	if m.current == nil {
		msg := "  Select a trace to preview."
		return padBlock(styleDim.Render(msg), width, height)
	}

	t := m.current

	labelW := 10 // "  created:" is 10 chars

	// Every metadata value gets truncated to fit the pane. Without
	// this, lipgloss soft-wraps an overlong id/author/tags row into
	// 2 visible rows, the internal line count underestimates the
	// true render height, and the whole UI scrolls up by one row.
	metaValW := width - labelW - 2
	if metaValW < 4 {
		metaValW = 4
	}
	metaLine := func(label, val string) string {
		l := styleLabel.Render(fmt.Sprintf("  %-*s", labelW, label+":"))
		v := styleValue.Render(truncRune(val, metaValW))
		return l + v
	}

	// chipLine renders a metadata row where the value is a single chip
	// (e.g. the type field). The label column is rendered plain so the
	// chip lines up with wrapped tag chips below.
	chipLine := func(label, value string) string {
		l := styleLabel.Render(fmt.Sprintf("  %-*s", labelW, label+":"))
		chip := styleChip.Render(chipContent(value))
		return l + chip
	}

	var lines []string
	lines = append(lines, metaLine("id", t.ID))
	lines = append(lines, metaLine("title", t.Title))
	lines = append(lines, chipLine("type", t.Type))
	// Tier + current vote count. Surfaced together so a user casting a
	// +/- vote can see both the tier they're influencing and the count
	// they're incrementing. Tier defaults to "short" when the field is
	// omitted from frontmatter (matches DB default).
	tier := t.Tier
	if tier == "" {
		tier = "short"
	}
	lines = append(lines, metaLine("tier", fmt.Sprintf("%s  (votes: %s)", tier, formatVotes(m.currentVotes))))
	if t.Author != "" {
		lines = append(lines, metaLine("author", t.Author))
	}
	if len(t.Tags) > 0 {
		lines = append(lines, renderTagChips(t.Tags, labelW, metaValW)...)
	}
	created := t.Created
	if len(created) > 10 {
		created = created[:10]
	}
	lines = append(lines, metaLine("created", created))

	// Pre-wrap the body so we can scroll line-by-line and compute an
	// accurate scroll indicator.
	bodyLines := m.detailBodyLines()
	totalBody := len(bodyLines)

	// Visible body region sits below the metadata + separator.
	visibleBodyH := height - len(lines) - 1 // -1 for the separator line
	if visibleBodyH < 0 {
		visibleBodyH = 0
	}

	// Clamp the stored scroll offset for this render. The clamp in
	// scrollDetail handles the common case; this catches the edge
	// where width changed (and therefore the wrap width, and therefore
	// total body line count) without a scroll key being pressed.
	scroll := m.detailScroll
	maxScroll := 0
	if totalBody > visibleBodyH {
		maxScroll = totalBody - visibleBodyH
	}
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}

	// Separator line — decorated with a scroll indicator on the right
	// when there's more body than fits, so users always know whether
	// there's content above/below regardless of focus.
	sepRule := width - 4
	if sepRule < 1 {
		sepRule = 1
	}
	if totalBody > visibleBodyH {
		// Glyph shows scroll direction: ▴ above, ▾ below, ▴▾ both.
		var glyph string
		switch {
		case scroll > 0 && scroll < maxScroll:
			glyph = "▴▾"
		case scroll > 0:
			glyph = "▴"
		default:
			glyph = "▾"
		}
		upper := scroll + visibleBodyH
		if upper > totalBody {
			upper = totalBody
		}
		indicator := fmt.Sprintf("%s %d/%d", glyph, upper, totalBody)
		indW := lipgloss.Width(indicator)
		if sepRule > indW+2 {
			dashW := sepRule - indW - 1
			lines = append(lines,
				surfacePad(2)+styleDivider.Render(strings.Repeat("─", dashW))+surfacePad(1)+styleDim.Render(indicator))
		} else {
			lines = append(lines, styleDivider.Render("  "+strings.Repeat("─", sepRule)))
		}
	} else {
		lines = append(lines, styleDivider.Render("  "+strings.Repeat("─", sepRule)))
	}

	// Body slice with scroll offset applied.
	switch {
	case totalBody == 0:
		lines = append(lines, styleDim.Render("  (no body)"))
	default:
		end := scroll + visibleBodyH
		if end > totalBody {
			end = totalBody
		}
		lines = append(lines, bodyLines[scroll:end]...)
	}

	// Clip to height as a belt-and-braces safety net.
	if len(lines) > height {
		lines = lines[:height]
	}

	content := strings.Join(lines, "\n")
	return styleSurface.Width(width).Height(height).Render(content)
}

func (m model) renderFooter() string {
	switch m.state {
	case stateSearch:
		return styleSurface.Render(" /") + m.search.View()

	case stateConfirm:
		prompt := fmt.Sprintf("  %s %q? [y/N] ", m.confirm.action, m.confirm.id)
		return styleError.Render(prompt)

	default:
		if m.err != nil {
			return styleError.Render("  " + m.err.Error())
		}
		if m.status != "" {
			return styleStatus.Render("  " + m.status)
		}
		var hint string
		switch {
		case m.focus == focusDetail:
			// Detail-pane focus: arrow keys scroll the body, everything
			// else is either reach-through (quit) or tab back to list.
			hint = "j/k:scroll  g/G:top/bot  PgUp/PgDn:half  ←/tab:list  esc:list  q:quit"
		case m.showTrashed:
			hint = "j/k:nav  r:recover  D:purge  t:back  /:search  →/tab:body  f:live  R:refresh  q:quit"
		default:
			hint = "j/k:nav  n:new  e:edit  +/-:vote  d:archive  u:unarchive  D:trash  t:trash-view  a:all  /:search  →/tab:body  f:live  R:refresh  q:quit"
		}
		if m.focus == focusList && m.searchQuery != "" {
			hint = "esc:clear  " + hint
		}
		styled := styleFooter.Render(hint)
		// Right-align the hint when the detail pane owns focus — a
		// secondary visual cue (on top of the pane-dim backdrop and
		// cursor glyph swap) that attention has moved to the right.
		if m.focus == focusDetail {
			pad := m.width - lipgloss.Width(styled) - 2
			if pad < 0 {
				pad = 0
			}
			return surfacePad(pad) + styled + surfacePad(2)
		}
		return surfacePad(2) + styled
	}
}

// surfacePad returns n space characters painted with the active
// surface background. Used anywhere raw spaces would otherwise leak
// the terminal default background through the brand surface —
// header/footer gaps, pane-fill padding, alignment indents.
func surfacePad(n int) string {
	if n <= 0 {
		return ""
	}
	return styleSurface.Render(strings.Repeat(" ", n))
}

// padLineToWidth right-pads an already-rendered line with surface
// background cells so its visual width equals `width`. Lines that
// already meet or exceed the target width are returned unchanged.
func padLineToWidth(line string, width int) string {
	cur := lipgloss.Width(line)
	if cur >= width {
		return line
	}
	return line + surfacePad(width-cur)
}

// chipContent returns the inner text of a chip — prefixed with a
// hash so the glyph reinforces the "this is a tag/label" meaning
// even before the inverted-bg color registers. `type` chips also
// share this prefix for visual consistency across the metadata
// block.
func chipContent(s string) string {
	return "#" + s
}

// renderTagChips returns one or more pre-rendered metadata lines for
// the tags field, wrapping chips across rows when the total width
// exceeds the pane's value column. Wrapped rows are indented to sit
// under the first chip (so the "tags:" label only appears once).
//
// The chips themselves are rendered with styleChip (inverted brand
// palette). Each chip consumes `chipCellWidth` cells: `#tag` plus one
// cell of padding on each side. A single trailing space between chips
// acts as the gap.
func renderTagChips(tags []string, labelW, valueW int) []string {
	if len(tags) == 0 {
		return nil
	}
	// Label column for the first row; subsequent rows use spaces so
	// wrapped chips line up under the first chip visually.
	head := styleLabel.Render(fmt.Sprintf("  %-*s", labelW, "tags:"))
	blank := styleLabel.Render(strings.Repeat(" ", labelW+2))

	// Gap between adjacent chips on the same row.
	const chipGap = " "
	gapW := lipgloss.Width(chipGap)

	var rows []string
	var current strings.Builder
	currentW := 0 // visual width of chips in `current`, excluding the leading label

	flush := func(first bool) {
		var prefix string
		if first {
			prefix = head
		} else {
			prefix = blank
		}
		rows = append(rows, prefix+current.String())
		current.Reset()
		currentW = 0
	}

	first := true
	for _, tag := range tags {
		chip := styleChip.Render(chipContent(tag))
		chipW := lipgloss.Width(chip)
		// Project the row width as if we appended this chip (plus the
		// gap separator if there's already something on the row). If
		// that overflows the value column, flush the current row first
		// and start a new one with this chip alone.
		//
		// Chips wider than the whole value column still get rendered
		// in full — truncation would lie about the tag content, and
		// the pane scroll math budgets by row count, not rendered
		// width, so an overflow row is harmless.
		projected := currentW + chipW
		if currentW > 0 {
			projected += gapW
		}
		if currentW > 0 && projected > valueW {
			flush(first)
			first = false
		}
		if currentW > 0 {
			current.WriteString(chipGap)
			currentW += gapW
		}
		current.WriteString(chip)
		currentW += chipW
	}
	if currentW > 0 {
		flush(first)
	}
	return rows
}

// ---- helpers ---------------------------------------------------------------

// truncRune truncates s to at most max runes, appending '…' if truncated.
func truncRune(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// padRunes right-pads s with spaces to exactly n runes.
func padRunes(s string, n int) string {
	r := []rune(s)
	if len(r) >= n {
		return string(r[:n])
	}
	return s + strings.Repeat(" ", n-len(r))
}

// padBlock pads a rendered block to width × height with empty lines
// painted in the active surface background. Without the palette paint
// the empty filler rows would leak the terminal default background
// through, producing a visible gray-shade mismatch against the styled
// rows above them.
func padBlock(content string, width, height int) string {
	lines := strings.Split(content, "\n")
	for len(lines) < height {
		lines = append(lines, styleSurface.Width(width).Render(""))
	}
	return strings.Join(lines[:height], "\n")
}

// wrapLine word-wraps a single line to width runes.
func wrapLine(line string, width int) []string {
	if width <= 0 {
		return []string{line}
	}
	runes := []rune(line)
	if len(runes) <= width {
		return []string{line}
	}
	var out []string
	for len(runes) > width {
		breakAt := width
		for breakAt > 0 && runes[breakAt-1] != ' ' {
			breakAt--
		}
		if breakAt == 0 {
			breakAt = width
		}
		out = append(out, string(runes[:breakAt]))
		runes = runes[breakAt:]
		for len(runes) > 0 && runes[0] == ' ' {
			runes = runes[1:]
		}
	}
	if len(runes) > 0 {
		out = append(out, string(runes))
	}
	return out
}

// ---- entry point -----------------------------------------------------------

// Run starts the full-screen TUI against the given Cortex using the
// supplied theme ("auto", "dark", or "light"). Callers resolve the
// theme string from their own flag/env/config chain and pass it in;
// this function is the single place that actually applies it.
func Run(cx *cortex.Cortex, theme string) error {
	loadPalette(theme)
	p := tea.NewProgram(
		initialModel(cx),
		tea.WithAltScreen(),
	)
	_, err := p.Run()
	return err
}
