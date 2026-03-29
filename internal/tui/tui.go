package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

type model struct {
	cx *cortex.Cortex
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "q" || msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m model) View() string {
	return fmt.Sprintf("Noema TUI — cortex: %s\n\nPress q to quit.\n(Full TUI coming soon)\n", m.cx.Name)
}

func Run(cx *cortex.Cortex) error {
	p := tea.NewProgram(model{cx: cx})
	_, err := p.Run()
	return err
}
