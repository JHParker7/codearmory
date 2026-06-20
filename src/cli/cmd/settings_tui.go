package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// settings_tui.go is the single client-side configuration screen. It walks the
// user through the conductor URL, color theme (with live preview), and an
// optional account log-in or sign-up — folding together what used to be a
// separate quick "settings" form and a multi-step "setup" wizard. It is
// reachable from the home menu, as `armory settings`, and as the automatic
// first-use prompt. The line-based `armory setup` command (setup.go) remains
// the scriptable, non-interactive counterpart.

// ── First-use detection ──────────────────────────────────────────────────────

// isFirstUse reports whether the CLI has never been configured on this machine.
// Returns false when env-vars supply a URL or token, since those users have an
// out-of-band configuration and don't need the prompt.
func isFirstUse() bool {
	if _, err := os.Stat(configPath()); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	if os.Getenv("CODEARMORY_URL") != "" || os.Getenv("CODEARMORY_TOKEN") != "" {
		return false
	}
	if flagURL != "" || flagToken != "" {
		return false
	}
	return true
}

// ── Messages ────────────────────────────────────────────────────────────────

// launchSettingsMsg asks the appModel to swap the active screen for the
// settings screen. Emitted by the first-use prompt when the user opts in.
type launchSettingsMsg struct{}

func launchSettings() tea.Msg { return launchSettingsMsg{} }

// ── First-use prompt ────────────────────────────────────────────────────────

// firstUseModel is the welcome screen shown when no config has been written.
// 'y' jumps straight into the settings screen; 'n' dismisses to the home menu.
type firstUseModel struct {
	width  int
	height int
}

func newFirstUseModel() firstUseModel { return firstUseModel{} }

func (m firstUseModel) Init() tea.Cmd { return nil }

func (m firstUseModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			return m, launchSettings
		case "n", "N", "esc", "q":
			return m, goHome
		case "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m firstUseModel) View() string {
	heading := tuiFormHeading.Render("Welcome to codearmory")
	body := strings.Join([]string{
		"It looks like this is your first time here.",
		"",
		"Would you like to configure codearmory now?",
		"It covers the conductor URL, theme, and account sign-in.",
	}, "\n")
	hint := tuiFormHint.Render("[y/enter] yes   [n/esc/q] skip to menu   [ctrl+c] quit")
	box := tuiFormBox.Render(heading + "\n\n" + body + "\n\n" + hint)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	}
	return "\n" + box
}

// ── Settings screen ──────────────────────────────────────────────────────────

type settingsStep int

const (
	settingsStepBasics      settingsStep = iota // theme + URL + account action
	settingsStepCredentials                     // email + (username) + password
	settingsStepResult                          // outcome summary
)

const (
	settingsAccountKeep   = "keep"
	settingsAccountLogin  = "log in"
	settingsAccountSignup = "sign up"
)

// settingsModel configures the CLI: conductor URL, theme (with live preview),
// and an optional account action. The basics step persists the URL/theme and,
// when the account selector is "keep", returns straight home — preserving the
// quick "just change my theme" path. Choosing "log in" or "sign up" advances to
// a credentials step, then a result summary.
//
// origTheme records the theme on entry so cancelling the basics step reverts the
// live preview. It is bumped to the persisted choice after a successful basics
// submission, so a back-track + cancel from credentials cannot undo a theme the
// user already committed.
type settingsModel struct {
	step    settingsStep
	form    tuiForm
	formCmd tea.Cmd
	width   int
	height  int

	origTheme string
	action    string // settingsAccountKeep / Login / Signup

	resultLines []string
}

func newSettingsModel() settingsModel {
	m := settingsModel{step: settingsStepBasics, origTheme: activeThemeName}
	m.form, m.formCmd = newSettingsBasicsForm()
	return m
}

func newSettingsBasicsForm() (tuiForm, tea.Cmd) {
	return newTUIForm("Settings",
		formSelectDefault("theme", "Theme", themeOrder, activeThemeName),
		formInputDefault("url", "Conductor", "http://localhost:8082", loadConfig().URL),
		formSelectDefault("account", "Account",
			[]string{settingsAccountKeep, settingsAccountLogin, settingsAccountSignup},
			settingsAccountKeep),
	)
}

func newSettingsCredentialsForm(signup bool) (tuiForm, tea.Cmd) {
	fields := []formField{formInput("email", "Email", "you@example.com")}
	if signup {
		fields = append(fields, formInput("username", "Username", "1–64 chars"))
	}
	fields = append(fields, formPassword("password", "Password", "password"))
	title := "Settings — log in"
	if signup {
		title = "Settings — sign up"
	}
	return newTUIForm(title, fields...)
}

// formPassword builds a labelled text field whose echo is masked.
func formPassword(key, label, placeholder string) formField {
	f := formInput(key, label, placeholder)
	f.input.EchoMode = textinput.EchoPassword
	f.input.EchoCharacter = '•'
	return f
}

func (m settingsModel) Init() tea.Cmd { return m.formCmd }

func (m settingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		// ctrl+c quits before the key can land in a text field.
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}

	// The result step is read-only — any key returns home.
	if m.step == settingsStepResult {
		if _, ok := msg.(tea.KeyMsg); ok {
			return m, goHome
		}
		return m, nil
	}

	var (
		action formAction
		cmd    tea.Cmd
	)
	m.form, action, cmd = m.form.update(msg)

	// Live theme preview on the basics step: cycling the selector re-themes the
	// whole UI on the spot. Reverted on cancel.
	if m.step == settingsStepBasics {
		if sel := m.form.value("theme"); sel != activeThemeName {
			setActiveTheme(sel)
		}
	}

	switch action {
	case formCancel:
		return m.cancel()
	case formSubmit:
		return m.submit()
	}
	return m, cmd
}

func (m settingsModel) cancel() (tea.Model, tea.Cmd) {
	switch m.step {
	case settingsStepBasics:
		setActiveTheme(m.origTheme) // discard the live preview
		return m, goHome
	case settingsStepCredentials:
		m.step = settingsStepBasics
		m.form, m.formCmd = newSettingsBasicsForm()
		return m, m.formCmd
	}
	return m, nil
}

func (m settingsModel) submit() (tea.Model, tea.Cmd) {
	switch m.step {
	case settingsStepBasics:
		return m.submitBasics()
	case settingsStepCredentials:
		return m.submitCredentials()
	}
	return m, nil
}

func (m settingsModel) submitBasics() (tea.Model, tea.Cmd) {
	url := strings.TrimRight(strings.TrimSpace(m.form.value("url")), "/")
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
	// Anchor the cancel-revert target at the committed theme so a later
	// back-track from the credentials step cannot undo a choice we already saved.
	m.origTheme = theme

	m.action = m.form.value("account")
	if m.action == settingsAccountKeep {
		return m, goHome
	}

	signup := m.action == settingsAccountSignup
	m.step = settingsStepCredentials
	m.form, m.formCmd = newSettingsCredentialsForm(signup)
	return m, m.formCmd
}

func (m settingsModel) submitCredentials() (tea.Model, tea.Cmd) {
	email := strings.TrimSpace(m.form.value("email"))
	if email == "" {
		m.form.errMsg = "Email is required"
		return m, nil
	}
	signup := m.action == settingsAccountSignup
	username := strings.TrimSpace(m.form.value("username"))
	if signup && username == "" {
		m.form.errMsg = "Username is required for sign up"
		return m, nil
	}
	password := m.form.value("password")
	if password == "" {
		m.form.errMsg = "Password is required"
		return m, nil
	}

	if signup {
		body, _ := json.Marshal(map[string]string{
			"email":    email,
			"username": username,
			"password": password,
		})
		if _, err := doRequest("POST", "/gatekeeper/signup", body); err != nil {
			m.form.errMsg = "Signup failed: " + err.Error()
			return m, nil
		}
	}

	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	data, err := doRequest("POST", "/gatekeeper/login", body)
	if err != nil {
		m.form.errMsg = "Login failed: " + err.Error()
		return m, nil
	}
	var resp struct {
		Token string `json:"token"`
	}
	if jerr := json.Unmarshal(data, &resp); jerr != nil || resp.Token == "" {
		m.form.errMsg = "Unexpected login response"
		return m, nil
	}
	where, err := storeToken(resp.Token)
	if err != nil {
		m.form.errMsg = "Saving token: " + err.Error()
		return m, nil
	}

	cfg := loadConfig()
	lines := []string{"Settings saved."}
	if signup {
		lines = append(lines, "Account created.")
	}
	lines = append(lines,
		fmt.Sprintf("Logged in as %s — token saved to %s.", email, where),
		"",
		"Conductor:  "+cfg.URL,
		"Theme:      "+activeThemeName,
	)
	m.resultLines = lines
	m.step = settingsStepResult
	return m, nil
}

func (m settingsModel) View() string {
	if m.step == settingsStepResult {
		return m.resultView()
	}
	return m.form.view(m.width, m.height)
}

func (m settingsModel) resultView() string {
	heading := tuiFormHeading.Render("Settings")
	body := strings.Join(m.resultLines, "\n")
	hint := tuiFormHint.Render("press any key to return to the menu")
	box := tuiFormBox.Render(heading + "\n\n" + body + "\n\n" + hint)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	}
	return "\n" + box
}

// goHome returns to the home screen (or quits, when running standalone).
func goHome() tea.Msg { return goHomeMsg{} }

// ── Entry point + module registration ───────────────────────────────────────

func runSettingsTUI() error {
	p := tea.NewProgram(standaloneWrap{newSettingsModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func init() {
	settingsCmd := &cobra.Command{
		Use:   "settings",
		Short: "Configure client-side settings (theme, conductor URL, account)",
		Long: `Interactive settings for terminal-side configuration:

  Theme         color palette for the TUIs (previews live as you cycle)
  Conductor URL the API endpoint armory talks to
  Account       optionally log in or sign up

Theme and URL are saved to ~/.config/codearmory/config.json. Use ←/→ to cycle
the selectors, enter to advance/save, esc to cancel. Set "Account" to "keep" to
leave your session untouched.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runSettingsTUI() },
	}
	RegisterModule(Module{
		Name:    "settings",
		Order:   100,
		Command: settingsCmd,
		Screens: []HubScreen{{
			Title: "Settings",
			Desc:  "Theme, conductor URL, and account sign-in",
			New:   func() tea.Model { return newSettingsModel() },
		}},
	})
}
