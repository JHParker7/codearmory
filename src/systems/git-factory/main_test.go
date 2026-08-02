package main

// Unit tests for the process/plumbing helpers in main.go: config resolution
// (envOrDefault / secret / secretOrDefault), the request-body cap, and the
// status-capturing ResponseWriter. Two of these encode ARCHITECTURE §6 warnings
// about scaffold defaults that are correct for JSON CRUD and wrong for git.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("CGF_TEST_ENV", "")
	if got := envOrDefault("CGF_TEST_ENV", "fallback"); got != "fallback" {
		t.Errorf("unset -> %q, want fallback", got)
	}
	t.Setenv("CGF_TEST_ENV", "value")
	if got := envOrDefault("CGF_TEST_ENV", "fallback"); got != "value" {
		t.Errorf("set -> %q, want value", got)
	}
}

func TestSecret_PlainEnv(t *testing.T) {
	t.Setenv("CGF_SECRET", "topsecret")
	if got := secret("CGF_SECRET"); got != "topsecret" {
		t.Errorf("secret = %q, want topsecret", got)
	}
}

// secret prefers ${NAME}_FILE (mounted k8s/Docker secrets) and strips the
// trailing newline a file secret usually carries.
func TestSecret_FileTakesPrecedenceAndTrims(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svc.key")
	if err := os.WriteFile(path, []byte("filesecret\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	t.Setenv("CGF_SECRET", "ignored-plain")
	t.Setenv("CGF_SECRET_FILE", path)
	if got := secret("CGF_SECRET"); got != "filesecret" {
		t.Errorf("secret = %q, want filesecret (from file, trailing newline stripped)", got)
	}
}

func TestSecretOrDefault(t *testing.T) {
	t.Setenv("CGF_MISSING", "")
	if got := secretOrDefault("CGF_MISSING", "def"); got != "def" {
		t.Errorf("missing -> %q, want def", got)
	}
	t.Setenv("CGF_MISSING", "present")
	if got := secretOrDefault("CGF_MISSING", "def"); got != "present" {
		t.Errorf("present -> %q, want present", got)
	}
}

// statusResponseWriter records the status the handler wrote (used by the request
// logger) while still forwarding it to the underlying writer.
func TestStatusResponseWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &statusResponseWriter{ResponseWriter: rec, status: http.StatusOK}
	rw.WriteHeader(http.StatusTeapot)
	if rw.status != http.StatusTeapot {
		t.Errorf("captured status = %d, want %d", rw.status, http.StatusTeapot)
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("underlying status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}

// limitBody caps request bodies at maxBodyBytes (1 MiB) — correct for JSON CRUD.
// A body within the cap reads fine; a body over it errors on read.
//
// ARCHITECTURE §6 flags the flip side: a `git push` POST is the whole packfile
// (potentially gigabytes), so the git-wire routes MUST bypass this middleware or
// pushes fail instantly. That bypass can't be asserted until the git routes and
// their own mux exist; this test pins the cap the git routes must NOT inherit.
func TestLimitBody_CapsOversizedBody(t *testing.T) {
	var readErr error
	var readLen int
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		readLen, readErr = len(b), err
	})
	h := limitBody(inner)

	// Within the cap: reads cleanly.
	small := strings.NewReader(strings.Repeat("a", 1024))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/repos", small))
	if readErr != nil {
		t.Fatalf("small body read error: %v", readErr)
	}
	if readLen != 1024 {
		t.Fatalf("small body len = %d, want 1024", readLen)
	}

	// Over the cap: MaxBytesReader trips.
	big := strings.NewReader(strings.Repeat("a", maxBodyBytes+1))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/repos", big))
	if readErr == nil {
		t.Fatalf("oversized body (%d bytes) read without error — limitBody did not cap at %d", maxBodyBytes+1, maxBodyBytes)
	}
}

// Pins the scaffold body cap constant ARCHITECTURE §6 calls out (1 MiB).
func TestMaxBodyBytesConstant(t *testing.T) {
	if maxBodyBytes != 1<<20 {
		t.Errorf("maxBodyBytes = %d, want %d (1 MiB)", maxBodyBytes, 1<<20)
	}
}
