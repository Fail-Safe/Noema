package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// ---- test helpers ----------------------------------------------------------

func setupCortex(t *testing.T) *cortex.Cortex {
	t.Helper()
	dir := t.TempDir()
	if err := cortex.Create("test", dir); err != nil {
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

	msg := loadRows(cx, "", false, false)()
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
	msg := loadRows(cx, "", false, true)()
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
	msg := loadRows(cx, "", false, true)()
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
