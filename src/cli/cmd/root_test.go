package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// ── parseData ────────────────────────────────────────────────────────────────

func TestParseData_Empty(t *testing.T) {
	got, err := parseData("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got %q, want nil", got)
	}
}

func TestParseData_InlineJSON(t *testing.T) {
	input := `{"key":"val"}`
	got, err := parseData(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != input {
		t.Errorf("got %q, want %q", got, input)
	}
}

func TestParseData_InvalidJSON(t *testing.T) {
	_, err := parseData(`not json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestParseData_AtFile(t *testing.T) {
	f, err := os.CreateTemp("", "armory-test-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	content := `{"from":"file"}`
	f.WriteString(content)
	f.Close()

	got, err := parseData("@" + f.Name())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != content {
		t.Errorf("got %q, want %q", got, content)
	}
}

func TestParseData_AtFileMissing(t *testing.T) {
	_, err := parseData("@/nonexistent/path.json")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// ── loadConfig / saveConfig ───────────────────────────────────────────────────

func TestSaveAndLoadConfig(t *testing.T) {
	isolateHome(t)

	want := cliConfig{URL: "http://test:9090", Token: "my-token"}
	if err := saveConfig(want); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	got := loadConfig()
	if got.URL != want.URL {
		t.Errorf("URL = %q, want %q", got.URL, want.URL)
	}
	if got.Token != want.Token {
		t.Errorf("Token = %q, want %q", got.Token, want.Token)
	}
}

func TestSaveConfig_CreatesDirectory(t *testing.T) {
	isolateHome(t)
	home, _ := os.UserHomeDir()
	cfgDir := filepath.Join(home, ".config", "codearmory")
	os.RemoveAll(cfgDir) // ensure it doesn't exist

	if err := saveConfig(cliConfig{URL: "http://localhost:8082"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if _, err := os.Stat(cfgDir); err != nil {
		t.Errorf("config directory not created: %v", err)
	}
}

func TestSaveConfig_FilePermissions(t *testing.T) {
	isolateHome(t)
	if err := saveConfig(cliConfig{Token: "secret"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	info, err := os.Stat(configPath())
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("config file mode = %o, want 0600", perm)
	}
}

func TestLoadConfig_DefaultURL(t *testing.T) {
	isolateHome(t)
	cfg := loadConfig()
	if cfg.URL != "http://localhost:8082" {
		t.Errorf("default URL = %q, want http://localhost:8082", cfg.URL)
	}
}

func TestLoadConfig_IgnoresMalformed(t *testing.T) {
	isolateHome(t)
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "codearmory")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("not json"), 0600)

	cfg := loadConfig()
	if cfg.URL != "http://localhost:8082" {
		t.Errorf("expected default URL after malformed config, got %q", cfg.URL)
	}
}

// ── bearerToken precedence ────────────────────────────────────────────────────

func TestBearerTokenPrecedence(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Setenv("CODEARMORY_TOKEN", "")
	t.Cleanup(func() { flagToken = "" })

	// 1. Nothing set → empty
	if got := bearerToken(); got != "" {
		t.Errorf("step 1: got %q, want empty", got)
	}

	// 2. Config file
	saveConfig(cliConfig{Token: "config-tok"})
	if got := bearerToken(); got != "config-tok" {
		t.Errorf("step 2: got %q, want config-tok", got)
	}

	// 3. Keychain overrides config
	keyring.Set(keychainService, keychainAccount, "keychain-tok")
	if got := bearerToken(); got != "keychain-tok" {
		t.Errorf("step 3: got %q, want keychain-tok", got)
	}

	// 4. Env var overrides keychain
	t.Setenv("CODEARMORY_TOKEN", "env-tok")
	if got := bearerToken(); got != "env-tok" {
		t.Errorf("step 4: got %q, want env-tok", got)
	}

	// 5. Flag overrides all
	flagToken = "flag-tok"
	if got := bearerToken(); got != "flag-tok" {
		t.Errorf("step 5: got %q, want flag-tok", got)
	}
}

// ── conductorURL precedence ───────────────────────────────────────────────────

func TestConductorURLPrecedence(t *testing.T) {
	isolateHome(t)
	t.Setenv("CODEARMORY_URL", "")
	t.Cleanup(func() { flagURL = "" })

	// Default
	if got := conductorURL(); got != "http://localhost:8082" {
		t.Errorf("default: got %q", got)
	}

	// Config file
	saveConfig(cliConfig{URL: "http://config:9090"})
	if got := conductorURL(); got != "http://config:9090" {
		t.Errorf("config: got %q", got)
	}

	// Env var overrides config
	t.Setenv("CODEARMORY_URL", "http://env:9090")
	if got := conductorURL(); got != "http://env:9090" {
		t.Errorf("env: got %q", got)
	}

	// Flag overrides all
	flagURL = "http://flag:9090"
	if got := conductorURL(); got != "http://flag:9090" {
		t.Errorf("flag: got %q", got)
	}
}

// ── storeToken / clearToken ───────────────────────────────────────────────────

func TestStoreToken_UsesKeychain(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Cleanup(func() { flagURL = "" })
	flagURL = "http://localhost:8082"

	where, err := storeToken("my-jwt")
	if err != nil {
		t.Fatalf("storeToken: %v", err)
	}
	if where != "keychain" {
		t.Errorf("where = %q, want keychain", where)
	}

	got, err := keyring.Get(keychainService, keychainAccount)
	if err != nil || got != "my-jwt" {
		t.Errorf("keyring.Get = %q (err %v), want my-jwt", got, err)
	}
}

func TestClearToken_RemovesBothSources(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)

	// Populate both sources
	keyring.Set(keychainService, keychainAccount, "tok")
	saveConfig(cliConfig{Token: "tok"})

	clearToken()

	if tok, err := keyring.Get(keychainService, keychainAccount); err == nil {
		t.Errorf("keychain still has token %q after clearToken", tok)
	}
	if cfg := loadConfig(); cfg.Token != "" {
		t.Errorf("config still has token %q after clearToken", cfg.Token)
	}
}

// ── printJSON ─────────────────────────────────────────────────────────────────

func TestPrintJSON_ValidJSON(t *testing.T) {
	// Redirect stdout so the output doesn't flood the test log; we just verify
	// no panic occurs and the function handles valid and invalid input.
	silenceStdout(t)
	printJSON([]byte(`{"a":1}`))         // valid — should pretty-print
	printJSON([]byte(`not json`))        // invalid — should print raw
	printJSON([]byte{})                  // empty — should be a no-op
}

// ── saveConfig JSON structure ─────────────────────────────────────────────────

func TestSaveConfig_JSONStructure(t *testing.T) {
	isolateHome(t)
	saveConfig(cliConfig{URL: "http://host", Token: "tok"})

	raw, _ := os.ReadFile(configPath())
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	if m["url"] != "http://host" {
		t.Errorf("url = %q, want http://host", m["url"])
	}
	if m["token"] != "tok" {
		t.Errorf("token = %q, want tok", m["token"])
	}
	// Sanity: file should be indented (pretty-printed)
	if !strings.Contains(string(raw), "\n") {
		t.Error("config file is not indented")
	}
}
