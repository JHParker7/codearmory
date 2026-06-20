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

func driveSettings(t *testing.T, m settingsModel, msg tea.Msg) (settingsModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	sm, ok := next.(settingsModel)
	if !ok {
		t.Fatalf("Update returned %T, want settingsModel", next)
	}
	return sm, cmd
}

// keySubmit submits the current form. The shared tuiForm submits via its Submit
// button or ctrl+s; a bare Enter advances between fields (so multi-line fields
// can use Enter for newlines), so tests drive submission with ctrl+s.
var keySubmit = tea.KeyMsg{Type: tea.KeyCtrlS}

// countValueFields counts a form's value-bearing fields, excluding the trailing
// Submit button that newTUIForm appends.
func countValueFields(f tuiForm) int {
	n := 0
	for _, fld := range f.fields {
		if fld.kind != fieldButton {
			n++
		}
	}
	return n
}

func emitsGoHome(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(goHomeMsg)
	return ok
}

func emitsLaunchSettings(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(launchSettingsMsg)
	return ok
}

// ── Basics step ──────────────────────────────────────────────────────────────

func TestSettingsModel_Defaults(t *testing.T) {
	restoreTheme(t)
	setActiveTheme("cyber")
	m := newSettingsModel()
	if m.step != settingsStepBasics {
		t.Errorf("step = %d, want settingsStepBasics", m.step)
	}
	if m.origTheme != "cyber" {
		t.Errorf("origTheme = %q, want cyber", m.origTheme)
	}
	if got := m.form.value("theme"); got != "cyber" {
		t.Errorf("theme field = %q, want cyber", got)
	}
	if got := m.form.value("url"); got == "" {
		t.Error("url field should be pre-filled with the configured URL")
	}
	if got := m.form.value("account"); got != settingsAccountKeep {
		t.Errorf("account field = %q, want %q (the quick path default)", got, settingsAccountKeep)
	}
}

func TestSettingsModel_LivePreviewCyclesTheme(t *testing.T) {
	restoreTheme(t)
	setActiveTheme("cyber")
	m := newSettingsModel()

	m, _ = driveSettings(t, m, tea.KeyMsg{Type: tea.KeyRight})
	if activeThemeName == "cyber" {
		t.Error("cycling the theme selector should preview a different theme live")
	}
	if m.form.value("theme") != activeThemeName {
		t.Errorf("form theme %q out of sync with active theme %q", m.form.value("theme"), activeThemeName)
	}
}

func TestSettingsModel_CancelRevertsPreview(t *testing.T) {
	restoreTheme(t)
	setActiveTheme("cyber")
	m := newSettingsModel()

	m, _ = driveSettings(t, m, tea.KeyMsg{Type: tea.KeyRight}) // preview a non-default theme
	if activeThemeName == "cyber" {
		t.Fatal("precondition: preview should have changed the active theme")
	}
	_, cmd := driveSettings(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if !emitsGoHome(cmd) {
		t.Error("esc should return to home")
	}
	if activeThemeName != "cyber" {
		t.Errorf("cancel left active theme = %q, want it reverted to cyber", activeThemeName)
	}
}

func TestSettingsModel_KeepSavesAndGoesHome(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	setActiveTheme("cyber")
	m := newSettingsModel()

	m, _ = driveSettings(t, m, tea.KeyMsg{Type: tea.KeyRight}) // cyber -> tokyo-night
	want := activeThemeName

	_, cmd := driveSettings(t, m, keySubmit)
	if !emitsGoHome(cmd) {
		t.Error("submitting with account=keep should save and return home")
	}
	cfg := loadConfig()
	if cfg.Theme != want {
		t.Errorf("persisted theme = %q, want %q", cfg.Theme, want)
	}
	if cfg.URL == "" {
		t.Error("persisted URL should not be empty")
	}
	if activeThemeName != want {
		t.Errorf("active theme after save = %q, want %q", activeThemeName, want)
	}
}

func TestSettingsModel_SaveRejectsEmptyURL(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSettingsModel()
	m.form.fields[1].input.SetValue("") // clear the URL field

	next, cmd := m.Update(keySubmit)
	if cmd != nil {
		t.Error("submitting an empty URL should not navigate away")
	}
	sm := next.(settingsModel)
	if sm.form.errMsg == "" {
		t.Error("expected an inline error when the URL is empty")
	}
	if sm.step != settingsStepBasics {
		t.Errorf("step = %d, want to stay on basics", sm.step)
	}
	if loadConfig().Theme != "" {
		t.Error("nothing should be persisted when validation fails")
	}
}

func TestSettingsModel_BasicsTrimsTrailingSlash(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSettingsModel()
	m.form.fields[1].input.SetValue("http://conductor.example/")
	m, _ = driveSettings(t, m, keySubmit)

	cfg := loadConfig()
	if cfg.URL != "http://conductor.example" {
		t.Errorf("persisted URL = %q, want trailing slash trimmed", cfg.URL)
	}
	if cfg.Theme == "" {
		t.Error("persisted theme should be set after basics submit")
	}
}

// ── Account flow ─────────────────────────────────────────────────────────────

// setAccount cycles the account selector to want. The selector is the third
// field (index 2) and defaults to "keep"; tabbing to it and cycling right walks
// keep -> log in -> sign up.
func setAccount(t *testing.T, m settingsModel, want string) settingsModel {
	t.Helper()
	// Move focus to the account selector (theme, url, account).
	m, _ = driveSettings(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m, _ = driveSettings(t, m, tea.KeyMsg{Type: tea.KeyTab})
	for i := 0; i < 3 && m.form.value("account") != want; i++ {
		m, _ = driveSettings(t, m, tea.KeyMsg{Type: tea.KeyRight})
	}
	if got := m.form.value("account"); got != want {
		t.Fatalf("account = %q, want %q", got, want)
	}
	return m
}

func TestSettingsModel_LoginGoesToCredentialsWithoutUsername(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSettingsModel()
	m.form.fields[1].input.SetValue("http://conductor.example")
	m = setAccount(t, m, settingsAccountLogin)
	m, _ = driveSettings(t, m, keySubmit)

	if m.step != settingsStepCredentials {
		t.Fatalf("step = %d, want settingsStepCredentials", m.step)
	}
	if n := countValueFields(m.form); n != 2 {
		t.Errorf("login credentials form has %d value fields, want 2 (email, password)", n)
	}
	if loadConfig().URL != "http://conductor.example" {
		t.Error("basics should be persisted before advancing to credentials")
	}
}

func TestSettingsModel_SignupGoesToCredentialsWithUsername(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSettingsModel()
	m.form.fields[1].input.SetValue("http://conductor.example")
	m = setAccount(t, m, settingsAccountSignup)
	m, _ = driveSettings(t, m, keySubmit)

	if m.step != settingsStepCredentials {
		t.Fatalf("step = %d, want settingsStepCredentials", m.step)
	}
	if n := countValueFields(m.form); n != 3 {
		t.Errorf("signup credentials form has %d value fields, want 3 (email, username, password)", n)
	}
}

func TestSettingsModel_CredentialsCancelGoesBackToBasics(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSettingsModel()
	m.form.fields[1].input.SetValue("http://conductor.example")
	m = setAccount(t, m, settingsAccountLogin)
	m, _ = driveSettings(t, m, keySubmit) // -> credentials
	if m.step != settingsStepCredentials {
		t.Fatal("precondition: should be on credentials step")
	}
	m, _ = driveSettings(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.step != settingsStepBasics {
		t.Errorf("step = %d, want settingsStepBasics after cancel from credentials", m.step)
	}
}

func TestSettingsModel_CredentialsRejectsEmptyFields(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSettingsModel()
	m.form.fields[1].input.SetValue("http://conductor.example")
	m = setAccount(t, m, settingsAccountLogin)
	m, _ = driveSettings(t, m, keySubmit) // -> credentials

	next, _ := m.Update(keySubmit)
	sm := next.(settingsModel)
	if sm.form.errMsg == "" {
		t.Error("empty email should produce an inline error")
	}
	if sm.step != settingsStepCredentials {
		t.Errorf("step = %d, want to stay on credentials when invalid", sm.step)
	}
}

func TestSettingsModel_CredentialsLoginFlow(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)

	var loginBody []byte
	srv := routeServer(t, http.NewServeMux())
	srv.Config.Handler.(*http.ServeMux).HandleFunc("/gatekeeper/login", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf) //nolint:errcheck
		loginBody = buf
		// A structurally valid token (header.payload.signature) is enough — the
		// screen only stores it; jwtExpiry returns false when there is no exp.
		json.NewEncoder(w).Encode(map[string]string{"token": "h.p.s"}) //nolint:errcheck
	})
	setupCLINoToken(t, srv)

	m := newSettingsModel()
	m.form.fields[1].input.SetValue(srv.URL)
	m = setAccount(t, m, settingsAccountLogin)
	m, _ = driveSettings(t, m, keySubmit) // basics -> credentials (log in)
	m.form.fields[0].input.SetValue("user@example.com")
	m.form.fields[1].input.SetValue("hunter2")
	m, _ = driveSettings(t, m, keySubmit)

	if m.step != settingsStepResult {
		t.Errorf("step = %d, want settingsStepResult after successful login", m.step)
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

func TestSettingsModel_ResultAnyKeyReturnsHome(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	m := newSettingsModel()
	m.step = settingsStepResult
	m.resultLines = []string{"done"}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if !emitsGoHome(cmd) {
		t.Error("any key on the result step should return home")
	}
}

// ── First-use prompt ─────────────────────────────────────────────────────────

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

func TestFirstUseModel_YLaunchesSettings(t *testing.T) {
	m := newFirstUseModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !emitsLaunchSettings(cmd) {
		t.Error("y should launch the settings screen")
	}
}

func TestFirstUseModel_EnterLaunchesSettings(t *testing.T) {
	m := newFirstUseModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !emitsLaunchSettings(cmd) {
		t.Error("enter should launch the settings screen")
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

// ── Theme aliases ────────────────────────────────────────────────────────────

func TestResolveThemeName_NewAliases(t *testing.T) {
	cases := map[string]string{
		"dracula":          "dracula",
		"nord":             "nord",
		"gruvbox-dark":     "gruvbox",
		"catppuccin-mocha": "catppuccin",
		"mocha":            "catppuccin",
		"solarized-dark":   "solarized",
	}
	for in, want := range cases {
		if got := resolveThemeName(in, "", ""); got != want {
			t.Errorf("resolveThemeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── Module registration ──────────────────────────────────────────────────────

func TestSettingsHubScreen_Registered(t *testing.T) {
	for _, s := range hubScreens() {
		if s.Title == "Settings" {
			return
		}
	}
	t.Error("Settings hub screen not registered on the home menu")
}

func TestSetupHubScreen_Removed(t *testing.T) {
	for _, s := range hubScreens() {
		if s.Title == "Setup" {
			t.Error("Setup hub screen should be removed after merging into Settings")
		}
	}
}

func TestSetupCmd_HasNoTuiSubcommand(t *testing.T) {
	for _, c := range setupCmd.Commands() {
		if c.Name() == "tui" {
			t.Error("`armory setup tui` should be removed after merging into settings")
		}
	}
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

func TestAppModel_LaunchSettingsMsgSwitchesToSettings(t *testing.T) {
	isolateHome(t)
	m := newAppModel()
	next, _ := m.Update(launchSettingsMsg{})
	app := next.(appModel)
	if _, ok := app.active.(settingsModel); !ok {
		t.Errorf("active = %T, want settingsModel after launchSettingsMsg", app.active)
	}
}
