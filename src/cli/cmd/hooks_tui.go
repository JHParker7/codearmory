package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// ── Types ─────────────────────────────────────────────────────────────────────

type hookRule struct {
	RuleID     string    `json:"rule_id"`
	Name       string    `json:"name"`
	Repo       string    `json:"source"`
	Events     []string  `json:"events"`
	RefFilter  string    `json:"ref_filter"`
	WorkflowID string    `json:"workflow_id"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
}

type hookEvent struct {
	EventID      string        `json:"event_id"`
	Repo         string        `json:"source"`
	EventType    string        `json:"event_type"`
	Ref          string        `json:"ref"`
	RulesMatched int           `json:"rules_matched"`
	Status       string        `json:"status"`
	Triggers     []hookTrigger `json:"triggers"`
	CreatedAt    time.Time     `json:"created_at"`
}

type hookTrigger struct {
	TriggerID  string    `json:"trigger_id"`
	RuleID     string    `json:"rule_id"`
	WorkflowID string    `json:"workflow_id"`
	RunID      *string   `json:"run_id,omitempty"`
	Status     string    `json:"status"`
	Error      *string   `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ── Messages ──────────────────────────────────────────────────────────────────

type hookRulesMsg        []hookRule
type hookEventsMsg       []hookEvent
type hookEventDetailMsg  hookEvent
type hooksErrMsg         struct{ err error }
type hookDeletedMsg      struct{}

// ── Views ─────────────────────────────────────────────────────────────────────

type hooksViewID int

const (
	hooksViewRules hooksViewID = iota
	hooksViewEvents
	hooksViewEventDetail
)

// ── Model ─────────────────────────────────────────────────────────────────────

type hooksModel struct {
	view    hooksViewID
	loading bool
	err     error
	width   int
	height  int

	rules      []hookRule
	events     []hookEvent
	selRule    *hookRule
	selEvent   *hookEvent
	confirmDel bool

	rTable table.Model
	eTable table.Model
	vp     viewport.Model
}

func newHooksModel() hooksModel {
	rTable := table.New(
		table.WithColumns([]table.Column{
			{Title: "NAME",    Width: 20},
			{Title: "REPO",    Width: 24},
			{Title: "EVENTS",  Width: 20},
			{Title: "ACTIVE",  Width: 7},
			{Title: "CREATED", Width: 14},
		}),
		table.WithFocused(true),
		table.WithHeight(14),
	)
	rTable.SetStyles(tuiTableStyles())

	eTable := table.New(
		table.WithColumns([]table.Column{
			{Title: "ID",      Width: 10},
			{Title: "TYPE",    Width: 16},
			{Title: "REPO",    Width: 24},
			{Title: "REF",     Width: 16},
			{Title: "MATCHED", Width: 8},
			{Title: "TIME",    Width: 14},
		}),
		table.WithFocused(true),
		table.WithHeight(14),
	)
	eTable.SetStyles(tuiTableStyles())

	return hooksModel{
		loading: true,
		rTable:  rTable,
		eTable:  eTable,
		vp:      viewport.New(100, 20),
	}
}

// ── Fetch commands ────────────────────────────────────────────────────────────

func hooksFetchRules() tea.Msg {
	data, err := doRequest("GET", "/hooks/rules", nil)
	if err != nil {
		return hooksErrMsg{err}
	}
	var rules []hookRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return hooksErrMsg{err}
	}
	return hookRulesMsg(rules)
}

func hooksFetchEvents(repo string) tea.Cmd {
	return func() tea.Msg {
		path := "/hooks/events"
		if repo != "" {
			path += "?repo=" + url.QueryEscape(repo)
		}
		data, err := doRequest("GET", path, nil)
		if err != nil {
			return hooksErrMsg{err}
		}
		var events []hookEvent
		if err := json.Unmarshal(data, &events); err != nil {
			return hooksErrMsg{err}
		}
		return hookEventsMsg(events)
	}
}

func hooksFetchEventDetail(eventID string) tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("GET", "/hooks/events/"+eventID, nil)
		if err != nil {
			return hooksErrMsg{err}
		}
		var e hookEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return hooksErrMsg{err}
		}
		return hookEventDetailMsg(e)
	}
}

func hooksDeleteRule(ruleID string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/hooks/rules/"+ruleID, nil); err != nil {
			return hooksErrMsg{err}
		}
		return hookDeletedMsg{}
	}
}

// ── Init ──────────────────────────────────────────────────────────────────────

func (m hooksModel) Init() tea.Cmd { return hooksFetchRules }

// ── Update ────────────────────────────────────────────────────────────────────

func (m hooksModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 8
		return m, nil

	case hooksErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case hookRulesMsg:
		m.loading = false
		m.rules = []hookRule(msg)
		rows := make([]table.Row, len(m.rules))
		for i, r := range m.rules {
			active := "●"
			if !r.Active {
				active = "○"
			}
			rows[i] = table.Row{
				tuiTrunc(r.Name, 20),
				tuiTrunc(r.Repo, 24),
				tuiTrunc(strings.Join(r.Events, ","), 20),
				active,
				r.CreatedAt.Local().Format("Jan 02 15:04"),
			}
		}
		m.rTable.SetRows(rows)
		return m, nil

	case hookEventsMsg:
		m.loading = false
		m.events = []hookEvent(msg)
		rows := make([]table.Row, len(m.events))
		for i, e := range m.events {
			rows[i] = table.Row{
				tuiShortID(e.EventID),
				tuiTrunc(e.EventType, 16),
				tuiTrunc(e.Repo, 24),
				tuiTrunc(e.Ref, 16),
				fmt.Sprintf("%d", e.RulesMatched),
				e.CreatedAt.Local().Format("Jan 02 15:04"),
			}
		}
		m.eTable.SetRows(rows)
		return m, nil

	case hookEventDetailMsg:
		m.loading = false
		e := hookEvent(msg)
		m.selEvent = &e
		m.vp.SetContent(hooksRenderEventDetail(e))
		m.vp.GotoTop()
		m.view = hooksViewEventDetail
		return m, nil

	case hookDeletedMsg:
		m.loading = true
		m.selRule = nil
		return m, hooksFetchRules

	case tea.KeyMsg:
		if m.err != nil {
			switch msg.String() {
			case "q":
				return m, func() tea.Msg { return goHomeMsg{} }
			case "ctrl+c":
				return m, tea.Quit
			case "r":
				m.err = nil
				m.loading = true
				return m, hooksFetchRules
			}
			return m, nil
		}
		switch m.view {
		case hooksViewRules:
			return m.hooksKeyRules(msg)
		case hooksViewEvents:
			return m.hooksKeyEvents(msg)
		case hooksViewEventDetail:
			return m.hooksKeyEventDetail(msg)
		}
	}
	return m.hooksDelegate(msg)
}

func (m hooksModel) hooksDelegate(msg tea.Msg) (hooksModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case hooksViewRules:
		m.rTable, cmd = m.rTable.Update(msg)
	case hooksViewEvents:
		m.eTable, cmd = m.eTable.Update(msg)
	case hooksViewEventDetail:
		m.vp, cmd = m.vp.Update(msg)
	}
	return m, cmd
}

func (m hooksModel) hooksKeyRules(msg tea.KeyMsg) (hooksModel, tea.Cmd) {
	if m.confirmDel {
		switch msg.String() {
		case "y", "Y":
			m.confirmDel = false
			if m.selRule != nil {
				return m, hooksDeleteRule(m.selRule.RuleID)
			}
		default:
			m.confirmDel = false
		}
		return m, nil
	}
	switch msg.String() {
	case "q":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		i := m.rTable.Cursor()
		if i >= 0 && i < len(m.rules) {
			m.selRule = &m.rules[i]
			m.view = hooksViewEvents
			m.loading = true
			return m, hooksFetchEvents(m.selRule.Repo)
		}
	case "e":
		m.view = hooksViewEvents
		m.selRule = nil
		m.loading = true
		return m, hooksFetchEvents("")
	case "D":
		i := m.rTable.Cursor()
		if i >= 0 && i < len(m.rules) {
			m.selRule = &m.rules[i]
			m.confirmDel = true
		}
	case "r":
		m.loading = true
		return m, hooksFetchRules
	}
	var cmd tea.Cmd
	m.rTable, cmd = m.rTable.Update(msg)
	return m, cmd
}

func (m hooksModel) hooksKeyEvents(msg tea.KeyMsg) (hooksModel, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "b", "esc":
		m.view = hooksViewRules
		return m, nil
	case "enter":
		i := m.eTable.Cursor()
		if i >= 0 && i < len(m.events) {
			m.loading = true
			m.view = hooksViewEventDetail
			return m, hooksFetchEventDetail(m.events[i].EventID)
		}
	case "r":
		m.loading = true
		repo := ""
		if m.selRule != nil {
			repo = m.selRule.Repo
		}
		return m, hooksFetchEvents(repo)
	}
	var cmd tea.Cmd
	m.eTable, cmd = m.eTable.Update(msg)
	return m, cmd
}

func (m hooksModel) hooksKeyEventDetail(msg tea.KeyMsg) (hooksModel, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "b", "esc":
		m.view = hooksViewEvents
		m.selEvent = nil
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m hooksModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" +
			tuiHelpStyle.Render("[q] home  [r] retry")
	}
	switch m.view {
	case hooksViewEvents:
		return m.hooksViewEvents()
	case hooksViewEventDetail:
		return m.hooksViewEventDetail()
	}
	return m.hooksViewRules()
}

func (m hooksModel) hooksViewRules() string {
	title := tuiTitleStyle.Render("Hooks Rules")
	help := tuiHelpStyle.Render("[↑↓/jk] navigate  [enter] events by repo  [e] all events  [D] delete  [r] refresh  [q] home")
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.confirmDel {
		name := ""
		if m.selRule != nil {
			name = " '" + m.selRule.Name + "'"
		}
		confirm := lipgloss.NewStyle().Foreground(lipgloss.Color("#d46b55")).
			Render("Delete rule" + name + "? [y] confirm  [any] cancel")
		if len(m.rules) == 0 {
			return title + "\n\n" + tuiMetaStyle.Render("No rules defined.") + "\n\n" + confirm
		}
		return title + "\n" + tuiBoxStyle.Render(m.rTable.View()) + "\n" + confirm
	}
	if len(m.rules) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No rules defined.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.rTable.View()) + "\n" + help
}

func (m hooksModel) hooksViewEvents() string {
	subtitle := "Events"
	if m.selRule != nil {
		subtitle += ": " + m.selRule.Repo
	}
	title := tuiTitleStyle.Render("Hooks " + subtitle)
	help := tuiHelpStyle.Render("[↑↓/jk] navigate  [enter] detail  [r] refresh  [b] back  [q] home")
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if len(m.events) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No events.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.eTable.View()) + "\n" + help
}

func (m hooksModel) hooksViewEventDetail() string {
	title := tuiTitleStyle.Render("Event Detail")
	if m.selEvent != nil {
		title = tuiTitleStyle.Render(m.selEvent.EventType) + "  " +
			tuiMetaStyle.Render(m.selEvent.Repo)
	}
	help := tuiHelpStyle.Render("[↑↓/pgup/pgdn] scroll  [b] back  [q] home")
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + help
}

func hooksRenderEventDetail(e hookEvent) string {
	var sb strings.Builder
	fmt.Fprintf(&sb,
		"Event ID:      %s\nRepo:          %s\nType:          %s\nRef:           %s\nStatus:        %s\nRules matched: %d\nReceived:      %s\n",
		e.EventID, e.Repo, e.EventType, e.Ref, e.Status,
		e.RulesMatched, e.CreatedAt.Local().Format(time.RFC3339),
	)
	if len(e.Triggers) > 0 {
		sb.WriteString("\nTRIGGERS:\n")
		for i, t := range e.Triggers {
			fmt.Fprintf(&sb, "\n  [%d] rule: %s  workflow: %s  status: %s\n",
				i+1, tuiShortID(t.RuleID), tuiShortID(t.WorkflowID), t.Status,
			)
			if t.RunID != nil {
				fmt.Fprintf(&sb, "      run: %s\n", *t.RunID)
			}
			if t.Error != nil {
				fmt.Fprintf(&sb, "      error: %s\n", *t.Error)
			}
		}
	}
	return sb.String()
}

// ── Command registration ──────────────────────────────────────────────────────

func startHooksTUI() error {
	p := tea.NewProgram(standaloneWrap{newHooksModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func init() {
	hooksCmd.RunE = func(cmd *cobra.Command, args []string) error {
		return startHooksTUI()
	}
	hooksCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Interactive TUI for browsing webhook rules and events",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return startHooksTUI() },
	})
}
