package cmd

// commands_test.go exercises the cobra command RunE closures for every
// resource file that was only tested via direct apiCall() in api_test.go.
// Calling RunE directly (same pattern used in auth_test.go) ensures that the
// parseData + apiCall pipeline inside each closure is covered.

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ── test helpers ─────────────────────────────────────────────────────────────

// findSubcmd returns the named direct sub-command of parent or fails the test.
func findSubcmd(t *testing.T, parent *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("sub-command %q not found under %q", name, parent.Name())
	return nil
}

// createTempStateFile writes content to a temp file and returns its path.
func createTempStateFile(t *testing.T, content string) (string, error) {
	t.Helper()
	f, err := os.CreateTemp("", "state-*.tfstate")
	if err != nil {
		return "", err
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	f.WriteString(content)
	f.Close()
	return f.Name(), nil
}

// ── Users command RunE ────────────────────────────────────────────────────────

func TestUsersCmd_List_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[{"id":"u1"}]`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, usersCmd, "list")
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("users list: %v", err)
	}
	if rec.Method != "GET" || rec.Path != "/users" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestUsersCmd_Get_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"id":"abc"}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, usersCmd, "get")
	if err := sub.RunE(sub, []string{"abc"}); err != nil {
		t.Fatalf("users get: %v", err)
	}
	if rec.Path != "/users/abc" {
		t.Errorf("path = %q, want /users/abc", rec.Path)
	}
}

func TestUsersCmd_Update_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, usersCmd, "update")
	sub.Flags().Set("data", `{"username":"newname"}`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"uid-1"}); err != nil {
		t.Fatalf("users update: %v", err)
	}
	if rec.Method != "PUT" || rec.Path != "/users/uid-1" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
	if !strings.Contains(string(rec.Body), "newname") {
		t.Errorf("body missing username: %q", rec.Body)
	}
}

func TestUsersCmd_Update_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, usersCmd, "update")
	sub.Flags().Set("data", `not json`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"uid-1"}); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestUsersCmd_Delete_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, usersCmd, "delete")
	if err := sub.RunE(sub, []string{"uid-del"}); err != nil {
		t.Fatalf("users delete: %v", err)
	}
	if rec.Method != "DELETE" || rec.Path != "/users/uid-del" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

// ── Invites command RunE ──────────────────────────────────────────────────────

func TestInvitesCmd_List_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, invitesCmd, "list")
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("invites list: %v", err)
	}
	if rec.Method != "GET" || rec.Path != "/invites" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestInvitesCmd_Get_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"id":"inv1"}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, invitesCmd, "get")
	if err := sub.RunE(sub, []string{"inv1"}); err != nil {
		t.Fatalf("invites get: %v", err)
	}
	if rec.Path != "/invites/inv1" {
		t.Errorf("path = %q, want /invites/inv1", rec.Path)
	}
}

func TestInvitesCmd_Accept_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, invitesCmd, "accept")
	if err := sub.RunE(sub, []string{"inv2"}); err != nil {
		t.Fatalf("invites accept: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/invites/inv2/accept" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestInvitesCmd_Decline_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, invitesCmd, "decline")
	if err := sub.RunE(sub, []string{"inv3"}); err != nil {
		t.Fatalf("invites decline: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/invites/inv3/decline" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestInvitesCmd_Delete_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, invitesCmd, "delete")
	if err := sub.RunE(sub, []string{"inv4"}); err != nil {
		t.Fatalf("invites delete: %v", err)
	}
	if rec.Method != "DELETE" || rec.Path != "/invites/inv4" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

// ── Orgs command RunE ─────────────────────────────────────────────────────────

func TestOrgsCmd_List_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, orgsCmd, "list")
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("orgs list: %v", err)
	}
	if rec.Method != "GET" || rec.Path != "/orgs" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestOrgsCmd_Get_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"id":"org1"}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, orgsCmd, "get")
	if err := sub.RunE(sub, []string{"org1"}); err != nil {
		t.Fatalf("orgs get: %v", err)
	}
	if rec.Path != "/orgs/org1" {
		t.Errorf("path = %q, want /orgs/org1", rec.Path)
	}
}

func TestOrgsCmd_Create_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"id":"new-org"}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, orgsCmd, "create")
	sub.Flags().Set("data", `{"name":"acme"}`) //nolint:errcheck
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("orgs create: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/orgs" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
	if !strings.Contains(string(rec.Body), "acme") {
		t.Errorf("body missing org name: %q", rec.Body)
	}
}

func TestOrgsCmd_Create_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, orgsCmd, "create")
	sub.Flags().Set("data", `{bad json}`) //nolint:errcheck
	if err := sub.RunE(sub, nil); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestOrgsCmd_Update_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, orgsCmd, "update")
	sub.Flags().Set("data", `{"name":"renamed"}`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"org2"}); err != nil {
		t.Fatalf("orgs update: %v", err)
	}
	if rec.Method != "PUT" || rec.Path != "/orgs/org2" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestOrgsCmd_Update_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, orgsCmd, "update")
	sub.Flags().Set("data", `not json`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"org2"}); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestOrgsCmd_Delete_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, orgsCmd, "delete")
	if err := sub.RunE(sub, []string{"org-del"}); err != nil {
		t.Fatalf("orgs delete: %v", err)
	}
	if rec.Method != "DELETE" || rec.Path != "/orgs/org-del" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestOrgsCmd_Invite_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, orgsCmd, "invite")
	sub.Flags().Set("data", `{"user_id":"u9"}`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"org3"}); err != nil {
		t.Fatalf("orgs invite: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/orgs/org3/invites" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
	if !strings.Contains(string(rec.Body), "u9") {
		t.Errorf("body missing user_id: %q", rec.Body)
	}
}

func TestOrgsCmd_Invite_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, orgsCmd, "invite")
	sub.Flags().Set("data", `not json`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"org3"}); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

// ── Teams command RunE ────────────────────────────────────────────────────────

func TestTeamsCmd_List_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, teamsCmd, "list")
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("teams list: %v", err)
	}
	if rec.Method != "GET" || rec.Path != "/teams" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestTeamsCmd_Get_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"id":"t1"}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, teamsCmd, "get")
	if err := sub.RunE(sub, []string{"t1"}); err != nil {
		t.Fatalf("teams get: %v", err)
	}
	if rec.Path != "/teams/t1" {
		t.Errorf("path = %q", rec.Path)
	}
}

func TestTeamsCmd_Create_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, teamsCmd, "create")
	sub.Flags().Set("data", `{"name":"backend"}`) //nolint:errcheck
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("teams create: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/teams" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
	if !strings.Contains(string(rec.Body), "backend") {
		t.Errorf("body missing team name: %q", rec.Body)
	}
}

func TestTeamsCmd_Create_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, teamsCmd, "create")
	sub.Flags().Set("data", `{bad}`) //nolint:errcheck
	if err := sub.RunE(sub, nil); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestTeamsCmd_Update_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, teamsCmd, "update")
	sub.Flags().Set("data", `{"name":"frontend"}`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"tid-1"}); err != nil {
		t.Fatalf("teams update: %v", err)
	}
	if rec.Method != "PUT" || rec.Path != "/teams/tid-1" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestTeamsCmd_Update_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, teamsCmd, "update")
	sub.Flags().Set("data", `bad`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"tid-1"}); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestTeamsCmd_Delete_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, teamsCmd, "delete")
	if err := sub.RunE(sub, []string{"tid-del"}); err != nil {
		t.Fatalf("teams delete: %v", err)
	}
	if rec.Method != "DELETE" || rec.Path != "/teams/tid-del" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestTeamsCmd_Invite_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, teamsCmd, "invite")
	sub.Flags().Set("data", `{"user_id":"u7"}`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"tid-2"}); err != nil {
		t.Fatalf("teams invite: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/teams/tid-2/invites" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestTeamsCmd_Invite_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, teamsCmd, "invite")
	sub.Flags().Set("data", `not json`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"tid-2"}); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

// ── Roles command RunE ────────────────────────────────────────────────────────

func TestRolesCmd_Get_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"id":"r1"}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, rolesCmd, "get")
	if err := sub.RunE(sub, []string{"r1"}); err != nil {
		t.Fatalf("roles get: %v", err)
	}
	if rec.Path != "/roles/r1" {
		t.Errorf("path = %q", rec.Path)
	}
}

func TestRolesCmd_Create_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, rolesCmd, "create")
	sub.Flags().Set("data", `{"name":"admin"}`) //nolint:errcheck
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("roles create: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/roles" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
	if !strings.Contains(string(rec.Body), "admin") {
		t.Errorf("body missing role name: %q", rec.Body)
	}
}

func TestRolesCmd_Create_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, rolesCmd, "create")
	sub.Flags().Set("data", `{bad}`) //nolint:errcheck
	if err := sub.RunE(sub, nil); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestRolesCmd_Update_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, rolesCmd, "update")
	sub.Flags().Set("data", `{"name":"viewer"}`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"rid-1"}); err != nil {
		t.Fatalf("roles update: %v", err)
	}
	if rec.Method != "PUT" || rec.Path != "/roles/rid-1" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestRolesCmd_Update_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, rolesCmd, "update")
	sub.Flags().Set("data", `bad json`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"rid-1"}); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestRolesCmd_Delete_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, rolesCmd, "delete")
	if err := sub.RunE(sub, []string{"rid-del"}); err != nil {
		t.Fatalf("roles delete: %v", err)
	}
	if rec.Method != "DELETE" || rec.Path != "/roles/rid-del" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

// ── Permissions command RunE ──────────────────────────────────────────────────

func TestPermissionsCmd_Get_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"id":"p1"}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, permissionsCmd, "get")
	if err := sub.RunE(sub, []string{"p1"}); err != nil {
		t.Fatalf("permissions get: %v", err)
	}
	if rec.Path != "/permissions/p1" {
		t.Errorf("path = %q", rec.Path)
	}
}

func TestPermissionsCmd_Create_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, permissionsCmd, "create")
	sub.Flags().Set("data", `{"action":"read","resource":"repos"}`) //nolint:errcheck
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("permissions create: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/permissions" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
	if !strings.Contains(string(rec.Body), "repos") {
		t.Errorf("body missing resource: %q", rec.Body)
	}
}

func TestPermissionsCmd_Create_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, permissionsCmd, "create")
	sub.Flags().Set("data", `{bad}`) //nolint:errcheck
	if err := sub.RunE(sub, nil); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestPermissionsCmd_Update_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, permissionsCmd, "update")
	sub.Flags().Set("data", `{"action":"write"}`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"pid-1"}); err != nil {
		t.Fatalf("permissions update: %v", err)
	}
	if rec.Method != "PUT" || rec.Path != "/permissions/pid-1" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestPermissionsCmd_Update_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, permissionsCmd, "update")
	sub.Flags().Set("data", `bad`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"pid-1"}); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestPermissionsCmd_Delete_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, permissionsCmd, "delete")
	if err := sub.RunE(sub, []string{"pid-del"}); err != nil {
		t.Fatalf("permissions delete: %v", err)
	}
	if rec.Method != "DELETE" || rec.Path != "/permissions/pid-del" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

// ── Sessions command RunE ─────────────────────────────────────────────────────

func TestSessionsCmd_Get_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"id":"s1"}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, sessionsCmd, "get")
	if err := sub.RunE(sub, []string{"s1"}); err != nil {
		t.Fatalf("sessions get: %v", err)
	}
	if rec.Method != "GET" || rec.Path != "/sessions/s1" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestSessionsCmd_Delete_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, sessionsCmd, "delete")
	if err := sub.RunE(sub, []string{"s-del"}); err != nil {
		t.Fatalf("sessions delete: %v", err)
	}
	if rec.Method != "DELETE" || rec.Path != "/sessions/s-del" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

// ── State command RunE (user-scoped) ──────────────────────────────────────────

func TestStateCmd_Get_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"serial":1}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, stateCmd, "get")
	if err := sub.RunE(sub, []string{"alice", "prod"}); err != nil {
		t.Fatalf("state get: %v", err)
	}
	if rec.Method != "GET" || rec.Path != "/state/alice/prod" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestStateCmd_Push_RunE(t *testing.T) {
	f, err := createTempStateFile(t, `{"serial":5}`)
	if err != nil {
		t.Fatal(err)
	}

	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, stateCmd, "push")
	sub.Flags().Set("file", f) //nolint:errcheck
	if err := sub.RunE(sub, []string{"alice", "prod"}); err != nil {
		t.Fatalf("state push: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/state/alice/prod" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
	if !strings.Contains(string(rec.Body), `"serial":5`) {
		t.Errorf("body missing serial: %q", rec.Body)
	}
}

func TestStateCmd_Push_MissingFile_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, stateCmd, "push")
	sub.Flags().Set("file", "/nonexistent/state.json") //nolint:errcheck
	if err := sub.RunE(sub, []string{"alice", "prod"}); err == nil {
		t.Fatal("expected error for missing state file, got nil")
	}
}

func TestStateCmd_Delete_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, stateCmd, "delete")
	if err := sub.RunE(sub, []string{"alice", "prod"}); err != nil {
		t.Fatalf("state delete: %v", err)
	}
	if rec.Method != "DELETE" || rec.Path != "/state/alice/prod" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestStateCmd_Lock_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, stateCmd, "lock")
	sub.Flags().Set("data", `{"ID":"lock-abc"}`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"alice", "prod"}); err != nil {
		t.Fatalf("state lock: %v", err)
	}
	if rec.Method != "LOCK" || rec.Path != "/state/alice/prod" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
	if !strings.Contains(string(rec.Body), "lock-abc") {
		t.Errorf("lock body missing ID: %q", rec.Body)
	}
}

func TestStateCmd_Lock_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, stateCmd, "lock")
	sub.Flags().Set("data", `not json`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"alice", "prod"}); err == nil {
		t.Fatal("expected error for invalid lock data, got nil")
	}
}

func TestStateCmd_Unlock_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, stateCmd, "unlock")
	sub.Flags().Set("data", `{"ID":"lock-abc"}`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"alice", "prod"}); err != nil {
		t.Fatalf("state unlock: %v", err)
	}
	if rec.Method != "UNLOCK" || rec.Path != "/state/alice/prod" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestStateCmd_Unlock_InvalidData_RunE(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, stateCmd, "unlock")
	sub.Flags().Set("data", `bad`) //nolint:errcheck
	if err := sub.RunE(sub, []string{"alice", "prod"}); err == nil {
		t.Fatal("expected error for invalid unlock data, got nil")
	}
}

// ── Org-state command RunE ────────────────────────────────────────────────────

