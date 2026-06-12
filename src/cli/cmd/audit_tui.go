package cmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// ── Types ─────────────────────────────────────────────────────────────────────

type auditEntry struct {
	AuditLogID string    `json:"audit_log_id"`
	ActorID    string    `json:"actor_id"`
	ActorType  string    `json:"actor_type"`
	Action     string    `json:"action"`
	ResourceID string    `json:"resource_id"`
	Detail     string    `json:"detail"`
	CreatedAt  time.Time `json:"created_at"`
}

// ── Messages ──────────────────────────────────────────────────────────────────

type auditEntriesMsg []auditEntry
type auditErrMsg     struct{ err error }

// ── Views ─────────────────────────────────────────────────────────────────────

type auditViewID int

const (
	auditViewList auditViewID = iota
	auditViewDetail
)

// ── Model ─────────────────────────────────────────────────────────────────────

type auditModel struct {
	view    auditViewID
	loading bool
	err     error
	width   int
	height  int

	entries  []auditEntry
	selEntry *auditEntry
	page     int

	aTable table.Model
	vp     viewport.Model
}

const auditPageSize = 50

func newAuditModel() auditModel {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "ACTOR",    Width: 22},
			{Title: "TYPE",     Width: 8},
			{Title: "ACTION",   Width: 22},
			{Title: "RESOURCE", Width: 12},
			{Title: "TIME",     Width: 14},
		}),
		table.WithFocused(true),
		table.WithHeight(16),
	)
	t.SetStyles(tuiTableStyles())
	return auditModel{
		loading: true,
		aTable:  t,
		vp:      viewport.New(100, 20),
	}
}

// ── Fetch commands ────────────────────────────────────────────────────────────

func auditFetch(page int) tea.Cmd {
	return func() tea.Msg {
		path := fmt.Sprintf("/gatekeeper/audit-logs?limit=%d&offset=%d", auditPageSize, page*auditPageSize)
		data, err := doRequest("GET", path, nil)
		if err != nil {
			return auditErrMsg{err}
		}
		var entries []auditEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			return auditErrMsg{err}
		}
		return auditEntriesMsg(entries)
	}
}

// ── Init ──────────────────────────────────────────────────────────────────────

func (m auditModel) Init() tea.Cmd { return auditFetch(0) }

// ── Update ────────────────────────────────────────────────────────────────────

func (m auditModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 8
		return m, nil

	case auditErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case auditEntriesMsg:
		m.loading = false
		m.entries = []auditEntry(msg)
		rows := make([]table.Row, len(m.entries))
		for i, e := range m.entries {
			rows[i] = table.Row{
				tuiTrunc(e.ActorID, 22),
				e.ActorType,
				tuiTrunc(e.Action, 22),
				tuiShortID(e.ResourceID),
				e.CreatedAt.Local().Format("Jan 02 15:04"),
			}
		}
		m.aTable.SetRows(rows)
		return m, nil

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
				return m, auditFetch(m.page)
			}
			return m, nil
		}
		switch m.view {
		case auditViewList:
			return m.auditKeyList(msg)
		case auditViewDetail:
			return m.auditKeyDetail(msg)
		}
	}
	return m.auditDelegate(msg)
}

func (m auditModel) auditDelegate(msg tea.Msg) (auditModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case auditViewList:
		m.aTable, cmd = m.aTable.Update(msg)
	case auditViewDetail:
		m.vp, cmd = m.vp.Update(msg)
	}
	return m, cmd
}

func (m auditModel) auditKeyList(msg tea.KeyMsg) (auditModel, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		i := m.aTable.Cursor()
		if i >= 0 && i < len(m.entries) {
			e := m.entries[i]
			m.selEntry = &e
			content := fmt.Sprintf(
				"ID:        %s\nActor:     %s (%s)\nAction:    %s\nResource:  %s\nTime:      %s\n\nDetail:\n%s",
				e.AuditLogID, e.ActorID, e.ActorType,
				e.Action, e.ResourceID,
				e.CreatedAt.Local().Format(time.RFC3339),
				e.Detail,
			)
			m.vp.SetContent(content)
			m.vp.GotoTop()
			m.view = auditViewDetail
		}
	case "]":
		if len(m.entries) == auditPageSize {
			m.page++
			m.loading = true
			return m, auditFetch(m.page)
		}
	case "[":
		if m.page > 0 {
			m.page--
			m.loading = true
			return m, auditFetch(m.page)
		}
	case "r":
		m.loading = true
		return m, auditFetch(m.page)
	}
	var cmd tea.Cmd
	m.aTable, cmd = m.aTable.Update(msg)
	return m, cmd
}

func (m auditModel) auditKeyDetail(msg tea.KeyMsg) (auditModel, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "b", "esc":
		m.view = auditViewList
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m auditModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" +
			tuiHelpStyle.Render("[q] home  [r] retry")
	}
	if m.view == auditViewDetail {
		return m.auditViewDetail()
	}
	return m.auditViewList()
}

func (m auditModel) auditViewList() string {
	title := tuiTitleStyle.Render("Audit Log")
	pageInfo := ""
	if m.page > 0 {
		pageInfo = tuiMetaStyle.Render(fmt.Sprintf("  page %d", m.page+1))
	}
	help := tuiHelpStyle.Render("[↑↓/jk] navigate  [enter] detail  [] next/prev page  [r] refresh  [q] home")
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if len(m.entries) == 0 {
		if m.page > 0 {
			return title + pageInfo + "\n\n" + tuiMetaStyle.Render("No more entries.") + "\n\n" + help
		}
		return title + "\n\n" + tuiMetaStyle.Render("No audit entries found.") + "\n\n" + help
	}
	return title + pageInfo + "\n" + tuiBoxStyle.Render(m.aTable.View()) + "\n" + help
}

func (m auditModel) auditViewDetail() string {
	title := tuiTitleStyle.Render("Audit Entry")
	if m.selEntry != nil {
		title = tuiTitleStyle.Render(m.selEntry.Action) + "  " +
			tuiMetaStyle.Render(m.selEntry.ActorID)
	}
	help := tuiHelpStyle.Render("[↑↓/pgup/pgdn] scroll  [b] back  [q] home")
	return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + help
}

// ── Command registration ──────────────────────────────────────────────────────

func startAuditTUI() error {
	p := tea.NewProgram(standaloneWrap{newAuditModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func init() {
	auditCmd.RunE = func(cmd *cobra.Command, args []string) error {
		return startAuditTUI()
	}
	auditCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Interactive TUI for browsing audit logs",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return startAuditTUI() },
	})
}
