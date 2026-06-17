package cmd

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// ── Helpers ──────────────────────────────────────────────────────────────────

func driveSetup(t *testing.T, m setupTUIModel, msg tea.Msg) (setupTUIModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	sm, ok := next.(setupTUIModel)
	if !ok {
		t.Fatalf("Update returned %T, want setupTUIModel", next)
	}
	return sm, cmd
}

func emitsLaunchSetup(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(launchSetupMsg)
	return ok
}

// ── isFirstUse ───────────────────────────────────────────────────────────────

func TestIsFirstUse_NoConfigNoEnv(t *testing.T) {
	isolateHome(t)
	t.Setenv("CODEARMORY_URL", "")
	t.Setenv("CODEARMORY_TOKEN", "")
	if !isFirstUse() {
		t.Error("a fresh HOME with no env vars should report first use")
	}
}

func TestIsFirstUse_ConfigExists(t *testing.T) {
	isolateHome(t)
	t.Setenv("CODEARMORY_URL", "")
	t.Setenv("CODEARMORY_TOKEN", "")
	if err := saveConfig(cliConfig{URL: "http://x"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if isFirstUse() {
		t.Error("once a config file exists, first-use should be false")
	}
}

func TestIsFirstUse_EnvURLSuppresses(t *testing.T) {
	isolateHome(t)
	t.Setenv("CODEARMORY_URL", "http://example.com")
	t.Setenv("CODEARMORY_TOKEN", "")
	if isFirstUse() {
		t.Error("CODEARMORY_URL should suppress the first-use prompt")
	}
}

// ── First-use prompt ─────────────────────────────────────────────────────────

func TestFirstUseModel_YLaunchesSetup(t *testing.T) {
	m := newFirstUseModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !emitsLaunchSetup(cmd) {
		t.Error("y should launch the setup wizard")
	}
}

func TestFirstUseModel_EnterLaunchesSetup(t *testing.T) {
	m := newFirstUseModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !emitsLaunchSetup(cmd) {
		t.Error("enter should launch the setup wizard")
	}
}

func TestFirstUseModel_NDismissesToHome(t *testing.T) {
	m := newFirstUseModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if !emitsGoHome(cmd) {
		t.Error("n should dismiss to the home menu")
	}
}

func TestFirstUseModel_EscDismissesToHome(t *testing.T) {
	m := newFirstUseModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !emitsGoHome(cmd) {
		t.Error("esc should dismiss to the home menu")
	}
}

// ── Wizard ───────────────────────────────────────────────────────────────────

func TestSetupTUI_StartsOnBasics(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSetupTUIModel()
	if m.step != setupStepBasics {
		t.Errorf("step = %d, want setupStepBasics", m.step)
	}
	if m.origTheme != activeThemeName {
		t.Errorf("origTheme = %q, want %q", m.origTheme, activeThemeName)
	}
}

func TestSetupTUI_BasicsLivePreview(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	setActiveTheme("cyber")
	m := newSetupTUIModel()
	// focus the theme selector (second field) and cycle.
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyRight})
	if activeThemeName == "cyber" {
		t.Error("cycling the theme on the basics step should preview a different theme")
	}
	if m.form.value("theme") != activeThemeName {
		t.Errorf("form theme %q out of sync with active theme %q", m.form.value("theme"), activeThemeName)
	}
}

func TestSetupTUI_BasicsCancelRevertsTheme(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	setActiveTheme("cyber")
	m := newSetupTUIModel()
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyRight})
	if activeThemeName == "cyber" {
		t.Fatal("precondition: preview should have changed the active theme")
	}
	_, cmd := driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if !emitsGoHome(cmd) {
		t.Error("esc on basics should return home")
	}
	if activeThemeName != "cyber" {
		t.Errorf("cancel left active theme = %q, want it reverted to cyber", activeThemeName)
	}
}

func TestSetupTUI_BasicsRejectsEmptyURL(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSetupTUIModel()
	m.form.fields[0].input.SetValue("") // clear URL

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("submitting an empty URL should not navigate away")
	}
	sm := next.(setupTUIModel)
	if sm.form.errMsg == "" {
		t.Error("expected an inline error when the URL is empty")
	}
	if sm.step != setupStepBasics {
		t.Errorf("step = %d, want to stay on basics", sm.step)
	}
}

func TestSetupTUI_BasicsAdvancesToAccount(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSetupTUIModel()
	m.form.fields[0].input.SetValue("http://conductor.example/")
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.step != setupStepAccount {
		t.Errorf("step = %d, want setupStepAccount after submitting basics", m.step)
	}
	cfg := loadConfig()
	if cfg.URL != "http://conductor.example" {
		t.Errorf("persisted URL = %q, want trailing slash trimmed", cfg.URL)
	}
	if cfg.Theme == "" {
		t.Error("persisted theme should be set after basics submit")
	}
}

func TestSetupTUI_AccountSkipGoesToResult(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSetupTUIModel()
	m.form.fields[0].input.SetValue("http://conductor.example")
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // basics -> account
	// account form's only field is a select; default is "log in". Cycle left to "skip".
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyLeft})
	if got := m.form.value("action"); got != setupActionSkip {
		t.Fatalf("action = %q, want %q", got, setupActionSkip)
	}
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.step != setupStepResult {
		t.Errorf("step = %d, want setupStepResult after skipping the account step", m.step)
	}
	if len(m.resultLines) == 0 {
		t.Error("result step should populate resultLines")
	}
}

func TestSetupTUI_AccountSignupGoesToCredentialsWithUsername(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSetupTUIModel()
	m.form.fields[0].input.SetValue("http://conductor.example")
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // -> account
	// cycle right once: log in -> sign up
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyRight})
	if got := m.form.value("action"); got != setupActionSignup {
		t.Fatalf("action = %q, want %q", got, setupActionSignup)
	}
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.step != setupStepCredentials {
		t.Fatalf("step = %d, want setupStepCredentials", m.step)
	}
	// signup form has email + username + password.
	if len(m.form.fields) != 3 {
		t.Errorf("signup credentials form has %d fields, want 3 (email, username, password)", len(m.form.fields))
	}
}

func TestSetupTUI_AccountLoginGoesToCredentialsWithoutUsername(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSetupTUIModel()
	m.form.fields[0].input.SetValue("http://conductor.example")
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // -> account
	// default action is log in; submit straight through.
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.step != setupStepCredentials {
		t.Fatalf("step = %d, want setupStepCredentials", m.step)
	}
	if len(m.form.fields) != 2 {
		t.Errorf("login credentials form has %d fields, want 2 (email, password)", len(m.form.fields))
	}
}

func TestSetupTUI_CredentialsCancelGoesBackToAccount(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSetupTUIModel()
	m.form.fields[0].input.SetValue("http://conductor.example")
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // -> account
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // -> credentials (log in)
	if m.step != setupStepCredentials {
		t.Fatal("precondition: should be on credentials step")
	}
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.step != setupStepAccount {
		t.Errorf("step = %d, want setupStepAccount after cancel from credentials", m.step)
	}
}

func TestSetupTUI_CredentialsRejectsEmptyFields(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSetupTUIModel()
	m.form.fields[0].input.SetValue("http://conductor.example")
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // -> account
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // -> credentials (log in)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	sm := next.(setupTUIModel)
	if sm.form.errMsg == "" {
		t.Error("empty email should produce an inline error")
	}
	if sm.step != setupStepCredentials {
		t.Errorf("step = %d, want to stay on credentials when invalid", sm.step)
	}
}

func TestSetupTUI_CredentialsLoginFlow(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)

	var loginBody []byte
	srv := routeServer(t, http.NewServeMux())
	srv.Config.Handler.(*http.ServeMux).HandleFunc("/gatekeeper/login", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf) //nolint:errcheck
		loginBody = buf
		// A structurally valid token (header.payload.signature) is enough — the
		// wizard only stores it; jwtExpiry returns false when there is no exp.
		json.NewEncoder(w).Encode(map[string]string{"token": "h.p.s"}) //nolint:errcheck
	})
	setupCLINoToken(t, srv)

	m := newSetupTUIModel()
	m.form.fields[0].input.SetValue(srv.URL)
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // basics -> account
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // account -> credentials (log in)
	m.form.fields[0].input.SetValue("user@example.com")
	m.form.fields[1].input.SetValue("hunter2")
	m, _ = driveSetup(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.step != setupStepResult {
		t.Errorf("step = %d, want setupStepResult after successful login", m.step)
	}
	if len(loginBody) == 0 {
		t.Fatal("login endpoint was not called")
	}
	var sent map[string]string
	if err := json.Unmarshal(loginBody, &sent); err != nil {
		t.Fatalf("login body not valid JSON: %v", err)
	}
	if sent["email"] != "user@example.com" || sent["password"] != "hunter2" {
		t.Errorf("login body = %v, want email/password populated from form", sent)
	}
}

func TestSetupTUI_ResultAnyKeyReturnsHome(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSetupTUIModel()
	m.step = setupStepResult
	m.resultLines = []string{"done"}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if !emitsGoHome(cmd) {
		t.Error("any key on the result step should return home")
	}
}

// ── Settings integration ─────────────────────────────────────────────────────

func TestSettingsModel_F2LaunchesSetup(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	setActiveTheme("cyber")
	m := newSettingsModel()
	// cycle the theme so we can verify cancellation reverts the preview.
	m, _ = driveSettings(t, m, tea.KeyMsg{Type: tea.KeyRight})
	if activeThemeName == "cyber" {
		t.Fatal("precondition: theme preview should have changed")
	}
	_, cmd := driveSettings(t, m, tea.KeyMsg{Type: tea.KeyF2})
	if !emitsLaunchSetup(cmd) {
		t.Error("F2 in settings should launch the setup wizard")
	}
	if activeThemeName != "cyber" {
		t.Errorf("F2 should discard the live theme preview before leaving; active = %q, want cyber", activeThemeName)
	}
}

// ── Module registration ──────────────────────────────────────────────────────

func TestSetupTUICmd_RegisteredUnderSetup(t *testing.T) {
	if findSubcmd(t, setupCmd, "tui") == nil {
		t.Error("tui subcommand not registered under setup")
	}
}

func TestSetupHubScreen_Registered(t *testing.T) {
	for _, s := range hubScreens() {
		if s.Title == "Setup" {
			return
		}
	}
	t.Error("Setup hub screen not registered on the home menu")
}

// ── appModel integration ─────────────────────────────────────────────────────

func TestAppModel_FirstUseShowsPrompt(t *testing.T) {
	isolateHome(t)
	t.Setenv("CODEARMORY_URL", "")
	t.Setenv("CODEARMORY_TOKEN", "")
	m := newAppModel()
	if _, ok := m.active.(firstUseModel); !ok {
		t.Errorf("active = %T, want firstUseModel on a fresh install", m.active)
	}
}

func TestAppModel_NotFirstUseShowsHome(t *testing.T) {
	isolateHome(t)
	t.Setenv("CODEARMORY_URL", "")
	t.Setenv("CODEARMORY_TOKEN", "")
	// Pre-seed a config so isFirstUse returns false.
	if err := os.MkdirAll(filepath.Dir(configPath()), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := saveConfig(cliConfig{URL: "http://x"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := newAppModel()
	if m.active != nil {
		t.Errorf("active = %T, want nil (home menu) when config already exists", m.active)
	}
}

func TestAppModel_LaunchSetupMsgSwitchesToWizard(t *testing.T) {
	isolateHome(t)
	m := newAppModel()
	next, _ := m.Update(launchSetupMsg{})
	app := next.(appModel)
	if _, ok := app.active.(setupTUIModel); !ok {
		t.Errorf("active = %T, want setupTUIModel after launchSetupMsg", app.active)
	}
}

func TestStandaloneWrap_LaunchSetupMsgSwapsInner(t *testing.T) {
	w := standaloneWrap{inner: newSettingsModel()}
	next, _ := w.Update(launchSetupMsg{})
	sw := next.(standaloneWrap)
	if _, ok := sw.inner.(setupTUIModel); !ok {
		t.Errorf("inner = %T, want setupTUIModel after launchSetupMsg in standalone mode", sw.inner)
	}
}
