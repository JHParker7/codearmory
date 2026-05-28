package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// ── envOrDefault ──────────────────────────────────────────────────────────────

func TestEnvOrDefaultUsesEnv(t *testing.T) {
	t.Setenv("TEST_ENV_VAR", "from_env")
	if got := envOrDefault("TEST_ENV_VAR", "fallback"); got != "from_env" {
		t.Fatalf("got %q, want %q", got, "from_env")
	}
}

func TestEnvOrDefaultUsesFallback(t *testing.T) {
	os.Unsetenv("TEST_ENV_VAR_MISSING")
	if got := envOrDefault("TEST_ENV_VAR_MISSING", "fallback"); got != "fallback" {
		t.Fatalf("got %q, want %q", got, "fallback")
	}
}

func TestEnvOrDefaultEmptyEnvUsesFallback(t *testing.T) {
	t.Setenv("TEST_ENV_VAR_EMPTY", "")
	if got := envOrDefault("TEST_ENV_VAR_EMPTY", "fallback"); got != "fallback" {
		t.Fatalf("got %q, want %q", got, "fallback")
	}
}

// ── secret ────────────────────────────────────────────────────────────────────

func TestSecretPlainEnv(t *testing.T) {
	t.Setenv("MY_SECRET", "plainvalue")
	os.Unsetenv("MY_SECRET_FILE")
	if got := secret("MY_SECRET"); got != "plainvalue" {
		t.Fatalf("got %q, want %q", got, "plainvalue")
	}
}

func TestSecretFromFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "secret")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("  file-value\n")
	f.Close()

	t.Setenv("MY_SECRET_FILE", f.Name())
	if got := secret("MY_SECRET"); got != "file-value" {
		t.Fatalf("got %q, want %q", got, "file-value")
	}
}

func TestSecretFileTakesPrecedenceOverPlainEnv(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "secret")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("from-file")
	f.Close()

	t.Setenv("MY_SECRET", "from-env")
	t.Setenv("MY_SECRET_FILE", f.Name())
	if got := secret("MY_SECRET"); got != "from-file" {
		t.Fatalf("_FILE must take precedence: got %q", got)
	}
}

func TestSecretMissingReturnsEmpty(t *testing.T) {
	os.Unsetenv("NO_SUCH_SECRET")
	os.Unsetenv("NO_SUCH_SECRET_FILE")
	if got := secret("NO_SUCH_SECRET"); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

// ── secretOrDefault ───────────────────────────────────────────────────────────

func TestSecretOrDefaultUsesFallback(t *testing.T) {
	os.Unsetenv("MISSING_SECRET")
	os.Unsetenv("MISSING_SECRET_FILE")
	if got := secretOrDefault("MISSING_SECRET", "default-val"); got != "default-val" {
		t.Fatalf("got %q, want %q", got, "default-val")
	}
}

func TestSecretOrDefaultUsesSecret(t *testing.T) {
	t.Setenv("PRESENT_SECRET", "real-val")
	os.Unsetenv("PRESENT_SECRET_FILE")
	if got := secretOrDefault("PRESENT_SECRET", "default-val"); got != "real-val" {
		t.Fatalf("got %q, want %q", got, "real-val")
	}
}

// ── userKey ───────────────────────────────────────────────────────────────────

func TestUserKey(t *testing.T) {
	r := httptest.NewRequest("GET", "/state/alice/dev", nil)
	// httptest.NewRequest does not populate PathValues, so set them manually.
	r.SetPathValue("username", "alice")
	r.SetPathValue("workspace", "dev")

	k, res := userKey(r)
	if k != "alice/dev" {
		t.Fatalf("workspace key: got %q, want %q", k, "alice/dev")
	}
	if res != "blueprints/states/alice/dev" {
		t.Fatalf("resource: got %q, want %q", res, "blueprints/states/alice/dev")
	}
}

// ── lockUnlock dispatcher ─────────────────────────────────────────────────────

// stubKeyFn is a minimal keyFn for testing the dispatcher without a real DB.
func stubKeyFn(r *http.Request) (string, string) {
	return "ws/key", "blueprints/states/ws/key"
}

// lockUnlockMethodNotAllowed verifies that an unsupported method is rejected.
func TestLockUnlockMethodNotAllowed(t *testing.T) {
	handler := lockUnlock(stubKeyFn)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer tok")

	handler(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", w.Code)
	}
}

// TestLockUnlockLockMethod verifies that a LOCK request is dispatched (reaching
// requireAuth and returning 401 when there are no credentials, meaning it did
// not hit the 405 branch).
func TestLockUnlockLockMethod(t *testing.T) {
	handler := lockUnlock(stubKeyFn)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("LOCK", "/", nil)
	// No auth header → requireAuth will return 401, confirming dispatch worked.

	handler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 (LOCK dispatched to handleLockState which calls requireAuth)", w.Code)
	}
}

// TestLockUnlockUnlockMethod verifies that an UNLOCK request is dispatched.
func TestLockUnlockUnlockMethod(t *testing.T) {
	handler := lockUnlock(stubKeyFn)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("UNLOCK", "/", nil)

	handler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 (UNLOCK dispatched to handleUnlockState which calls requireAuth)", w.Code)
	}
}

// ── requireAuth ───────────────────────────────────────────────────────────────

func TestRequireAuthNoHeader(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)

	_, ok := requireAuth(r.Context(), w, r)

	if ok {
		t.Fatal("expected ok=false with no Authorization header")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") != "Basic" {
		t.Fatal("expected WWW-Authenticate: Basic header")
	}
}

func TestRequireAuthBearerToken(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer validtoken")

	tok, ok := requireAuth(r.Context(), w, r)

	if !ok {
		t.Fatal("expected ok=true for Bearer token")
	}
	if tok != "validtoken" {
		t.Fatalf("got %q, want %q", tok, "validtoken")
	}
}
