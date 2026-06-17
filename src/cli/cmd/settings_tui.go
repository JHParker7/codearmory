package cmd

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// settingsModel is a small form for client-side (terminal) configuration: the
// color theme and the conductor URL, both persisted to ~/.config/codearmory/
// config.json. It is reachable from the home screen and as `armory settings`.
//
// The theme selector previews live — cycling it re-themes the whole UI on the
// spot. origTheme records the theme on entry so cancelling reverts the preview.
type settingsModel struct {
	form      tuiForm
	width     int
	height    int
	origTheme string
	initCmd   tea.Cmd
}

func newSettingsModel() settingsModel {
	form, cmd := newTUIForm("Settings",
		formSelectDefault("theme", "Theme", themeOrder, activeThemeName),
		formInputDefault("url", "Conductor", "http://localhost:8082", loadConfig().URL),
	)
	form.extraHint = "F2: setup wizard"
	return settingsModel{form: form, origTheme: activeThemeName, initCmd: cmd}
}

func (m settingsModel) Init() tea.Cmd { return m.initCmd }

func (m settingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		// ctrl+c quits before the key can land in the URL text field.
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		// F2 jumps to the full setup wizard. Intercepted before the form gets
		// the key so it works while the URL input has focus. The textinput
		// component does not consume function keys, so this is safe.
		if msg.String() == "f2" {
			setActiveTheme(m.origTheme) // discard the live preview before leaving
			return m, launchSetup
		}
	}

	var (
		action formAction
		cmd    tea.Cmd
	)
	m.form, action, cmd = m.form.update(msg)

	// Live preview: keep the active theme in sync with the selector so cycling
	// it re-themes the screen immediately.
	if sel := m.form.value("theme"); sel != activeThemeName {
		setActiveTheme(sel)
	}

	switch action {
	case formCancel:
		setActiveTheme(m.origTheme) // discard the live preview
		return m, goHome
	case formSubmit:
		return m.save()
	}
	return m, cmd
}

// save validates and persists the form to config. Validation failures stay in
// the form with an inline error rather than exiting.
func (m settingsModel) save() (tea.Model, tea.Cmd) {
	url := strings.TrimSpace(m.form.value("url"))
	if url == "" {
		m.form.errMsg = "Conductor URL cannot be empty"
		return m, nil
	}
	theme := normalizeTheme(m.form.value("theme"))

	cfg := loadConfig()
	cfg.URL = url
	cfg.Theme = theme
	if err := saveConfig(cfg); err != nil {
		m.form.errMsg = "save failed: " + err.Error()
		return m, nil
	}
	setActiveTheme(theme)
	return m, goHome
}

func (m settingsModel) View() string { return m.form.view(m.width, m.height) }

// goHome returns to the home screen (or quits, when running standalone).
func goHome() tea.Msg { return goHomeMsg{} }

func runSettingsTUI() error {
	p := tea.NewProgram(standaloneWrap{newSettingsModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func init() {
	settingsCmd := &cobra.Command{
		Use:   "settings",
		Short: "Configure client-side settings (theme, conductor URL)",
		Long: `Interactive settings for terminal-side configuration:

  Theme         color palette for the TUIs (previews live as you cycle)
  Conductor URL the API endpoint armory talks to

Changes are saved to ~/.config/codearmory/config.json. Use ←/→ to cycle the
theme, enter to save, esc to cancel.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runSettingsTUI() },
	}
	RegisterModule(Module{
		Name:    "settings",
		Order:   100,
		Command: settingsCmd,
		Screens: []HubScreen{{
			Title: "Settings",
			Desc:  "Theme and conductor URL",
			New:   func() tea.Model { return newSettingsModel() },
		}},
	})
}
