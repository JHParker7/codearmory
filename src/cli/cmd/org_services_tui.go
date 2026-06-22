package cmd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// The Org Services TUI is the admin screen for the builder service: it lets an
// org admin enable, disable, configure, register and remove the services that are
// available to their org. A null/"default" org is the baseline every org inherits;
// the screen can target either the admin's own org or that default scope (switched
// with [s]). Effective state comes from builder, which overlays the live service
// catalog with the default baseline and the org's overrides, so the list shows
// every available service, whether it is on, and where that state comes from.
//
// Permission enforcement stays server-side: a user without the org-admin grant
// (or who targets the default scope without platform-admin rights) still sees the
// action but gets a clear 403 in the status line, matching the other admin TUIs.

// ── Records ─────────────────────────────────────────────────────────────────

// osvRecord is one decoded serviceView object from builder. It shares the
// gatekeeper/forge record helpers (gkStr/frBoolDot/frMapCount) on map[string]any.
type osvRecord = map[string]any

func osvBool(r osvRecord, key string) bool {
	b, _ := r[key].(bool)
	return b
}

// osvConfigJSON renders a record's free-form config object as indented JSON for
// the config textarea. Empty/absent config renders as an empty string.
func osvConfigJSON(r osvRecord) string {
	cfg, ok := r["config"].(map[string]any)
	if !ok || len(cfg) == 0 {
		return ""
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// parseConfigJSON parses the config textarea into a JSON object. Blank input is a
// nil config (no config); non-object JSON is rejected so the stored shape stays a
// map the services and the Phase 2 controller can consume.
func parseConfigJSON(s string) (map[string]any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(s), &cfg); err != nil {
		return nil, fmt.Errorf("must be a JSON object: %w", err)
	}
	return cfg, nil
}

// ── Views / forms ───────────────────────────────────────────────────────────

type osvViewID int

const (
	osvViewList osvViewID = iota
	osvViewDetail
	osvViewForm
)

type osvFormKind int

const (
	osvFormNone   osvFormKind = iota
	osvFormConfig             // configure an existing service (enabled + config [+ custom fields])
	osvFormCustom             // register a new custom service
)

type osvPending struct {
	prompt string
	run    tea.Cmd
}

// ── Messages ────────────────────────────────────────────────────────────────

type osvRecordsMsg struct {
	scopeMode string
	scopeID   string
	records   []osvRecord
}
type osvErrMsg struct{ err error }
type osvActionMsg struct {
	label string
	err   error
}
type osvFormErrMsg struct{ err error }
type osvFormDoneMsg struct{ status string }

// ── Model ───────────────────────────────────────────────────────────────────

var osvCols = []tuiColSpec{
	{"SERVICE", 20, 2},
	{"ENABLED", 8, 0},
	{"KIND", 9, 0},
	{"SOURCE", 10, 1},
	{"CONFIG", 7, 0},
}

func osvRow(r osvRecord) table.Row {
	return table.Row{
		gkStr(r, "service"),
		frBoolDot(r, "enabled"),
		frDash(gkStr(r, "kind")),
		frDash(gkStr(r, "source")),
		frMapCount(r, "config"),
	}
}

type orgServicesModel struct {
	scopeMode string // "org" (the admin's own org) | "default" (the baseline)
	scopeID   string // resolved id used in PUT/DELETE paths ("default" or an org uuid)

	view    osvViewID
	loading bool
	err     error
	width   int
	height  int

	records []osvRecord
	table   table.Model
	vp      viewport.Model

	form       tuiForm
	formKind   osvFormKind
	formMode   string // "create" | "edit"
	editTarget string // service name being configured (PUT path segment)
	editKind   string // kind of the record being edited ("platform"|"custom")

	pending *osvPending

	selLabel string

	status    string
	statusErr bool
}

func newOrgServicesModel() orgServicesModel {
	m := orgServicesModel{
		scopeMode: "org",
		loading:   true,
		width:     tuiDefaultWidth,
		height:    tuiDefaultHeight,
		vp:        viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-9),
	}
	t := table.New(table.WithFocused(true))
	t.SetStyles(tuiTableStyles())
	m.table = t
	m.applyTableLayout()
	return m
}

func (m *orgServicesModel) applyTableLayout() {
	m.table.SetColumns(tuiFitColumns(osvCols, m.width))
	m.table.SetHeight(tuiTableHeight(m.height, tuiListChrome))
}

func (m *orgServicesModel) currentRecord() (osvRecord, bool) {
	i := m.table.Cursor()
	if i >= 0 && i < len(m.records) {
		return m.records[i], true
	}
	return nil, false
}

// ── Fetch / mutate ──────────────────────────────────────────────────────────

// osvScopePath resolves the path id for a scope mode. "org" resolves to the
// caller's org id; a caller with no org (or the explicit "default" mode) targets
// the default baseline scope, which they inherit.
func osvScopePath(scopeMode string) string {
	if scopeMode == "org" {
		if oid, err := myFieldID("org_id"); err == nil && oid != "" {
			return oid
		}
	}
	return "default"
}

func osvFetch(scopeMode string) tea.Cmd {
	return func() tea.Msg {
		id := osvScopePath(scopeMode)
		data, err := doRequest("GET", "/builder/orgs/"+id+"/services", nil)
		if err != nil {
			return osvErrMsg{err}
		}
		var recs []osvRecord
		if err := json.Unmarshal(data, &recs); err != nil {
			return osvErrMsg{err}
		}
		return osvRecordsMsg{scopeMode: scopeMode, scopeID: id, records: recs}
	}
}

func osvSet(scopeID, service string, payload map[string]any) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(payload)
		if _, err := doRequest("PUT", "/builder/orgs/"+scopeID+"/services/"+service, body); err != nil {
			return osvFormErrMsg{err}
		}
		verb := "updated"
		if payload["__create"] == true {
			verb = "registered"
		}
		return osvFormDoneMsg{status: "✓ " + service + " " + verb}
	}
}

// osvToggle flips enabled for a record without a form, preserving its kind and
// config so a quick on/off never drops the rest of the row's desired state.
func osvToggle(scopeID string, rec osvRecord) tea.Cmd {
	payload := map[string]any{
		"enabled": !osvBool(rec, "enabled"),
		"kind":    gkStr(rec, "kind"),
	}
	if cfg, ok := rec["config"].(map[string]any); ok && len(cfg) > 0 {
		payload["config"] = cfg
	}
	if gkStr(rec, "kind") == "custom" {
		payload["image"] = gkStr(rec, "image")
		payload["port"] = rec["port"]
		payload["description"] = gkStr(rec, "description")
	}
	service := gkStr(rec, "service")
	return func() tea.Msg {
		body, _ := json.Marshal(payload)
		if _, err := doRequest("PUT", "/builder/orgs/"+scopeID+"/services/"+service, body); err != nil {
			return osvActionMsg{label: "toggle", err: err}
		}
		return osvActionMsg{label: "toggle"}
	}
}

func osvDelete(scopeID, service string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/builder/orgs/"+scopeID+"/services/"+service, nil); err != nil {
			return osvActionMsg{label: "remove", err: err}
		}
		return osvActionMsg{label: "remove"}
	}
}

// ── Init / Update ───────────────────────────────────────────────────────────

func (m orgServicesModel) Init() tea.Cmd { return osvFetch(m.scopeMode) }

func (m orgServicesModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 9
		m.applyTableLayout()
		return m, nil

	case osvErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case osvRecordsMsg:
		m.loading = false
		m.scopeID = msg.scopeID
		m.records = msg.records
		rows := make([]table.Row, len(msg.records))
		for i, r := range msg.records {
			rows[i] = osvRow(r)
		}
		m.table.SetRows(rows)
		return m, nil

	case osvActionMsg:
		if msg.err != nil {
			m.status = "✗ " + msg.label + ": " + msg.err.Error()
			m.statusErr = true
			return m, nil
		}
		m.status = "✓ " + msg.label + "d"
		m.statusErr = false
		m.loading = true
		return m, osvFetch(m.scopeMode)

	case osvFormDoneMsg:
		m.view = osvViewList
		m.formKind = osvFormNone
		m.status = msg.status
		m.statusErr = false
		m.loading = true
		return m, osvFetch(m.scopeMode)

	case osvFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil

	case tuiAutoRefreshMsg:
		if m.view == osvViewList {
			return m, osvFetch(m.scopeMode)
		}
		return m, nil

	case tea.KeyMsg:
		if m.err != nil {
			switch msg.String() {
			case "esc":
				return m, osvGoHome
			case "ctrl+c":
				return m, tea.Quit
			case "r":
				m.err = nil
				m.loading = true
				return m, osvFetch(m.scopeMode)
			}
			return m, nil
		}
		switch m.view {
		case osvViewList:
			return m.keyList(msg)
		case osvViewDetail:
			return m.keyDetail(msg)
		case osvViewForm:
			return m.keyForm(msg)
		}
	}
	return m.delegate(msg)
}

func osvGoHome() tea.Msg { return goHomeMsg{} }

func (m orgServicesModel) delegate(msg tea.Msg) (orgServicesModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case osvViewList:
		m.table, cmd = m.table.Update(msg)
	case osvViewDetail:
		m.vp, cmd = m.vp.Update(msg)
	case osvViewForm:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

func (m orgServicesModel) keyList(msg tea.KeyMsg) (orgServicesModel, tea.Cmd) {
	if m.pending != nil {
		run := m.pending.run
		m.pending = nil
		switch msg.String() {
		case "y", "Y":
			return m, run
		default:
			return m, nil
		}
	}

	switch msg.String() {
	case "esc":
		return m, osvGoHome
	case "ctrl+c":
		return m, tea.Quit
	case "s":
		// Switch scope between the admin's own org and the default baseline.
		if m.scopeMode == "org" {
			m.scopeMode = "default"
		} else {
			m.scopeMode = "org"
		}
		m.loading = true
		m.status = ""
		return m, osvFetch(m.scopeMode)
	case "enter":
		if rec, ok := m.currentRecord(); ok {
			m.selLabel = gkStr(rec, "service")
			m.view = osvViewDetail
			pretty, _ := json.MarshalIndent(rec, "", "  ")
			m.vp.SetContent(string(pretty))
			m.vp.GotoTop()
		}
		return m, nil
	case " ", "t":
		if rec, ok := m.currentRecord(); ok {
			if osvBool(rec, "core") {
				m.status = "✗ core services cannot be toggled"
				m.statusErr = true
				return m, nil
			}
			return m, osvToggle(m.scopeID, rec)
		}
	case "e", "c":
		if rec, ok := m.currentRecord(); ok {
			if osvBool(rec, "core") {
				m.status = "✗ core services cannot be configured"
				m.statusErr = true
				return m, nil
			}
			return m.openConfigForm(rec)
		}
	case "n":
		return m.openCustomForm()
	case "D":
		if rec, ok := m.currentRecord(); ok {
			service := gkStr(rec, "service")
			src := gkStr(rec, "source")
			if src != "override" && src != "custom" {
				m.status = "✗ nothing to remove (not an override) — disable it instead"
				m.statusErr = true
				return m, nil
			}
			m.pending = &osvPending{
				prompt: fmt.Sprintf("Remove %q override for this scope? [y] confirm  [any] cancel", service),
				run:    osvDelete(m.scopeID, service),
			}
			return m, nil
		}
	case "r":
		m.loading = true
		m.status = ""
		return m, osvFetch(m.scopeMode)
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m orgServicesModel) keyDetail(msg tea.KeyMsg) (orgServicesModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = osvViewList
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── Forms ─────────────────────────────────────────────────────────────────

// openConfigForm opens the enable/configure dialog for an existing service. For a
// custom service the image/port/description fields are included so they can be
// edited too.
func (m orgServicesModel) openConfigForm(rec osvRecord) (orgServicesModel, tea.Cmd) {
	m.formMode = "edit"
	m.formKind = osvFormConfig
	m.editTarget = gkStr(rec, "service")
	m.editKind = gkStr(rec, "kind")

	enabledDefault := "true"
	if !osvBool(rec, "enabled") {
		enabledDefault = "false"
	}
	fields := []formField{
		formSelectDefault("enabled", "Enabled", []string{"true", "false"}, enabledDefault),
	}
	if m.editKind == "custom" {
		fields = append(fields,
			formInputDefault("image", "Image", "repo/image:tag", gkStr(rec, "image")),
			formInputDefault("port", "Port", "1-65535", osvPortStr(rec)),
			formInputDefault("description", "Description", "", gkStr(rec, "description")),
		)
	}
	fields = append(fields,
		formTextarea("config", "Config (JSON)", "{\n  \"key\": \"value\"\n}"),
		formPassword("db_url", "DB URL", "postgres://… (blank = keep current)"),
	)

	f, cmd := newTUIForm("Configure "+m.editTarget, fields...)
	dbState := "no DB URL set"
	if v, ok := rec["db_configured"].(bool); ok && v {
		dbState = "DB URL set (" + gkStr(rec, "db_host") + ")"
	}
	f.help = "Enable/disable + free-form JSON config. DB URL is write-only (" + dbState + ")."
	f.setValues(map[string]string{"config": osvConfigJSON(rec)})
	m.form = f
	m.view = osvViewForm
	return m, cmd
}

// openCustomForm opens the register-a-custom-service dialog.
func (m orgServicesModel) openCustomForm() (orgServicesModel, tea.Cmd) {
	m.formMode = "create"
	m.formKind = osvFormCustom
	m.editTarget = ""
	m.editKind = "custom"

	f, cmd := newTUIForm("Register Custom Service",
		formInput("service", "Service name", "e.g. my-service (required)"),
		formInput("image", "Image", "repo/image:tag (required)"),
		formInput("port", "Port", "1-65535 (required)"),
		formInput("description", "Description", "optional"),
		formSelectDefault("enabled", "Enabled", []string{"true", "false"}, "true"),
		formTextarea("config", "Config (JSON)", "{\n  \"key\": \"value\"\n}"),
		formPassword("db_url", "DB URL", "postgres://… (optional)"),
	)
	f.help = "Declare a service for this org. Phase 1 records the desired state; the Phase 2 controller deploys it."
	m.form = f
	m.view = osvViewForm
	return m, cmd
}

func osvPortStr(r osvRecord) string {
	if v, ok := r["port"].(float64); ok && v != 0 {
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

func (m orgServicesModel) keyForm(msg tea.KeyMsg) (orgServicesModel, tea.Cmd) {
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
		m.view = osvViewList
		m.formKind = osvFormNone
		return m, nil
	case formSubmit:
		return m.submitForm()
	}
	return m, cmd
}

func (m orgServicesModel) submitForm() (orgServicesModel, tea.Cmd) {
	cfg, err := parseConfigJSON(m.form.value("config"))
	if err != nil {
		m.form.errMsg = "config: " + err.Error()
		return m, nil
	}

	switch m.formKind {
	case osvFormConfig:
		payload := map[string]any{
			"enabled": m.form.value("enabled") == "true",
			"kind":    m.editKind,
			"config":  cfg,
		}
		if m.editKind == "custom" {
			port, perr := strconv.Atoi(strings.TrimSpace(m.form.value("port")))
			if perr != nil || port <= 0 || port > 65535 {
				m.form.errMsg = "port must be 1-65535"
				return m, nil
			}
			payload["image"] = m.form.value("image")
			payload["port"] = port
			payload["description"] = m.form.value("description")
		}
		if db := strings.TrimSpace(m.form.value("db_url")); db != "" {
			payload["db_url"] = db
		}
		m.form.errMsg = ""
		return m, osvSet(m.scopeID, m.editTarget, payload)

	case osvFormCustom:
		service := strings.TrimSpace(m.form.value("service"))
		if service == "" {
			m.form.errMsg = "service name is required"
			return m, nil
		}
		image := strings.TrimSpace(m.form.value("image"))
		if image == "" {
			m.form.errMsg = "image is required"
			return m, nil
		}
		port, perr := strconv.Atoi(strings.TrimSpace(m.form.value("port")))
		if perr != nil || port <= 0 || port > 65535 {
			m.form.errMsg = "port must be 1-65535"
			return m, nil
		}
		payload := map[string]any{
			"__create":    true,
			"enabled":     m.form.value("enabled") == "true",
			"kind":        "custom",
			"image":       image,
			"port":        port,
			"description": m.form.value("description"),
			"config":      cfg,
		}
		if db := strings.TrimSpace(m.form.value("db_url")); db != "" {
			payload["db_url"] = db
		}
		m.form.errMsg = ""
		return m, osvSet(m.scopeID, service, payload)
	}
	return m, nil
}

// ── View ────────────────────────────────────────────────────────────────────

func (m orgServicesModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" +
			tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	switch m.view {
	case osvViewForm:
		return m.form.view(m.width, m.height)
	case osvViewDetail:
		return m.viewDetail()
	}
	return m.viewList()
}

func (m orgServicesModel) scopeLabel() string {
	if m.scopeMode == "default" {
		return "default baseline (inherited by all orgs)"
	}
	return "your org"
}

func (m orgServicesModel) listHelp() string {
	parts := []string{
		"[↑↓/jk] nav", "[space] toggle", "[c] configure", "[n] new custom",
		"[D] remove", "[s] scope", "[enter] detail", "[r] refresh", "[esc] home",
	}
	return strings.Join(parts, "  ")
}

func (m orgServicesModel) statusLine() string {
	if m.status == "" {
		return ""
	}
	if m.statusErr {
		return tuiErrStyle.Render(m.status) + "\n"
	}
	return tuiMetaStyle.Render(m.status) + "\n"
}

func (m orgServicesModel) viewList() string {
	title := tuiTitleStyle.Render("Org Services") + "  " + tuiMetaStyle.Render(m.scopeLabel())
	help := tuiHelp(m.listHelp(), m.width)

	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.pending != nil {
		body := tuiMetaStyle.Render("No services.")
		if len(m.records) > 0 {
			body = tuiBoxStyle.Render(m.table.View())
		}
		return title + "\n" + body + "\n" + tuiErrStyle.Render(m.pending.prompt)
	}
	if len(m.records) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No services in the catalog.") + "\n\n" + m.statusLine() + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.table.View()) + "\n" + m.statusLine() + help
}

func (m orgServicesModel) viewDetail() string {
	title := tuiTitleStyle.Render(m.selLabel) + "  " + tuiMetaStyle.Render("service")
	help := tuiHelp("[↑↓/pgup/pgdn] scroll  [esc] back", m.width)
	return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + help
}

// ── Command registration ──────────────────────────────────────────────────────

func startOrgServicesTUI() error {
	p := tea.NewProgram(standaloneWrap{newOrgServicesModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// orgServicesCmd is an admin control: `armory admin org-services` opens the
// org-level service enable/disable/configure TUI backed by the builder service.
var orgServicesCmd = &cobra.Command{
	Use:     "org-services",
	Aliases: []string{"org-service", "services"},
	Short:   "Enable, disable and configure services for your org (TUI)",
	Args:    cobra.NoArgs,
	RunE:    func(cmd *cobra.Command, args []string) error { return startOrgServicesTUI() },
}

func init() {
	orgServicesCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Interactive TUI for org-level service configuration",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return startOrgServicesTUI() },
	})
	RegisterModule(Module{
		Name:    "org-services",
		Admin:   true,
		Order:   33, // after forge-runtimes (31), ahead of audit (50)
		Command: orgServicesCmd,
		Screens: []HubScreen{{
			Title: "Org Services",
			Desc:  "Enable, disable and configure services for your org",
			New:   func() tea.Model { return newOrgServicesModel() },
		}},
	})
}
