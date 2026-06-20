package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func applyEditor(e roleEditorModel, msg tea.Msg) roleEditorModel {
	updated, _ := e.update(msg)
	return updated
}

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// ── Pure helpers ───────────────────────────────────────────────────────────────

func TestSplitList(t *testing.T) {
	got := splitList(" createExecution, listExecution  getExecution ")
	if len(got) != 3 || got[0] != "createExecution" || got[2] != "getExecution" {
		t.Errorf("splitList = %v, want 3 trimmed tokens", got)
	}
	if len(splitList("   ")) != 0 {
		t.Error("blank input should yield no tokens")
	}
}

func TestPermSnapshot_DetectsChange(t *testing.T) {
	a := permSnapshot("n", "forge", []string{"x"}, []string{"r"})
	if a != permSnapshot("n", "forge", []string{"x"}, []string{"r"}) {
		t.Error("identical permissions should snapshot equal")
	}
	if a == permSnapshot("n", "forge", []string{"y"}, []string{"r"}) {
		t.Error("a changed action should change the snapshot")
	}
}

// ── Editor interactions ────────────────────────────────────────────────────────

func TestRoleEditor_AddPermission(t *testing.T) {
	e := newRoleEditor("create", "", "deployer", 100, 30)
	e = applyEditor(e, key("n"))
	if e.sub != rePermForm {
		t.Fatalf("after 'n' sub = %v, want rePermForm", e.sub)
	}
	e.form.setValues(map[string]string{"service": "forge", "actions": "createExecution listExecution", "resources": "alice/forge/executions/*"})
	e = applyEditor(e, tea.KeyMsg{Type: tea.KeyCtrlS})
	if e.sub != reList {
		t.Error("submitting the perm form should return to the list")
	}
	if len(e.perms) != 1 {
		t.Fatalf("perms = %d, want 1", len(e.perms))
	}
	if e.perms[0].service != "forge" || len(e.perms[0].actions) != 2 {
		t.Errorf("added perm = %+v, want forge with 2 actions", e.perms[0])
	}
}

func TestRoleEditor_AddPermission_RequiresService(t *testing.T) {
	e := newRoleEditor("create", "", "r", 100, 30)
	e = applyEditor(e, key("n"))
	e = applyEditor(e, tea.KeyMsg{Type: tea.KeyCtrlS}) // no service entered
	if e.sub != rePermForm {
		t.Error("submitting without a service should keep the perm form open")
	}
	if e.form.errMsg == "" {
		t.Error("missing service should set an inline error")
	}
	if len(e.perms) != 0 {
		t.Error("no permission should have been added")
	}
}

func TestRoleEditor_EditPermission(t *testing.T) {
	e := newRoleEditor("edit", "role-1", "admins", 100, 30)
	e.perms = []rolePerm{{id: "perm-a", name: "a", service: "forge", actions: []string{"old"}, snapshot: permSnapshot("a", "forge", []string{"old"}, nil)}}
	e.refreshTable()
	e = applyEditor(e, key("e"))
	if e.sub != rePermForm || e.editIdx != 0 {
		t.Fatalf("editing should open the perm form at index 0 (sub=%v idx=%d)", e.sub, e.editIdx)
	}
	e.form.setValues(map[string]string{"actions": "new"})
	e = applyEditor(e, tea.KeyMsg{Type: tea.KeyCtrlS})
	if len(e.perms) != 1 || len(e.perms[0].actions) != 1 || e.perms[0].actions[0] != "new" {
		t.Errorf("perm actions = %v, want [new]", e.perms[0].actions)
	}
	// The id and snapshot must be preserved so save can detect the change as a PUT.
	if e.perms[0].id != "perm-a" {
		t.Errorf("edited perm lost its id: %q", e.perms[0].id)
	}
}

func TestRoleEditor_RemovePermission(t *testing.T) {
	e := newRoleEditor("edit", "role-1", "admins", 100, 30)
	e.perms = []rolePerm{{id: "perm-a", service: "forge"}}
	e.refreshTable()
	e = applyEditor(e, key("D"))
	if len(e.perms) != 0 {
		t.Errorf("perms after remove = %d, want 0", len(e.perms))
	}
}

func TestRoleEditor_SetName(t *testing.T) {
	e := newRoleEditor("create", "", "", 100, 30)
	e = applyEditor(e, key("r"))
	if e.sub != reNameForm {
		t.Fatalf("'r' should open the name form, sub=%v", e.sub)
	}
	e.form.setValues(map[string]string{"name": "deployer"})
	e = applyEditor(e, tea.KeyMsg{Type: tea.KeyCtrlS})
	if e.name != "deployer" {
		t.Errorf("name = %q, want deployer", e.name)
	}
}

func TestRoleEditor_SaveRequiresName(t *testing.T) {
	e := newRoleEditor("create", "", "", 100, 30)
	updated, cmd := e.update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd != nil {
		t.Error("save with an empty name should not emit a request cmd")
	}
	if updated.err == nil {
		t.Error("save with an empty name should set an inline error")
	}
}

func TestRoleEditor_Esc_Cancels(t *testing.T) {
	e := newRoleEditor("create", "", "r", 100, 30)
	_, cmd := e.update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should emit a cmd")
	}
	if _, ok := cmd().(roleEditorCancelMsg); !ok {
		t.Errorf("esc returned %T, want roleEditorCancelMsg", cmd())
	}
}

// ── Save (the split) ────────────────────────────────────────────────────────

func TestRoleEditorSave_CreateSplitsPermsThenRole(t *testing.T) {
	var permBody, roleBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /gatekeeper/permissions", func(w http.ResponseWriter, r *http.Request) {
		permBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"permissions_id":"perm-new"}`)) //nolint:errcheck
	})
	mux.HandleFunc("POST /gatekeeper/roles", func(w http.ResponseWriter, r *http.Request) {
		roleBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"role_id":"role-1"}`)) //nolint:errcheck
	})
	setupCLI(t, routeServer(t, mux))

	perms := []rolePerm{{name: "p1", service: "forge", actions: []string{"createExecution"}, resources: []string{"alice/forge/executions"}}}
	msg := roleEditorSave("create", "", "deployer", perms)()
	if _, ok := msg.(roleEditorDoneMsg); !ok {
		t.Fatalf("msg = %T, want roleEditorDoneMsg", msg)
	}

	var pb map[string]any
	if err := json.Unmarshal(permBody, &pb); err != nil {
		t.Fatalf("permission body not JSON: %v", err)
	}
	if pb["service"] != "forge" {
		t.Errorf("permission service = %v, want forge", pb["service"])
	}
	var rb map[string]any
	if err := json.Unmarshal(roleBody, &rb); err != nil {
		t.Fatalf("role body not JSON: %v", err)
	}
	if rb["name"] != "deployer" {
		t.Errorf("role name = %v, want deployer", rb["name"])
	}
	ids, _ := rb["permissions_ids"].([]any)
	if len(ids) != 1 || ids[0] != "perm-new" {
		t.Errorf("role permissions_ids = %v, want [perm-new] (the freshly created id)", ids)
	}
}

func TestRoleEditorSave_EditPutsChangedSkipsUnchanged(t *testing.T) {
	var puts []string
	var rolePath string
	var roleBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /gatekeeper/permissions/{id}", func(w http.ResponseWriter, r *http.Request) {
		puts = append(puts, r.PathValue("id"))
		w.Write([]byte(`{}`)) //nolint:errcheck
	})
	mux.HandleFunc("PUT /gatekeeper/roles/{id}", func(w http.ResponseWriter, r *http.Request) {
		rolePath = r.URL.Path
		roleBody, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{}`)) //nolint:errcheck
	})
	setupCLI(t, routeServer(t, mux))

	changed := rolePerm{id: "perm-a", name: "a", service: "forge", actions: []string{"new"}, snapshot: permSnapshot("a", "forge", []string{"old"}, nil)}
	unchanged := rolePerm{id: "perm-b", name: "b", service: "forge", actions: []string{"y"}, snapshot: permSnapshot("b", "forge", []string{"y"}, nil)}
	msg := roleEditorSave("edit", "role-1", "admins", []rolePerm{changed, unchanged})()
	if _, ok := msg.(roleEditorDoneMsg); !ok {
		t.Fatalf("msg = %T, want roleEditorDoneMsg", msg)
	}
	if len(puts) != 1 || puts[0] != "perm-a" {
		t.Errorf("permission PUTs = %v, want only the changed [perm-a]", puts)
	}
	if rolePath != "/gatekeeper/roles/role-1" {
		t.Errorf("role PUT path = %q, want /gatekeeper/roles/role-1", rolePath)
	}
	var rb map[string]any
	json.Unmarshal(roleBody, &rb) //nolint:errcheck
	if ids, _ := rb["permissions_ids"].([]any); len(ids) != 2 {
		t.Errorf("role permissions_ids = %v, want both ids", rb["permissions_ids"])
	}
}

func TestRoleEditorSave_ReadonlyPassedThroughWithoutCall(t *testing.T) {
	var permCalls int
	var roleBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/gatekeeper/permissions", func(w http.ResponseWriter, r *http.Request) { permCalls++ })
	mux.HandleFunc("/gatekeeper/permissions/{id}", func(w http.ResponseWriter, r *http.Request) { permCalls++ })
	mux.HandleFunc("POST /gatekeeper/roles", func(w http.ResponseWriter, r *http.Request) {
		roleBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`)) //nolint:errcheck
	})
	setupCLI(t, routeServer(t, mux))

	msg := roleEditorSave("create", "", "r", []rolePerm{{id: "perm-ro", readonly: true}})()
	if _, ok := msg.(roleEditorDoneMsg); !ok {
		t.Fatalf("msg = %T, want roleEditorDoneMsg", msg)
	}
	if permCalls != 0 {
		t.Errorf("a read-only permission must not trigger any permission call, got %d", permCalls)
	}
	var rb map[string]any
	json.Unmarshal(roleBody, &rb) //nolint:errcheck
	if ids, _ := rb["permissions_ids"].([]any); len(ids) != 1 || ids[0] != "perm-ro" {
		t.Errorf("role permissions_ids = %v, want [perm-ro] preserved by id", rb["permissions_ids"])
	}
}

func TestRoleEditorSave_PermFailureStops(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /gatekeeper/permissions", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	mux.HandleFunc("POST /gatekeeper/roles", func(w http.ResponseWriter, r *http.Request) {
		t.Error("role must not be saved when a permission failed")
	})
	setupCLI(t, routeServer(t, mux))

	msg := roleEditorSave("create", "", "r", []rolePerm{{service: "forge"}})()
	if _, ok := msg.(roleEditorErrMsg); !ok {
		t.Fatalf("msg = %T, want roleEditorErrMsg", msg)
	}
}

// ── Resolve ─────────────────────────────────────────────────────────────────

func TestRoleEditorResolve_BuildsPermsAndReadonlyStub(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /gatekeeper/permissions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "bad" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Write([]byte(`{"permissions_id":"ok","name":"n","service":"forge","actions":["a"],"resources":["r"]}`)) //nolint:errcheck
	})
	setupCLI(t, routeServer(t, mux))

	msg := roleEditorResolve("role-1", "admins", []string{"ok", "bad"})()
	resolved, ok := msg.(rolePermsResolvedMsg)
	if !ok {
		t.Fatalf("msg = %T, want rolePermsResolvedMsg", msg)
	}
	if len(resolved.perms) != 2 {
		t.Fatalf("perms = %d, want 2", len(resolved.perms))
	}
	if resolved.perms[0].service != "forge" || resolved.perms[0].snapshot == "" {
		t.Errorf("first perm should be fully resolved with a snapshot: %+v", resolved.perms[0])
	}
	if !resolved.perms[1].readonly {
		t.Error("an unfetchable permission should become a read-only stub")
	}
}

// ── Gatekeeper integration ──────────────────────────────────────────────────

func gkRolesModel(t *testing.T) gatekeeperModel {
	t.Helper()
	m := newGatekeeperModel()
	m.section = gkRoles
	updated, _ := m.Update(gkRecordsMsg{section: gkRoles, records: []gkRecord{
		{"role_id": "role-1", "name": "admins", "permissions_ids": []any{"perm-a"}},
	}})
	return updated.(gatekeeperModel)
}

func TestGatekeeper_RolesN_OpensCreateEditor(t *testing.T) {
	m := gkRolesModel(t)
	updated, _ := m.Update(key("n"))
	m2 := updated.(gatekeeperModel)
	if m2.view != gkViewRoleEdit || m2.roleEditor.mode != "create" {
		t.Errorf("'n' on roles should open the create editor (view=%v mode=%q)", m2.view, m2.roleEditor.mode)
	}
}

func TestGatekeeper_RolesE_OpensEditEditor(t *testing.T) {
	m := gkRolesModel(t)
	updated, cmd := m.Update(key("e"))
	m2 := updated.(gatekeeperModel)
	if m2.view != gkViewRoleEdit || m2.roleEditor.mode != "edit" || m2.roleEditor.roleID != "role-1" {
		t.Errorf("'e' on roles should open the edit editor for role-1 (view=%v mode=%q id=%q)", m2.view, m2.roleEditor.mode, m2.roleEditor.roleID)
	}
	if cmd == nil {
		t.Error("opening the edit editor should emit a permission-resolve cmd")
	}
}

func TestGatekeeper_RoleEditorDone_RefreshesRoles(t *testing.T) {
	m := gkRolesModel(t)
	m.view = gkViewRoleEdit
	updated, cmd := m.Update(roleEditorDoneMsg{})
	m2 := updated.(gatekeeperModel)
	if m2.view != gkViewList {
		t.Error("a saved role should return to the list view")
	}
	if !m2.loading || cmd == nil {
		t.Error("a saved role should refetch the roles section")
	}
}

func TestGatekeeper_RoleEditorCancel_ReturnsToList(t *testing.T) {
	m := gkRolesModel(t)
	m.view = gkViewRoleEdit
	updated, _ := m.Update(roleEditorCancelMsg{})
	if updated.(gatekeeperModel).view != gkViewList {
		t.Error("cancelling the editor should return to the list view")
	}
}
