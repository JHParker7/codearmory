package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// makeTestJWT constructs a minimal syntactically-valid JWT with the given exp claim.
// The signature is fake — this is only used for local decoding tests.
func makeTestJWT(exp int64) string {
	payload, _ := json.Marshal(map[string]any{"exp": exp, "sub": "test-user"})
	return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".fakesig"
}

// captureStdoutDuring runs fn, captures everything written to os.Stdout, and
// returns it. Panics if the pipe can't be created.
func captureStdoutDuring(fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	saved := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = saved
	var buf bytes.Buffer
	io.Copy(&buf, r) //nolint:errcheck
	r.Close()
	return buf.String()
}

func TestLogin_Success(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	silenceStdout(t)

	srv, rec := recordingServer(t, http.StatusOK, `{"token":"jwt-from-server"}`)
	setupCLINoToken(t, srv)

	loginCmd.Flags().Set("email", "user@example.com") //nolint:errcheck
	orig := readPassword
	readPassword = func() (string, error) { return "supersecret", nil }
	t.Cleanup(func() { readPassword = orig })

	if err := loginCmd.RunE(loginCmd, nil); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Correct request was sent
	if rec.Method != http.MethodPost || rec.Path != "/gatekeeper/login" {
		t.Errorf("request = %s %s, want POST /gatekeeper/login", rec.Method, rec.Path)
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
	orig := readPassword
	readPassword = func() (string, error) { return "wrong", nil }
	t.Cleanup(func() { readPassword = orig })

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
	orig := readPassword
	readPassword = func() (string, error) { return "secret123", nil }
	t.Cleanup(func() { readPassword = orig })

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
	orig := readPassword
	readPassword = func() (string, error) { return "password123", nil }
	t.Cleanup(func() { readPassword = orig })

	if err := signupCmd.RunE(signupCmd, nil); err != nil {
		t.Fatalf("signup: %v", err)
	}

	if rec.Method != http.MethodPost || rec.Path != "/gatekeeper/signup" {
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
	orig := readPassword
	readPassword = func() (string, error) { return "supersecret", nil }
	t.Cleanup(func() { readPassword = orig })

	if err := loginCmd.RunE(loginCmd, nil); err == nil {
		t.Fatal("expected error when token cannot be stored, got nil")
	}
}

func TestJWTExpiry_FutureExp(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	tok := makeTestJWT(exp)
	got, ok := jwtExpiry(tok)
	if !ok {
		t.Fatal("jwtExpiry returned false for valid JWT")
	}
	if got.Unix() != exp {
		t.Errorf("jwtExpiry exp = %d, want %d", got.Unix(), exp)
	}
}

func TestJWTExpiry_PastExp(t *testing.T) {
	exp := time.Now().Add(-time.Hour).Unix()
	tok := makeTestJWT(exp)
	got, ok := jwtExpiry(tok)
	if !ok {
		t.Fatal("jwtExpiry returned false for JWT with past exp")
	}
	if got.Unix() != exp {
		t.Errorf("jwtExpiry exp = %d, want %d", got.Unix(), exp)
	}
}

func TestJWTExpiry_NoExp(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"sub": "user"})
	tok := "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	_, ok := jwtExpiry(tok)
	if ok {
		t.Fatal("jwtExpiry should return false for JWT without exp claim")
	}
}

func TestJWTExpiry_NotAJWT(t *testing.T) {
	_, ok := jwtExpiry("not-a-jwt")
	if ok {
		t.Fatal("jwtExpiry should return false for non-JWT string")
	}
}

func TestFriendlyDuration_Formats(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{2*time.Hour + 15*time.Minute, "2h15m"},
		{3 * time.Hour, "3h"},
		{45 * time.Minute, "45m"},
		{30 * time.Second, "less than a minute"},
		{-(2*time.Hour + 15*time.Minute), "2h15m"},
	}
	for _, tc := range cases {
		if got := friendlyDuration(tc.d); got != tc.want {
			t.Errorf("friendlyDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestBearerToken_ExpiredStoredToken(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Setenv("CODEARMORY_TOKEN", "")
	t.Cleanup(func() { flagToken = "" })
	flagToken = ""

	expiredTok := makeTestJWT(time.Now().Add(-time.Hour).Unix())
	keyring.Set(keychainService, keychainAccount, expiredTok) //nolint:errcheck

	if got := bearerToken(); got != "" {
		t.Errorf("bearerToken() = %q for expired keychain token, want empty", got)
	}
}

func TestBearerToken_ExpiredConfigToken(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Setenv("CODEARMORY_TOKEN", "")
	t.Cleanup(func() { flagToken = "" })
	flagToken = ""

	expiredTok := makeTestJWT(time.Now().Add(-time.Hour).Unix())
	saveConfig(cliConfig{Token: expiredTok}) //nolint:errcheck

	if got := bearerToken(); got != "" {
		t.Errorf("bearerToken() = %q for expired config token, want empty", got)
	}
}

func TestAuthStatus_ExpiredToken(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Setenv("CODEARMORY_TOKEN", "")
	t.Cleanup(func() { flagToken = ""; flagURL = "" })
	flagToken = ""
	flagURL = "http://test:8082"

	expiredTok := makeTestJWT(time.Now().Add(-time.Hour).Unix())
	keyring.Set(keychainService, keychainAccount, expiredTok) //nolint:errcheck

	out := captureStdoutDuring(func() {
		if err := authStatusCmd.RunE(authStatusCmd, nil); err != nil {
			t.Fatalf("status: %v", err)
		}
	})
	if !strings.Contains(out, "expired") {
		t.Errorf("authStatusCmd output should indicate expired token, got: %q", out)
	}
	if !strings.Contains(out, "auth login") {
		t.Errorf("authStatusCmd output should hint at auth login, got: %q", out)
	}
}

func TestAuthStatus_ValidToken(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Setenv("CODEARMORY_TOKEN", "")
	t.Cleanup(func() { flagToken = ""; flagURL = "" })
	flagToken = ""
	flagURL = "http://test:8082"

	validTok := makeTestJWT(time.Now().Add(2 * time.Hour).Unix())
	keyring.Set(keychainService, keychainAccount, validTok) //nolint:errcheck

	out := captureStdoutDuring(func() {
		if err := authStatusCmd.RunE(authStatusCmd, nil); err != nil {
			t.Fatalf("status: %v", err)
		}
	})
	if !strings.Contains(out, "valid") {
		t.Errorf("authStatusCmd output should indicate valid token, got: %q", out)
	}
	if !strings.Contains(out, "expires in") {
		t.Errorf("authStatusCmd output should show expiry time, got: %q", out)
	}
}
