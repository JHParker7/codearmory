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

// setup_tui.go is the TUI counterpart to the line-based wizard in setup.go.
// It is reachable three ways: the "Setup" entry on the home menu, the
// `armory setup tui` CLI command, and an automatic prompt on first use.

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

// launchSetupMsg asks the appModel (or standaloneWrap) to swap the active
// screen for the setup wizard. Emitted by the first-use prompt and by the
// settings screen's F2 shortcut.
type launchSetupMsg struct{}

func launchSetup() tea.Msg { return launchSetupMsg{} }

// ── First-use prompt ────────────────────────────────────────────────────────

// firstUseModel is the welcome screen shown when no config has been written.
// 'y' jumps straight into the setup wizard; 'n' dismisses to the home menu.
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
			return m, launchSetup
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
		"Would you like to run the setup wizard now?",
		"It covers the conductor URL, theme, and account sign-in.",
	}, "\n")
	hint := tuiFormHint.Render("[y/enter] yes   [n/esc/q] skip to menu   [ctrl+c] quit")
	box := tuiFormBox.Render(heading + "\n\n" + body + "\n\n" + hint)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	}
	return "\n" + box
}

// ── Wizard ──────────────────────────────────────────────────────────────────

type setupTUIStep int

const (
	setupStepBasics      setupTUIStep = iota // URL + theme
	setupStepAccount                         // skip / log in / sign up
	setupStepCredentials                     // email + (username) + password
	setupStepResult                          // outcome summary
)

const (
	setupActionSkip   = "skip"
	setupActionLogin  = "log in"
	setupActionSignup = "sign up"
)

// setupTUIModel walks the user through configuring the CLI: conductor URL,
// theme (with live preview), and an optional log-in or sign-up. It mirrors
// `armory setup` but as a TUI form.
//
// origTheme records the theme on first entry so a cancel from the basics step
// reverts the live preview. It is bumped to the persisted choice after each
// successful basics submission, so a back-track + cancel does not undo a
// theme the user already committed.
type setupTUIModel struct {
	step    setupTUIStep
	form    tuiForm
	formCmd tea.Cmd
	width   int
	height  int

	origTheme string
	action    string // setupActionSkip / Login / Signup

	resultLines []string
	resultIsErr bool
}

func newSetupTUIModel() setupTUIModel {
	m := setupTUIModel{step: setupStepBasics, origTheme: activeThemeName}
	m.form, m.formCmd = newSetupBasicsForm()
	return m
}

func newSetupBasicsForm() (tuiForm, tea.Cmd) {
	cfg := loadConfig()
	return newTUIForm("Setup — basics",
		formInputDefault("url", "Conductor", "http://localhost:8082", cfg.URL),
		formSelectDefault("theme", "Theme", themeOrder, activeThemeName),
	)
}

func newSetupAccountForm() (tuiForm, tea.Cmd) {
	return newTUIForm("Setup — account",
		formSelectDefault("action", "Action",
			[]string{setupActionSkip, setupActionLogin, setupActionSignup},
			setupActionLogin),
	)
}

func newSetupCredentialsForm(signup bool) (tuiForm, tea.Cmd) {
	fields := []formField{formInput("email", "Email", "you@example.com")}
	if signup {
		fields = append(fields, formInput("username", "Username", "1–64 chars"))
	}
	fields = append(fields, formPassword("password", "Password", "password"))
	title := "Setup — log in"
	if signup {
		title = "Setup — sign up"
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

func (m setupTUIModel) Init() tea.Cmd { return m.formCmd }

func (m setupTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
	if m.step == setupStepResult {
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

	// Live theme preview on the basics step: cycling the selector re-themes
	// the whole UI on the spot. Reverted on cancel.
	if m.step == setupStepBasics {
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

func (m setupTUIModel) cancel() (tea.Model, tea.Cmd) {
	switch m.step {
	case setupStepBasics:
		setActiveTheme(m.origTheme)
		return m, goHome
	case setupStepAccount:
		m.step = setupStepBasics
		m.form, m.formCmd = newSetupBasicsForm()
		return m, m.formCmd
	case setupStepCredentials:
		m.step = setupStepAccount
		m.form, m.formCmd = newSetupAccountForm()
		return m, m.formCmd
	}
	return m, nil
}

func (m setupTUIModel) submit() (tea.Model, tea.Cmd) {
	switch m.step {
	case setupStepBasics:
		return m.submitBasics()
	case setupStepAccount:
		return m.submitAccount()
	case setupStepCredentials:
		return m.submitCredentials()
	}
	return m, nil
}

func (m setupTUIModel) submitBasics() (tea.Model, tea.Cmd) {
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
	// back-track from the account step cannot undo a choice we already saved.
	m.origTheme = theme

	m.step = setupStepAccount
	m.form, m.formCmd = newSetupAccountForm()
	return m, m.formCmd
}

func (m setupTUIModel) submitAccount() (tea.Model, tea.Cmd) {
	m.action = m.form.value("action")
	if m.action == setupActionSkip {
		cfg := loadConfig()
		m.resultLines = []string{
			"Setup complete.",
			"",
			"Conductor:  " + cfg.URL,
			"Theme:      " + activeThemeName,
			"",
			"Sign in any time with `armory auth login`.",
		}
		m.step = setupStepResult
		return m, nil
	}

	signup := m.action == setupActionSignup
	m.step = setupStepCredentials
	m.form, m.formCmd = newSetupCredentialsForm(signup)
	return m, m.formCmd
}

func (m setupTUIModel) submitCredentials() (tea.Model, tea.Cmd) {
	email := strings.TrimSpace(m.form.value("email"))
	if email == "" {
		m.form.errMsg = "Email is required"
		return m, nil
	}
	signup := m.action == setupActionSignup
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
	lines := []string{"Setup complete."}
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
	m.step = setupStepResult
	return m, nil
}

func (m setupTUIModel) View() string {
	if m.step == setupStepResult {
		return m.resultView()
	}
	return m.form.view(m.width, m.height)
}

func (m setupTUIModel) resultView() string {
	heading := tuiFormHeading.Render("Setup")
	body := strings.Join(m.resultLines, "\n")
	hint := tuiFormHint.Render("press any key to return to the menu")
	box := tuiFormBox.Render(heading + "\n\n" + body + "\n\n" + hint)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	}
	return "\n" + box
}

// ── Entry point + module registration ───────────────────────────────────────

func runSetupTUI() error {
	p := tea.NewProgram(standaloneWrap{newSetupTUIModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func init() {
	setupCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Launch the interactive setup wizard",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runSetupTUI() },
	})
	RegisterModule(Module{
		Name:    "setup",
		Order:   110, // sits just after Settings (Order=100) on the home menu
		Command: setupCmd,
		Screens: []HubScreen{{
			Title: "Setup",
			Desc:  "Conductor URL, theme, and account sign-in",
			New:   func() tea.Model { return newSetupTUIModel() },
		}},
	})
}
