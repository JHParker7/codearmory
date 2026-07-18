package cmd

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// The Forge Runtimes TUI is the admin counterpart to the Forge executions
// screen: it manages the two server-side configuration resources forge exposes —
// runner classes (resource tiers) and runtime backends (docker/kubernetes/
// kata/gvisor targets) — behind a tab-style section switcher modelled on the
// gatekeeper TUI. Both sections share the same list/detail/form machinery; what
// differs (the endpoint, the table columns, how a record maps to a row, and the
// create/edit form) is small enough to branch on per section. Records are kept
// as decoded JSON objects so the detail view can render any resource verbatim.
//
// Permission enforcement lives server-side: listing is granted to every user by
// default, while create/update/delete require an admin grant. A user without the
// grant still sees the action but gets a clear 403 in the status line rather than
// the capability being hidden — the same contract as the gatekeeper TUI.

// ── Sections ────────────────────────────────────────────────────────────────

type frSection int

const (
	frRunnerClasses frSection = iota
	frRuntimeBackends
	frImages // read-only: the configured execution image allowlist
)

// frRecord is one decoded JSON object from a list endpoint. It shares the
// gatekeeper record helpers (gkStr/gkTimeCell), which operate on the same
// underlying map type.
type frRecord = map[string]any

// frSectionDef describes one section's list endpoint and how its records render.
type frSectionDef struct {
	name  string // tab label
	label string // singular noun for prompts/status
	path  string // list/CRUD endpoint base
	cols  []tuiColSpec
	row   func(frRecord) table.Row
}

var frDefs = []frSectionDef{
	frRunnerClasses: {
		name: "Runner Classes", label: "runner class",
		path: "/forge/runner-classes",
		cols: []tuiColSpec{{"NAME", 14, 2}, {"MEMORY", 8, 0}, {"CPU", 7, 0}, {"DISK", 6, 0}, {"BACKEND", 12, 1}, {"PRIV", 5, 0}, {"ENABLED", 8, 0}},
		row: func(r frRecord) table.Row {
			return table.Row{
				gkStr(r, "name"), frInt(r, "memory_mb"), frInt(r, "cpu_millicores"),
				frInt(r, "disk_gb"), frDash(gkStr(r, "backend")), frBoolDot(r, "privileged"), frBoolDot(r, "enabled"),
			}
		},
	},
	frRuntimeBackends: {
		name: "Runtime Backends", label: "runtime backend",
		path: "/forge/runtime-backends",
		cols: []tuiColSpec{{"NAME", 16, 2}, {"TYPE", 11, 0}, {"ENABLED", 8, 0}, {"CONFIG", 7, 0}, {"SECRETS", 8, 0}, {"CREATED", 14, 0}},
		row: func(r frRecord) table.Row {
			return table.Row{
				gkStr(r, "name"), frDash(gkStr(r, "type")), frBoolDot(r, "enabled"),
				frMapCount(r, "config"), frMapCount(r, "secret_refs"), gkTimeCell(r, "created_at"),
			}
		},
	},
	frImages: {
		name: "Images", label: "image",
		path: "/forge/images",
		cols: []tuiColSpec{{"ALLOWED IMAGE", 50, 2}},
		row:  func(r frRecord) table.Row { return table.Row{gkStr(r, "image")} },
	},
}

// frReadOnly reports whether a section only supports viewing (no create/edit/
// delete). The image allowlist is server-configured (ALLOWED_IMAGES); the API
// exposes only a list, so it is read-only here.
func frReadOnly(s frSection) bool { return s == frImages }

// ── Record helpers ──────────────────────────────────────────────────────────

func frDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// frInt renders a JSON number field (decoded as float64) as an integer string.
func frInt(r frRecord, key string) string {
	if v, ok := r[key].(float64); ok {
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

// frBoolDot renders a boolean field as a filled/empty dot, matching the
// gatekeeper active column.
func frBoolDot(r frRecord, key string) string {
	if b, ok := r[key].(bool); ok && b {
		return "●"
	}
	return "○"
}

// frBoolDotBool renders a plain boolean as a filled/empty dot (the typed variant
// of frBoolDot, for tables whose rows are typed structs rather than frRecord maps).
func frBoolDotBool(b bool) string {
	if b {
		return "●"
	}
	return "○"
}

// frBoolStr renders a boolean field as "true"/"false" for pre-filling selectors.
func frBoolStr(r frRecord, key string) string {
	if b, ok := r[key].(bool); ok && b {
		return "true"
	}
	return "false"
}

// frMapCount renders the number of keys in a JSON object field.
func frMapCount(r frRecord, key string) string {
	if m, ok := r[key].(map[string]any); ok {
		return strconv.Itoa(len(m))
	}
	return "0"
}

// frMapToLines renders a JSON object field as sorted "key=value" lines, the form
// the config/secret_refs textareas accept. Sorting keeps an edit round-trip
// deterministic.
func frMapToLines(r frRecord, key string) string {
	m, ok := r[key].(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s=%v", k, m[k])
	}
	return b.String()
}

// parseKVLines parses newline-separated "key=value" pairs into a map. Blank
// lines are skipped; a non-blank line without an '=' (or with an empty key) is an
// error. Used for the runtime-backend config and secret_refs fields, whose values
// (URLs, env var names) may contain characters that space-splitting would mangle.
func parseKVLines(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid line %q: expected key=value", line)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

// ── Views / forms ───────────────────────────────────────────────────────────

type frViewID int

const (
	frViewList frViewID = iota
	frViewDetail
	frViewForm
)

type frFormKind int

const (
	frFormNone frFormKind = iota
	frFormRunnerClass
	frFormRuntimeBackend
)

// frPending is a destructive action awaiting y/n confirmation.
type frPending struct {
	prompt string
	run    tea.Cmd
}

// ── Messages ────────────────────────────────────────────────────────────────

type frRecordsMsg struct {
	section frSection
	records []frRecord
}
type frErrMsg struct{ err error }
type frActionMsg struct {
	label string
	err   error
}
type frFormErrMsg struct{ err error }
type frFormDoneMsg struct {
	section frSection
	status  string
}
type frBackendsMsg []string

// ── Model ───────────────────────────────────────────────────────────────────

type frSectionState struct {
	records []frRecord
	table   table.Model
	loaded  bool
}

type forgeRuntimesModel struct {
	section frSection
	view    frViewID
	loading bool
	err     error
	width   int
	height  int

	sections []frSectionState
	vp       viewport.Model

	// backends is the runtime-backend name catalog for the runner-class form's
	// backend selector, prefetched on Init and refreshed when a backend changes.
	backends []string

	form       tuiForm
	formKind   frFormKind
	formMode   string // "create" or "edit"
	editTarget string // name of the record being edited (the PUT path segment)

	pending *frPending // confirm prompt in the list view

	selLabel string // selected record name, for the detail title

	status    string // transient one-line status (action result)
	statusErr bool
}

func newForgeRuntimesModel() forgeRuntimesModel {
	m := forgeRuntimesModel{
		loading:  true,
		width:    tuiDefaultWidth,
		height:   tuiDefaultHeight,
		sections: make([]frSectionState, len(frDefs)),
		vp:       viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-9),
	}
	for i := range m.sections {
		t := table.New(table.WithFocused(true))
		t.SetStyles(tuiTableStyles())
		m.sections[i].table = t
	}
	m.applyTableLayout()
	return m
}

// applyTableLayout resizes every section table to the current terminal.
func (m *forgeRuntimesModel) applyTableLayout() {
	for i := range m.sections {
		m.sections[i].table.SetColumns(tuiFitColumns(frDefs[i].cols, m.width))
		m.sections[i].table.SetHeight(tuiTableHeight(m.height, tuiListChrome+1)) // +1 for the section bar
	}
}

func (m *forgeRuntimesModel) def() frSectionDef { return frDefs[m.section] }

func (m *forgeRuntimesModel) state() *frSectionState { return &m.sections[m.section] }

// currentRecord returns the record under the active section's cursor.
func (m *forgeRuntimesModel) currentRecord() (frRecord, bool) {
	st := m.state()
	i := st.table.Cursor()
	if i >= 0 && i < len(st.records) {
		return st.records[i], true
	}
	return nil, false
}

// ── Fetch ─────────────────────────────────────────────────────────────────

func frFetch(section frSection) tea.Cmd {
	path := frDefs[section].path
	return func() tea.Msg {
		data, err := doRequest("GET", path, nil)
		if err != nil {
			return frErrMsg{err}
		}
		// The image allowlist endpoint returns a bare []string; wrap each entry as
		// a record so it renders through the shared section machinery.
		if section == frImages {
			var imgs []string
			if err := json.Unmarshal(data, &imgs); err != nil {
				return frErrMsg{err}
			}
			recs := make([]frRecord, len(imgs))
			for i, im := range imgs {
				recs[i] = frRecord{"image": im}
			}
			return frRecordsMsg{section: section, records: recs}
		}
		var recs []frRecord
		if err := json.Unmarshal(data, &recs); err != nil {
			return frErrMsg{err}
		}
		return frRecordsMsg{section: section, records: recs}
	}
}

// frFetchBackends loads the runtime-backend names for the runner-class form's
// backend selector, degrading to an empty list on failure so the field falls
// back to a free-text input (and a permission error never blanks the TUI).
func frFetchBackends() tea.Msg {
	data, err := doRequest("GET", "/forge/runtime-backends", nil)
	if err != nil {
		return frBackendsMsg(nil)
	}
	var recs []frRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return frBackendsMsg(nil)
	}
	var names []string
	for _, r := range recs {
		if n := gkStr(r, "name"); n != "" {
			names = append(names, n)
		}
	}
	return frBackendsMsg(names)
}

func frDelete(section frSection, name string) tea.Cmd {
	path := frDefs[section].path + "/" + name
	return func() tea.Msg {
		if _, err := doRequest("DELETE", path, nil); err != nil {
			return frActionMsg{label: "delete", err: err}
		}
		return frActionMsg{label: "delete"}
	}
}

// frSubmit POSTs (create) or PUTs (edit) a runner-class or runtime-backend
// payload, surfacing failures as a form error so the dialog stays open with the
// reason rather than silently closing.
func frSubmit(section frSection, mode, target string, payload map[string]any) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(payload)
		method, path := "POST", frDefs[section].path
		if mode == "edit" {
			method, path = "PUT", frDefs[section].path+"/"+target
		}
		if _, err := doRequest(method, path, body); err != nil {
			return frFormErrMsg{err}
		}
		verb := "created"
		if mode == "edit" {
			verb = "updated"
		}
		return frFormDoneMsg{section: section, status: "✓ " + frDefs[section].label + " " + verb}
	}
}

// ── Init / Update ───────────────────────────────────────────────────────────

func (m forgeRuntimesModel) Init() tea.Cmd {
	return tea.Batch(frFetch(m.section), frFetchBackends)
}

func (m forgeRuntimesModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 9
		m.applyTableLayout()
		return m, nil

	case frErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case frRecordsMsg:
		m.loading = false
		st := &m.sections[msg.section]
		st.records = msg.records
		st.loaded = true
		rows := make([]table.Row, len(msg.records))
		for i, r := range msg.records {
			rows[i] = frDefs[msg.section].row(r)
		}
		st.table.SetRows(rows)
		return m, nil

	case frBackendsMsg:
		m.backends = []string(msg)
		return m, nil

	case frActionMsg:
		if msg.err != nil {
			m.status = "✗ " + msg.label + ": " + msg.err.Error()
			m.statusErr = true
			return m, nil
		}
		m.status = "✓ " + msg.label + "d"
		m.statusErr = false
		m.loading = true
		// A deleted runtime backend drops out of the runner-class selector.
		if m.section == frRuntimeBackends {
			return m, tea.Batch(frFetch(m.section), frFetchBackends)
		}
		return m, frFetch(m.section)

	case frFormDoneMsg:
		m.view = frViewList
		m.formKind = frFormNone
		m.status = msg.status
		m.statusErr = false
		m.loading = true
		// A changed runtime backend may have appeared/renamed/toggled — refresh
		// the catalog so the runner-class selector stays in sync.
		if msg.section == frRuntimeBackends {
			return m, tea.Batch(frFetch(msg.section), frFetchBackends)
		}
		return m, frFetch(msg.section)

	case frFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil

	case tuiAutoRefreshMsg:
		// Silently re-fetch the current section's list so changes appear without a
		// loading flash or losing the cursor. The detail view renders an immutable
		// snapshot and the form must not be disturbed, so both are skipped.
		if m.view == frViewList {
			return m, frFetch(m.section)
		}
		return m, nil

	case tea.KeyMsg:
		if m.err != nil {
			switch msg.String() {
			case "esc":
				return m, frGoHome
			case "ctrl+c":
				return m, tea.Quit
			case "r":
				m.err = nil
				m.loading = true
				return m, frFetch(m.section)
			}
			return m, nil
		}
		switch m.view {
		case frViewList:
			return m.keyList(msg)
		case frViewDetail:
			return m.keyDetail(msg)
		case frViewForm:
			return m.keyForm(msg)
		}
	}
	return m.delegate(msg)
}

// frGoHome returns the hub (or quits, in standalone mode) via the shared
// goHomeMsg the host wrapper interprets.
func frGoHome() tea.Msg { return goHomeMsg{} }

func (m forgeRuntimesModel) delegate(msg tea.Msg) (forgeRuntimesModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case frViewList:
		m.sections[m.section].table, cmd = m.state().table.Update(msg)
	case frViewDetail:
		m.vp, cmd = m.vp.Update(msg)
	case frViewForm:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

// switchSection moves to section s, lazily fetching it the first time.
func (m forgeRuntimesModel) switchSection(s frSection) (forgeRuntimesModel, tea.Cmd) {
	m.section = s
	m.view = frViewList
	m.pending = nil
	m.status = ""
	if !m.state().loaded {
		m.loading = true
		return m, frFetch(s)
	}
	return m, nil
}

func (m forgeRuntimesModel) keyList(msg tea.KeyMsg) (forgeRuntimesModel, tea.Cmd) {
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

	n := frSection(len(frDefs))
	switch msg.String() {
	case "esc":
		return m, frGoHome
	case "ctrl+c":
		return m, tea.Quit
	case "tab", "]":
		return m.switchSection((m.section + 1) % n)
	case "shift+tab", "[":
		return m.switchSection((m.section - 1 + n) % n)
	case "1", "2", "3":
		if s := frSection(msg.String()[0] - '1'); s < n {
			return m.switchSection(s)
		}
	case "enter":
		if rec, ok := m.currentRecord(); ok {
			m.selLabel = gkStr(rec, "name")
			m.view = frViewDetail
			pretty, _ := json.MarshalIndent(rec, "", "  ")
			m.vp.SetContent(string(pretty))
			m.vp.GotoTop()
		}
		return m, nil
	case "n":
		if frReadOnly(m.section) {
			return m, nil
		}
		return m.openForm("create", nil)
	case "e":
		if frReadOnly(m.section) {
			return m, nil
		}
		if rec, ok := m.currentRecord(); ok {
			return m.openForm("edit", rec)
		}
	case "D":
		if frReadOnly(m.section) {
			return m, nil
		}
		if rec, ok := m.currentRecord(); ok {
			name := gkStr(rec, "name")
			m.pending = &frPending{
				prompt: fmt.Sprintf("Delete %s %q? [y] confirm  [any] cancel", m.def().label, name),
				run:    frDelete(m.section, name),
			}
			return m, nil
		}
	case "r":
		m.loading = true
		m.status = ""
		return m, frFetch(m.section)
	}

	var cmd tea.Cmd
	m.sections[m.section].table, cmd = m.state().table.Update(msg)
	return m, cmd
}

func (m forgeRuntimesModel) keyDetail(msg tea.KeyMsg) (forgeRuntimesModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = frViewList
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── Forms ─────────────────────────────────────────────────────────────────

// frBackendField builds the runner-class backend picker: a selector over the
// known backend names (defaulting to "default") when the catalog is available,
// or a free-text fallback. current is the value to pre-select when editing.
func frBackendField(backends []string, current string) formField {
	def := current
	if def == "" {
		def = "default"
	}
	if len(backends) == 0 {
		return formInputDefault("backend", "Backend", "default", def)
	}
	opts := append([]string{}, backends...)
	if !slices.Contains(opts, "default") {
		opts = append([]string{"default"}, opts...)
	}
	// Keep a stored value the catalog no longer offers (e.g. a since-removed
	// backend) so an edit round-trips faithfully instead of snapping to default.
	if !slices.Contains(opts, def) {
		opts = append(opts, def)
	}
	return formSelectDefault("backend", "Backend", opts, def)
}

// openForm opens the create or edit dialog for the active section. For "create"
// rec is nil; for "edit" rec is the selected record and supplies both the PUT
// target name and the pre-filled field values.
func (m forgeRuntimesModel) openForm(mode string, rec frRecord) (forgeRuntimesModel, tea.Cmd) {
	m.formMode = mode
	m.editTarget = gkStr(rec, "name") // "" for create
	var (
		f   tuiForm
		cmd tea.Cmd
	)
	switch m.section {
	case frRunnerClasses:
		m.formKind = frFormRunnerClass
		f, cmd = frRunnerClassForm(rec, m.backends, mode)
	case frRuntimeBackends:
		m.formKind = frFormRuntimeBackend
		f, cmd = frRuntimeBackendForm(rec, mode)
	}
	m.form = f
	m.view = frViewForm
	return m, cmd
}

// frRunnerClassForm builds the runner-class create/edit dialog. The name is the
// immutable primary key, so on edit it is pre-filled and the update PUTs to it.
func frRunnerClassForm(rec frRecord, backends []string, mode string) (tuiForm, tea.Cmd) {
	title, nameField := "New Runner Class", formInput("name", "Name", "e.g. large (required)")
	if mode == "edit" {
		title = "Edit Runner Class"
		nameField = formInputDefault("name", "Name", "", gkStr(rec, "name"))
	}
	f, cmd := newTUIForm(title,
		nameField,
		formInput("memory_mb", "Memory MB", "min 64"),
		formInput("cpu_millicores", "CPU (m)", "min 100"),
		formInput("pids_limit", "PIDs", "min 8"),
		formInput("tmpfs_mb", "tmpfs MB", "min 16"),
		formInput("disk_gb", "Disk GB", "reserved; currently unused"),
		frBackendField(backends, gkStr(rec, "backend")),
		formSelect("enabled", "Enabled", []string{"true", "false"}),
		formSelect("privileged", "Privileged", []string{"false", "true"}),
	)
	f.help = "Privileged runs as root and only works on a kernel-isolated backend (kata/gvisor). Disk GB is reserved and currently unused."
	if mode == "edit" {
		f.setValues(map[string]string{
			"memory_mb":      frInt(rec, "memory_mb"),
			"cpu_millicores": frInt(rec, "cpu_millicores"),
			"pids_limit":     frInt(rec, "pids_limit"),
			"tmpfs_mb":       frInt(rec, "tmpfs_mb"),
			"disk_gb":        frInt(rec, "disk_gb"),
			"enabled":        frBoolStr(rec, "enabled"),
			"privileged":     frBoolStr(rec, "privileged"),
		})
	} else {
		f.setValues(map[string]string{
			"memory_mb":      "512",
			"cpu_millicores": "500",
			"pids_limit":     "64",
			"tmpfs_mb":       "128",
			"disk_gb":        "10",
		})
	}
	return f, cmd
}

// frRuntimeBackendForm builds the runtime-backend create/edit dialog. Config and
// secret_refs are entered as "key=value" lines; the server validates type-
// specific requirements (kata/gvisor) and surfaces any failure inline.
func frRuntimeBackendForm(rec frRecord, mode string) (tuiForm, tea.Cmd) {
	title, nameField := "New Runtime Backend", formInput("name", "Name", "e.g. kata-prod (required)")
	if mode == "edit" {
		title = "Edit Runtime Backend"
		nameField = formInputDefault("name", "Name", "", gkStr(rec, "name"))
	}
	f, cmd := newTUIForm(title,
		nameField,
		formSelect("type", "Type", []string{"docker", "kubernetes", "kata", "gvisor"}),
		formSelect("enabled", "Enabled", []string{"true", "false"}),
		formTextarea("config", "Config", "key=value per line\nkata: runtime_class=kata-qemu\ngvisor: runtime_class=gvisor"),
		formTextarea("secret_refs", "Secrets", "logical=ENV_VAR_NAME per line"),
	)
	f.help = "kata/gvisor need config runtime_class. docker/kubernetes read their settings from env."
	if mode == "edit" {
		f.setValues(map[string]string{
			"type":        gkStr(rec, "type"),
			"enabled":     frBoolStr(rec, "enabled"),
			"config":      frMapToLines(rec, "config"),
			"secret_refs": frMapToLines(rec, "secret_refs"),
		})
	}
	return f, cmd
}

func (m forgeRuntimesModel) keyForm(msg tea.KeyMsg) (forgeRuntimesModel, tea.Cmd) {
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
		m.view = frViewList
		m.formKind = frFormNone
		return m, nil
	case formSubmit:
		return m.submitForm()
	}
	return m, cmd
}

// submitForm validates the active form and returns the create/update cmd.
// Validation failures stay in the form with an inline message; the server
// performs the authoritative checks (resource minimums, privileged-backend and
// kata/gvisor rules) and any rejection comes back as a form error.
func (m forgeRuntimesModel) submitForm() (forgeRuntimesModel, tea.Cmd) {
	switch m.formKind {
	case frFormRunnerClass:
		name := m.form.value("name")
		if name == "" {
			m.form.errMsg = "name is required"
			return m, nil
		}
		// parse reads an integer field, recording the first bad field's message.
		var perr string
		parse := func(key, label string) int64 {
			v, err := strconv.ParseInt(strings.TrimSpace(m.form.value(key)), 10, 64)
			if err != nil && perr == "" {
				perr = label + " must be an integer"
			}
			return v
		}
		payload := map[string]any{
			"name":           name,
			"memory_mb":      parse("memory_mb", "memory_mb"),
			"cpu_millicores": parse("cpu_millicores", "cpu_millicores"),
			"pids_limit":     parse("pids_limit", "pids_limit"),
			"tmpfs_mb":       parse("tmpfs_mb", "tmpfs_mb"),
			"disk_gb":        parse("disk_gb", "disk_gb"),
			"backend":        m.form.value("backend"),
			"enabled":        m.form.value("enabled") == "true",
			"privileged":     m.form.value("privileged") == "true",
		}
		if perr != "" {
			m.form.errMsg = perr
			return m, nil
		}
		m.form.errMsg = ""
		return m, frSubmit(frRunnerClasses, m.formMode, m.editTarget, payload)

	case frFormRuntimeBackend:
		name := m.form.value("name")
		if name == "" {
			m.form.errMsg = "name is required"
			return m, nil
		}
		config, err := parseKVLines(m.form.value("config"))
		if err != nil {
			m.form.errMsg = "config: " + err.Error()
			return m, nil
		}
		secrets, err := parseKVLines(m.form.value("secret_refs"))
		if err != nil {
			m.form.errMsg = "secrets: " + err.Error()
			return m, nil
		}
		payload := map[string]any{
			"name":        name,
			"type":        m.form.value("type"),
			"enabled":     m.form.value("enabled") == "true",
			"config":      config,
			"secret_refs": secrets,
		}
		m.form.errMsg = ""
		return m, frSubmit(frRuntimeBackends, m.formMode, m.editTarget, payload)
	}
	return m, nil
}

// ── View ────────────────────────────────────────────────────────────────────

func (m forgeRuntimesModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" +
			tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	switch m.view {
	case frViewForm:
		return m.form.view(m.width, m.height)
	case frViewDetail:
		return m.viewDetail()
	}
	return m.viewList()
}

// sectionBar renders the tab strip with the active section highlighted.
func (m forgeRuntimesModel) sectionBar() string {
	active := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(activeTheme.Accent))
	inactive := lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	labels := make([]string, len(frDefs))
	for i, d := range frDefs {
		if frSection(i) == m.section {
			labels[i] = active.Render(d.name)
		} else {
			labels[i] = inactive.Render(d.name)
		}
	}
	return strings.Join(labels, tuiMetaStyle.Render("  ·  "))
}

func (m forgeRuntimesModel) listHelp() string {
	parts := []string{"[tab] section", "[↑↓/jk] nav", "[enter] detail"}
	if frReadOnly(m.section) {
		parts = append(parts, tuiMetaStyle.Render("(read-only)"))
	} else {
		parts = append(parts, "[n] new", "[e] edit", "[D] delete")
	}
	parts = append(parts, "[r] refresh", "[esc] home")
	return strings.Join(parts, "  ")
}

func (m forgeRuntimesModel) statusLine() string {
	if m.status == "" {
		return ""
	}
	if m.statusErr {
		return tuiErrStyle.Render(m.status) + "\n"
	}
	return tuiMetaStyle.Render(m.status) + "\n"
}

func (m forgeRuntimesModel) viewList() string {
	title := tuiTitleStyle.Render("Forge Runtimes")
	bar := m.sectionBar()
	help := tuiHelp(m.listHelp(), m.width)
	header := title + "\n" + bar

	if m.loading {
		return header + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.pending != nil {
		body := tuiMetaStyle.Render("No " + strings.ToLower(m.def().name) + ".")
		if len(m.state().records) > 0 {
			body = tuiBoxStyle.Render(m.state().table.View())
		}
		return header + "\n" + body + "\n" + tuiErrStyle.Render(m.pending.prompt)
	}
	if len(m.state().records) == 0 {
		return header + "\n\n" + tuiMetaStyle.Render("No "+strings.ToLower(m.def().name)+".") + "\n\n" + m.statusLine() + help
	}
	return header + "\n" + tuiBoxStyle.Render(m.state().table.View()) + "\n" + m.statusLine() + help
}

func (m forgeRuntimesModel) viewDetail() string {
	title := tuiTitleStyle.Render(m.def().name + " detail")
	if m.selLabel != "" {
		title = tuiTitleStyle.Render(m.selLabel) + "  " + tuiMetaStyle.Render(m.def().label)
	}
	help := tuiHelp("[↑↓/pgup/pgdn] scroll  [esc] back", m.width)
	return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + help
}

// ── Command registration ──────────────────────────────────────────────────────

func startForgeRuntimesTUI() error {
	p := tea.NewProgram(standaloneWrap{newForgeRuntimesModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// forgeRuntimesCmd is an admin control: `armory admin forge-runtimes` opens the
// runner-class / runtime-backend TUI. It lives under `armory admin` rather than
// the user-facing `armory forge` so runtime configuration sits with the other
// platform-administration tools.
var forgeRuntimesCmd = &cobra.Command{
	Use:     "forge-runtimes",
	Aliases: []string{"forge-runtime", "runtimes"},
	Short:   "Manage forge runner classes and runtime backends (TUI)",
	Args:    cobra.NoArgs,
	RunE:    func(cmd *cobra.Command, args []string) error { return startForgeRuntimesTUI() },
}

func init() {
	forgeRuntimesCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Interactive TUI for forge runner classes and runtime backends",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return startForgeRuntimesTUI() },
	})
	RegisterModule(Module{
		Name:    "forge-runtimes",
		Service: "forge",
		Admin:   true,
		Order:   31, // first in the admin hub, ahead of audit (50) and gatekeeper (60)
		Command: forgeRuntimesCmd,
		Screens: []HubScreen{{
			Title: "Forge Runtimes",
			Desc:  "Manage runner classes and runtime backends",
			New:   func() tea.Model { return newForgeRuntimesModel() },
		}},
	})
}
