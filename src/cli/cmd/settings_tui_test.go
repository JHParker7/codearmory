package cmd

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func driveSettings(t *testing.T, m settingsModel, msg tea.Msg) (settingsModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	sm, ok := next.(settingsModel)
	if !ok {
		t.Fatalf("Update returned %T, want settingsModel", next)
	}
	return sm, cmd
}

func emitsGoHome(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(goHomeMsg)
	return ok
}

func TestSettingsModel_Defaults(t *testing.T) {
	restoreTheme(t)
	setActiveTheme("cyber")
	m := newSettingsModel()
	if m.origTheme != "cyber" {
		t.Errorf("origTheme = %q, want cyber", m.origTheme)
	}
	if got := m.form.value("theme"); got != "cyber" {
		t.Errorf("theme field = %q, want cyber", got)
	}
	if got := m.form.value("url"); got == "" {
		t.Error("url field should be pre-filled with the configured URL")
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

func TestSettingsModel_SavePersists(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)
	setActiveTheme("cyber")
	m := newSettingsModel()

	m, _ = driveSettings(t, m, tea.KeyMsg{Type: tea.KeyRight}) // cyber -> tokyo-night
	want := activeThemeName

	_, cmd := driveSettings(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !emitsGoHome(cmd) {
		t.Error("enter should save and return to home")
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

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("submitting an empty URL should not navigate away")
	}
	if sm := next.(settingsModel); sm.form.errMsg == "" {
		t.Error("expected an inline error when the URL is empty")
	}
	if loadConfig().Theme != "" {
		t.Error("nothing should be persisted when validation fails")
	}
}

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
