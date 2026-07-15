package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"
)

const (
	keychainService = "codearmory"
	keychainAccount = "token"
)

var (
	flagURL     string
	flagToken   string
	flagVerbose bool
)

type cliConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	Theme string `json:"theme,omitempty"`
	// Providers selects which module fills each capability slot, keyed by slot
	// name (e.g. {"repos": "github"}). Slots with no entry use the first
	// registered provider. See Module.Slot in module.go.
	Providers map[string]string `json:"providers,omitempty"`
	// CurrentProject is the sticky workspace label the user is "working in". When
	// set, list views auto-filter to it and creates are tagged with it, unless
	// overridden per-command by --project or --all. See project.go.
	CurrentProject string `json:"current_project,omitempty"`
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "codearmory", "config.json")
}

func loadConfig() cliConfig {
	cfg := cliConfig{URL: "http://localhost:8082"}
	if data, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(data, &cfg) //nolint:errcheck
	}
	return cfg
}

func saveConfig(cfg cliConfig) error {
	path := configPath()
	// 0700/0600: restrict directory and file to owner only so other users on
	// the same machine cannot read the stored token.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, data, 0600)
}

func conductorURL() string {
	if flagURL != "" {
		return flagURL
	}
	if u := os.Getenv("CODEARMORY_URL"); u != "" {
		return u
	}
	return loadConfig().URL
}

func bearerToken() string {
	if flagToken != "" {
		if exp, ok := jwtExpiry(flagToken); ok && time.Now().After(exp) {
			return ""
		}
		return flagToken
	}
	if t := os.Getenv("CODEARMORY_TOKEN"); t != "" {
		if exp, ok := jwtExpiry(t); ok && time.Now().After(exp) {
			return ""
		}
		return t
	}
	if t, err := keyring.Get(keychainService, keychainAccount); err == nil && t != "" {
		if exp, ok := jwtExpiry(t); ok && time.Now().After(exp) {
			return ""
		}
		return t
	}
	// No plaintext config-file fallback: on a keychain-less host the token lives in
	// the CODEARMORY_TOKEN env var (handled above), never on disk.
	return ""
}

// isSignedIn reports whether a usable (non-expired) auth token is available. The
// TUI hub gates its service menu on this: while signed out, registeredServices()
// can't tell which services the caller can reach, so the menu would fail open and
// list every service.
func isSignedIn() bool { return bearerToken() != "" }

// storeToken saves the token to the OS keychain. When the keychain is unavailable
// (e.g. a headless server) it does NOT write the token to disk — the token is a
// secret and the config file is plaintext. Instead it prints an `export
// CODEARMORY_TOKEN=…` line so the user can keep the token in their environment (the
// env var already takes precedence in bearerToken). Returns a human-readable
// description of where the token was stored, or "" when it was only printed for the
// user to export.
func storeToken(token string) (string, error) {
	// A new token means a (possibly different) caller; drop the cached
	// per-token routing-table probe so the TUI hub re-resolves it after sign-in.
	defer resetRegisteredServices()
	if err := keyring.Set(keychainService, keychainAccount, token); err == nil {
		return "the OS keychain", nil
	}
	fmt.Fprintf(os.Stderr,
		"OS keychain unavailable — not writing the token to disk. Export it to authenticate future commands:\n\n    export CODEARMORY_TOKEN=%s\n\n", token)
	return "", nil
}

// clearToken removes the token from the keychain and clears any token left in the
// config file by older CLI versions (current versions never write it there).
func clearToken() {
	keyring.Delete(keychainService, keychainAccount) //nolint:errcheck
	cfg := loadConfig()
	if cfg.Token != "" {
		cfg.Token = ""
		saveConfig(cfg) //nolint:errcheck
	}
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// doRequest executes an HTTP request and returns the raw response body.
// Returns an error for non-2xx responses that includes the status code and body.
func doRequest(method, path string, body []byte) ([]byte, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, conductorURL()+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "armory-cli")
	if t := bearerToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// apiCall executes a request and prints a human-readable response to stdout.
func apiCall(method, path string, body []byte) error {
	data, err := doRequest(method, path, body)
	if err != nil {
		return err
	}
	if method == "DELETE" && len(strings.TrimSpace(string(data))) == 0 {
		fmt.Println("Deleted.")
		return nil
	}
	printResponse(data)
	return nil
}

func printJSON(data []byte) {
	var buf bytes.Buffer
	if json.Indent(&buf, data, "", "  ") == nil && buf.Len() > 0 {
		fmt.Println(buf.String())
	} else if len(data) > 0 {
		fmt.Println(string(data))
	}
}

// parseData converts a --data flag value to a JSON byte slice.
// Prefix the value with '@' to read from a file; use '-' to read from stdin.
func parseData(data string) ([]byte, error) {
	if data == "" {
		return nil, nil
	}
	var raw []byte
	var err error
	switch {
	case data == "@-" || data == "-":
		raw, err = io.ReadAll(os.Stdin)
	case strings.HasPrefix(data, "@"):
		raw, err = os.ReadFile(strings.TrimPrefix(data, "@"))
	default:
		raw = []byte(data)
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid JSON")
	}
	return raw, nil
}

var rootCmd = &cobra.Command{
	Use:   "armory",
	Short: "CLI for the CodeArmory platform",
	Long: `armory interacts with the CodeArmory conductor API.

Token lookup order (highest to lowest precedence):
  1. --token flag
  2. CODEARMORY_TOKEN environment variable
  3. OS keychain (Secret Service on Linux, Keychain on macOS)

When the OS keychain is unavailable (e.g. a headless server), the token is never
written to disk: 'armory auth login' prints an 'export CODEARMORY_TOKEN=…' line to
set in your shell instead.

The CODEARMORY_URL environment variable and --url flag override the stored URL.`,
}

// Execute runs the CLI. It wires the registered modules onto the root command
// first (init() can't, since per-file init order would miss late registrants),
// then dispatches.
func Execute() {
	wireModules()
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flagURL, "url", "", "conductor base URL (overrides CODEARMORY_URL and config)")
	rootCmd.PersistentFlags().StringVar(&flagToken, "token", "", "bearer token (prefer CODEARMORY_TOKEN env var)")
	rootCmd.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "show all fields including IDs and timestamps")
	rootCmd.PersistentFlags().MarkHidden("token") //nolint:errcheck
}
