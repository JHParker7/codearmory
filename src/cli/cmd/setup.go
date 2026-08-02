package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"
)

var stdinReader = bufio.NewReader(os.Stdin)

// prompt writes label to stderr and reads one line from stdin.
// Returns defaultVal if the user presses enter without typing.
func prompt(label, defaultVal string) (string, error) {
	if defaultVal != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, defaultVal)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	line, err := stdinReader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultVal, nil
	}
	return line, nil
}

var (
	flagSetupURL      string
	flagSetupEmail    string
	flagSetupUsername string
	flagSetupSignup   bool
)

// setupCmd is the line-based, scriptable setup wizard. The interactive TUI lives
// in the settings screen (settings_tui.go), reachable via `armory settings`.
var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure conductor URL and authenticate",
	Long: `Interactive wizard to configure the CodeArmory CLI.

Saves the conductor URL to ~/.config/codearmory/config.json and stores
the session token in the OS keychain (falls back to config file when the
keychain is unavailable).

  # Fully interactive:
  armory setup

  # Set URL only (skips login prompt):
  armory setup --url http://conductor:8080

  # Set URL and log in non-interactively:
  armory setup --url http://conductor:8080 --email admin@example.com

For a TUI form, use ` + "`armory settings`" + `.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// ── Step 1: conductor URL ─────────────────────────────────────

		urlVal := flagSetupURL
		if urlVal == "" {
			var err error
			urlVal, err = prompt("Conductor URL", loadConfig().URL)
			if err != nil {
				return fmt.Errorf("reading URL: %w", err)
			}
		}
		// Normalise before saving so the stored value is a real URL, not just before use.
		urlVal = normalizeConductorURL(urlVal)
		if urlVal == "" {
			return fmt.Errorf("conductor URL is required")
		}
		// The URL the user just chose is authoritative for the rest of setup: the
		// connectivity check and login below must hit it. conductorURL() resolves in
		// the order flagURL → CODEARMORY_URL env → config, so without this a stale
		// CODEARMORY_URL env var (or the setup --url landing in the command-local
		// flagSetupURL, which shadows the root --url) silently wins and setup appears
		// to ignore the URL it just saved. Propagate it into flagURL so it takes
		// precedence for this process.
		flagURL = urlVal

		cfg := loadConfig()
		cfg.URL = urlVal
		if err := saveConfig(cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		fmt.Fprintf(os.Stderr, "URL saved: %s\n", urlVal)

		// ── Step 2: connectivity check ────────────────────────────────

		fmt.Fprint(os.Stderr, "Testing connection... ")
		if _, err := doRequest("GET", "/healthz", nil); err != nil {
			fmt.Fprintln(os.Stderr, "✗")
			fmt.Fprintf(os.Stderr, "warning: %s/healthz unreachable: %v\n", urlVal, err)
			fmt.Fprintln(os.Stderr, "         Verify the URL and ensure conductor is running.")
		} else {
			fmt.Fprintln(os.Stderr, "✓")
		}

		// ── Step 3: sign up or log in ─────────────────────────────────

		if err := setupAuth(flagSetupEmail, flagSetupUsername, flagSetupSignup); err != nil {
			return err
		}

		// ── Step 4: git_factory integration ───────────────────────────
		// If the platform runs git_factory, offer to wire the local git client up to it.
		// Best-effort: a failure here must not fail the whole wizard.
		if err := setupGitFactory(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: git_factory setup skipped: %v\n", err)
		}

		// ── Step 5: shell completions ─────────────────────────────────

		if shell := detectedShell(); shell != "" && isTerminal() {
			answer, err := prompt(fmt.Sprintf("Install shell completions for %s? [Y/n]", shell), "y")
			if err == nil && strings.ToLower(answer) != "n" && strings.ToLower(answer) != "no" {
				if msg, err := installCompletions(cmd.Root(), shell); err != nil {
					fmt.Fprintf(os.Stderr, "warning: completions not installed: %v\n", err)
				} else {
					fmt.Fprintln(os.Stderr, msg)
				}
			}
		}

		fmt.Fprintln(os.Stderr, "\nSetup complete. Run `armory auth status` to verify.")
		return nil
	},
}

func init() {
	setupCmd.Flags().StringVar(&flagSetupURL, "url", "", "conductor base URL (skips URL prompt)")
	setupCmd.Flags().StringVar(&flagSetupEmail, "email", "", "email address (skips login prompt, still asks for password)")
	setupCmd.Flags().StringVar(&flagSetupUsername, "username", "", "username for sign up (implies --signup)")
	setupCmd.Flags().BoolVar(&flagSetupSignup, "signup", false, "create a new account instead of logging in to an existing one")

	// Wire the scriptable `armory setup` command. It has no hub-menu screen: the
	// interactive flow lives in the settings screen (settings_tui.go).
	RegisterModule(Module{Name: "setup", Order: 110, Command: setupCmd})
}

func isTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && (info.Mode()&os.ModeCharDevice) != 0
}

func detectedShell() string {
	shell := filepath.Base(os.Getenv("SHELL"))
	switch shell {
	case "bash", "zsh", "fish":
		return shell
	}
	return ""
}

// tokenSource returns a short human-readable label for where the current token
// came from, for display in the "already logged in" prompt.
func tokenSource() string {
	if flagToken != "" {
		return "--token flag"
	}
	if os.Getenv("CODEARMORY_TOKEN") != "" {
		return "CODEARMORY_TOKEN"
	}
	if t, err := keyring.Get(keychainService, keychainAccount); err == nil && t != "" {
		return "keychain"
	}
	return "config file"
}

// installCompletions configures shell completions for the given shell.
//
// For zsh with oh-my-zsh: writes _armory to the custom completions directory,
// which oh-my-zsh adds to $fpath before compinit runs. This is the correct
// approach for zsh-autocomplete compatibility.
//
// For zsh without oh-my-zsh, bash: appends "source <(armory completion <shell>)"
// to the rc file.
//
// For fish: writes directly to the auto-load completions directory.
func installCompletions(root *cobra.Command, shell string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	switch shell {
	case "bash":
		rcFile := filepath.Join(home, ".bashrc")
		if err := appendCompletionLine(rcFile, shell); err != nil {
			return "", err
		}
		return fmt.Sprintf("Added completion line to %s.\n  Reload your shell or run: source <(armory completion bash)", rcFile), nil

	case "zsh":
		rcFile := filepath.Join(home, ".zshrc")
		ohmyzshCustom := filepath.Join(home, ".oh-my-zsh", "custom", "completions")
		if _, err := os.Stat(filepath.Join(home, ".oh-my-zsh")); err == nil {
			// oh-my-zsh detected: write to custom completions dir so it lands in
			// $fpath before compinit/zsh-autocomplete initialises.
			dest := filepath.Join(ohmyzshCustom, "_armory")
			if err := os.MkdirAll(ohmyzshCustom, 0755); err != nil {
				return "", fmt.Errorf("creating directory: %w", err)
			}
			var buf bytes.Buffer
			if err := root.GenZshCompletion(&buf); err != nil {
				return "", fmt.Errorf("generating completions: %w", err)
			}
			if err := os.WriteFile(dest, buf.Bytes(), 0644); err != nil {
				return "", fmt.Errorf("writing %s: %w", dest, err)
			}
			removeCompletionBlock(rcFile)
			return fmt.Sprintf("Completions installed to %s.\n  Reload your shell to activate.", dest), nil
		}
		if err := appendCompletionLine(rcFile, shell); err != nil {
			return "", err
		}
		return fmt.Sprintf("Added completion line to %s.\n  Reload your shell or run: source <(armory completion zsh)", rcFile), nil

	case "fish":
		dest := filepath.Join(home, ".config", "fish", "completions", "armory.fish")
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return "", fmt.Errorf("creating directory: %w", err)
		}
		var buf bytes.Buffer
		if err := root.GenFishCompletion(&buf, true); err != nil {
			return "", fmt.Errorf("generating completions: %w", err)
		}
		if err := os.WriteFile(dest, buf.Bytes(), 0644); err != nil {
			return "", fmt.Errorf("writing %s: %w", dest, err)
		}
		return fmt.Sprintf("Completions installed to %s.\n  Reload your shell to activate.", dest), nil

	default:
		return "", fmt.Errorf("unsupported shell %q", shell)
	}
}

// appendRCBlock appends "marker\nline\n" to rcFile unless marker is already
// present. It creates the file (and parent dirs) if needed.
func appendRCBlock(rcFile, marker, line string) error {
	existing, _ := os.ReadFile(rcFile)
	if strings.Contains(string(existing), marker) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(rcFile), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(rcFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("updating %s: %w", rcFile, err)
	}
	_, werr := fmt.Fprintf(f, "\n%s\n%s\n", marker, line)
	f.Close()
	return werr
}

// setupAuth handles the interactive sign-up or log-in step of `armory setup`.
// email and username may be pre-set via flags; signup=true forces the sign-up
// path. When neither is set the function asks the user interactively.
// Returns nil without prompting if the user already has a token and declines
// to re-authenticate.
func setupAuth(email, username string, signup bool) error {
	signup = signup || username != ""
	doLogin := true

	if !signup && email == "" {
		if tok := bearerToken(); tok != "" {
			answer, err := prompt(fmt.Sprintf("Already logged in (%s). Sign in again? [y/N]", tokenSource()), "n")
			if err != nil {
				return fmt.Errorf("reading input: %w", err)
			}
			if strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes" {
				doLogin = false
			}
		}
	}

	if doLogin && !signup && email == "" {
		answer, err := prompt("Account: (l)og in or (s)ign up? [l/s]", "l")
		if err != nil {
			return fmt.Errorf("reading input: %w", err)
		}
		switch strings.ToLower(answer) {
		case "s", "signup", "sign up":
			signup = true
		case "n", "skip", "":
			if answer != "l" && answer != "login" && answer != "log in" {
				fmt.Fprintln(os.Stderr, "Skipped — run `armory auth login` when ready.")
				doLogin = false
			}
		}
	}

	if !doLogin {
		return nil
	}

	if email == "" {
		var err error
		email, err = prompt("Email", "")
		if err != nil {
			return fmt.Errorf("reading email: %w", err)
		}
		if email == "" {
			return fmt.Errorf("email is required")
		}
	}

	if signup && username == "" {
		var err error
		username, err = prompt("Username", "")
		if err != nil {
			return fmt.Errorf("reading username: %w", err)
		}
		if username == "" {
			return fmt.Errorf("username is required for sign up")
		}
	}

	password, err := readPassword()
	if err != nil {
		return fmt.Errorf("reading password: %w", err)
	}

	if signup {
		body, _ := json.Marshal(map[string]string{
			"email":    email,
			"username": username,
			"password": password,
		})
		if _, err := doRequest("POST", "/gatekeeper/signup", body); err != nil {
			return fmt.Errorf("signup: %w", err)
		}
		fmt.Fprintln(os.Stderr, "Account created.")
	}

	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	data, err := doRequest("POST", "/gatekeeper/login", body)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &resp); err != nil || resp.Token == "" {
		return fmt.Errorf("unexpected login response: %s", data)
	}
	where, err := storeToken(resp.Token)
	if err != nil {
		return fmt.Errorf("saving token: %w", err)
	}
	if where != "" {
		fmt.Fprintf(os.Stderr, "Logged in — token saved to %s\n", where)
	} else {
		fmt.Fprintln(os.Stderr, "Logged in.")
	}
	return nil
}

const completionMarker = "# armory completions"

// removeCompletionBlock deletes the "# armory completions" marker and the
// source line that follows it from rcFile. Used when switching to the fpath
// approach so the old inline source line is not left behind.
func removeCompletionBlock(rcFile string) {
	data, err := os.ReadFile(rcFile)
	if err != nil || !strings.Contains(string(data), completionMarker) {
		return
	}
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines))
	skip := false
	for _, l := range lines {
		if l == completionMarker {
			skip = true
			continue
		}
		if skip {
			skip = false
			continue // drop the source line that follows the marker
		}
		out = append(out, l)
	}
	_ = os.WriteFile(rcFile, []byte(strings.Join(out, "\n")), 0644)
}

// appendCompletionLine ensures "source <(armory completion <shell>)" appears in
// rcFile. If a previous file-based setup is detected (marker present but old
// source line), the old line is replaced in-place. Idempotent.
func appendCompletionLine(rcFile, shell string) error {
	newLine := fmt.Sprintf("source <(armory completion %s)", shell)

	existing, _ := os.ReadFile(rcFile)
	content := string(existing)

	if strings.Contains(content, newLine) {
		return nil // already configured with new-style line
	}

	if strings.Contains(content, completionMarker) {
		// Old file-based setup: replace the source line after the marker.
		lines := strings.Split(content, "\n")
		for i, l := range lines {
			if l == completionMarker && i+1 < len(lines) {
				lines[i+1] = newLine
				return os.WriteFile(rcFile, []byte(strings.Join(lines, "\n")), 0644)
			}
		}
	}

	return appendRCBlock(rcFile, completionMarker, newLine)
}
