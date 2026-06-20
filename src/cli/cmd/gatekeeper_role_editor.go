package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// The role editor lets an admin edit a role together with the permissions it
// grants as a single item — a role has no meaning without its permissions, and
// gatekeeper has no list-permissions endpoint, so editing them apart is clumsy.
// On save the composite is SPLIT back into the API's separate resources: each
// permission is created (POST) or, if changed, updated (PUT); the resulting ids
// are collected; then the role is created/updated (POST/PUT) referencing them.
//
// Removing a permission here detaches it from the role (drops it from
// permissions_ids); it does not DELETE the permission record, since the same
// permission may be referenced by other roles. The standalone
// `armory admin permissions` command remains for deleting permission records.

// ── Permission row ────────────────────────────────────────────────────────────

// rolePerm is one permission as edited inside a role. id is empty for a
// permission added in this session (created on save). snapshot is the canonical
// form of the persisted permission, used to detect edits so unchanged
// permissions skip their PUT. readonly marks a permission whose record could not
// be fetched (e.g. the admin lacks getPermission): it is preserved by id on save
// but cannot be edited.
type rolePerm struct {
	id        string
	name      string
	service   string
	actions   []string
	resources []string
	snapshot  string
	readonly  bool
}

// permSnapshot is a stable serialisation of a permission's editable fields, used
// to tell whether an existing permission changed and needs a PUT.
func permSnapshot(name, service string, actions, resources []string) string {
	b, _ := json.Marshal([]any{name, service, actions, resources})
	return string(b)
}

// splitList parses a comma/whitespace-separated field into a trimmed,
// empty-free slice. Permission actions and resources contain neither commas nor
// spaces, so either separator works.
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// ── Messages ──────────────────────────────────────────────────────────────────

// rolePermsResolvedMsg carries the permissions resolved for an edited role.
type rolePermsResolvedMsg struct {
	roleID string
	name   string
	perms  []rolePerm
}
type roleEditorDoneMsg struct{}
type roleEditorCancelMsg struct{}
type roleEditorErrMsg struct{ err error }

// ── Model ──────────────────────────────────────────────────────────────────────

type reView int

const (
	reList     reView = iota // role name + permissions table
	rePermForm               // add/edit one permission
	reNameForm               // edit the role name
)

type roleEditorModel struct {
	mode    string // "create" | "edit"
	roleID  string // PUT target (edit mode)
	name    string
	perms   []rolePerm
	sub     reView // active sub-view (list / perm form / name form)
	loading bool   // resolving an existing role's permissions
	saving  bool
	err     error

	table   table.Model
	form    tuiForm
	editIdx int // perm index being edited; -1 when adding

	width  int
	height int
}

var roleEditorPermCols = []tuiColSpec{{"NAME", 14, 1}, {"SERVICE", 12, 0}, {"ACTIONS", 18, 2}, {"RESOURCES", 22, 2}}

func newRoleEditor(mode, roleID, name string, width, height int) roleEditorModel {
	t := table.New(table.WithFocused(true))
	t.SetStyles(tuiTableStyles())
	e := roleEditorModel{
		mode: mode, roleID: roleID, name: name,
		sub: reList, editIdx: -1,
		width: width, height: height, table: t,
	}
	e.applyTableLayout()
	return e
}

func (e *roleEditorModel) applyTableLayout() {
	e.table.SetColumns(tuiFitColumns(roleEditorPermCols, e.width))
	// Extra chrome beyond a plain list: the title and name header lines.
	e.table.SetHeight(tuiTableHeight(e.height, tuiListChrome+2))
}

func (e *roleEditorModel) refreshTable() {
	rows := make([]table.Row, len(e.perms))
	for i, p := range e.perms {
		rows[i] = table.Row{frDash(p.name), frDash(p.service), strings.Join(p.actions, ", "), strings.Join(p.resources, ", ")}
	}
	e.table.SetRows(rows)
}

// ── Commands ────────────────────────────────────────────────────────────────

// roleEditorResolve fetches each of a role's permissions so they can be edited.
// A permission that can't be fetched is kept as a read-only stub (preserved by
// id on save) so one unreadable permission doesn't drop the rest or block edits.
func roleEditorResolve(roleID, name string, ids []string) tea.Cmd {
	return func() tea.Msg {
		perms := make([]rolePerm, 0, len(ids))
		for _, id := range ids {
			data, err := doRequest("GET", "/gatekeeper/permissions/"+id, nil)
			if err != nil {
				perms = append(perms, rolePerm{id: id, name: "(unreadable)", readonly: true})
				continue
			}
			var p struct {
				PermissionsID string   `json:"permissions_id"`
				Name          string   `json:"name"`
				Service       string   `json:"service"`
				Actions       []string `json:"actions"`
				Resources     []string `json:"resources"`
			}
			if err := json.Unmarshal(data, &p); err != nil {
				perms = append(perms, rolePerm{id: id, name: "(unreadable)", readonly: true})
				continue
			}
			perms = append(perms, rolePerm{
				id: p.PermissionsID, name: p.Name, service: p.Service,
				actions: p.Actions, resources: p.Resources,
				snapshot: permSnapshot(p.Name, p.Service, p.Actions, p.Resources),
			})
		}
		return rolePermsResolvedMsg{roleID: roleID, name: name, perms: perms}
	}
}

// roleEditorSave performs the split: create/update permissions, then create/
// update the role with the resulting permission ids. Any failure stops the run
// and reports which permission/role failed, so a partial save is never silently
// reported as success.
func roleEditorSave(mode, roleID, name string, perms []rolePerm) tea.Cmd {
	return func() tea.Msg {
		ids := make([]string, 0, len(perms))
		for _, p := range perms {
			if p.readonly {
				if p.id != "" {
					ids = append(ids, p.id)
				}
				continue
			}
			actions, resources := p.actions, p.resources
			if actions == nil {
				actions = []string{}
			}
			if resources == nil {
				resources = []string{}
			}
			body, _ := json.Marshal(map[string]any{
				"name": p.name, "service": p.service, "actions": actions, "resources": resources,
			})
			label := p.name
			if label == "" {
				label = p.service
			}
			if p.id == "" {
				data, err := doRequest("POST", "/gatekeeper/permissions", body)
				if err != nil {
					return roleEditorErrMsg{fmt.Errorf("create permission %q: %w", label, err)}
				}
				var created struct {
					PermissionsID string `json:"permissions_id"`
				}
				if err := json.Unmarshal(data, &created); err != nil || created.PermissionsID == "" {
					return roleEditorErrMsg{fmt.Errorf("create permission %q: no id returned", label)}
				}
				ids = append(ids, created.PermissionsID)
				continue
			}
			if permSnapshot(p.name, p.service, p.actions, p.resources) != p.snapshot {
				if _, err := doRequest("PUT", "/gatekeeper/permissions/"+p.id, body); err != nil {
					return roleEditorErrMsg{fmt.Errorf("update permission %q: %w", label, err)}
				}
			}
			ids = append(ids, p.id)
		}

		roleBody, _ := json.Marshal(map[string]any{"name": name, "permissions_ids": ids})
		method, path := "POST", "/gatekeeper/roles"
		if mode == "edit" {
			method, path = "PUT", "/gatekeeper/roles/"+roleID
		}
		if _, err := doRequest(method, path, roleBody); err != nil {
			return roleEditorErrMsg{fmt.Errorf("save role: %w", err)}
		}
		return roleEditorDoneMsg{}
	}
}

// ── Update ──────────────────────────────────────────────────────────────────

func (e roleEditorModel) update(msg tea.Msg) (roleEditorModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		e.width, e.height = msg.Width, msg.Height
		e.applyTableLayout()
		return e, nil
	case rolePermsResolvedMsg:
		if msg.roleID == e.roleID {
			e.perms = msg.perms
			e.name = msg.name
			e.loading = false
			e.refreshTable()
		}
		return e, nil
	case roleEditorErrMsg:
		e.saving = false
		e.err = msg.err
		return e, nil
	case tea.KeyMsg:
		switch e.sub {
		case reList:
			return e.keyList(msg)
		case rePermForm:
			return e.keyPermForm(msg)
		case reNameForm:
			return e.keyNameForm(msg)
		}
	}
	var cmd tea.Cmd
	switch e.sub {
	case reList:
		e.table, cmd = e.table.Update(msg)
	case rePermForm, reNameForm:
		e.form, _, cmd = e.form.update(msg)
	}
	return e, cmd
}

func (e roleEditorModel) keyList(msg tea.KeyMsg) (roleEditorModel, tea.Cmd) {
	if e.saving {
		if msg.String() == "ctrl+c" {
			return e, tea.Quit
		}
		return e, nil
	}
	switch msg.String() {
	case "ctrl+c":
		return e, tea.Quit
	case "esc":
		return e, func() tea.Msg { return roleEditorCancelMsg{} }
	case "ctrl+s":
		if strings.TrimSpace(e.name) == "" {
			e.err = fmt.Errorf("role name is required — press [r] to set it")
			return e, nil
		}
		e.saving = true
		e.err = nil
		return e, roleEditorSave(e.mode, e.roleID, e.name, e.perms)
	case "r":
		f, cmd := newTUIForm("Role Name",
			formInputDefault("name", "Name", "role name (required)", e.name))
		e.form = f
		e.sub = reNameForm
		return e, cmd
	case "n":
		f, cmd := newPermForm(rolePerm{}, "Add Permission")
		e.form = f
		e.editIdx = -1
		e.sub = rePermForm
		return e, cmd
	case "e":
		if i := e.table.Cursor(); i >= 0 && i < len(e.perms) {
			if e.perms[i].readonly {
				e.err = fmt.Errorf("permission %q can't be edited (no read access)", e.perms[i].id)
				return e, nil
			}
			f, cmd := newPermForm(e.perms[i], "Edit Permission")
			e.form = f
			e.editIdx = i
			e.sub = rePermForm
			return e, cmd
		}
		return e, nil
	case "D", "d":
		if i := e.table.Cursor(); i >= 0 && i < len(e.perms) {
			e.perms = append(e.perms[:i], e.perms[i+1:]...)
			e.refreshTable()
		}
		return e, nil
	}
	var cmd tea.Cmd
	e.table, cmd = e.table.Update(msg)
	return e, cmd
}

func (e roleEditorModel) keyNameForm(msg tea.KeyMsg) (roleEditorModel, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return e, tea.Quit
	}
	var (
		action formAction
		cmd    tea.Cmd
	)
	e.form, action, cmd = e.form.update(msg)
	switch action {
	case formCancel:
		e.sub = reList
		return e, nil
	case formSubmit:
		e.name = e.form.value("name")
		e.sub = reList
		return e, nil
	}
	return e, cmd
}

func (e roleEditorModel) keyPermForm(msg tea.KeyMsg) (roleEditorModel, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return e, tea.Quit
	}
	var (
		action formAction
		cmd    tea.Cmd
	)
	e.form, action, cmd = e.form.update(msg)
	switch action {
	case formCancel:
		e.sub = reList
		return e, nil
	case formSubmit:
		service := e.form.value("service")
		if service == "" {
			e.form.errMsg = "service is required"
			return e, nil
		}
		np := rolePerm{
			name:      e.form.value("name"),
			service:   service,
			actions:   splitList(e.form.value("actions")),
			resources: splitList(e.form.value("resources")),
		}
		// Preserve the id/snapshot/readonly of the permission being edited so the
		// save step can tell an in-place edit (PUT) from a brand-new permission.
		if e.editIdx >= 0 && e.editIdx < len(e.perms) {
			orig := e.perms[e.editIdx]
			np.id, np.snapshot, np.readonly = orig.id, orig.snapshot, orig.readonly
			e.perms[e.editIdx] = np
		} else {
			e.perms = append(e.perms, np)
		}
		e.sub = reList
		e.refreshTable()
		return e, nil
	}
	return e, cmd
}

// newPermForm builds the add/edit dialog for a single permission, pre-filled
// from p (zero value for a new permission).
func newPermForm(p rolePerm, title string) (tuiForm, tea.Cmd) {
	f, cmd := newTUIForm(title,
		formInput("name", "Name", "label (optional)"),
		formInput("service", "Service", "forge, workflows, gatekeeper, … (required)"),
		formInput("actions", "Actions", "comma/space separated, e.g. createExecution listExecution"),
		formInput("resources", "Resources", "comma/space separated, e.g. alice/forge/executions/*"),
	)
	f.setValues(map[string]string{
		"name":      p.name,
		"service":   p.service,
		"actions":   strings.Join(p.actions, ", "),
		"resources": strings.Join(p.resources, ", "),
	})
	return f, cmd
}

// ── View ────────────────────────────────────────────────────────────────────

func (e roleEditorModel) view(width, height int) string {
	if e.sub == rePermForm || e.sub == reNameForm {
		return e.form.view(width, height)
	}
	titleVerb := "New Role"
	if e.mode == "edit" {
		titleVerb = "Edit Role"
	}
	name := e.name
	if strings.TrimSpace(name) == "" {
		name = tuiMetaStyle.Render("(unset — press r)")
	}
	header := tuiTitleStyle.Render(titleVerb) + "  " + tuiMetaStyle.Render("name: ") + name

	help := tuiHelp("[r] name  [n] add perm  [e] edit  [D] remove  [ctrl+s] save  [esc] cancel", width)

	if e.loading {
		return header + "\n\n" + tuiMetaStyle.Render("Loading permissions…") + "\n\n" + help
	}

	var body string
	if len(e.perms) == 0 {
		body = tuiMetaStyle.Render("No permissions yet — press [n] to add one.")
	} else {
		body = tuiBoxStyle.Render(e.table.View())
	}

	status := ""
	switch {
	case e.saving:
		status = tuiMetaStyle.Render("Saving…") + "\n"
	case e.err != nil:
		status = tuiErrStyle.Render(e.err.Error()) + "\n"
	}
	return header + "\n" + body + "\n" + status + help
}
