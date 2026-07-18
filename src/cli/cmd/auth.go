package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"
	"golang.org/x/term"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage authentication",
}

// Credential env vars for non-interactive login (CI, cron, containers), where there is
// no terminal to prompt at.
const (
	envEmail    = "CODEARMORY_EMAIL"
	envPassword = "CODEARMORY_PASSWORD"
)

// readPassword is a function variable so tests can inject a fake implementation
// without spawning a real terminal.
var readPassword = func() (string, error) {
	fmt.Fprint(os.Stderr, "Password: ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(raw), err
}

// readPasswordStdin consumes the password from stdin (--password-stdin), the idiom for
// piping a secret from a file or secret store without it ever reaching a flag — where it
// would be visible in `ps` and the shell history. Trailing newlines from `echo`/heredocs
// are stripped; the password itself may contain spaces.
var readPasswordStdin = func() (string, error) {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return trimPassword(string(raw)), nil
}

// trimPassword strips only the line ending a pipe adds (`echo secret |`, a heredoc), not
// interior or leading whitespace — a password may legitimately contain spaces.
func trimPassword(s string) string { return strings.TrimRight(s, "\r\n") }

// stdinIsTerminal reports whether we can prompt. Split out so tests can force the
// non-interactive path.
var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// resolvePassword picks the password source, most explicit first: --password-stdin, then
// CODEARMORY_PASSWORD, then an interactive prompt. With no terminal and no credential
// supplied it fails with the fix rather than "inappropriate ioctl for device" — the error
// term.ReadPassword returns when stdin is not a TTY.
func resolvePassword(fromStdin bool) (string, error) {
	if fromStdin {
		return readPasswordStdin()
	}
	if pw := os.Getenv(envPassword); pw != "" {
		return pw, nil
	}
	if !stdinIsTerminal() {
		return "", fmt.Errorf("no password and no terminal to prompt at — set %s, or pipe it with --password-stdin", envPassword)
	}
	return readPassword()
}

// jwtExpiry decodes the JWT payload and returns the exp claim as a time.Time.
// Returns the zero time and false if the token is not a structurally valid JWT
// or carries no exp claim.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// friendlyDuration formats a duration as "2h15m", "45m", or "less than a minute".
func friendlyDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h == 0 {
		if m == 0 {
			return "less than a minute"
		}
		return fmt.Sprintf("%dm", m)
	}
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate and save token",
	Long: `Log in to CodeArmory and save the session token.

Credentials are resolved most-explicit-first, so the command works both interactively
and unattended (CI, cron, a container) with no terminal:

  email     --email  >  CODEARMORY_EMAIL     >  prompt
  password  --password-stdin  >  CODEARMORY_PASSWORD  >  prompt

There is deliberately no --password flag: a password in argv is visible to every other
process via ` + "`ps`" + ` and lands in the shell history. Pipe it or use the env var.

  # Fully interactive:
  armory auth login

  # Email on the command line (password still prompted):
  armory auth login --email you@example.com

  # Unattended, credentials from the environment:
  CODEARMORY_EMAIL=ci@example.com CODEARMORY_PASSWORD=... armory auth login

  # Unattended, password piped from a secret store (never hits the environment):
  read-secret ci-password | armory auth login --email ci@example.com --password-stdin`,
	RunE: func(cmd *cobra.Command, args []string) error {
		email, _ := cmd.Flags().GetString("email")
		passwordStdin, _ := cmd.Flags().GetBool("password-stdin")
		if email == "" {
			email = os.Getenv(envEmail)
		}

		if email == "" {
			if !stdinIsTerminal() {
				return fmt.Errorf("no email and no terminal to prompt at — pass --email or set %s", envEmail)
			}
			// If already logged in, offer to skip. Only reachable interactively: an
			// unattended run supplies an email and re-authenticates unconditionally,
			// which is what a CI job wants (its cached token may be expired).
			if tok := bearerToken(); tok != "" {
				answer, err := prompt(
					fmt.Sprintf("Already logged in (%s). Sign in again? [y/N]", tokenSource()), "n",
				)
				if err != nil {
					return fmt.Errorf("reading input: %w", err)
				}
				if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
					fmt.Fprintln(os.Stderr, "Skipped — already logged in.")
					return nil
				}
			}

			var err error
			email, err = prompt("Email", "")
			if err != nil {
				return fmt.Errorf("reading email: %w", err)
			}
			if email == "" {
				return fmt.Errorf("email is required")
			}
		}

		password, err := resolvePassword(passwordStdin)
		if err != nil {
			return fmt.Errorf("reading password: %w", err)
		}
		if password == "" {
			return fmt.Errorf("password is empty")
		}
		body, _ := json.Marshal(map[string]string{"email": email, "password": password})
		data, err := doRequest("POST", "/gatekeeper/login", body)
		if err != nil {
			return err
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
	},
}

var signupCmd = &cobra.Command{
	Use:   "signup",
	Short: "Register a new account",
	Long: `Register a new CodeArmory account.

If --email or --username are omitted they are prompted for interactively
(the password is always prompted, never taken from a flag).

  # Fully interactive:
  armory auth signup

  # Non-interactive:
  armory auth signup --email you@example.com --username you`,
	RunE: func(cmd *cobra.Command, args []string) error {
		email, _ := cmd.Flags().GetString("email")
		username, _ := cmd.Flags().GetString("username")

		if email == "" {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return fmt.Errorf("--email is required (pass --email, or run interactively to be prompted)")
			}
			var err error
			if email, err = prompt("Email", ""); err != nil {
				return fmt.Errorf("reading email: %w", err)
			}
		}
		if email == "" {
			return fmt.Errorf("email is required")
		}

		if username == "" {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return fmt.Errorf("--username is required (pass --username, or run interactively to be prompted)")
			}
			var err error
			if username, err = prompt("Username", ""); err != nil {
				return fmt.Errorf("reading username: %w", err)
			}
		}
		if username == "" {
			return fmt.Errorf("username is required")
		}

		password, err := readPassword()
		if err != nil {
			return fmt.Errorf("reading password: %w", err)
		}
		body, _ := json.Marshal(map[string]string{
			"email":    email,
			"username": username,
			"password": password,
		})
		return apiCall("POST", "/gatekeeper/signup", body)
	},
}

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Clear saved token from keychain and config",
	RunE: func(cmd *cobra.Command, args []string) error {
		clearToken()
		fmt.Println("Logged out.")
		return nil
	},
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show current auth configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("URL:   %s\n", conductorURL())

		switch {
		case flagToken != "":
			fmt.Println("Token: (set) (--token flag)")
		case os.Getenv("CODEARMORY_TOKEN") != "":
			fmt.Println("Token: (set) (CODEARMORY_TOKEN)")
		default:
			var tok, src string
			if t, err := keyring.Get(keychainService, keychainAccount); err == nil && t != "" {
				tok, src = t, "keychain"
			} else if t := loadConfig().Token; t != "" {
				tok, src = t, "config file"
			}
			if tok == "" {
				fmt.Println("Token: not set — run `armory auth login`")
			} else if exp, ok := jwtExpiry(tok); ok {
				if time.Now().After(exp) {
					fmt.Printf("Token: expired %s ago (%s) — run `armory auth login`\n",
						friendlyDuration(time.Since(exp)), src)
				} else {
					fmt.Printf("Token: valid, expires in %s (%s)\n",
						friendlyDuration(time.Until(exp)), src)
				}
			} else {
				fmt.Printf("Token: (set) (%s)\n", src)
			}
		}
		return nil
	},
}

func init() {
	loginCmd.Flags().String("email", "", "email address (falls back to $CODEARMORY_EMAIL, then a prompt)")
	loginCmd.Flags().Bool("password-stdin", false, "read the password from stdin (for CI; safer than $CODEARMORY_PASSWORD, which is visible to the process tree)")

	signupCmd.Flags().String("email", "", "email address (optional — prompted if omitted)")
	signupCmd.Flags().String("username", "", "username (alphanumeric, hyphens, underscores; 1–64 chars; prompted if omitted)")

	authCmd.AddCommand(loginCmd, signupCmd, logoutCmd, authStatusCmd)
	RegisterModule(Module{Name: "auth", Command: authCmd})
}
