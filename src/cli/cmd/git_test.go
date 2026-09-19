package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// ── cobra command RunE (target the command var, not rootCmd) ───────────────────

func TestGitCmd_Backends_RunE(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[{"id":"b1","name":"gh","type":"github","host":"github.com","auth_mode":"pat"}]`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, gitCmd, "backends")
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("git backends: %v", err)
	}
	if rec.Method != "GET" || rec.Path != "/git_connector/backends" {
		t.Errorf("request = %s %s, want GET /git_connector/backends", rec.Method, rec.Path)
	}
}

func TestGitCmd_Add_PostsAuthByMode(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"id":"b1"}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, gitCmd, "add")
	sub.Flags().Set("name", "gh")          //nolint:errcheck
	sub.Flags().Set("type", "github")      //nolint:errcheck
	sub.Flags().Set("auth-mode", "pat")    //nolint:errcheck
	sub.Flags().Set("token", "ghp_secret") //nolint:errcheck
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/git_connector/backends" {
		t.Errorf("request = %s %s, want POST /git_connector/backends", rec.Method, rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	auth, _ := got["auth"].(map[string]any)
	if got["name"] != "gh" || got["type"] != "github" || auth["mode"] != "pat" || auth["token"] != "ghp_secret" {
		t.Errorf("body = %v, want github pat auth", got)
	}
}

func TestGitCmd_Add_RejectsMissingAuthFields(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusCreated, `{}`)
	setupCLI(t, srv)

	sub := findSubcmd(t, gitCmd, "add")
	// Flag vars are package-level and persist across tests; clear --token so a
	// prior test's value can't satisfy the requirement we're asserting is missing.
	sub.Flags().Set("token", "")        //nolint:errcheck
	sub.Flags().Set("name", "gh")       //nolint:errcheck
	sub.Flags().Set("type", "github")   //nolint:errcheck
	sub.Flags().Set("auth-mode", "pat") //nolint:errcheck
	// no --token → must error before any request
	if err := sub.RunE(sub, nil); err == nil {
		t.Fatal("git add without required auth field should error")
	}
}

func TestGitCmd_Rm_Deletes(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusNoContent, ``)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, gitCmd, "rm")
	if err := sub.RunE(sub, []string{"b1"}); err != nil {
		t.Fatalf("git rm: %v", err)
	}
	if rec.Method != "DELETE" || rec.Path != "/git_connector/backends/b1" {
		t.Errorf("request = %s %s, want DELETE /git_connector/backends/b1", rec.Method, rec.Path)
	}
}

func TestGitCmd_Test_PostsToTest(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"ok":true,"backend_type":"github","auth_mode":"pat"}`)
	setupCLI(t, srv)
	silenceStdout(t)

	sub := findSubcmd(t, gitCmd, "test")
	if err := sub.RunE(sub, []string{"b1"}); err != nil {
		t.Fatalf("git test: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/git_connector/backends/b1/test" {
		t.Errorf("request = %s %s, want POST /git_connector/backends/b1/test", rec.Method, rec.Path)
	}
}

func TestGitCmd_Creds_RedactsByDefault(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK,
		`{"type":"basic","username":"x-token","secret":"TOPSECRET","clone_url":"https://github.com/o/r.git","backend":"gh","backend_type":"github"}`)
	setupCLI(t, srv)

	out := captureStdoutDuring(func() {
		sub := findSubcmd(t, gitCmd, "creds")
		if err := sub.RunE(sub, []string{"https://github.com/o/r.git"}); err != nil {
			t.Fatalf("git creds: %v", err)
		}
	})
	if rec.Method != "POST" || rec.Path != "/git_connector/credentials" {
		t.Errorf("request = %s %s, want POST /git_connector/credentials", rec.Method, rec.Path)
	}
	var body map[string]any
	json.Unmarshal(rec.Body, &body) //nolint:errcheck
	if body["repo_url"] != "https://github.com/o/r.git" {
		t.Errorf("body = %v, want repo_url", body)
	}
	if strings.Contains(out, "TOPSECRET") {
		t.Errorf("creds output leaked the secret without --show:\n%s", out)
	}
	if !strings.Contains(out, "https://github.com/o/r.git") {
		t.Errorf("creds output missing clone_url:\n%s", out)
	}
}

func TestGitCmd_Creds_ShowRevealsSecret(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{"secret":"TOPSECRET","clone_url":"https://x/y.git"}`)
	setupCLI(t, srv)

	out := captureStdoutDuring(func() {
		sub := findSubcmd(t, gitCmd, "creds")
		sub.Flags().Set("show", "true") //nolint:errcheck
		if err := sub.RunE(sub, []string{"https://x/y.git"}); err != nil {
			t.Fatalf("git creds --show: %v", err)
		}
	})
	if !strings.Contains(out, "TOPSECRET") {
		t.Errorf("creds --show should reveal the secret:\n%s", out)
	}
}

// ── auth matrix ───────────────────────────────────────────────────────────────

func TestGitBackendAuth_Matrix(t *testing.T) {
	cases := []struct {
		typ, mode string
		flags     gitAuthFlags
		wantKey   string
		wantErr   bool
	}{
		{"github", "app", gitAuthFlags{appID: 1, installationID: 2, privateKey: "pem"}, "private_key", false},
		{"github", "app", gitAuthFlags{appID: 1}, "", true},
		{"github", "pat", gitAuthFlags{token: "t"}, "token", false},
		{"gitlab", "token", gitAuthFlags{token: "t"}, "token", false},
		{"gitlab", "oauth", gitAuthFlags{refreshToken: "r", clientID: "c", clientSecret: "s"}, "refresh_token", false},
		{"forgejo", "token", gitAuthFlags{token: "t", username: "u"}, "username", false},
		{"forgejo", "admin", gitAuthFlags{adminToken: "a", username: "u"}, "admin_token", false},
		{"generic", "basic", gitAuthFlags{username: "u", password: "p"}, "password", false},
		{"generic", "token", gitAuthFlags{token: "t"}, "", true},
		{"bogus", "x", gitAuthFlags{}, "", true},
	}
	for _, c := range cases {
		auth, err := gitBackendAuth(c.typ, c.mode, c.flags)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s/%s: want error, got auth %v", c.typ, c.mode, auth)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s/%s: unexpected error %v", c.typ, c.mode, err)
			continue
		}
		if auth["mode"] != c.mode {
			t.Errorf("%s/%s: mode = %v, want %s", c.typ, c.mode, auth["mode"], c.mode)
		}
		if _, ok := auth[c.wantKey]; !ok {
			t.Errorf("%s/%s: missing field %q in %v", c.typ, c.mode, c.wantKey, auth)
		}
	}
}

// ── TUI ───────────────────────────────────────────────────────────────────────

func applyGitMsg(m gitModel, msg tea.Msg) gitModel {
	updated, _ := m.Update(msg)
	return updated.(gitModel)
}

func TestGtFetchBackends_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[{"id":"b1","name":"gh","type":"github","host":"github.com","auth_mode":"pat"}]`)
	setupCLI(t, srv)
	msg := gtFetchBackends()
	if rec.Path != "/git_connector/backends" {
		t.Errorf("path = %q, want /git_connector/backends", rec.Path)
	}
	backends, ok := msg.(gtBackendsMsg)
	if !ok || len(backends) != 1 || backends[0].Name != "gh" {
		t.Fatalf("msg = %#v, want one backend gh", msg)
	}
}

func TestGtCreateBackend_PostsPayload(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"id":"b1"}`)
	setupCLI(t, srv)
	msg := gtCreateBackend(map[string]any{"name": "gh", "type": "github", "auth": map[string]any{"mode": "pat", "token": "t"}})()
	if _, ok := msg.(gtDoneMsg); !ok {
		t.Fatalf("msg = %T, want gtDoneMsg", msg)
	}
	if rec.Method != "POST" || rec.Path != "/git_connector/backends" {
		t.Errorf("request = %s %s, want POST /git_connector/backends", rec.Method, rec.Path)
	}
}

func TestGtTestBackend_PostsToTest(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"ok":true,"backend_type":"github","auth_mode":"pat","expires_at":"2026-01-01"}`)
	setupCLI(t, srv)
	msg := gtTestBackend(gtBackend{ID: "b1", Name: "gh"})()
	if rec.Method != "POST" || rec.Path != "/git_connector/backends/b1/test" {
		t.Errorf("request = %s %s, want POST /git_connector/backends/b1/test", rec.Method, rec.Path)
	}
	res, ok := msg.(gtResultMsg)
	if !ok || !strings.Contains(res.content, "✓ ok") {
		t.Fatalf("msg = %#v, want a passing test result", msg)
	}
}

func TestGtMintCreds_RedactsUntilRevealed(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"secret":"TOPSECRET","clone_url":"https://x/y.git","backend":"gh"}`)
	setupCLI(t, srv)
	msg := gtMintCreds("https://x/y.git")()
	if rec.Method != "POST" || rec.Path != "/git_connector/credentials" {
		t.Errorf("request = %s %s, want POST /git_connector/credentials", rec.Method, rec.Path)
	}
	cm, ok := msg.(gtCredsMsg)
	if !ok || cm.creds.Secret != "TOPSECRET" {
		t.Fatalf("msg = %#v, want minted creds", msg)
	}
	if strings.Contains(gtCredsContent(cm.creds, false), "TOPSECRET") {
		t.Error("redacted creds content must not contain the secret")
	}
	if !strings.Contains(gtCredsContent(cm.creds, true), "TOPSECRET") {
		t.Error("revealed creds content must contain the secret")
	}
}

func TestGtModel_DeleteRequiresConfirm(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)
	m := applyGitMsg(newGitModel(), gtBackendsMsg([]gtBackend{{ID: "b1", Name: "gh"}}))
	// Press D → a pending confirmation, no request issued yet.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	m2 := updated.(gitModel)
	if m2.pending == nil {
		t.Fatal("D should arm a delete confirmation")
	}
	if cmd != nil {
		t.Error("D alone must not run the delete")
	}
	// Confirm with y → runs the delete command.
	updated2, cmd2 := m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if updated2.(gitModel).pending != nil {
		t.Error("y should clear the pending confirmation")
	}
	if cmd2 == nil {
		t.Error("y should run the delete command")
	}
}

func TestGitScreen_InUserHub(t *testing.T) {
	isolateHome(t)
	for _, s := range hubScreens() {
		if s.Title == "Git Backends" {
			return
		}
	}
	t.Error("Git Backends screen should be registered in the user hub")
}
