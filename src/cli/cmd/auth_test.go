package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestLogin_Success(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	silenceStdout(t)

	srv, rec := recordingServer(t, http.StatusOK, `{"token":"jwt-from-server"}`)
	setupCLINoToken(t, srv)

	loginCmd.Flags().Set("email", "user@example.com") //nolint:errcheck
	loginCmd.Flags().Set("password", "supersecret")   //nolint:errcheck

	if err := loginCmd.RunE(loginCmd, nil); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Correct request was sent
	if rec.Method != http.MethodPost || rec.Path != "/login" {
		t.Errorf("request = %s %s, want POST /login", rec.Method, rec.Path)
	}
	var body map[string]string
	json.Unmarshal(rec.Body, &body) //nolint:errcheck
	if body["email"] != "user@example.com" {
		t.Errorf("body email = %q, want user@example.com", body["email"])
	}

	// Token stored in keychain
	tok, err := keyring.Get(keychainService, keychainAccount)
	if err != nil {
		t.Fatalf("keyring.Get: %v", err)
	}
	if tok != "jwt-from-server" {
		t.Errorf("stored token = %q, want jwt-from-server", tok)
	}
}

func TestLogin_HTTPError(t *testing.T) {
	isolateHome(t)
	silenceStdout(t)

	srv, _ := recordingServer(t, http.StatusUnauthorized, "invalid credentials")
	setupCLINoToken(t, srv)

	loginCmd.Flags().Set("email", "bad@example.com") //nolint:errcheck
	loginCmd.Flags().Set("password", "wrong")        //nolint:errcheck

	if err := loginCmd.RunE(loginCmd, nil); err == nil {
		t.Fatal("expected error for 401, got nil")
	}
}

func TestLogin_MissingTokenInResponse(t *testing.T) {
	isolateHome(t)
	silenceStdout(t)

	// Server returns 200 but with no token field
	srv, _ := recordingServer(t, http.StatusOK, `{"message":"ok"}`)
	setupCLINoToken(t, srv)

	loginCmd.Flags().Set("email", "user@example.com") //nolint:errcheck
	loginCmd.Flags().Set("password", "secret123")     //nolint:errcheck

	if err := loginCmd.RunE(loginCmd, nil); err == nil {
		t.Fatal("expected error when token field is absent, got nil")
	}
}

func TestSignup_SendsCorrectBody(t *testing.T) {
	isolateHome(t)
	silenceStdout(t)

	srv, rec := recordingServer(t, http.StatusCreated, `{"id":"new-user"}`)
	setupCLINoToken(t, srv)

	signupCmd.Flags().Set("email", "new@example.com") //nolint:errcheck
	signupCmd.Flags().Set("username", "newuser")      //nolint:errcheck
	signupCmd.Flags().Set("password", "password123")  //nolint:errcheck

	if err := signupCmd.RunE(signupCmd, nil); err != nil {
		t.Fatalf("signup: %v", err)
	}

	if rec.Method != http.MethodPost || rec.Path != "/signup" {
		t.Errorf("request = %s %s, want POST /signup", rec.Method, rec.Path)
	}
	var body map[string]string
	json.Unmarshal(rec.Body, &body) //nolint:errcheck
	if body["email"] != "new@example.com" || body["username"] != "newuser" {
		t.Errorf("signup body = %v", body)
	}
}

func TestLogout_ClearsAllSources(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	silenceStdout(t)

	// Pre-populate both sources
	keyring.Set(keychainService, keychainAccount, "old-token") //nolint:errcheck
	saveConfig(cliConfig{Token: "old-token"})                  //nolint:errcheck

	if err := logoutCmd.RunE(logoutCmd, nil); err != nil {
		t.Fatalf("logout: %v", err)
	}

	if tok, err := keyring.Get(keychainService, keychainAccount); err == nil {
		t.Errorf("keychain token %q still present after logout", tok)
	}
	if cfg := loadConfig(); cfg.Token != "" {
		t.Errorf("config token %q still present after logout", cfg.Token)
	}
}

func TestAuthStatus_NoError(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Setenv("CODEARMORY_TOKEN", "")
	t.Cleanup(func() { flagToken = ""; flagURL = "" })
	flagToken = ""
	flagURL = "http://test:8082"
	silenceStdout(t)

	if err := authStatusCmd.RunE(authStatusCmd, nil); err != nil {
		t.Fatalf("status: %v", err)
	}
}

func TestAuthStatus_FlagToken(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Setenv("CODEARMORY_TOKEN", "")
	t.Cleanup(func() { flagToken = ""; flagURL = "" })
	flagToken = "my-flag-token-that-is-long-enough"
	flagURL = "http://test:8082"
	silenceStdout(t)

	if err := authStatusCmd.RunE(authStatusCmd, nil); err != nil {
		t.Fatalf("status with --token flag: %v", err)
	}
}

func TestAuthStatus_EnvToken(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Setenv("CODEARMORY_TOKEN", "env-token-long-enough-to-slice")
	t.Cleanup(func() { flagToken = ""; flagURL = "" })
	flagToken = ""
	flagURL = "http://test:8082"
	silenceStdout(t)

	if err := authStatusCmd.RunE(authStatusCmd, nil); err != nil {
		t.Fatalf("status with env token: %v", err)
	}
}

func TestAuthStatus_KeychainToken(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Setenv("CODEARMORY_TOKEN", "")
	t.Cleanup(func() { flagToken = ""; flagURL = "" })
	flagToken = ""
	flagURL = "http://test:8082"
	keyring.Set(keychainService, keychainAccount, "keychain-token-long-enough") //nolint:errcheck
	silenceStdout(t)

	if err := authStatusCmd.RunE(authStatusCmd, nil); err != nil {
		t.Fatalf("status with keychain token: %v", err)
	}
}

func TestAuthStatus_ConfigFileToken(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Setenv("CODEARMORY_TOKEN", "")
	t.Cleanup(func() { flagToken = ""; flagURL = "" })
	flagToken = ""
	flagURL = "http://test:8082"
	// No keychain token, but a config-file token.
	saveConfig(cliConfig{Token: "config-file-token-long-enough", URL: "http://test:8082"}) //nolint:errcheck
	silenceStdout(t)

	if err := authStatusCmd.RunE(authStatusCmd, nil); err != nil {
		t.Fatalf("status with config-file token: %v", err)
	}
}

func TestLogin_StoreTokenError(t *testing.T) {
	// Force both keyring and saveConfig to fail so loginCmd.RunE returns
	// "saving token: ..." error (covering auth.go:35-37).
	keyring.MockInitWithError(fmt.Errorf("keyring unavailable"))
	t.Cleanup(func() { keyring.MockInit() })
	isolateHome(t)
	silenceStdout(t)

	// Make $HOME/.config a regular file to cause saveConfig / MkdirAll to fail.
	home, _ := os.UserHomeDir()
	cfgParent := home + "/.config"
	if err := os.WriteFile(cfgParent, []byte("not a dir"), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	srv, _ := recordingServer(t, http.StatusOK, `{"token":"jwt-from-server"}`)
	setupCLINoToken(t, srv)

	loginCmd.Flags().Set("email", "user@example.com") //nolint:errcheck
	loginCmd.Flags().Set("password", "supersecret")   //nolint:errcheck

	if err := loginCmd.RunE(loginCmd, nil); err == nil {
		t.Fatal("expected error when token cannot be stored, got nil")
	}
}
