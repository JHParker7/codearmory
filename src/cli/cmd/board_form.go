package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── Mode ──────────────────────────────────────────────────────────────────────

type boardMode int

const (
	boardModeNav boardMode = iota
	boardModeCreate
	boardModeEdit
	boardModeConfirmDelete
)

const (
	prioFieldIdx   = 1
	statusFieldIdx = 2
)

// ── Messages ──────────────────────────────────────────────────────────────────

type boardMutatedMsg struct{ notice string }

// ── HTTP commands ─────────────────────────────────────────────────────────────

func sendCreateTicket(title, priority, status, timescale, dueDate, description string) tea.Cmd {
	return func() tea.Msg {
		payload := map[string]any{"title": title}
		if priority != "" {
			payload["priority"] = priority
		}
		if status != "" {
			payload["status"] = status
		}
		if timescale != "" {
			payload["timescale"] = timescale
		}
		if dueDate != "" {
			payload["due_date"] = dueDate
		}
		if description != "" {
			payload["description"] = description
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest("POST", "/tickets/tickets", body); err != nil {
			return boardErrMsg{err}
		}
		return boardMutatedMsg{"Created."}
	}
}

func sendUpdateTicket(base boardTicket, title, priority, status, timescale, dueDate, description string) tea.Cmd {
	return func() tea.Msg {
		payload := map[string]any{
			"title":       title,
			"priority":    priority,
			"status":      status,
			"timescale":   timescale,
			"description": description,
		}
		if dueDate != "" {
			payload["due_date"] = dueDate
		}
		if base.AssigneeID != nil {
			payload["assignee_id"] = *base.AssigneeID
		}
		if base.WorkflowID != nil {
			payload["workflow_id"] = *base.WorkflowID
		}
		if base.RunID != nil {
			payload["run_id"] = *base.RunID
		}
		if base.ForgeExecutionID != nil {
			payload["forge_execution_id"] = *base.ForgeExecutionID
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest("PUT", "/tickets/tickets/"+base.ID, body); err != nil {
			return boardErrMsg{err}
		}
		return boardMutatedMsg{"Updated."}
	}
}

func sendDeleteTicket(id string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/tickets/tickets/"+id, nil); err != nil {
			return boardErrMsg{err}
		}
		return boardMutatedMsg{"Deleted."}
	}
}

// ── Form helpers ──────────────────────────────────────────────────────────────

func newFormInput(placeholder string) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.PromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	ti.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Text))
	ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Accent))
	return ti
}

func newDescInput() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "description (optional)"
	ta.ShowLineNumbers = false
	ta.SetWidth(formInputW)
	ta.SetHeight(4)
	// Remove the cursor-line highlight that would paint a background stripe.
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Base = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Text))
	ta.FocusedStyle.Text = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Text))
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	ta.BlurredStyle.Base = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	ta.BlurredStyle.Text = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	ta.BlurredStyle.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	ta.Cursor.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Accent))
	return ta
}

func openCreateForm(m boardModel) boardModel {
	m.mode = boardModeCreate
	m.formTitle = newFormInput("ticket title (required)")
	m.formTs = newFormInput("e.g. Q1 2026 (optional)")
	m.formDue = newFormInput("YYYY-MM-DD (optional)")
	m.formDesc = newDescInput()
	m.formPIdx = 0
	m.formSIdx = m.col
	m.formFocus = 0
	return m
}

func openEditForm(m boardModel, t boardTicket) boardModel {
	m.mode = boardModeEdit
	m.editTarget = t
	m.formTitle = newFormInput("")
	m.formTitle.SetValue(t.Title)
	m.formTitle.CursorEnd()
	m.formTs = newFormInput("e.g. Q1 2026 (optional)")
	m.formTs.SetValue(t.Timescale)
	m.formDue = newFormInput("YYYY-MM-DD (optional)")
	if t.DueDate != nil && len(*t.DueDate) >= 10 {
		m.formDue.SetValue((*t.DueDate)[:10])
	}
	m.formDesc = newDescInput()
	m.formDesc.SetValue(t.Description)
	m.formPIdx = 0
	for i, p := range m.priorities {
		if p.Value == t.Priority {
			m.formPIdx = i
			break
		}
	}
	m.formSIdx = m.col
	for i, s := range m.statuses {
		if s.Value == t.Status {
			m.formSIdx = i
			break
		}
	}
	m.formFocus = 0
	return m
}

// formFieldCount returns the number of fields for the current form mode.
// create: title(0), priority(1), timescale(2), due_date(3), description(4)
// edit:   title(0), priority(1), status(2), timescale(3), due_date(4), description(5)
func (m boardModel) formFieldCount() int {
	if m.mode == boardModeCreate {
		return 5
	}
	return 6
}

// tsFieldIdx is the index of the timescale textinput (third to last).
func (m boardModel) tsFieldIdx() int { return m.formFieldCount() - 3 }

// dueFieldIdx is the index of the due date textinput (second to last).
func (m boardModel) dueFieldIdx() int { return m.formFieldCount() - 2 }

// descFieldIdx is the index of the description textarea (last).
func (m boardModel) descFieldIdx() int { return m.formFieldCount() - 1 }

// syncFormFocus blurs all inputs and focuses the active one.
func syncFormFocus(m boardModel) (boardModel, tea.Cmd) {
	m.formTitle.Blur()
	m.formTs.Blur()
	m.formDue.Blur()
	m.formDesc.Blur()
	var cmd tea.Cmd
	switch m.formFocus {
	case 0:
		cmd = m.formTitle.Focus()
	default:
		switch m.formFocus {
		case m.tsFieldIdx():
			cmd = m.formTs.Focus()
		case m.dueFieldIdx():
			cmd = m.formDue.Focus()
		case m.descFieldIdx():
			cmd = m.formDesc.Focus()
		}
	}
	return m, cmd
}

func (m boardModel) submitForm() (tea.Model, tea.Cmd) {
	title := strings.TrimSpace(m.formTitle.Value())
	if title == "" {
		return m, nil
	}
	pVal := ""
	if len(m.priorities) > 0 {
		pVal = m.priorities[m.formPIdx].Value
	}
	sVal := ""
	if len(m.statuses) > 0 {
		sVal = m.statuses[m.formSIdx].Value
	}
	ts := strings.TrimSpace(m.formTs.Value())
	due := strings.TrimSpace(m.formDue.Value())
	desc := strings.TrimSpace(m.formDesc.Value())
	if m.mode == boardModeCreate {
		return m, sendCreateTicket(title, pVal, sVal, ts, due, desc)
	}
	return m, sendUpdateTicket(m.editTarget, title, pVal, sVal, ts, due, desc)
}

// ── Form update ───────────────────────────────────────────────────────────────

func (m boardModel) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, isKey := msg.(tea.KeyMsg)
	if isKey {
		inDesc := m.formFocus == m.descFieldIdx()

		switch keyMsg.String() {
		case "esc":
			m.mode = boardModeNav
			return m, nil

		case "tab":
			m.formFocus = (m.formFocus + 1) % m.formFieldCount()
			m, cmd := syncFormFocus(m)
			return m, cmd

		case "shift+tab":
			m.formFocus = (m.formFocus - 1 + m.formFieldCount()) % m.formFieldCount()
			m, cmd := syncFormFocus(m)
			return m, cmd

		case "down":
			if !inDesc {
				m.formFocus = (m.formFocus + 1) % m.formFieldCount()
				m, cmd := syncFormFocus(m)
				return m, cmd
			}
			// in description: let textarea handle cursor movement

		case "up":
			if !inDesc {
				m.formFocus = (m.formFocus - 1 + m.formFieldCount()) % m.formFieldCount()
				m, cmd := syncFormFocus(m)
				return m, cmd
			}
			// in description: let textarea handle cursor movement

		case "left":
			if m.formFocus == prioFieldIdx && len(m.priorities) > 0 {
				m.formPIdx = (m.formPIdx - 1 + len(m.priorities)) % len(m.priorities)
				return m, nil
			}
			if m.formFocus == statusFieldIdx && m.mode == boardModeEdit && len(m.statuses) > 0 {
				m.formSIdx = (m.formSIdx - 1 + len(m.statuses)) % len(m.statuses)
				return m, nil
			}

		case "right":
			if m.formFocus == prioFieldIdx && len(m.priorities) > 0 {
				m.formPIdx = (m.formPIdx + 1) % len(m.priorities)
				return m, nil
			}
			if m.formFocus == statusFieldIdx && m.mode == boardModeEdit && len(m.statuses) > 0 {
				m.formSIdx = (m.formSIdx + 1) % len(m.statuses)
				return m, nil
			}

		case "enter":
			if !inDesc {
				return m.submitForm()
			}
			// in description: let textarea insert a newline

		case "ctrl+s":
			return m.submitForm()
		}
	}

	// Delegate to the active input.
	var cmd tea.Cmd
	switch m.formFocus {
	case 0:
		m.formTitle, cmd = m.formTitle.Update(msg)
	case m.tsFieldIdx():
		m.formTs, cmd = m.formTs.Update(msg)
	case m.dueFieldIdx():
		m.formDue, cmd = m.formDue.Update(msg)
	case m.descFieldIdx():
		m.formDesc, cmd = m.formDesc.Update(msg)
	}
	return m, cmd
}

// ── Confirm-delete update ─────────────────────────────────────────────────────

func (m boardModel) updateConfirmDelete(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "y", "Y":
		t, ok := m.focusedCard()
		if !ok {
			m.mode = boardModeNav
			return m, nil
		}
		return m, sendDeleteTicket(t.ID)
	case "esc", "q", "n", "N":
		m.mode = boardModeNav
	}
	return m, nil
}

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	bsFormBox         lipgloss.Style
	bsFormHeading     lipgloss.Style
	bsFormLabel       lipgloss.Style
	bsFormLabelActive lipgloss.Style
	bsFormCycle       lipgloss.Style
	bsFormHint        lipgloss.Style
)

// buildBoardFormStyles rebuilds the board create/edit form styles from the
// active theme.
func buildBoardFormStyles() {
	bsFormBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(activeTheme.Accent)).
		Padding(1, 3).
		Width(54)

	bsFormHeading = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(activeTheme.Accent)).
		MarginBottom(1)

	bsFormLabel = lipgloss.NewStyle().
		Foreground(lipgloss.Color(activeTheme.Muted)).
		Width(11)

	bsFormLabelActive = lipgloss.NewStyle().
		Foreground(lipgloss.Color(activeTheme.Text)).
		Width(11)

	bsFormCycle = lipgloss.NewStyle().
		Foreground(lipgloss.Color(activeTheme.Text))

	bsFormHint = lipgloss.NewStyle().
		Foreground(lipgloss.Color(activeTheme.Muted))
}

const formInputW = 34

// ── Form view ─────────────────────────────────────────────────────────────────

func (m boardModel) viewForm() string {
	heading := "New Ticket"
	if m.mode == boardModeEdit {
		heading = "Edit Ticket"
	}

	lbl := func(text string, fieldIdx int) string {
		if m.formFocus == fieldIdx {
			return bsFormLabelActive.Render(text)
		}
		return bsFormLabel.Render(text)
	}

	rows := []string{bsFormHeading.Render(heading)}

	// Title
	ti := m.formTitle
	ti.Width = formInputW
	rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, lbl("Title", 0), ti.View()))

	// Priority
	pVal := "-"
	if len(m.priorities) > 0 {
		pVal = m.priorities[m.formPIdx].Label
	}
	rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top,
		lbl("Priority", prioFieldIdx), bsFormCycle.Render("‹ "+pVal+" ›")))

	// Status (edit only)
	if m.mode == boardModeEdit {
		sVal := "-"
		if len(m.statuses) > 0 {
			sVal = m.statuses[m.formSIdx].Label
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top,
			lbl("Status", statusFieldIdx), bsFormCycle.Render("‹ "+sVal+" ›")))
	}

	// Timescale
	tsi := m.formTs
	tsi.Width = formInputW
	rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, lbl("Timescale", m.tsFieldIdx()), tsi.View()))

	// Due date
	duei := m.formDue
	duei.Width = formInputW
	rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, lbl("Due date", m.dueFieldIdx()), duei.View()))

	// Description
	rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top,
		lbl("Desc", m.descFieldIdx()), m.formDesc.View()))

	rows = append(rows, "")
	rows = append(rows, bsFormHint.Render("tab: next field   ←/→: cycle   ctrl+s: save   esc: cancel"))

	box := bsFormBox.Render(strings.Join(rows, "\n"))
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	}
	return "\n" + box
}

// ── Delete confirm view ───────────────────────────────────────────────────────

func (m boardModel) viewConfirmDelete() string {
	t, ok := m.focusedCard()
	what := "this ticket"
	if ok {
		what = fmt.Sprintf("%q", truncate(t.Title, 40))
	}
	content := strings.Join([]string{
		bsFormHeading.Render("Delete Ticket"),
		"",
		lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Text)).Render("Delete " + what + "?"),
		"",
		bsFormHint.Render("y: confirm   esc: cancel"),
	}, "\n")
	box := bsFormBox.Render(content)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	}
	return "\n" + box
}
