package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func applyFRMsg(m forgeRuntimesModel, msg tea.Msg) forgeRuntimesModel {
	updated, _ := m.Update(msg)
	return updated.(forgeRuntimesModel)
}

// ── Initial state ─────────────────────────────────────────────────────────────

func TestForgeRuntimesModel_InitialState(t *testing.T) {
	m := newForgeRuntimesModel()
	if m.view != frViewList {
		t.Errorf("initial view = %v, want frViewList", m.view)
	}
	if m.section != frRunnerClasses {
		t.Errorf("initial section = %v, want frRunnerClasses", m.section)
	}
	if !m.loading {
		t.Error("loading should be true on startup")
	}
	if len(m.sections) != len(frDefs) {
		t.Errorf("section states = %d, want %d", len(m.sections), len(frDefs))
	}
}

// ── Messages: frRecordsMsg ────────────────────────────────────────────────────

func TestForgeRuntimesModel_RecordsMsg_PopulatesSection(t *testing.T) {
	recs := []frRecord{
		{"name": "standard", "memory_mb": 512.0, "cpu_millicores": 500.0, "backend": "default", "enabled": true},
		{"name": "large", "memory_mb": 2048.0, "cpu_millicores": 2000.0, "backend": "default", "enabled": true},
	}
	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frRunnerClasses, records: recs})
	if m.loading {
		t.Error("loading should be false after recordsMsg")
	}
	st := &m.sections[frRunnerClasses]
	if !st.loaded || len(st.records) != 2 {
		t.Fatalf("records = %d loaded=%v, want 2 loaded", len(st.records), st.loaded)
	}
	if !strings.Contains(m.View(), "large") {
		t.Errorf("list view should show a runner class name, got: %q", m.View())
	}
}

func TestForgeRuntimesModel_RecordsMsg_TargetsCorrectSection(t *testing.T) {
	// A records message for the backends section must not populate the runner
	// classes section the model is currently showing.
	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frRuntimeBackends, records: []frRecord{{"name": "kata-prod", "type": "kata"}}})
	if m.sections[frRunnerClasses].loaded {
		t.Error("runner classes section should stay unloaded")
	}
	if len(m.sections[frRuntimeBackends].records) != 1 {
		t.Error("runtime backends section should be populated")
	}
}

// ── Fetch ─────────────────────────────────────────────────────────────────────

func TestFRFetch_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[{"name":"standard","enabled":true}]`)
	setupCLI(t, srv)
	msg := frFetch(frRunnerClasses)()
	if rec.Method != "GET" || rec.Path != "/forge/runner-classes" {
		t.Errorf("request = %s %s, want GET /forge/runner-classes", rec.Method, rec.Path)
	}
	got, ok := msg.(frRecordsMsg)
	if !ok || got.section != frRunnerClasses || len(got.records) != 1 {
		t.Fatalf("msg = %#v, want frRecordsMsg{runnerClasses, 1 record}", msg)
	}
}

func TestFRFetch_BackendsPath(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)
	frFetch(frRuntimeBackends)()
	if rec.Path != "/forge/runtime-backends" {
		t.Errorf("path = %q, want /forge/runtime-backends", rec.Path)
	}
}

func TestFRFetch_ImagesWrapsStringList(t *testing.T) {
	// The /forge/images endpoint returns a bare []string; the section wraps each
	// as a record so it renders through the shared list machinery.
	srv, rec := recordingServer(t, http.StatusOK, `["ubuntu:22.04","python:3.12"]`)
	setupCLI(t, srv)
	msg := frFetch(frImages)()
	if rec.Path != "/forge/images" {
		t.Errorf("path = %q, want /forge/images", rec.Path)
	}
	recs, ok := msg.(frRecordsMsg)
	if !ok || recs.section != frImages || len(recs.records) != 2 {
		t.Fatalf("msg = %#v, want 2 image records", msg)
	}
	if gkStr(recs.records[0], "image") != "ubuntu:22.04" {
		t.Errorf("first image = %q, want ubuntu:22.04", gkStr(recs.records[0], "image"))
	}
}

func TestForgeRuntimes_ImagesReadOnly(t *testing.T) {
	// The images section is read-only: 'n' must not open a create form.
	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frImages, records: []frRecord{{"image": "ubuntu:22.04"}}})
	m.section = frImages
	m2 := applyFRMsg(m, key("n"))
	if m2.view == frViewForm {
		t.Error("'n' must not open a form in the read-only images section")
	}
	if !frReadOnly(frImages) || frReadOnly(frRunnerClasses) {
		t.Error("frReadOnly should be true only for images")
	}
}

func TestFRFetch_HTTPError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusInternalServerError, `boom`)
	setupCLI(t, srv)
	if _, ok := frFetch(frRunnerClasses)().(frErrMsg); !ok {
		t.Error("HTTP error should return frErrMsg")
	}
}

func TestFRFetchBackends_Success(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `[{"name":"default"},{"name":"kata-prod"},{"name":""}]`)
	setupCLI(t, srv)
	msg, ok := frFetchBackends().(frBackendsMsg)
	if !ok {
		t.Fatalf("msg = %T, want frBackendsMsg", frFetchBackends())
	}
	// Empty names are dropped.
	if len(msg) != 2 || msg[0] != "default" || msg[1] != "kata-prod" {
		t.Errorf("backends = %v, want [default kata-prod]", msg)
	}
}

func TestFRFetchBackends_ErrorDegradesToEmpty(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `nope`)
	setupCLI(t, srv)
	// A permission error must degrade to an empty list, not blank the TUI.
	msg, ok := frFetchBackends().(frBackendsMsg)
	if !ok || len(msg) != 0 {
		t.Errorf("msg = %#v, want empty frBackendsMsg on error", frFetchBackends())
	}
}

func TestForgeRuntimesModel_BackendsMsg_Caches(t *testing.T) {
	m := applyFRMsg(newForgeRuntimesModel(), frBackendsMsg{"default", "kata-prod"})
	if len(m.backends) != 2 || m.backends[1] != "kata-prod" {
		t.Errorf("backends = %v, want [default kata-prod]", m.backends)
	}
}

// ── Section switching ─────────────────────────────────────────────────────────

func TestForgeRuntimesModel_TabSwitchesSection(t *testing.T) {
	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frRunnerClasses, records: []frRecord{}})
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m2 := updated.(forgeRuntimesModel)
	if m2.section != frRuntimeBackends {
		t.Errorf("section after tab = %v, want frRuntimeBackends", m2.section)
	}
	if cmd == nil {
		t.Error("switching to an unloaded section should emit a fetch cmd")
	}
}

func TestForgeRuntimesModel_NumberKeyJumpsSection(t *testing.T) {
	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frRunnerClasses, records: []frRecord{}})
	m2 := applyFRMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if m2.section != frRuntimeBackends {
		t.Errorf("section after '2' = %v, want frRuntimeBackends", m2.section)
	}
}

// ── Detail ────────────────────────────────────────────────────────────────────

func TestForgeRuntimesModel_Enter_OpensDetail(t *testing.T) {
	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frRunnerClasses, records: []frRecord{{"name": "large", "memory_mb": 2048.0}}})
	m2 := applyFRMsg(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m2.view != frViewDetail {
		t.Errorf("view after enter = %v, want frViewDetail", m2.view)
	}
	if !strings.Contains(m2.vp.View(), "large") {
		t.Errorf("detail should render the record JSON, got: %q", m2.vp.View())
	}
}

// ── Create form: runner class ──────────────────────────────────────────────────

func TestForgeRuntimesModel_N_OpensRunnerClassForm(t *testing.T) {
	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frRunnerClasses, records: []frRecord{}})
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m2 := updated.(forgeRuntimesModel)
	if m2.view != frViewForm || m2.formKind != frFormRunnerClass {
		t.Errorf("view=%v kind=%v, want form/runnerClass", m2.view, m2.formKind)
	}
	if m2.formMode != "create" {
		t.Errorf("formMode = %q, want create", m2.formMode)
	}
	// Defaults are pre-filled so a quick create needs only a name.
	if got := m2.form.value("memory_mb"); got != "512" {
		t.Errorf("default memory_mb = %q, want 512", got)
	}
	if cmd == nil {
		t.Error("opening the form should return a focus cmd")
	}
}

func TestForgeRuntimesModel_SubmitRunnerClass_PostsPayload(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"name":"large"}`)
	setupCLI(t, srv)

	m, _ := newForgeRuntimesModel().openForm("create", nil)
	m.form.setValues(map[string]string{"name": "large", "memory_mb": "2048", "cpu_millicores": "2000", "privileged": "true"})
	_, cmd := m.submitForm()
	if cmd == nil {
		t.Fatal("submit should emit a request cmd")
	}
	msg := cmd()
	if done, ok := msg.(frFormDoneMsg); !ok || done.section != frRunnerClasses {
		t.Fatalf("msg = %#v, want frFormDoneMsg{runnerClasses}", msg)
	}
	if rec.Method != "POST" || rec.Path != "/forge/runner-classes" {
		t.Errorf("request = %s %s, want POST /forge/runner-classes", rec.Method, rec.Path)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got["name"] != "large" || got["memory_mb"].(float64) != 2048 {
		t.Errorf("payload name/memory = %v/%v, want large/2048", got["name"], got["memory_mb"])
	}
	if got["backend"] != "default" {
		t.Errorf("backend = %v, want default", got["backend"])
	}
	if got["privileged"] != true {
		t.Errorf("privileged = %v, want true", got["privileged"])
	}
	if got["enabled"] != true {
		t.Errorf("enabled = %v, want true (default)", got["enabled"])
	}
}

func TestForgeRuntimesModel_SubmitRunnerClass_MissingName(t *testing.T) {
	m, _ := newForgeRuntimesModel().openForm("create", nil)
	m.form.setValues(map[string]string{"name": ""})
	m2, cmd := m.submitForm()
	if cmd != nil {
		t.Error("submit with no name should not emit a request cmd")
	}
	if m2.form.errMsg == "" {
		t.Error("submit with no name should set an inline error")
	}
}

func TestForgeRuntimesModel_SubmitRunnerClass_NonIntegerField(t *testing.T) {
	m, _ := newForgeRuntimesModel().openForm("create", nil)
	m.form.setValues(map[string]string{"name": "x", "memory_mb": "lots"})
	m2, cmd := m.submitForm()
	if cmd != nil {
		t.Error("a non-integer field should block submission")
	}
	if !strings.Contains(m2.form.errMsg, "memory_mb") {
		t.Errorf("errMsg = %q, want to mention memory_mb", m2.form.errMsg)
	}
}

// ── Edit form: runner class ────────────────────────────────────────────────────

func TestForgeRuntimesModel_E_OpensPrefilledEditForm(t *testing.T) {
	rec := frRecord{"name": "large", "memory_mb": 2048.0, "cpu_millicores": 2000.0, "pids_limit": 128.0, "tmpfs_mb": 256.0, "disk_gb": 20.0, "backend": "default", "enabled": false, "privileged": true}
	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frRunnerClasses, records: []frRecord{rec}})
	m2 := applyFRMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if m2.view != frViewForm || m2.formMode != "edit" || m2.editTarget != "large" {
		t.Fatalf("view=%v mode=%q target=%q, want form/edit/large", m2.view, m2.formMode, m2.editTarget)
	}
	if got := m2.form.value("memory_mb"); got != "2048" {
		t.Errorf("prefilled memory_mb = %q, want 2048", got)
	}
	if got := m2.form.value("privileged"); got != "true" {
		t.Errorf("prefilled privileged = %q, want true", got)
	}
	if got := m2.form.value("enabled"); got != "false" {
		t.Errorf("prefilled enabled = %q, want false", got)
	}
}

func TestForgeRuntimesModel_SubmitRunnerClass_EditPuts(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"name":"large"}`)
	setupCLI(t, srv)

	record := frRecord{"name": "large", "memory_mb": 2048.0, "cpu_millicores": 2000.0, "pids_limit": 64.0, "tmpfs_mb": 128.0, "disk_gb": 10.0, "backend": "default", "enabled": true}
	m, _ := newForgeRuntimesModel().openForm("edit", record)
	_, cmd := m.submitForm()
	if cmd == nil {
		t.Fatal("edit submit should emit a request cmd")
	}
	cmd()
	if rec.Method != "PUT" || rec.Path != "/forge/runner-classes/large" {
		t.Errorf("request = %s %s, want PUT /forge/runner-classes/large", rec.Method, rec.Path)
	}
}

// ── Runtime backend form ───────────────────────────────────────────────────────

func TestForgeRuntimesModel_SubmitRuntimeBackend_ParsesConfig(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"name":"kata-prod"}`)
	setupCLI(t, srv)

	m := newForgeRuntimesModel()
	m.section = frRuntimeBackends
	m, _ = m.openForm("create", nil)
	m.form.setValues(map[string]string{
		"name":        "kata-prod",
		"type":        "kata",
		"enabled":     "true",
		"config":      "runtime_class=kata-qemu",
		"secret_refs": "token=REGISTRY_TOKEN",
	})
	_, cmd := m.submitForm()
	if cmd == nil {
		t.Fatal("submit should emit a request cmd")
	}
	cmd()
	if rec.Method != "POST" || rec.Path != "/forge/runtime-backends" {
		t.Errorf("request = %s %s, want POST /forge/runtime-backends", rec.Method, rec.Path)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got["type"] != "kata" {
		t.Errorf("type = %v, want kata", got["type"])
	}
	cfg, _ := got["config"].(map[string]any)
	if cfg["runtime_class"] != "kata-qemu" {
		t.Errorf("config.runtime_class = %v, want kata-qemu", cfg["runtime_class"])
	}
	sec, _ := got["secret_refs"].(map[string]any)
	if sec["token"] != "REGISTRY_TOKEN" {
		t.Errorf("secret_refs.token = %v, want REGISTRY_TOKEN", sec["token"])
	}
}

// A gvisor backend submits the same shape as kata (type + runtime_class config) —
// it is the kernel-isolated, no-KVM alternative selectable from the same form.
func TestForgeRuntimesModel_SubmitRuntimeBackend_Gvisor(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"name":"gvisor-prod"}`)
	setupCLI(t, srv)

	m := newForgeRuntimesModel()
	m.section = frRuntimeBackends
	m, _ = m.openForm("create", nil)
	m.form.setValues(map[string]string{
		"name":    "gvisor-prod",
		"type":    "gvisor",
		"enabled": "true",
		"config":  "runtime_class=gvisor",
	})
	_, cmd := m.submitForm()
	if cmd == nil {
		t.Fatal("submit should emit a request cmd")
	}
	cmd()
	if rec.Method != "POST" || rec.Path != "/forge/runtime-backends" {
		t.Errorf("request = %s %s, want POST /forge/runtime-backends", rec.Method, rec.Path)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got["type"] != "gvisor" {
		t.Errorf("type = %v, want gvisor", got["type"])
	}
	cfg, _ := got["config"].(map[string]any)
	if cfg["runtime_class"] != "gvisor" {
		t.Errorf("config.runtime_class = %v, want gvisor", cfg["runtime_class"])
	}
}

func TestForgeRuntimesModel_SubmitRuntimeBackend_BadConfigLine(t *testing.T) {
	m := newForgeRuntimesModel()
	m.section = frRuntimeBackends
	m, _ = m.openForm("create", nil)
	m.form.setValues(map[string]string{"name": "x", "type": "docker", "config": "novalue"})
	m2, cmd := m.submitForm()
	if cmd != nil {
		t.Error("a malformed config line should block submission")
	}
	if !strings.Contains(m2.form.errMsg, "config") {
		t.Errorf("errMsg = %q, want to mention config", m2.form.errMsg)
	}
}

// ── Delete ─────────────────────────────────────────────────────────────────────

func TestForgeRuntimesModel_Delete_ConfirmThenRuns(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusNoContent, ``)
	setupCLI(t, srv)

	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frRunnerClasses, records: []frRecord{{"name": "large"}}})
	m2 := applyFRMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if m2.pending == nil {
		t.Fatal("D should set a pending confirm")
	}
	if !strings.Contains(m2.pending.prompt, "large") {
		t.Errorf("prompt = %q, want to mention the record name", m2.pending.prompt)
	}
	_, cmd := m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("confirming should emit the delete cmd")
	}
	cmd()
	if rec.Method != "DELETE" || rec.Path != "/forge/runner-classes/large" {
		t.Errorf("request = %s %s, want DELETE /forge/runner-classes/large", rec.Method, rec.Path)
	}
}

func TestForgeRuntimesModel_Delete_CancelDoesNothing(t *testing.T) {
	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frRunnerClasses, records: []frRecord{{"name": "large"}}})
	m = applyFRMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if updated.(forgeRuntimesModel).pending != nil {
		t.Error("a non-y key should clear the pending confirm")
	}
	if cmd != nil {
		t.Error("cancelling a delete should not emit a cmd")
	}
}

// ── Action / form-done messages ────────────────────────────────────────────────

func TestForgeRuntimesModel_ActionMsg_Error_SetsStatus(t *testing.T) {
	m := applyFRMsg(newForgeRuntimesModel(), frActionMsg{label: "delete", err: fmt.Errorf("HTTP 403: forbidden")})
	if !m.statusErr || !strings.Contains(m.status, "403") {
		t.Errorf("status = %q err=%v, want a 403 error status", m.status, m.statusErr)
	}
	// A permission denial must not blank the screen with a fatal error.
	if m.err != nil {
		t.Error("an action error should be a status line, not a fatal err")
	}
}

func TestForgeRuntimesModel_FormDone_ReturnsToList(t *testing.T) {
	m := newForgeRuntimesModel()
	m.view = frViewForm
	updated, cmd := m.Update(frFormDoneMsg{section: frRunnerClasses, status: "✓ runner class created"})
	m2 := updated.(forgeRuntimesModel)
	if m2.view != frViewList {
		t.Error("frFormDoneMsg should return to the list view")
	}
	if m2.status == "" || !m2.loading {
		t.Error("frFormDoneMsg should set a status and refetch")
	}
	if cmd == nil {
		t.Error("frFormDoneMsg should emit a refetch cmd")
	}
}

// ── Auto-refresh ───────────────────────────────────────────────────────────────

func TestForgeRuntimesModel_AutoRefresh_ListRefetches(t *testing.T) {
	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frRunnerClasses, records: []frRecord{}})
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd == nil {
		t.Error("auto-refresh in list view should emit a fetch cmd")
	}
}

func TestForgeRuntimesModel_AutoRefresh_FormNoop(t *testing.T) {
	m := newForgeRuntimesModel()
	m.view = frViewForm
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd != nil {
		t.Error("auto-refresh must not disturb an open form")
	}
}

// ── Navigation ─────────────────────────────────────────────────────────────────

func TestForgeRuntimesModel_Esc_GoesHome(t *testing.T) {
	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frRunnerClasses, records: []frRecord{}})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Errorf("esc returned %T, want goHomeMsg", cmd())
	}
}

func TestForgeRuntimesModel_CtrlC_Quits(t *testing.T) {
	m := applyFRMsg(newForgeRuntimesModel(), frRecordsMsg{section: frRunnerClasses, records: []frRecord{}})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should return a cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c should return QuitMsg")
	}
}

// ── Pure helpers ───────────────────────────────────────────────────────────────

func TestParseKVLines(t *testing.T) {
	got, err := parseKVLines("a = 1\n\n  b=two  \n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got["a"] != "1" || got["b"] != "two" {
		t.Errorf("parsed = %v, want {a:1 b:two}", got)
	}
}

func TestParseKVLines_Errors(t *testing.T) {
	for _, bad := range []string{"novalue", "=onlyvalue"} {
		if _, err := parseKVLines(bad); err == nil {
			t.Errorf("parseKVLines(%q) should error", bad)
		}
	}
	// Blank input yields an empty (non-nil) map.
	if m, err := parseKVLines("  \n  "); err != nil || len(m) != 0 {
		t.Errorf("blank input = %v, %v, want empty map", m, err)
	}
}

func TestFRMapToLines_Sorted(t *testing.T) {
	got := frMapToLines(frRecord{"config": map[string]any{"zeta": "1", "alpha": "2"}}, "config")
	if got != "alpha=2\nzeta=1" {
		t.Errorf("frMapToLines = %q, want sorted alpha then zeta", got)
	}
	if frMapToLines(frRecord{}, "config") != "" {
		t.Error("missing map should render empty")
	}
}

func TestFRBackendField_SelectorWhenCatalogKnown(t *testing.T) {
	// With a catalog, the backend field is a selector that always offers "default".
	f := frBackendField([]string{"kata-prod"}, "")
	if f.kind != fieldSelect {
		t.Fatalf("kind = %v, want fieldSelect", f.kind)
	}
	if f.selectDisplay() != "default" {
		t.Errorf("default selection = %q, want default", f.selectDisplay())
	}
}

func TestFRBackendField_FreeTextFallback(t *testing.T) {
	// With no catalog the field falls back to free text pre-filled with default.
	f := frBackendField(nil, "")
	if f.kind != fieldText {
		t.Errorf("kind = %v, want fieldText fallback", f.kind)
	}
}

// ── Command / screen registration ──────────────────────────────────────────────

// Forge runtime config is an admin control, so it is its own command
// (wired under `armory admin`) rather than a subcommand of the user-facing
// `armory forge`. wireModules only runs from Execute(), so assert against the
// command var directly.
func TestForgeRuntimesCmd_Registered(t *testing.T) {
	if forgeRuntimesCmd.RunE == nil {
		t.Error("forgeRuntimesCmd.RunE should launch the TUI")
	}
	if findSubcmd(t, forgeRuntimesCmd, "tui") == nil {
		t.Error("tui subcommand not registered under forge-runtimes")
	}
}

func TestForgeRuntimesScreen_RegisteredInAdminHub(t *testing.T) {
	for _, s := range adminScreens() {
		if s.Title == "Forge Runtimes" {
			if len(s.Desc) > homeDescWidth {
				t.Errorf("Forge Runtimes desc %q is %d chars, exceeds %d", s.Desc, len(s.Desc), homeDescWidth)
			}
			return
		}
	}
	t.Fatal("Forge Runtimes screen not registered in the admin hub")
}
