package tui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// ansiSGR matches the SGR escape sequences lipgloss emits so tests
// that need to assert on visible (post-style) text structure can
// strip them. Using a narrow SGR-only pattern — not a general ESC
// matcher — so malformed sequences still show up in diagnostics.
var ansiSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiSGR.ReplaceAllString(s, "") }

// TestMain forces lipgloss into a known color profile for the whole
// test binary. Without this, lipgloss detects the go-test environment
// as non-TTY and strips every escape sequence, so style-assertion
// tests can't distinguish `styleSelected` from `styleSelectedDim`.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	os.Exit(m.Run())
}

// ---- test helpers ----------------------------------------------------------

func setupCortex(t *testing.T) *cortex.Cortex {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("test", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("test", filepath.Join(dir, "test"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx
}

func addTrace(t *testing.T, cx *cortex.Cortex, title, traceType string) *trace.Trace {
	t.Helper()
	tr := trace.New(title, traceType, "", nil, "body of "+title)
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return tr
}

func loadedModel(t *testing.T, cx *cortex.Cortex) model {
	t.Helper()
	m := initialModel(cx)
	m.width = 120
	m.height = 30

	msg := loadRows(cx, "", false, false, nil)()
	result, _ := m.Update(msg)
	return result.(model)
}

// ---- helper function tests -------------------------------------------------

func TestTruncRune(t *testing.T) {
	cases := []struct {
		s    string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel…"},
		{"hello", 1, "…"},
		{"", 5, ""},
		{"héllo", 4, "hél…"},
		{"日本語テスト", 4, "日本語…"},
	}
	for _, c := range cases {
		got := truncRune(c.s, c.max)
		if got != c.want {
			t.Errorf("truncRune(%q, %d) = %q, want %q", c.s, c.max, got, c.want)
		}
	}
}

func TestPadRunes(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"hi", 5, "hi   "},
		{"hello", 5, "hello"},
		{"toolong", 4, "tool"},
		{"", 3, "   "},
		{"日本語", 5, "日本語  "},
	}
	for _, c := range cases {
		got := padRunes(c.s, c.n)
		if got != c.want {
			t.Errorf("padRunes(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
}

func TestWrapLine(t *testing.T) {
	cases := []struct {
		line  string
		width int
		want  []string
	}{
		{"short", 20, []string{"short"}},
		{"", 20, []string{""}},
		{"one two three four five", 10, []string{"one two ", "three ", "four five"}},
		{"nospacesinhere", 8, []string{"nospaces", "inhere"}},
		{"a b", 3, []string{"a b"}},
		{"12345678 abcd", 8, []string{"12345678", "abcd"}},
	}
	for _, c := range cases {
		got := wrapLine(c.line, c.width)
		if len(got) != len(c.want) {
			t.Errorf("wrapLine(%q, %d) = %v, want %v", c.line, c.width, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("wrapLine(%q, %d)[%d] = %q, want %q", c.line, c.width, i, got[i], c.want[i])
			}
		}
	}
}

// ---- newTraceTempFile ------------------------------------------------------

func TestNewTraceTempFile(t *testing.T) {
	path, err := newTraceTempFile()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading temp file: %v", err)
	}
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		t.Errorf("expected YAML frontmatter, got: %q", content[:min(40, len(content))])
	}
	if !strings.Contains(content, "type: note") {
		t.Errorf("expected default type=note in template")
	}
}

// ---- model navigation ------------------------------------------------------

func TestNavigation_JK(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "First", "fact")
	addTrace(t, cx, "Second", "note")
	addTrace(t, cx, "Third", "decision")

	m := loadedModel(t, cx)
	if m.cursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.cursor)
	}

	// j moves cursor down
	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m2.cursor != 1 {
		t.Errorf("after j: cursor = %d, want 1", m2.cursor)
	}

	// k moves cursor back up
	m3, _ := m2.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m3.cursor != 0 {
		t.Errorf("after k: cursor = %d, want 0", m3.cursor)
	}

	// k at top stays at 0
	m4, _ := m3.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m4.cursor != 0 {
		t.Errorf("k at top: cursor = %d, want 0", m4.cursor)
	}
}

func TestNavigation_GG(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "A", "note")
	addTrace(t, cx, "B", "note")
	addTrace(t, cx, "C", "note")

	m := loadedModel(t, cx)

	// G jumps to last
	mEnd, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	if mEnd.cursor != len(mEnd.rows)-1 {
		t.Errorf("G: cursor = %d, want %d", mEnd.cursor, len(mEnd.rows)-1)
	}

	// g jumps to first
	mTop, _ := mEnd.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if mTop.cursor != 0 {
		t.Errorf("g: cursor = %d, want 0", mTop.cursor)
	}
}

// ---- search state machine --------------------------------------------------

func TestSearch_EnterAndApply(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)

	// / enters search mode
	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if m2.state != stateSearch {
		t.Fatalf("state after /: %v, want stateSearch", m2.state)
	}

	// Type a query
	for _, ch := range "hello" {
		m2.search.SetValue(m2.search.Value() + string(ch))
	}

	// Enter applies the search
	m3, cmd := m2.updateSearch(tea.KeyMsg{Type: tea.KeyEnter})
	if m3.state != stateList {
		t.Errorf("state after enter: %v, want stateList", m3.state)
	}
	if m3.searchQuery != "hello" {
		t.Errorf("searchQuery = %q, want %q", m3.searchQuery, "hello")
	}
	if cmd == nil {
		t.Error("expected a load command after search apply")
	}
}

func TestSearch_Escape(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)
	m.searchQuery = "prior"

	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m3, _ := m2.updateSearch(tea.KeyMsg{Type: tea.KeyEsc})

	if m3.state != stateList {
		t.Errorf("state after esc: %v, want stateList", m3.state)
	}
	// search query unchanged on esc
	if m3.searchQuery != "prior" {
		t.Errorf("searchQuery changed on esc: got %q, want %q", m3.searchQuery, "prior")
	}
}

func TestSearch_EscClearsQueryFromList(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)
	m.searchQuery = "something"

	m2, cmd := m.updateList(tea.KeyMsg{Type: tea.KeyEsc})
	if m2.searchQuery != "" {
		t.Errorf("esc from list did not clear searchQuery: got %q", m2.searchQuery)
	}
	if cmd == nil {
		t.Error("expected reload command after clearing search")
	}
}

// ---- confirm state machine -------------------------------------------------

func TestConfirm_Archive_Yes(t *testing.T) {
	cx := setupCortex(t)
	tr := addTrace(t, cx, "To Archive", "note")
	m := loadedModel(t, cx)

	// Press d to initiate archive
	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if m2.state != stateConfirm {
		t.Fatalf("state after d: %v, want stateConfirm", m2.state)
	}
	if m2.confirm.action != "archive" {
		t.Errorf("confirm.action = %q, want archive", m2.confirm.action)
	}

	// Press y to confirm
	m3, cmd := m2.updateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if m3.state != stateList {
		t.Errorf("state after y: %v, want stateList", m3.state)
	}
	if cmd == nil {
		t.Error("expected reload command after archive")
	}

	// Verify the trace is archived in the DB
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get after archive: %v", err)
	}
	if row.ArchivedAt == "" {
		t.Errorf("expected trace to be archived, got empty archived_at")
	}
}

func TestConfirm_Archive_No(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "Stay", "fact")
	m := loadedModel(t, cx)

	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m3, _ := m2.updateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})

	if m3.state != stateList {
		t.Errorf("state after n: %v, want stateList", m3.state)
	}
	// confirm cleared
	if m3.confirm.id != "" {
		t.Errorf("confirm.id not cleared: %q", m3.confirm.id)
	}
}

func TestConfirm_Trash_Yes(t *testing.T) {
	cx := setupCortex(t)
	tr := addTrace(t, cx, "Trash Me", "note")
	m := loadedModel(t, cx)

	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if m2.state != stateConfirm {
		t.Fatalf("state after D: %v, want stateConfirm", m2.state)
	}
	if m2.confirm.action != "trash" {
		t.Errorf("confirm.action = %q, want trash", m2.confirm.action)
	}

	m3, _ := m2.updateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if m3.state != stateList {
		t.Errorf("state after y: %v, want stateList", m3.state)
	}

	// Trace should exist in DB but be trashed
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get after trash: %v", err)
	}
	if row.TrashedAt == "" {
		t.Error("expected trace to be trashed")
	}
}

func TestConfirm_Purge_Yes(t *testing.T) {
	cx := setupCortex(t)
	tr := addTrace(t, cx, "Purge Me", "note")
	if err := cx.Trash(tr.ID); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	// Load model in trash view
	m := loadedModel(t, cx)
	m.showTrashed = true
	msg := loadRows(cx, "", false, true, nil)()
	result, _ := m.Update(msg)
	m = result.(model)

	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if m2.confirm.action != "purge" {
		t.Errorf("confirm.action = %q, want purge", m2.confirm.action)
	}

	m3, _ := m2.updateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if m3.state != stateList {
		t.Errorf("state after y: %v, want stateList", m3.state)
	}

	_, err := cx.Get(tr.ID)
	if err == nil {
		t.Error("expected error getting purged trace, got nil")
	}
}

func TestRecover(t *testing.T) {
	cx := setupCortex(t)
	tr := addTrace(t, cx, "Recover Me", "note")
	if err := cx.Trash(tr.ID); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	m := loadedModel(t, cx)
	m.showTrashed = true
	msg := loadRows(cx, "", false, true, nil)()
	result, _ := m.Update(msg)
	m = result.(model)

	m2, cmd := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if m2.err != nil {
		t.Fatalf("recover error: %v", m2.err)
	}
	if cmd == nil {
		t.Error("expected reload command after recover")
	}

	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get after recover: %v", err)
	}
	if row.TrashedAt != "" {
		t.Error("expected TrashedAt to be cleared after recover")
	}
}

// ---- editorDone (new trace) ------------------------------------------------

func TestHandleEditorDone_NewTrace(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)

	// Write a valid trace template to a temp file
	f, err := os.CreateTemp("", "noema-test-*.md")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("---\ntitle: \"Test Trace\"\ntype: decision\nauthor: tester\ntags: [go]\n---\n\nsome body\n")
	f.Close()
	// Don't defer Remove — handleEditorDone removes it

	m2, cmd := m.handleEditorDone(editorDoneMsg{
		isNew: true,
		path:  f.Name(),
	})
	if m2.err != nil {
		t.Fatalf("unexpected error: %v", m2.err)
	}
	if cmd == nil {
		t.Error("expected reload command after add")
	}
	if !strings.HasPrefix(m2.status, "Added ") {
		t.Errorf("status = %q, want 'Added ...'", m2.status)
	}

	// File should be removed
	if _, err := os.Stat(f.Name()); !os.IsNotExist(err) {
		t.Error("temp file was not removed")
	}

	// Trace should exist in cortex
	rows, err := cx.List(cortex.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(rows))
	}
	if rows[0].Title != "Test Trace" {
		t.Errorf("title = %q, want 'Test Trace'", rows[0].Title)
	}
}

func TestHandleEditorDone_NewTrace_EmptyTitle(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)

	f, err := os.CreateTemp("", "noema-test-*.md")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("---\ntitle: \"\"\ntype: note\nauthor: \"\"\ntags: []\n---\n\n")
	f.Close()

	m2, cmd := m.handleEditorDone(editorDoneMsg{isNew: true, path: f.Name()})
	if m2.err != nil {
		t.Fatalf("unexpected error: %v", m2.err)
	}
	// Empty title = cancelled, no reload command needed
	_ = cmd

	// No trace should be added
	rows, _ := cx.List(cortex.ListOptions{})
	if len(rows) != 0 {
		t.Errorf("expected 0 traces after empty-title cancel, got %d", len(rows))
	}
}

func TestHandleEditorDone_EditTrace(t *testing.T) {
	cx := setupCortex(t)
	tr := addTrace(t, cx, "Original", "note")
	m := loadedModel(t, cx)

	// Simulate editing: write updated content to the trace file
	path := cx.TraceFile(tr.ID, false)
	updated := "---\nid: " + tr.ID + "\ntitle: Updated Title\ntype: fact\nauthor: editor\ntags: [edited]\ncreated: " + tr.Created + "\nupdated: " + tr.Updated + "\n---\n\nupdated body\n"
	if err := os.WriteFile(path, []byte(updated), 0o640); err != nil {
		t.Fatalf("writing updated trace: %v", err)
	}

	m2, cmd := m.handleEditorDone(editorDoneMsg{isNew: false, id: tr.ID})
	if m2.err != nil {
		t.Fatalf("unexpected error: %v", m2.err)
	}
	if cmd == nil {
		t.Error("expected reload command after edit")
	}
	if !strings.HasPrefix(m2.status, "Updated ") {
		t.Errorf("status = %q, want 'Updated ...'", m2.status)
	}

	// DB should reflect updated fields
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Title != "Updated Title" {
		t.Errorf("title = %q, want 'Updated Title'", row.Title)
	}
	if row.Type != "fact" {
		t.Errorf("type = %q, want 'fact'", row.Type)
	}
}

// ---- rendering smoke tests -------------------------------------------------

func TestView_EmptyCortex(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)

	view := m.View()
	if !strings.Contains(view, "Noema") {
		t.Error("expected 'Noema' in view")
	}
	if !strings.Contains(view, "0 traces") {
		t.Errorf("expected '0 traces' in view, got:\n%s", view)
	}
}

func TestView_WithTraces(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "First trace", "fact")
	addTrace(t, cx, "Second trace", "decision")
	m := loadedModel(t, cx)

	view := m.View()
	if !strings.Contains(view, "2 traces") {
		t.Errorf("expected '2 traces' in view, got:\n%s", view)
	}
	// At least one trace title should appear
	if !strings.Contains(view, "trace") {
		t.Error("expected trace titles in view")
	}
}

func TestView_SearchFooter(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)
	m.state = stateSearch
	m.search.SetValue("hello")

	view := m.View()
	if !strings.Contains(view, "hello") {
		t.Errorf("expected search query in footer, view:\n%s", view)
	}
}

func TestView_ConfirmFooter(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "Some trace", "note")
	m := loadedModel(t, cx)
	m.state = stateConfirm
	m.confirm = confirmAction{action: "archive", id: "some-id"}

	view := m.View()
	if !strings.Contains(view, "archive") {
		t.Errorf("expected 'archive' in confirm footer, view:\n%s", view)
	}
}

// ---- follow mode (live auto-refresh) ---------------------------------------

func TestFollow_ToggleStartsAndStopsTickChain(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)

	if m.follow {
		t.Fatalf("follow should default to false")
	}

	// Press f to enable follow mode.
	m2, cmd := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if !m2.follow {
		t.Errorf("after f: follow = false, want true")
	}
	if m2.followGen != 1 {
		t.Errorf("after first f: followGen = %d, want 1", m2.followGen)
	}
	if cmd == nil {
		t.Fatal("enabling follow should return a tickCmd")
	}

	// Press f again to disable. No new command should be scheduled.
	m3, cmd := m2.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if m3.follow {
		t.Errorf("after second f: follow = true, want false")
	}
	if cmd != nil {
		t.Error("disabling follow should not return a command")
	}

	// Re-enable: generation must bump so any stale tick from the first
	// session would be discarded.
	m4, _ := m3.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if m4.followGen != 2 {
		t.Errorf("after re-enable: followGen = %d, want 2", m4.followGen)
	}
}

func TestFollow_TickMsgRefreshesWhenOn(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "only", "note")
	m := loadedModel(t, cx)
	m.follow = true
	m.followGen = 1

	result, cmd := m.Update(tickMsg{gen: 1})
	m2 := result.(model)

	if !m2.follow {
		t.Error("tick should not clear follow")
	}
	if cmd == nil {
		t.Fatal("active tick should return a batch (next tick + loadRows)")
	}
}

func TestFollow_StaleTickIsDiscarded(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)
	m.follow = true
	m.followGen = 5

	// A tickMsg from generation 3 (e.g. left over from a prior session)
	// must be ignored — no reschedule, no refresh.
	_, cmd := m.Update(tickMsg{gen: 3})
	if cmd != nil {
		t.Error("stale tick should not schedule anything")
	}
}

func TestFollow_TickWhileFollowOffIsDiscarded(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)
	// follow is false (default)

	_, cmd := m.Update(tickMsg{gen: 0})
	if cmd != nil {
		t.Error("tick while follow is off should not schedule anything")
	}
}

func TestFollow_TickSuppressesRefreshInSearchState(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)
	m.follow = true
	m.followGen = 1
	m.state = stateSearch

	// In search state the refresh is suppressed but the tick chain
	// must keep spinning so polling resumes once search closes.
	_, cmd := m.Update(tickMsg{gen: 1})
	if cmd == nil {
		t.Fatal("tick should still reschedule even when refresh is suppressed")
	}
}

func TestFollow_ManualRefreshKey(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "one", "note")
	m := loadedModel(t, cx)

	// Press R to trigger a manual reload (works regardless of follow state).
	_, cmd := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	if cmd == nil {
		t.Error("R should return a load command")
	}
}

func TestFollow_HeaderShowsLiveBadge(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)

	// The distinctive badge is "● live" — plain "live" is also in the
	// footer hint ("f:live  R:refresh") so we can't use that as the
	// needle without false-positives.
	const badge = "● live"

	if strings.Contains(m.View(), badge) {
		t.Error("live badge should not appear when follow is off")
	}

	m.follow = true
	if !strings.Contains(m.View(), badge) {
		t.Errorf("live badge should appear when follow is on, view:\n%s", m.View())
	}
}

// ---- sticky cursor ---------------------------------------------------------

func TestStickyCursor_PreservesSelectionWhenNewRowArrivesAtTop(t *testing.T) {
	cx := setupCortex(t)
	a := addTrace(t, cx, "alpha", "note")
	b := addTrace(t, cx, "beta", "note")
	m := loadedModel(t, cx)

	// Traces are ordered created_at DESC, so "beta" is at index 0 and
	// "alpha" is at index 1. Put the cursor on alpha.
	var alphaIdx int
	for i, r := range m.rows {
		if r.ID == a.ID {
			alphaIdx = i
		}
	}
	m.cursor = alphaIdx

	// Simulate a new trace arriving at the top (what an agent writing
	// in the background would produce).
	c := addTrace(t, cx, "gamma", "note")
	_ = c
	_ = b

	msg := loadRows(cx, "", false, false, nil)()
	result, _ := m.Update(msg)
	m2 := result.(model)

	if len(m2.rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(m2.rows))
	}
	// Cursor should still be pointing at alpha, even though its index
	// shifted down by one when gamma was inserted at the top.
	if m2.rows[m2.cursor].ID != a.ID {
		t.Errorf("cursor drifted: now on %q, want alpha (%q)",
			m2.rows[m2.cursor].ID, a.ID)
	}
}

func TestStickyCursor_ClampsWhenSelectedRowDisappears(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "keep-1", "note")
	gone := addTrace(t, cx, "gone", "note")
	addTrace(t, cx, "keep-2", "note")
	m := loadedModel(t, cx)

	for i, r := range m.rows {
		if r.ID == gone.ID {
			m.cursor = i
		}
	}

	// Archive the selected trace so it leaves the active view.
	if err := cx.Archive(gone.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	msg := loadRows(cx, "", false, false, nil)()
	result, _ := m.Update(msg)
	m2 := result.(model)

	if len(m2.rows) != 2 {
		t.Fatalf("expected 2 rows after archive, got %d", len(m2.rows))
	}
	if m2.cursor < 0 || m2.cursor >= len(m2.rows) {
		t.Errorf("cursor out of bounds after sticky fallback: %d", m2.cursor)
	}
}

// ---- new-row highlight -----------------------------------------------------

func TestNewRowHighlight_MarksArrivalsOnRefresh(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "first", "note")
	m := loadedModel(t, cx)

	// Initial load should not highlight anything — there's no
	// "previous" snapshot to diff against.
	if len(m.newRowTTL) != 0 {
		t.Errorf("initial load should not mark any rows as new, got %d",
			len(m.newRowTTL))
	}

	// Now add a new trace and refresh. The new trace ID should be
	// flagged, but the pre-existing one should not.
	second := addTrace(t, cx, "second", "note")

	msg := loadRows(cx, "", false, false, nil)()
	result, _ := m.Update(msg)
	m2 := result.(model)

	if m2.newRowTTL[second.ID] != newRowHighlightTicks {
		t.Errorf("new row TTL = %d, want %d",
			m2.newRowTTL[second.ID], newRowHighlightTicks)
	}
	// Sanity: only the new row is flagged.
	if len(m2.newRowTTL) != 1 {
		t.Errorf("expected 1 highlighted row, got %d", len(m2.newRowTTL))
	}
}

func TestNewRowHighlight_FadesAcrossConsecutiveRefreshes(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "first", "note")
	m := loadedModel(t, cx)

	// Add a new trace — refresh #1 flags it with a fresh TTL.
	fresh := addTrace(t, cx, "fresh", "note")
	msg := loadRows(cx, "", false, false, nil)()
	result, _ := m.Update(msg)
	m1 := result.(model)

	if m1.newRowTTL[fresh.ID] != newRowHighlightTicks {
		t.Fatalf("after first refresh: ttl = %d, want %d",
			m1.newRowTTL[fresh.ID], newRowHighlightTicks)
	}

	// Refresh #2 — no new arrivals, existing TTL should decrement.
	msg = loadRows(cx, "", false, false, nil)()
	result, _ = m1.Update(msg)
	m2 := result.(model)

	if m2.newRowTTL[fresh.ID] != newRowHighlightTicks-1 {
		t.Errorf("after second refresh: ttl = %d, want %d",
			m2.newRowTTL[fresh.ID], newRowHighlightTicks-1)
	}

	// Refresh #3 — TTL expires and the row is removed from the map.
	msg = loadRows(cx, "", false, false, nil)()
	result, _ = m2.Update(msg)
	m3 := result.(model)

	if _, still := m3.newRowTTL[fresh.ID]; still {
		t.Errorf("after third refresh: row still highlighted, want expired")
	}
}

func TestNewRowHighlight_ContextChangeWipesHighlights(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "active", "note")
	m := loadedModel(t, cx)

	// Create a highlighted state by adding a trace and refreshing.
	added := addTrace(t, cx, "added", "note")
	msg := loadRows(cx, "", false, false, nil)()
	result, _ := m.Update(msg)
	m1 := result.(model)

	if _, ok := m1.newRowTTL[added.ID]; !ok {
		t.Fatalf("setup failed: expected highlight on added trace")
	}

	// User toggles `a` (show all) — a context change. The next
	// rowsLoadedMsg should wipe the old highlight map, not pretend the
	// toggled-in rows are "new arrivals".
	m1.showAll = true
	msg = loadRows(cx, "", true, false, nil)()
	result, _ = m1.Update(msg)
	m2 := result.(model)

	if len(m2.newRowTTL) != 0 {
		t.Errorf("context change should wipe highlights, got %d entries",
			len(m2.newRowTTL))
	}
}

// TestHeader_BrandWordmark verifies the TUI header renders "Noema."
// split into two styled runs: "Noema" in brand cream and "." in brand
// red — matching the website wordmark. Locks in the CompleteColor
// fallbacks (223 for cream, 161 for red) so a future style refactor
// can't regress to termenv's broken auto-downconversion, which maps
// #e10032 to color 232 (near-black).
func TestHeader_BrandWordmark(t *testing.T) {
	cx := setupCortex(t)
	m := loadedModel(t, cx)

	header := m.renderHeader()

	// Both the wordmark and the accent period must appear.
	if !strings.Contains(header, "Noema") {
		t.Errorf("header missing 'Noema':\n%q", header)
	}
	if !strings.Contains(header, ".") {
		t.Errorf("header missing brand period '.':\n%q", header)
	}

	// Brand cream resolves to 256-color 223 on the ANSI256 profile
	// pinned by TestMain. Bold + fg 223. The matcher anchors on the
	// fg portion of the SGR sequence so it tolerates a trailing
	// background specifier (added when the dark surface was painted
	// across the whole TUI in v0.4.1).
	if !strings.Contains(header, "\x1b[1;38;5;223") {
		t.Errorf("header missing brand cream SGR (1;38;5;223):\n%q", header)
	}

	// Brand red resolves to 256-color 161 on the ANSI256 profile.
	// Bold + fg 161. The old bug downconverted to 232 (near-black);
	// guard against any regression that reintroduces it.
	if !strings.Contains(header, "\x1b[1;38;5;161") {
		t.Errorf("header missing brand red SGR (1;38;5;161):\n%q", header)
	}
	// Foreground 232 specifically — the broken downconversion shows
	// up as `[1;38;5;232` (period only, bold). Background 232 is fine.
	if strings.Contains(header, "\x1b[1;38;5;232") {
		t.Error("header period downconverted to near-black (232) — " +
			"CompleteColor ANSI256 fallback is broken")
	}
	if strings.Contains(header, "\x1b[38;5;99m") {
		t.Error("header still uses old purple (xterm-256 color 99)")
	}
}

// ---- pane focus + detail scroll --------------------------------------------

// longBody builds a body with n lines so the detail pane definitely
// overflows. Each line is short enough to avoid wrap concerns at width=120.
func longBody(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("line ")
		// left-pad digit so every line has the same byte count and we
		// don't accidentally wrap later.
		sb.WriteString(strings.Repeat("0", 3))
	}
	return sb.String()
}

// addTraceWithBody inserts a trace with a custom body — the default
// addTrace helper uses "body of <title>" which is too short for scroll
// tests.
func addTraceWithBody(t *testing.T, cx *cortex.Cortex, title, body string) *trace.Trace {
	t.Helper()
	tr := trace.New(title, "note", "", nil, body)
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return tr
}

func TestTab_TogglesFocus(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "first", "note")
	m := loadedModel(t, cx)

	if m.focus != focusList {
		t.Fatalf("initial focus = %v, want focusList", m.focus)
	}

	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyTab})
	if m2.focus != focusDetail {
		t.Errorf("after tab: focus = %v, want focusDetail", m2.focus)
	}

	m3, _ := m2.updateList(tea.KeyMsg{Type: tea.KeyTab})
	if m3.focus != focusList {
		t.Errorf("after 2nd tab: focus = %v, want focusList", m3.focus)
	}
}

func TestArrowKeys_SetFocusAbsolutely(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "first", "note")
	m := loadedModel(t, cx)

	// From list focus: right moves to detail.
	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRight})
	if m2.focus != focusDetail {
		t.Errorf("after right from list: focus = %v, want focusDetail", m2.focus)
	}

	// From detail focus: left moves back to list.
	m3, _ := m2.updateList(tea.KeyMsg{Type: tea.KeyLeft})
	if m3.focus != focusList {
		t.Errorf("after left from detail: focus = %v, want focusList", m3.focus)
	}

	// Right is idempotent — pressing it while detail-focused is a no-op.
	m4, _ := m2.updateList(tea.KeyMsg{Type: tea.KeyRight})
	if m4.focus != focusDetail {
		t.Errorf("right on detail-focus: focus = %v, want focusDetail", m4.focus)
	}

	// Left is idempotent too — pressing it while list-focused stays put.
	m5, _ := m.updateList(tea.KeyMsg{Type: tea.KeyLeft})
	if m5.focus != focusList {
		t.Errorf("left on list-focus: focus = %v, want focusList", m5.focus)
	}
}

func TestArrowKeys_DoNotMoveCursor(t *testing.T) {
	// Left/right arrow keys are for pane switching only — they must
	// not advance or rewind the list cursor. Regression guard against
	// accidentally aliasing them to j/k.
	cx := setupCortex(t)
	addTrace(t, cx, "a", "note")
	addTrace(t, cx, "b", "note")
	addTrace(t, cx, "c", "note")
	m := loadedModel(t, cx)
	m.cursor = 1 // start mid-list

	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRight})
	if m2.cursor != 1 {
		t.Errorf("right moved cursor: %d -> %d", 1, m2.cursor)
	}

	m3, _ := m2.updateList(tea.KeyMsg{Type: tea.KeyLeft})
	if m3.cursor != 1 {
		t.Errorf("left moved cursor: %d -> %d", 1, m3.cursor)
	}
}

func TestTab_EscFromDetailReturnsToList(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "first", "note")
	m := loadedModel(t, cx)

	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyTab})
	if m2.focus != focusDetail {
		t.Fatalf("setup: focus = %v, want focusDetail", m2.focus)
	}

	m3, _ := m2.updateList(tea.KeyMsg{Type: tea.KeyEsc})
	if m3.focus != focusList {
		t.Errorf("after esc: focus = %v, want focusList", m3.focus)
	}
}

func TestDetailScroll_JKScrollsBody(t *testing.T) {
	cx := setupCortex(t)
	addTraceWithBody(t, cx, "long", longBody(200))
	m := loadedModel(t, cx)
	m.focus = focusDetail

	if m.detailScroll != 0 {
		t.Fatalf("initial detailScroll = %d, want 0", m.detailScroll)
	}

	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m2.detailScroll != 1 {
		t.Errorf("after j: detailScroll = %d, want 1", m2.detailScroll)
	}
	// List cursor must NOT have moved.
	if m2.cursor != m.cursor {
		t.Errorf("j in detail focus moved list cursor: %d -> %d", m.cursor, m2.cursor)
	}

	m3, _ := m2.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m3.detailScroll != 0 {
		t.Errorf("after k: detailScroll = %d, want 0", m3.detailScroll)
	}
}

func TestDetailScroll_Bounds(t *testing.T) {
	cx := setupCortex(t)
	addTraceWithBody(t, cx, "long", longBody(200))
	m := loadedModel(t, cx)
	m.focus = focusDetail

	// k at top stays at 0.
	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m2.detailScroll != 0 {
		t.Errorf("k at top: detailScroll = %d, want 0", m2.detailScroll)
	}

	// G jumps to bottom = max.
	m3, _ := m2.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	max := m3.detailMaxScroll()
	if max == 0 {
		t.Fatalf("test setup broken: max scroll = 0 — body too short")
	}
	if m3.detailScroll != max {
		t.Errorf("after G: detailScroll = %d, want %d (max)", m3.detailScroll, max)
	}

	// j at bottom stays at max.
	m4, _ := m3.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m4.detailScroll != max {
		t.Errorf("j at bottom: detailScroll = %d, want %d (max)", m4.detailScroll, max)
	}
}

func TestDetailScroll_GJumpsToTopBottom(t *testing.T) {
	cx := setupCortex(t)
	addTraceWithBody(t, cx, "long", longBody(200))
	m := loadedModel(t, cx)
	m.focus = focusDetail

	// G bottom, then g top.
	mEnd, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	if mEnd.detailScroll == 0 {
		t.Fatalf("G in detail focus should advance past 0")
	}

	mTop, _ := mEnd.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if mTop.detailScroll != 0 {
		t.Errorf("g in detail focus: detailScroll = %d, want 0", mTop.detailScroll)
	}

	// And cursor never moved during any of this.
	if mTop.cursor != m.cursor {
		t.Errorf("G/g in detail focus moved list cursor: %d -> %d", m.cursor, mTop.cursor)
	}
}

func TestDetailScroll_HalfPage(t *testing.T) {
	cx := setupCortex(t)
	addTraceWithBody(t, cx, "long", longBody(200))
	m := loadedModel(t, cx)
	m.focus = focusDetail

	step := m.detailVisibleBodyH() / 2
	if step < 1 {
		t.Fatalf("test setup broken: half-page step = %d", step)
	}

	// Ctrl+D scrolls down a half page.
	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyCtrlD})
	if m2.detailScroll != step {
		t.Errorf("after ctrl+d: detailScroll = %d, want %d", m2.detailScroll, step)
	}

	// Ctrl+U scrolls back up a half page.
	m3, _ := m2.updateList(tea.KeyMsg{Type: tea.KeyCtrlU})
	if m3.detailScroll != 0 {
		t.Errorf("after ctrl+u: detailScroll = %d, want 0", m3.detailScroll)
	}

	// PgDown is an alias for ctrl+d.
	m4, _ := m.updateList(tea.KeyMsg{Type: tea.KeyPgDown})
	if m4.detailScroll != step {
		t.Errorf("after pgdown: detailScroll = %d, want %d", m4.detailScroll, step)
	}

	// PgUp is an alias for ctrl+u.
	m5, _ := m4.updateList(tea.KeyMsg{Type: tea.KeyPgUp})
	if m5.detailScroll != 0 {
		t.Errorf("after pgup: detailScroll = %d, want 0", m5.detailScroll)
	}
}

func TestDetailScroll_ResetsOnTraceChange(t *testing.T) {
	cx := setupCortex(t)
	addTraceWithBody(t, cx, "first", longBody(200))
	addTraceWithBody(t, cx, "second", longBody(200))
	m := loadedModel(t, cx)
	m.focus = focusDetail

	// Scroll the first trace.
	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	if m2.detailScroll == 0 {
		t.Fatalf("setup: G should have scrolled first trace")
	}

	// Switch back to list focus and move cursor to second trace.
	m3, _ := m2.updateList(tea.KeyMsg{Type: tea.KeyTab})
	m4, _ := m3.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})

	if m4.detailScroll != 0 {
		t.Errorf("trace change should reset detailScroll, got %d", m4.detailScroll)
	}
}

func TestDetailScroll_PreservedOnTabToggle(t *testing.T) {
	cx := setupCortex(t)
	addTraceWithBody(t, cx, "long", longBody(200))
	m := loadedModel(t, cx)
	m.focus = focusDetail

	// Scroll down 5 lines.
	cur := m
	for i := 0; i < 5; i++ {
		cur, _ = cur.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	}
	if cur.detailScroll != 5 {
		t.Fatalf("setup: detailScroll = %d, want 5", cur.detailScroll)
	}

	// Tab to list, tab back to detail — same trace, scroll should persist.
	back, _ := cur.updateList(tea.KeyMsg{Type: tea.KeyTab})
	forth, _ := back.updateList(tea.KeyMsg{Type: tea.KeyTab})
	if forth.detailScroll != 5 {
		t.Errorf("tab-toggle dropped scroll: got %d, want 5", forth.detailScroll)
	}
}

func TestDetailScroll_NoOpWhenBodyFitsInPane(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "short", "note") // default body is tiny
	m := loadedModel(t, cx)
	m.focus = focusDetail

	if m.detailMaxScroll() != 0 {
		t.Fatalf("setup: short body shouldn't overflow")
	}

	m2, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m2.detailScroll != 0 {
		t.Errorf("j with body-fits-in-pane: detailScroll = %d, want 0", m2.detailScroll)
	}
}

// ---- metadata truncation (UI-shift bug fix) --------------------------------

func TestMetadataOverflow_LongIDsTruncated(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "x", "note")
	m := loadedModel(t, cx)

	// Blast the current trace with overlong metadata. Without
	// per-field truncation these would soft-wrap and push the metadata
	// block past its expected line count.
	m.current.ID = strings.Repeat("A", 500)
	m.current.Author = strings.Repeat("B", 500)
	m.current.Tags = []string{strings.Repeat("c", 500)}

	_, detailW := m.paneWidths()
	rendered := m.renderDetail(detailW, m.bodyHeight())

	// The raw 500-char strings must never appear verbatim in the
	// rendered output — every metadata value passes through truncRune.
	if strings.Contains(rendered, strings.Repeat("A", 500)) {
		t.Error("id was rendered without truncation")
	}
	if strings.Contains(rendered, strings.Repeat("B", 500)) {
		t.Error("author was rendered without truncation")
	}
	if strings.Contains(rendered, strings.Repeat("c", 500)) {
		t.Error("tags were rendered without truncation")
	}
	// And the truncation glyph should be present somewhere.
	if !strings.Contains(rendered, "…") {
		t.Error("expected truncation glyph '…' in metadata row")
	}
}

// ---- focus-aware rendering -------------------------------------------------

func TestFooter_HintChangesOnFocus(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "first", "note")
	m := loadedModel(t, cx)

	listHint := m.renderFooter()
	if !strings.Contains(listHint, "tab:body") {
		t.Errorf("list-focus footer missing 'tab:body' hint:\n%s", listHint)
	}
	if !strings.Contains(listHint, "n:new") {
		t.Errorf("list-focus footer missing 'n:new' hint:\n%s", listHint)
	}

	m.focus = focusDetail
	detailHint := m.renderFooter()
	if !strings.Contains(detailHint, "scroll") {
		t.Errorf("detail-focus footer missing 'scroll' hint:\n%s", detailHint)
	}
	if !strings.Contains(detailHint, "tab:list") {
		t.Errorf("detail-focus footer missing 'tab:list' hint:\n%s", detailHint)
	}
	if strings.Contains(detailHint, "n:new") {
		t.Errorf("detail-focus footer should not advertise n:new:\n%s", detailHint)
	}
}

// TestFooter_AlignmentTracksFocus locks in the "footer flips sides with
// focus" behavior: list focus keeps the hint on the left (2-space
// gutter), detail focus pushes it to the right edge (2-space right
// gutter). This is a secondary focus cue on top of the pane-dim and
// cursor-glyph swap.
func TestFooter_AlignmentTracksFocus(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "first", "note")
	m := loadedModel(t, cx)

	// List focus: leading gutter, no trailing pad run. Strip SGR so
	// the prefix check sees the visible text — surfacePad now wraps
	// padding in background-fill escape codes to stop terminal
	// default gray from bleeding through.
	listHint := stripANSI(m.renderFooter())
	if !strings.HasPrefix(listHint, "  ") {
		t.Errorf("list-focus footer missing leading 2-space gutter:\n%q", listHint)
	}

	// Detail focus: trailing gutter, and the visible width of the
	// footer (leading pad + styled hint + trailing pad) should equal
	// the model width — i.e. the hint is pinned to the right edge.
	m.focus = focusDetail
	rawDetail := m.renderFooter()
	detailHint := stripANSI(rawDetail)
	if !strings.HasSuffix(detailHint, "  ") {
		t.Errorf("detail-focus footer missing trailing 2-space gutter:\n%q", detailHint)
	}
	if lipgloss.Width(rawDetail) != m.width {
		t.Errorf("detail-focus footer width = %d, want %d (full pane width)",
			lipgloss.Width(rawDetail), m.width)
	}
	// Spot-check: a lot of leading spaces, meaning the hint was pushed
	// right — it should start with at least a dozen blanks.
	leading := len(detailHint) - len(strings.TrimLeft(detailHint, " "))
	if leading < 12 {
		t.Errorf("detail-focus footer leading pad = %d, want many more spaces:\n%q",
			leading, detailHint)
	}
}

func TestList_CursorGlyphChangesOnFocus(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "alpha", "note")
	m := loadedModel(t, cx)

	listW, _ := m.paneWidths()
	bodyH := m.bodyHeight()
	activeRender := m.renderList(listW, bodyH)
	if !strings.Contains(activeRender, "▸") {
		t.Errorf("list-focus render missing active cursor '▸':\n%s", activeRender)
	}
	if strings.Contains(activeRender, "·") {
		t.Errorf("list-focus render should not use dim cursor '·':\n%s", activeRender)
	}

	m.focus = focusDetail
	dimRender := m.renderList(listW, bodyH)
	if !strings.Contains(dimRender, "·") {
		t.Errorf("detail-focus render missing dim cursor '·':\n%s", dimRender)
	}
	if strings.Contains(dimRender, "▸") {
		t.Errorf("detail-focus render should not use active cursor '▸':\n%s", dimRender)
	}
}

// TestList_WholePaneDimsWhenDetailFocused verifies the "modal backdrop"
// behavior: every row — not just the selected one — swaps to a dim
// style when focus is on the detail pane, so the entire list visibly
// recedes.
func TestList_WholePaneDimsWhenDetailFocused(t *testing.T) {
	cx := setupCortex(t)
	addTrace(t, cx, "alpha", "note")
	addTrace(t, cx, "beta", "note")
	addTrace(t, cx, "gamma", "note")
	m := loadedModel(t, cx)

	listW, _ := m.paneWidths()
	bodyH := m.bodyHeight()

	// The two renders must differ — if they match byte-for-byte, the
	// dim treatment isn't being applied to non-selected rows.
	active := m.renderList(listW, bodyH)

	m.focus = focusDetail
	dim := m.renderList(listW, bodyH)

	if active == dim {
		t.Errorf("renderList output unchanged between list/detail focus — " +
			"non-selected rows aren't dimming")
	}

	// The dim-row foreground (238) should be somewhere in the detail-
	// focus render, and absent from the active render (where unselected
	// rows render through a plain zero-style). The matcher anchors on
	// the fg portion of the SGR (`38;5;238`) so it tolerates a
	// trailing background specifier added by the v0.4.1 surface paint.
	const dimFgSeq = "\x1b[38;5;238"
	if !strings.Contains(dim, dimFgSeq) {
		t.Errorf("detail-focus render missing dim fg ANSI (238):\n%q", dim)
	}
	if strings.Contains(active, dimFgSeq) {
		t.Errorf("list-focus render should not contain dim fg ANSI (238):\n%q", active)
	}
}

// ---- theme palette ---------------------------------------------------------

// TestLoadPalette_DarkSetsBrandInkBackground verifies that loading the
// dark theme paints the surface background as brand ink (#1a1a1a →
// ANSI256 234) and body text as soft gray (250). Brand cream (223) is
// reserved for the wordmark and chip fill — styleSurface intentionally
// does NOT use cream as its foreground.
func TestLoadPalette_DarkSetsBrandInkBackground(t *testing.T) {
	loadPalette("dark")
	defer loadPalette("dark") // restore for downstream tests

	rendered := styleSurface.Render("hello")
	if !strings.Contains(rendered, "48;5;234") {
		t.Errorf("dark surface missing brand-ink background (48;5;234):\n%q", rendered)
	}
	if !strings.Contains(rendered, "38;5;250") {
		t.Errorf("dark surface missing body foreground (38;5;250):\n%q", rendered)
	}
	// Cream must still appear on the wordmark, not on the surface.
	wordmark := styleHeader.Render("Noema")
	if !strings.Contains(wordmark, "38;5;223") {
		t.Errorf("dark wordmark missing brand-cream (38;5;223):\n%q", wordmark)
	}
}

// TestLoadPalette_LightInvertsRoles verifies the light theme uses a
// cool off-white background (230) — not full cream — so cream can
// serve as the selected-row accent. Ink stays the wordmark color.
func TestLoadPalette_LightInvertsRoles(t *testing.T) {
	loadPalette("light")
	defer loadPalette("dark") // restore

	rendered := styleSurface.Render("hello")
	if !strings.Contains(rendered, "48;5;255") {
		t.Errorf("light surface missing near-white background (48;5;255):\n%q", rendered)
	}
	if !strings.Contains(rendered, "38;5;238") {
		t.Errorf("light surface missing body foreground (38;5;238):\n%q", rendered)
	}
	// Cream is now the selected-row accent, not the surface bg.
	sel := styleSelected.Render("cursor")
	if !strings.Contains(sel, "48;5;223") {
		t.Errorf("light selected row missing cream background (48;5;223):\n%q", sel)
	}
}

// TestResolveTheme_PassesThroughExplicit verifies the auto-detection
// guard only kicks in when the input is empty or "auto" — explicit
// "dark" and "light" pass through unchanged so the user's flag/env/
// config choice is honored on terminals where lipgloss would otherwise
// guess differently.
func TestResolveTheme_PassesThroughExplicit(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"dark", "dark"},
		{"light", "light"},
	}
	for _, c := range cases {
		if got := resolveTheme(c.in); got != c.want {
			t.Errorf("resolveTheme(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- tag chip rendering ----------------------------------------------------

// TestRenderTagChips_FitsOneRow checks the happy path: a handful of
// short tags whose total rendered width fits comfortably in the value
// column produces exactly one output row.
func TestRenderTagChips_FitsOneRow(t *testing.T) {
	loadPalette("dark")
	tags := []string{"go", "tui", "brand"}
	rows := renderTagChips(tags, 10, 80)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d:\n%v", len(rows), rows)
	}
	// Each tag must appear with its hash prefix.
	for _, tag := range tags {
		if !strings.Contains(rows[0], "#"+tag) {
			t.Errorf("row missing chip for %q:\n%q", tag, rows[0])
		}
	}
	// The label appears exactly once (on the first row only).
	if !strings.Contains(rows[0], "tags:") {
		t.Errorf("first row missing 'tags:' label:\n%q", rows[0])
	}
}

// TestRenderTagChips_WrapsWhenOverflow verifies the wrap behavior:
// when the accumulated chip width exceeds the value column, chips
// flow to a second row whose label column is blank (so the "tags:"
// label only renders on the first row). The total chip count must
// be preserved across the two rows.
func TestRenderTagChips_WrapsWhenOverflow(t *testing.T) {
	loadPalette("dark")
	// Each chip renders as `#tagN ` with 1-cell padding on each side
	// — roughly 8 cells per chip. With valueW=20 we should get
	// ~2 chips per row, forcing the 5-tag set onto multiple rows.
	tags := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	rows := renderTagChips(tags, 10, 20)
	if len(rows) < 2 {
		t.Fatalf("expected wrap to produce ≥2 rows, got %d:\n%v", len(rows), rows)
	}
	// First row carries the "tags:" label; subsequent rows do not.
	if !strings.Contains(rows[0], "tags:") {
		t.Errorf("first row missing 'tags:' label:\n%q", rows[0])
	}
	for i := 1; i < len(rows); i++ {
		if strings.Contains(rows[i], "tags:") {
			t.Errorf("wrapped row %d should not repeat the 'tags:' label:\n%q", i, rows[i])
		}
	}
	// Every tag's chip text appears exactly once across all rows.
	joined := strings.Join(rows, "\n")
	for _, tag := range tags {
		if c := strings.Count(joined, "#"+tag); c != 1 {
			t.Errorf("expected exactly 1 chip for %q across wrapped rows, got %d", tag, c)
		}
	}
}

// TestRenderTagChips_EmptyReturnsNil documents the no-tags case so a
// future caller doesn't accidentally render an empty "tags:" row.
func TestRenderTagChips_EmptyReturnsNil(t *testing.T) {
	loadPalette("dark")
	if rows := renderTagChips(nil, 10, 80); rows != nil {
		t.Errorf("expected nil for empty tags, got %v", rows)
	}
	if rows := renderTagChips([]string{}, 10, 80); rows != nil {
		t.Errorf("expected nil for empty slice, got %v", rows)
	}
}

// TestRenderTagChips_UsesInvertedPalette confirms each chip renders
// with the inverted brand palette in dark mode — cream background,
// ink foreground — so the chip reads as a "patch of light mode"
// against the dark surface. This is the brand-compliant alternative
// to introducing a fourth accent color.
func TestRenderTagChips_UsesInvertedPalette(t *testing.T) {
	loadPalette("dark")
	rows := renderTagChips([]string{"go"}, 10, 80)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	// 48;5;223 = cream bg, 38;5;234 = ink fg.
	if !strings.Contains(rows[0], "48;5;223") {
		t.Errorf("chip missing inverted cream background (48;5;223):\n%q", rows[0])
	}
	if !strings.Contains(rows[0], "38;5;234") {
		t.Errorf("chip missing inverted ink foreground (38;5;234):\n%q", rows[0])
	}
}
