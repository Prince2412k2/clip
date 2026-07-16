package cli

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"clippy/internal/transport"
)

var (
	arrowStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	onlineStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	offlineName = lipgloss.NewStyle().Faint(true)
	helpStyle   = lipgloss.NewStyle().Faint(true)
)

// pickerModel is a minimal inline (no alt-screen) arrow-key list picker.
type pickerModel struct {
	peers     []transport.PeerDTO
	cursor    int
	chosen    string
	cancelled bool
}

type containerPickerModel struct {
	containers []transport.ContainerDTO
	cursor     int
	chosen     string
	cancelled  bool
}

func (m pickerModel) Init() tea.Cmd { return nil }

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch keyMsg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.peers)-1 {
			m.cursor++
		}
	case "enter":
		m.chosen = m.peers[m.cursor].Name
		return m, tea.Quit
	case "esc", "q", "ctrl+c":
		m.cancelled = true
		return m, tea.Quit
	}
	return m, nil
}

func (m pickerModel) View() string {
	var b strings.Builder
	for i, p := range m.peers {
		dot := "○"
		name := offlineName.Render(p.Name)
		if p.Online {
			dot = onlineStyle.Render("●")
			name = p.Name
		}
		prefix := "  "
		if i == m.cursor {
			prefix = arrowStyle.Render("▸ ")
		}
		fmt.Fprintf(&b, "%s%s  %s\n", prefix, dot, name)
	}
	b.WriteString(helpStyle.Render("↑/↓ or j/k to choose, enter to confirm, q to cancel"))
	return b.String()
}

// pickTarget renders an inline arrow-key picker over peers, starting on
// current if it's among them. It returns the chosen name, or "" if the user
// cancelled (Esc/q/Ctrl-C). Requires stdin/stdout to be a TTY.
func pickTarget(peers []transport.PeerDTO, current string) (string, error) {
	if len(peers) == 0 {
		return "", fmt.Errorf("cli: no peers to choose from")
	}

	cursor := 0
	for i, p := range peers {
		if p.Name == current {
			cursor = i
		}
	}

	p := tea.NewProgram(pickerModel{peers: peers, cursor: cursor})
	final, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("cli: run picker: %w", err)
	}
	m := final.(pickerModel)
	if m.cancelled {
		return "", nil
	}
	return m.chosen, nil
}

func (m containerPickerModel) Init() tea.Cmd { return nil }

func (m containerPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch keyMsg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.containers)-1 {
			m.cursor++
		}
	case "enter":
		m.chosen = m.containers[m.cursor].Name
		return m, tea.Quit
	case "esc", "q", "ctrl+c":
		m.cancelled = true
		return m, tea.Quit
	}
	return m, nil
}

func (m containerPickerModel) View() string {
	var b strings.Builder
	for i, container := range m.containers {
		prefix := "  "
		if i == m.cursor {
			prefix = arrowStyle.Render("▸ ")
		}
		fmt.Fprintf(&b, "%s%-24s %s\n", prefix, container.Name, helpStyle.Render(container.Status))
	}
	b.WriteString(helpStyle.Render("↑/↓ or j/k to choose, enter to confirm, q to cancel"))
	return b.String()
}

func pickContainer(containers []transport.ContainerDTO, current string) (string, error) {
	if len(containers) == 0 {
		return "", fmt.Errorf("cli: no Docker containers to choose from")
	}
	cursor := 0
	for i, container := range containers {
		if container.Name == current {
			cursor = i
		}
	}
	p := tea.NewProgram(containerPickerModel{containers: containers, cursor: cursor})
	final, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("cli: run picker: %w", err)
	}
	m := final.(containerPickerModel)
	if m.cancelled {
		return "", nil
	}
	return m.chosen, nil
}
