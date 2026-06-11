package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// ── Data types ────────────────────────────────────────────────────────────────

type boardTicket struct {
	ID               string  `json:"ticket_id"`
	Title            string  `json:"title"`
	Description      string  `json:"description"`
	Priority         string  `json:"priority"`
	Status           string  `json:"status"`
	Timescale        string  `json:"timescale"`
	DueDate          *string `json:"due_date"`
	AssigneeID       *string `json:"assignee_id,omitempty"`
	WorkflowID       *string `json:"workflow_id,omitempty"`
	RunID            *string `json:"run_id,omitempty"`
	ForgeExecutionID *string `json:"forge_execution_id,omitempty"`
}

type boardFieldDef struct {
	FieldDefID string `json:"field_def_id"`
	Kind       string `json:"kind"`
	Value      string `json:"value"`
	Label      string `json:"label"`
	Color      string `json:"color"`
	Position   int    `json:"position"`
}

// fallback field defs used when the API is unavailable or returns empty.
var defaultBoardStatuses = []boardFieldDef{
	{Value: "open",        Label: "Open",        Position: 0},
	{Value: "in_progress", Label: "In Progress",  Position: 1},
	{Value: "resolved",    Label: "Resolved",     Position: 2},
	{Value: "closed",      Label: "Closed",       Position: 3},
}
var defaultBoardPriorities = []boardFieldDef{
	{Value: "low",      Label: "Low",      Color: "#4a5346"},
	{Value: "medium",   Label: "Medium",   Color: "#7d8a78"},
	{Value: "high",     Label: "High",     Color: "#c9b060"},
	{Value: "critical", Label: "Critical", Color: "#d46b55"},
}

// ── Messages ──────────────────────────────────────────────────────────────────

type boardDataMsg struct {
	statuses   []boardFieldDef
	priorities []boardFieldDef
	cols       [][]boardTicket
}
type boardMovedMsg  struct{}
type boardErrMsg    struct{ err error }

// ── Model ─────────────────────────────────────────────────────────────────────

type boardModel struct {
	statuses   []boardFieldDef
	priorities []boardFieldDef
	cols       [][]boardTicket
	col        int
	row        int
	loading    bool
	err        error
	status     string
	width      int
	height     int
	// form state
	mode       boardMode
	editTarget boardTicket
	formTitle  textinput.Model
	formTs     textinput.Model
	formDue    textinput.Model
	formDesc   textarea.Model
	formPIdx   int
	formSIdx   int
	formFocus  int
	// followID, when non-empty, moves the cursor to that ticket after next refresh
	followID string
}

func newBoardModel() boardModel { return boardModel{loading: true} }

// ── Styles ────────────────────────────────────────────────────────────────────

const boardCardW = 26

var (
	bsHeader = lipgloss.NewStyle().
		Bold(true).
		Width(boardCardW + 4).
		Padding(0, 1).
		Foreground(lipgloss.Color("#a8b4a2"))

	bsHeaderFocus = lipgloss.NewStyle().
		Bold(true).
		Width(boardCardW + 4).
		Padding(0, 1).
		Foreground(lipgloss.Color("#39ff14"))

	bsCard = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#1a2018")).
		Foreground(lipgloss.Color("#a8b4a2"))

	bsCardFocus = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#39ff14")).
		Foreground(lipgloss.Color("#d6dcd2"))

	bsCol = lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, true, false, false).
		BorderForeground(lipgloss.Color("#1a2018")).
		Padding(0, 1)

	bsColFocus = lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, true, false, false).
		BorderForeground(lipgloss.Color("#39ff14")).
		Padding(0, 1)

	bsHelp = lipgloss.NewStyle().Foreground(lipgloss.Color("#a8b4a2"))
)

// ── Commands ──────────────────────────────────────────────────────────────────

func fetchBoardData() tea.Msg {
	statuses := fetchBoardFieldDefs("status", defaultBoardStatuses)
	priorities := fetchBoardFieldDefs("priority", defaultBoardPriorities)

	data, err := doRequest("GET", "/tickets/tickets", nil)
	if err != nil {
		return boardErrMsg{err}
	}
	var tickets []boardTicket
	if err := json.Unmarshal(data, &tickets); err != nil {
		return boardErrMsg{fmt.Errorf("parse response: %w", err)}
	}

	statusIdx := map[string]int{}
	for i, s := range statuses {
		statusIdx[s.Value] = i
	}
	cols := make([][]boardTicket, len(statuses))
	for i := range cols {
		cols[i] = []boardTicket{}
	}
	for _, t := range tickets {
		if i, ok := statusIdx[t.Status]; ok {
			cols[i] = append(cols[i], t)
		}
	}

	return boardDataMsg{statuses: statuses, priorities: priorities, cols: cols}
}

func fetchBoardFieldDefs(kind string, fallback []boardFieldDef) []boardFieldDef {
	data, err := doRequest("GET", "/tickets/field-defs?kind="+kind, nil)
	if err != nil {
		return fallback
	}
	var defs []boardFieldDef
	if err := json.Unmarshal(data, &defs); err != nil || len(defs) == 0 {
		return fallback
	}
	return defs
}

func sendMoveTicket(t boardTicket, newStatus string) tea.Cmd {
	return func() tea.Msg {
		payload := map[string]any{
			"title":       t.Title,
			"status":      newStatus,
			"priority":    t.Priority,
			"description": t.Description,
			"timescale":   t.Timescale,
		}
		if t.DueDate != nil {
			payload["due_date"] = *t.DueDate
		}
		if t.AssigneeID != nil {
			payload["assignee_id"] = *t.AssigneeID
		}
		if t.WorkflowID != nil {
			payload["workflow_id"] = *t.WorkflowID
		}
		if t.RunID != nil {
			payload["run_id"] = *t.RunID
		}
		if t.ForgeExecutionID != nil {
			payload["forge_execution_id"] = *t.ForgeExecutionID
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest("PUT", "/tickets/tickets/"+t.ID, body); err != nil {
			return boardErrMsg{err}
		}
		return boardMovedMsg{}
	}
}

// ── Init / Update / View ─────────────────────────────────────────────────────

func (m boardModel) Init() tea.Cmd { return fetchBoardData }

func (m boardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Mode-independent messages.
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case boardDataMsg:
		m.statuses, m.priorities, m.cols = msg.statuses, msg.priorities, msg.cols
		m.loading, m.err = false, nil
		if m.status == "Refreshing…" {
			m.status = ""
		}
		if m.followID != "" {
			found := false
			for c, col := range m.cols {
				for r, t := range col {
					if t.ID == m.followID {
						m.col, m.row, found = c, r, true
						break
					}
				}
				if found {
					break
				}
			}
			m.followID = ""
		}
		m.col = boardClamp(m.col, len(m.statuses))
		if len(m.cols) > 0 {
			m.row = boardClamp(m.row, len(m.cols[m.col]))
		}
		return m, nil
	case boardMutatedMsg:
		m.status = msg.notice
		m.mode = boardModeNav
		m.loading = true
		return m, fetchBoardData
	case boardMovedMsg:
		m.status = "Moved."
		return m, fetchBoardData
	case boardErrMsg:
		m.loading, m.err = false, msg.err
		m.mode = boardModeNav
		return m, nil
	}

	switch m.mode {
	case boardModeCreate, boardModeEdit:
		return m.updateForm(msg)
	case boardModeConfirmDelete:
		return m.updateConfirmDelete(msg)
	}
	return m.updateNav(msg)
}

func (m boardModel) updateNav(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.loading {
		return m, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "q", "esc":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "n":
		m = openCreateForm(m)
		m, cmd := syncFormFocus(m)
		return m, cmd
	case "enter", "e":
		if t, focused := m.focusedCard(); focused {
			m = openEditForm(m, t)
			m, cmd := syncFormFocus(m)
			return m, cmd
		}
	case "d":
		if _, focused := m.focusedCard(); focused {
			m.mode = boardModeConfirmDelete
		}
	case "r":
		m.err = nil
		m.loading, m.status = true, "Refreshing…"
		return m, fetchBoardData
	case "left", "h":
		if m.col > 0 {
			m.col--
			m.row = boardClamp(m.row, len(m.cols[m.col]))
		}
	case "right", "l":
		if m.col < len(m.statuses)-1 {
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
			if t, focused := m.focusedCard(); focused {
				m.status = "Moving…"
				m.followID = t.ID
				return m, sendMoveTicket(t, m.statuses[m.col-1].Value)
			}
		}
	case "L", "shift+right":
		if m.col < len(m.statuses)-1 {
			if t, focused := m.focusedCard(); focused {
				m.status = "Moving…"
				m.followID = t.ID
				return m, sendMoveTicket(t, m.statuses[m.col+1].Value)
			}
		}
	}
	return m, nil
}

func (m boardModel) focusedCard() (boardTicket, bool) {
	if len(m.statuses) == 0 {
		return boardTicket{}, false
	}
	cards := m.cols[m.col]
	if len(cards) == 0 || m.row >= len(cards) {
		return boardTicket{}, false
	}
	return cards[m.row], true
}

func boardClamp(idx, n int) int {
	if n <= 0 {
		return idx
	}
	if idx < n {
		return idx
	}
	return n - 1
}

func (m boardModel) View() string {
	if m.loading {
		return "\n  " + tuiMetaStyle.Render("Loading…") + "\n"
	}
	if m.err != nil {
		hint := "r: retry  q: quit"
		if strings.Contains(m.err.Error(), "401") || strings.Contains(m.err.Error(), "unauthorized") {
			hint += "\n\n  Not authenticated — run `armory auth login`, then press r"
		}
		return "\n  " + tuiErrStyle.Render("Error: "+m.err.Error()) + "\n\n  " + tuiHelpStyle.Render(hint) + "\n"
	}

	switch m.mode {
	case boardModeCreate, boardModeEdit:
		return m.viewForm()
	case boardModeConfirmDelete:
		return m.viewConfirmDelete()
	}

	if len(m.statuses) == 0 {
		return "\n  " + tuiMetaStyle.Render("No statuses configured.") + "\n"
	}

	cols := make([]string, len(m.statuses))
	for i := range m.statuses {
		cols[i] = m.renderBoardCol(i)
	}

	statusPrefix := ""
	if m.status != "" {
		statusPrefix = m.status + "  ·  "
	}
	help := bsHelp.Render(statusPrefix + "← → h l: col   ↑ ↓ j k: card   H/L: move   n: new   e: edit   d: delete   r: refresh   q: home")

	return lipgloss.JoinHorizontal(lipgloss.Top, cols...) + "\n" + help
}

func (m boardModel) renderBoardCol(i int) string {
	focused := i == m.col
	label := fmt.Sprintf("%s (%d)", m.statuses[i].Label, len(m.cols[i]))

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

func (m boardModel) priorityColor(p string) string {
	for _, def := range m.priorities {
		if def.Value == p && def.Color != "" {
			return def.Color
		}
	}
	return "#a8b4a2"
}

func (m boardModel) renderBoardCard(t boardTicket, selected bool) string {
	innerW := boardCardW - 2
	row := lipgloss.NewStyle().Width(boardCardW).Padding(0, 1)

	title := row.Render(truncate(t.Title, innerW))
	prio := row.Foreground(lipgloss.Color(m.priorityColor(t.Priority))).Render("● " + t.Priority)

	tsStr := t.Timescale
	if tsStr == "" {
		tsStr = "—"
	}
	ts := row.Foreground(lipgloss.Color("#a8b4a2")).Render(truncate(tsStr, innerW))

	dueStr := ""
	if t.DueDate != nil && len(*t.DueDate) >= 10 {
		dueStr = "due " + (*t.DueDate)[:10]
	}
	due := row.Foreground(lipgloss.Color("#a8b4a2")).Render(dueStr)

	content := title + "\n" + prio + "\n" + ts + "\n" + due

	if selected {
		return bsCardFocus.Render(content)
	}
	return bsCard.Render(content)
}

// ── Registration ──────────────────────────────────────────────────────────────

func init() {
	ticketsCmd.AddCommand(&cobra.Command{
		Use:   "board",
		Short: "Interactive kanban board",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := tea.NewProgram(standaloneWrap{newBoardModel()}, tea.WithAltScreen())
			_, err := p.Run()
			return err
		},
	})
}
