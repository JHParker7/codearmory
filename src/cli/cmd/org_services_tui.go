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

// The Org Services TUI is the system-admin screen for the builder service: it lets
// the admin enable, disable, configure, register and remove platform services for
// the whole instance. There is no per-org scope — every change targets the single
// global baseline (the literal id "default"). Effective state comes from builder,
// which overlays the live service catalog with that baseline, so the list shows
// every available service, whether it is on, and where that state comes from.
//
// Permission enforcement stays server-side: only the system admin holds the write
// grant on builder/orgs/default, so a non-admin still sees the action but gets a
// clear 403 in the status line, matching the other admin TUIs.

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

// parseSecretsJSON parses the secrets textarea into a write-only env-key → value map
// (e.g. REDIS_URL, GITEA_ADMIN_TOKEN). Blank input is nil (keep existing). Every value
// must be a string.
func parseSecretsJSON(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("must be a JSON object: %w", err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		sv, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("secret %q must be a string value", k)
		}
		out[k] = sv
	}
	return out, nil
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
	records []osvRecord
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
		loading: true,
		width:   tuiDefaultWidth,
		height:  tuiDefaultHeight,
		vp:      viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-9),
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

// Builder manages the single global baseline (the literal id "default"); there is
// no per-org scope. Reads are granted to all users; only the system admin may
// write, so a non-admin's toggles come back as a 403 status line.
func osvFetch() tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("GET", "/builder/services", nil)
		if err != nil {
			return osvErrMsg{err}
		}
		var recs []osvRecord
		if err := json.Unmarshal(data, &recs); err != nil {
			return osvErrMsg{err}
		}
		return osvRecordsMsg{records: recs}
	}
}

func osvSet(service string, payload map[string]any) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(payload)
		if _, err := doRequest("PUT", "/builder/services/"+service, body); err != nil {
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
func osvToggle(rec osvRecord) tea.Cmd {
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
		if _, err := doRequest("PUT", "/builder/services/"+service, body); err != nil {
			return osvActionMsg{label: "toggle", err: err}
		}
		return osvActionMsg{label: "toggle"}
	}
}

func osvDelete(service string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/builder/services/"+service, nil); err != nil {
			return osvActionMsg{label: "remove", err: err}
		}
		return osvActionMsg{label: "remove"}
	}
}

// ── Init / Update ───────────────────────────────────────────────────────────

func (m orgServicesModel) Init() tea.Cmd { return osvFetch() }

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
		return m, osvFetch()

	case osvFormDoneMsg:
		m.view = osvViewList
		m.formKind = osvFormNone
		m.status = msg.status
		m.statusErr = false
		m.loading = true
		return m, osvFetch()

	case osvFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil

	case tuiAutoRefreshMsg:
		if m.view == osvViewList {
			return m, osvFetch()
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
				return m, osvFetch()
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
			return m, osvToggle(rec)
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
				prompt: fmt.Sprintf("Remove %q from the global baseline? [y] confirm  [any] cancel", service),
				run:    osvDelete(service),
			}
			return m, nil
		}
	case "r":
		m.loading = true
		m.status = ""
		return m, osvFetch()
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
		formTextarea("secrets", "Secrets (JSON, write-only · e.g. REDIS_URL, GITEA_ADMIN_TOKEN)", "blank = keep · {\n  \"REDIS_URL\": \"redis://…\"\n}"),
	)

	f, cmd := newTUIForm("Configure "+m.editTarget, fields...)
	dbState := "no DB URL set"
	if v, ok := rec["db_configured"].(bool); ok && v {
		dbState = "DB URL set (" + gkStr(rec, "db_host") + ")"
	}
	f.help = "Enable/disable + free-form JSON config. DB URL and secrets are write-only (" + dbState + ")."
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
		formTextarea("secrets", "Secrets (JSON, write-only)", "{\n  \"REDIS_URL\": \"redis://…\"\n}"),
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
	secrets, serr := parseSecretsJSON(m.form.value("secrets"))
	if serr != nil {
		m.form.errMsg = "secrets: " + serr.Error()
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
		if len(secrets) > 0 {
			payload["secrets"] = secrets
		}
		m.form.errMsg = ""
		return m, osvSet(m.editTarget, payload)

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
		if len(secrets) > 0 {
			payload["secrets"] = secrets
		}
		m.form.errMsg = ""
		return m, osvSet(service, payload)
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
	return "global baseline (whole instance)"
}

func (m orgServicesModel) listHelp() string {
	parts := []string{
		"[↑↓/jk] nav", "[space] toggle", "[c] configure", "[n] new custom",
		"[D] remove", "[enter] detail", "[r] refresh", "[esc] home",
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
