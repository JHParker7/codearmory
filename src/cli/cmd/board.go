package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// ── Data types ────────────────────────────────────────────────────────────────

type boardTicket struct {
	ID       string `json:"ticket_id"`
	Title    string `json:"title"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

var (
	boardStatusOrder = []string{"open", "in_progress", "resolved", "closed"}
	boardStatusLabel = map[string]string{
		"open":        "Open",
		"in_progress": "In Progress",
		"resolved":    "Resolved",
		"closed":      "Closed",
	}
	boardPriorityColor = map[string]string{
		"critical": "196",
		"high":     "208",
		"medium":   "220",
		"low":      "245",
	}
)

// ── Messages ──────────────────────────────────────────────────────────────────

type boardLoadedMsg struct{ cols [4][]boardTicket }
type boardMovedMsg  struct{}
type boardErrMsg    struct{ err error }

// ── Model ─────────────────────────────────────────────────────────────────────

type boardModel struct {
	cols    [4][]boardTicket
	col     int // focused column (0–3)
	row     int // focused card within column
	loading bool
	err     error
	status  string
	width   int
	height  int
}

func newBoardModel() boardModel { return boardModel{loading: true} }

// ── Styles ────────────────────────────────────────────────────────────────────

const boardCardW = 26

var (
	bsHeader = lipgloss.NewStyle().
		Bold(true).
		Width(boardCardW + 4).
		Padding(0, 1)

	bsHeaderFocus = lipgloss.NewStyle().
		Bold(true).
		Width(boardCardW + 4).
		Padding(0, 1).
		Foreground(lipgloss.Color("99"))

	bsCard = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Width(boardCardW).
		Padding(0, 1)

	bsCardFocus = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("99")).
		Width(boardCardW).
		Padding(0, 1)

	bsCol = lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, true, false, false).
		Padding(0, 1)

	bsColFocus = lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, true, false, false).
		BorderForeground(lipgloss.Color("99")).
		Padding(0, 1)

	bsHelp = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

// ── Commands ──────────────────────────────────────────────────────────────────

func fetchBoardTickets() tea.Msg {
	data, err := doRequest("GET", "/tickets/tickets", nil)
	if err != nil {
		return boardErrMsg{err}
	}
	var tickets []boardTicket
	if err := json.Unmarshal(data, &tickets); err != nil {
		return boardErrMsg{fmt.Errorf("parse response: %w", err)}
	}
	colIdx := map[string]int{"open": 0, "in_progress": 1, "resolved": 2, "closed": 3}
	var cols [4][]boardTicket
	for _, t := range tickets {
		if i, ok := colIdx[t.Status]; ok {
			cols[i] = append(cols[i], t)
		}
	}
	return boardLoadedMsg{cols}
}

func sendMoveTicket(t boardTicket, newStatus string) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]string{"title": t.Title, "status": newStatus})
		if _, err := doRequest("PUT", "/tickets/tickets/"+t.ID, body); err != nil {
			return boardErrMsg{err}
		}
		return boardMovedMsg{}
	}
}

// ── Init / Update / View ─────────────────────────────────────────────────────

func (m boardModel) Init() tea.Cmd { return fetchBoardTickets }

func (m boardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case boardLoadedMsg:
		m.cols, m.loading, m.err = msg.cols, false, nil
		m.row = boardClamp(m.row, len(m.cols[m.col]))
		if m.status == "Refreshing…" {
			m.status = ""
		}

	case boardMovedMsg:
		m.status = "Moved."
		return m, fetchBoardTickets

	case boardErrMsg:
		m.loading, m.err = false, msg.err

	case tea.KeyMsg:
		if m.loading {
			break
		}
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "r":
			m.loading, m.status = true, "Refreshing…"
			return m, fetchBoardTickets
		case "left", "h":
			if m.col > 0 {
				m.col--
				m.row = boardClamp(m.row, len(m.cols[m.col]))
			}
		case "right", "l":
			if m.col < 3 {
				m.col++
				m.row = boardClamp(m.row, len(m.cols[m.col]))
			}
		case "up", "k":
			if m.row > 0 {
				m.row--
			}
		case "down", "j":
			if m.row < len(m.cols[m.col])-1 {
				m.row++
			}
		case "H", "shift+left":
			if m.col > 0 {
				if t, ok := m.focusedCard(); ok {
					m.status = "Moving…"
					return m, sendMoveTicket(t, boardStatusOrder[m.col-1])
				}
			}
		case "L", "shift+right":
			if m.col < 3 {
				if t, ok := m.focusedCard(); ok {
					m.status = "Moving…"
					return m, sendMoveTicket(t, boardStatusOrder[m.col+1])
				}
			}
		}
	}

	return m, nil
}

func (m boardModel) focusedCard() (boardTicket, bool) {
	cards := m.cols[m.col]
	if len(cards) == 0 || m.row >= len(cards) {
		return boardTicket{}, false
	}
	return cards[m.row], true
}

func boardClamp(row, n int) int {
	if n <= 0 || row < n {
		return row
	}
	return n - 1
}

func (m boardModel) View() string {
	if m.loading {
		return "\n  Loading tickets…\n"
	}
	if m.err != nil {
		return fmt.Sprintf("\n  Error: %s\n\n  r: retry  q: quit\n", m.err)
	}

	cols := make([]string, 4)
	for i, status := range boardStatusOrder {
		cols[i] = m.renderBoardCol(i, status)
	}

	statusPrefix := ""
	if m.status != "" {
		statusPrefix = m.status + "  ·  "
	}
	help := bsHelp.Render(statusPrefix + "← → h l: column   ↑ ↓ j k: card   H/L: move card   r: refresh   q: quit")

	return lipgloss.JoinHorizontal(lipgloss.Top, cols...) + "\n" + help
}

func (m boardModel) renderBoardCol(i int, status string) string {
	focused := i == m.col
	label := fmt.Sprintf("%s (%d)", boardStatusLabel[status], len(m.cols[i]))

	var header string
	if focused {
		header = bsHeaderFocus.Render(label)
	} else {
		header = bsHeader.Render(label)
	}

	parts := []string{header}
	for j, t := range m.cols[i] {
		parts = append(parts, m.renderBoardCard(t, focused && j == m.row))
	}

	body := strings.Join(parts, "\n")
	if focused {
		return bsColFocus.Render(body)
	}
	return bsCol.Render(body)
}

func (m boardModel) renderBoardCard(t boardTicket, selected bool) string {
	title := truncate(t.Title, boardCardW-2)
	color, ok := boardPriorityColor[t.Priority]
	if !ok {
		color = "245"
	}
	prio := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render("● " + t.Priority)

	content := title + "\n" + prio
	if selected {
		return bsCardFocus.Render(content)
	}
	return bsCard.Render(content)
}

// ── Registration ──────────────────────────────────────────────────────────────

func init() {
	ticketsCmd.AddCommand(&cobra.Command{
		Use:   "board",
		Short: "Interactive kanban board (open → in_progress → resolved → closed)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := tea.NewProgram(newBoardModel(), tea.WithAltScreen())
			_, err := p.Run()
			return err
		},
	})
}
