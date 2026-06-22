package cmd

import (
	"encoding/json"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// The Chaos TUI runs chaos-engineering experiments against a cluster via an
// outpost: list, create (pick an experiment type + target workload), inspect,
// and delete. Experiment types come from /chaos/experiment-types and outposts
// from the gateway, both driving form selectors. Mirrors the chaos service API.

type chaosExp struct {
	ExperimentID   string    `json:"experiment_id"`
	ExperimentType string    `json:"experiment_type"`
	OutpostID      string    `json:"outpost_id"`
	TargetAppNS    string    `json:"target_app_ns"`
	TargetAppLabel string    `json:"target_app_label"`
	Status         string    `json:"status"`
	Verdict        string    `json:"verdict"`
	CreatedAt      time.Time `json:"created_at"`
}

type chaosListMsg []chaosExp
type chaosTypesMsg []string
type chaosOutpostsMsg struct {
	names []string
	ids   []string
}
type chaosErrMsg struct{ err error }
type chaosDoneMsg struct{ status string }
type chaosFormErrMsg struct{ err error }

type chaosViewID int

const (
	chaosViewList chaosViewID = iota
	chaosViewForm
	chaosViewDetail
)

type chaosModel struct {
	view    chaosViewID
	loading bool
	err     error
	width   int
	height  int

	exps         []chaosExp
	types        []string // experiment-type names for the form selector
	outpostNames []string // outpost name/id pairs for the form selector
	outpostIDs   []string

	table table.Model
	form  tuiForm
	vp    viewport.Model

	pending   *frPending
	status    string
	statusErr bool
}

var chaosCols = []tuiColSpec{{"TYPE", 16, 1}, {"TARGET", 22, 2}, {"STATUS", 10, 0}, {"VERDICT", 9, 0}, {"CREATED", 14, 0}}

func newChaosModel() chaosModel {
	t := table.New(table.WithFocused(true))
	t.SetStyles(tuiTableStyles())
	m := chaosModel{loading: true, width: tuiDefaultWidth, height: tuiDefaultHeight, table: t, vp: viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-8)}
	m.applyTableLayout()
	return m
}

func (m *chaosModel) applyTableLayout() {
	m.table.SetColumns(tuiFitColumns(chaosCols, m.width))
	m.table.SetHeight(tuiTableHeight(m.height, tuiListChrome))
}

// ── Fetch / mutate ────────────────────────────────────────────────────────────

func chaosFetch() tea.Msg {
	data, err := doRequest("GET", "/chaos/experiments", nil)
	if err != nil {
		return chaosErrMsg{err}
	}
	var exps []chaosExp
	if err := json.Unmarshal(data, &exps); err != nil {
		return chaosErrMsg{err}
	}
	return chaosListMsg(exps)
}

// chaosFetchTypes loads the experiment-type catalog for the form selector,
// degrading to an empty list on failure (the field then falls back to free text).
func chaosFetchTypes() tea.Msg {
	data, err := doRequest("GET", "/chaos/experiment-types", nil)
	if err != nil {
		return chaosTypesMsg(nil)
	}
	var ts []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &ts); err != nil {
		return chaosTypesMsg(nil)
	}
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	return chaosTypesMsg(names)
}

// chaosFetchOutposts loads outpost name→id pairs for the form's outpost selector.
func chaosFetchOutposts() tea.Msg {
	data, err := doRequest("GET", "/outpost-gateway/outposts", nil)
	if err != nil {
		return chaosOutpostsMsg{}
	}
	var ops []struct {
		OutpostID string `json:"outpost_id"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(data, &ops); err != nil {
		return chaosOutpostsMsg{}
	}
	var names, ids []string
	for _, o := range ops {
		if o.OutpostID == "" {
			continue
		}
		names = append(names, frDash(o.Name))
		ids = append(ids, o.OutpostID)
	}
	return chaosOutpostsMsg{names: names, ids: ids}
}

func chaosDelete(id string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/chaos/experiments/"+id, nil); err != nil {
			return chaosErrMsg{err}
		}
		return chaosDoneMsg{status: "✓ experiment deleted"}
	}
}

func chaosCreate(payload map[string]any) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(payload)
		if _, err := doRequest("POST", "/chaos/experiments", body); err != nil {
			return chaosFormErrMsg{err}
		}
		return chaosDoneMsg{status: "✓ experiment started"}
	}
}

// ── Init / Update ─────────────────────────────────────────────────────────────

func (m chaosModel) Init() tea.Cmd {
	return tea.Batch(chaosFetch, chaosFetchTypes, chaosFetchOutposts)
}

func (m chaosModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 8
		m.applyTableLayout()
		return m, nil
	case chaosErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	case chaosListMsg:
		m.loading = false
		m.exps = []chaosExp(msg)
		rows := make([]table.Row, len(m.exps))
		for i, e := range m.exps {
			rows[i] = table.Row{frDash(e.ExperimentType), chaosTarget(e), frDash(e.Status), frDash(e.Verdict), e.CreatedAt.Local().Format("Jan 02 15:04")}
		}
		m.table.SetRows(rows)
		return m, nil
	case chaosTypesMsg:
		m.types = []string(msg)
		return m, nil
	case chaosOutpostsMsg:
		m.outpostNames = msg.names
		m.outpostIDs = msg.ids
		return m, nil
	case chaosDoneMsg:
		m.view = chaosViewList
		m.status = msg.status
		m.statusErr = false
		m.loading = true
		return m, chaosFetch
	case chaosFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil
	case tuiAutoRefreshMsg:
		if m.view == chaosViewList {
			return m, chaosFetch
		}
		return m, nil
	case tea.KeyMsg:
		if m.err != nil {
			switch msg.String() {
			case "esc":
				return m, func() tea.Msg { return goHomeMsg{} }
			case "ctrl+c":
				return m, tea.Quit
			case "r":
				m.err = nil
				m.loading = true
				return m, chaosFetch
			}
			return m, nil
		}
		switch m.view {
		case chaosViewList:
			return m.keyList(msg)
		case chaosViewForm:
			return m.keyForm(msg)
		case chaosViewDetail:
			return m.keyDetail(msg)
		}
	}
	var cmd tea.Cmd
	switch m.view {
	case chaosViewList:
		m.table, cmd = m.table.Update(msg)
	case chaosViewForm:
		m.form, _, cmd = m.form.update(msg)
	case chaosViewDetail:
		m.vp, cmd = m.vp.Update(msg)
	}
	return m, cmd
}

func chaosTarget(e chaosExp) string {
	if e.TargetAppNS == "" {
		return frDash(e.TargetAppLabel)
	}
	return e.TargetAppNS + "/" + e.TargetAppLabel
}

func (m chaosModel) keyList(msg tea.KeyMsg) (chaosModel, tea.Cmd) {
	if m.pending != nil {
		run := m.pending.run
		m.pending = nil
		if s := msg.String(); s == "y" || s == "Y" {
			return m, run
		}
		return m, nil
	}
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		if i := m.table.Cursor(); i >= 0 && i < len(m.exps) {
			pretty, _ := json.MarshalIndent(m.exps[i], "", "  ")
			m.vp.SetContent(string(pretty))
			m.vp.GotoTop()
			m.view = chaosViewDetail
			return m, nil
		}
	case "n":
		m.form, _ = m.newChaosForm()
		m.view = chaosViewForm
		return m, nil
	case "D":
		if i := m.table.Cursor(); i >= 0 && i < len(m.exps) {
			e := m.exps[i]
			m.pending = &frPending{prompt: "Delete experiment " + tuiShortID(e.ExperimentID) + "? [y] confirm  [any] cancel", run: chaosDelete(e.ExperimentID)}
			return m, nil
		}
	case "r":
		m.loading = true
		m.status = ""
		return m, chaosFetch
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m chaosModel) newChaosForm() (tuiForm, tea.Cmd) {
	var typeField formField
	if len(m.types) > 0 {
		typeField = formSelect("type", "Type", m.types)
	} else {
		typeField = formInput("type", "Type", "experiment type (required)")
	}
	var outpostField formField
	if len(m.outpostIDs) > 0 {
		outpostField = formSelectKV("outpost", "Outpost", m.outpostNames, m.outpostIDs)
	} else {
		outpostField = formInput("outpost", "Outpost", "outpost_id (required)")
	}
	return newTUIForm("New Experiment",
		typeField,
		outpostField,
		formInput("ns", "Namespace", "target namespace (required)"),
		formInput("label", "Label", "app.kubernetes.io/name=foo (required)"),
		formInputDefault("kind", "Kind", "deployment", "deployment"),
		formTextarea("params", "Params", "key=value per line (optional)"))
}

func (m chaosModel) keyForm(msg tea.KeyMsg) (chaosModel, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	var (
		action formAction
		cmd    tea.Cmd
	)
	m.form, action, cmd = m.form.update(msg)
	switch action {
	case formCancel:
		m.view = chaosViewList
		return m, nil
	case formSubmit:
		etype := m.form.value("type")
		outpost := m.form.value("outpost")
		ns := m.form.value("ns")
		label := m.form.value("label")
		switch {
		case etype == "":
			m.form.errMsg = "experiment type is required"
		case outpost == "":
			m.form.errMsg = "an outpost is required"
		case ns == "":
			m.form.errMsg = "namespace is required"
		case label == "":
			m.form.errMsg = "target label is required"
		default:
			params, err := parseKVLines(m.form.value("params"))
			if err != nil {
				m.form.errMsg = "params: " + err.Error()
				return m, nil
			}
			m.form.errMsg = ""
			payload := map[string]any{
				"outpost_id":       outpost,
				"experiment_type":  etype,
				"target_app_ns":    ns,
				"target_app_label": label,
				"target_app_kind":  m.form.value("kind"),
				"params":           params,
			}
			return m, chaosCreate(payload)
		}
		return m, nil
	}
	return m, cmd
}

func (m chaosModel) keyDetail(msg tea.KeyMsg) (chaosModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = chaosViewList
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── View ────────────────────────────────────────────────────────────────────

func (m chaosModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" + tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	switch m.view {
	case chaosViewForm:
		return m.form.view(m.width, m.height)
	case chaosViewDetail:
		return tuiTitleStyle.Render("Experiment") + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + tuiHelp("[↑↓/pgup/pgdn] scroll  [esc] back", m.width)
	}
	title := tuiTitleStyle.Render("Chaos Experiments")
	help := tuiHelp("[↑↓/jk] nav  [enter] detail  [n] new  [D] delete  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.pending != nil {
		body := tuiMetaStyle.Render("No experiments.")
		if len(m.exps) > 0 {
			body = tuiBoxStyle.Render(m.table.View())
		}
		return title + "\n" + body + "\n" + tuiErrStyle.Render(m.pending.prompt)
	}
	status := ""
	if m.status != "" {
		status = tuiMetaStyle.Render(m.status) + "\n"
	}
	if len(m.exps) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No experiments. Press [n] to start one.") + "\n\n" + status + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.table.View()) + "\n" + status + help
}

// ── Command registration ──────────────────────────────────────────────────────

func startChaosTUI() error {
	p := tea.NewProgram(standaloneWrap{newChaosModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

var chaosCmd = &cobra.Command{
	Use:   "chaos",
	Short: "Run chaos-engineering experiments (TUI)",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, args []string) error { return startChaosTUI() },
}

func init() {
	chaosCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Interactive TUI for chaos experiments",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return startChaosTUI() },
	})
	RegisterModule(Module{
		Name:    "chaos",
		Service: "chaos",
		Order:   45,
		Command: chaosCmd,
		Screens: []HubScreen{{
			Title: "Chaos",
			Desc:  "Run and track chaos-engineering experiments",
			New:   func() tea.Model { return newChaosModel() },
		}},
	})
}
