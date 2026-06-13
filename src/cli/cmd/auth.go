package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// readPassword is a function variable so tests can inject a fake implementation
// without spawning a real terminal.
var readPassword = func() (string, error) {
	fmt.Fprint(os.Stderr, "Password: ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(raw), err
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

If --email is omitted the command prompts for it interactively.

  # Fully interactive:
  armory auth login

  # Email on the command line (password still prompted):
  armory auth login --email you@example.com`,
	RunE: func(cmd *cobra.Command, args []string) error {
		email, _ := cmd.Flags().GetString("email")

		if email == "" {
			// If already logged in, offer to skip.
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

		password, err := readPassword()
		if err != nil {
			return fmt.Errorf("reading password: %w", err)
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
		fmt.Fprintf(os.Stderr, "Logged in — token saved to %s\n", where)
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
	loginCmd.Flags().String("email", "", "email address (optional — prompted if omitted)")

	signupCmd.Flags().String("email", "", "email address (optional — prompted if omitted)")
	signupCmd.Flags().String("username", "", "username (alphanumeric, hyphens, underscores; 1–64 chars; prompted if omitted)")

	authCmd.AddCommand(loginCmd, signupCmd, logoutCmd, authStatusCmd)
	rootCmd.AddCommand(authCmd)
}
