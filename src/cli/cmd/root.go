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
		return flagToken
	}
	if t := os.Getenv("CODEARMORY_TOKEN"); t != "" {
		return t
	}
	if t, err := keyring.Get(keychainService, keychainAccount); err == nil && t != "" {
		return t
	}
	return loadConfig().Token
}

// storeToken saves the token to the OS keychain. If the keychain is
// unavailable (e.g. headless server), it falls back to the config file.
// Returns a human-readable description of where the token was stored.
func storeToken(token string) (string, error) {
	if err := keyring.Set(keychainService, keychainAccount, token); err == nil {
		return "keychain", nil
	}
	fmt.Fprintf(os.Stderr, "warning: OS keychain unavailable; token stored in plaintext at %s\n", configPath())
	cfg := loadConfig()
	cfg.Token = token
	if err := saveConfig(cfg); err != nil {
		return "", err
	}
	return configPath(), nil
}

// clearToken removes the token from both the keychain and the config file.
func clearToken() {
	keyring.Delete(keychainService, keychainAccount) //nolint:errcheck
	cfg := loadConfig()
	cfg.Token = ""
	saveConfig(cfg) //nolint:errcheck
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
	data, _ := io.ReadAll(resp.Body)
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
  4. ~/.config/codearmory/config.json (fallback when keychain is unavailable)

The CODEARMORY_URL environment variable and --url flag override the stored URL.`,
}

// Execute runs the CLI.
func Execute() {
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
