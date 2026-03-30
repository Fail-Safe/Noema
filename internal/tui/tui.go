package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// ---- styles ----------------------------------------------------------------

var (
	styleHeader = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("99"))

	styleSelected = lipgloss.NewStyle().
			Bold(true).
			Background(lipgloss.Color("236")).
			Foreground(lipgloss.Color("255"))

	styleDim = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	styleBadge = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214"))

	styleDivider = lipgloss.NewStyle().
			Foreground(lipgloss.Color("238"))

	styleLabel = lipgloss.NewStyle().
			Foreground(lipgloss.Color("244"))

	styleValue = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	styleFooter = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	styleError = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))

	styleStatus = lipgloss.NewStyle().
			Foreground(lipgloss.Color("71"))
)

// ---- state machine ---------------------------------------------------------

type viewState int

const (
	stateList    viewState = iota
	stateSearch
	stateConfirm
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

// ---- model -----------------------------------------------------------------

type confirmAction struct {
	action string // "archive" or "delete"
	id     string
}

type model struct {
	cx          *cortex.Cortex
	rows        []cortex.Row
	cursor      int
	current     *trace.Trace // cached detail for selected row
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
}

func initialModel(cx *cortex.Cortex) model {
	ti := textinput.New()
	ti.Placeholder = "search traces…"
	ti.CharLimit = 120

	return model{
		cx:     cx,
		search: ti,
		state:  stateList,
	}
}

// ---- commands --------------------------------------------------------------

func loadRows(cx *cortex.Cortex, query string, all, trashed bool) tea.Cmd {
	return func() tea.Msg {
		opts := cortex.ListOptions{All: all, Trashed: trashed}
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
	return loadRows(m.cx, "", false, false)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case rowsLoadedMsg:
		m.rows = []cortex.Row(msg)
		if m.cursor >= len(m.rows) {
			m.cursor = max(0, len(m.rows)-1)
		}
		m.current = m.loadCurrent()
		return m, nil

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

	case "j", "down":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
			m.current = m.loadCurrent()
		}

	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
			m.current = m.loadCurrent()
		}

	case "g":
		m.cursor = 0
		m.current = m.loadCurrent()

	case "G":
		if len(m.rows) > 0 {
			m.cursor = len(m.rows) - 1
			m.current = m.loadCurrent()
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
		return m, loadRows(m.cx, m.searchQuery, m.showAll, m.showTrashed)

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
		return m, loadRows(m.cx, m.searchQuery, m.showAll, m.showTrashed)

	case "a":
		m.showAll = !m.showAll
		m.showTrashed = false
		m.cursor = 0
		return m, loadRows(m.cx, m.searchQuery, m.showAll, false)

	case "t":
		m.showTrashed = !m.showTrashed
		m.showAll = false
		m.cursor = 0
		return m, loadRows(m.cx, m.searchQuery, false, m.showTrashed)

	case "/":
		m.state = stateSearch
		m.search.SetValue("")
		m.search.Focus()
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(nil)
		return m, cmd

	case "esc":
		if m.searchQuery != "" {
			m.searchQuery = ""
			m.cursor = 0
			return m, loadRows(m.cx, "", m.showAll, m.showTrashed)
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
		return m, loadRows(m.cx, m.searchQuery, m.showAll, m.showTrashed)

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
		return m, loadRows(m.cx, m.searchQuery, m.showAll, m.showTrashed)

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

	return m, loadRows(m.cx, m.searchQuery, m.showAll, m.showTrashed)
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

	listW := m.width * listPct / 100
	detailW := m.width - listW - 1 // -1 for divider column
	bodyH := m.height - 2          // header + footer

	list := m.renderList(listW, bodyH)
	detail := m.renderDetail(detailW, bodyH)

	// Single-column divider
	divLines := make([]string, bodyH)
	for i := range divLines {
		divLines[i] = styleDivider.Render("│")
	}
	divider := strings.Join(divLines, "\n")

	body := lipgloss.JoinHorizontal(lipgloss.Top, list, divider, detail)

	return lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(),
		body,
		m.renderFooter(),
	)
}

func (m model) renderHeader() string {
	left := styleHeader.Render("Noema") + styleDim.Render("  "+m.cx.Name)
	if m.searchQuery != "" {
		left += styleDim.Render(`  search:"` + m.searchQuery + `"`)
	}
	switch {
	case m.showTrashed:
		left += styleDim.Render("  [trash]")
	case m.showAll:
		left += styleDim.Render("  [all]")
	}
	right := styleDim.Render(fmt.Sprintf("%d traces", len(m.rows)))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
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
		// 1 cursor + 1 space + title + 1 space + badge(14) + 1 space + date(10)
		titleW := width - 2 - badgeW - 1 - dateW - 1
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

		// Build plain-text line with rune-padding for alignment
		cursor := " "
		if i == m.cursor {
			cursor = ">"
		}
		line := fmt.Sprintf("%s %-*s %-*s %s",
			cursor,
			titleW, padRunes(title, titleW),
			badgeW, badge,
			date,
		)

		if i == m.cursor {
			sb.WriteString(styleSelected.Width(width).Render(line))
		} else {
			sb.WriteString(lipgloss.NewStyle().Width(width).Render(line))
		}
		if i < end-1 {
			sb.WriteByte('\n')
		}
	}

	// Pad to height
	rendered := sb.String()
	lines := strings.Count(rendered, "\n") + 1
	for lines < height {
		rendered += "\n" + lipgloss.NewStyle().Width(width).Render("")
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

	metaLine := func(label, val string) string {
		l := styleLabel.Render(fmt.Sprintf("  %-*s", labelW, label+":"))
		v := styleValue.Render(val)
		return l + v
	}

	var lines []string
	lines = append(lines, metaLine("id", t.ID))
	lines = append(lines, metaLine("title", truncRune(t.Title, width-labelW-4)))
	lines = append(lines, metaLine("type", t.Type))
	if t.Author != "" {
		lines = append(lines, metaLine("author", t.Author))
	}
	if len(t.Tags) > 0 {
		lines = append(lines, metaLine("tags", strings.Join(t.Tags, ", ")))
	}
	created := t.Created
	if len(created) > 10 {
		created = created[:10]
	}
	lines = append(lines, metaLine("created", created))
	lines = append(lines, styleDivider.Render("  "+strings.Repeat("─", width-4)))

	bodyW := width - 4
	if bodyW < 10 {
		bodyW = 10
	}
	if t.Body == "" {
		lines = append(lines, styleDim.Render("  (no body)"))
	} else {
		for _, raw := range strings.Split(t.Body, "\n") {
			for _, wrapped := range wrapLine(raw, bodyW) {
				lines = append(lines, "  "+wrapped)
			}
		}
	}

	// Clip to height
	if len(lines) > height {
		lines = lines[:height]
	}

	content := strings.Join(lines, "\n")
	return lipgloss.NewStyle().Width(width).Height(height).Render(content)
}

func (m model) renderFooter() string {
	switch m.state {
	case stateSearch:
		return " /" + m.search.View()

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
		if m.showTrashed {
			hint = "j/k:nav  r:recover  D:purge  t:back  /:search  q:quit"
		} else {
			hint = "j/k:nav  n:new  e:edit  d:archive  u:unarchive  D:trash  t:trash-view  a:all  /:search  q:quit"
		}
		if m.searchQuery != "" {
			hint = "esc:clear  " + hint
		}
		return styleFooter.Render("  " + hint)
	}
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

// padBlock pads a rendered block to width × height with empty lines.
func padBlock(content string, width, height int) string {
	lines := strings.Split(content, "\n")
	for len(lines) < height {
		lines = append(lines, lipgloss.NewStyle().Width(width).Render(""))
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

// Run starts the full-screen TUI against the given Cortex.
func Run(cx *cortex.Cortex) error {
	p := tea.NewProgram(
		initialModel(cx),
		tea.WithAltScreen(),
	)
	_, err := p.Run()
	return err
}
