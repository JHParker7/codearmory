package cmd

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// ── readStateFile ─────────────────────────────────────────────────────────────

func TestReadStateFile_FromFile(t *testing.T) {
	f, err := os.CreateTemp("", "state-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(`{"serial":1}`)
	f.Close()

	got, err := readStateFile(f.Name())
	if err != nil {
		t.Fatalf("readStateFile: %v", err)
	}
	if string(got) != `{"serial":1}` {
		t.Errorf("got %q, want {\"serial\":1}", got)
	}
}

func TestReadStateFile_Empty(t *testing.T) {
	// Empty string should read from stdin (we just verify no panic here;
	// stdin is a pipe during `go test` so it returns empty immediately).
	f, _ := os.Open(os.DevNull)
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; f.Close() })

	got, err := readStateFile("")
	if err != nil {
		t.Fatalf("readStateFile empty: %v", err)
	}
	_ = got
}

func TestReadStateFile_Missing(t *testing.T) {
	_, err := readStateFile("/nonexistent/path/state.json")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// ── User-scoped state ─────────────────────────────────────────────────────────

func TestUserState_Get(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"serial":2}`)
	setupCLI(t, srv)
	silenceStdout(t)

	if err := apiCall("GET", "/state/alice/prod", nil); err != nil {
		t.Fatalf("state get: %v", err)
	}
	if rec.Method != "GET" || rec.Path != "/state/alice/prod" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestUserState_Push_FromFile(t *testing.T) {
	f, err := os.CreateTemp("", "state-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	stateJSON := `{"version":4,"serial":7}`
	f.WriteString(stateJSON)
	f.Close()

	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	body, err := readStateFile(f.Name())
	if err != nil {
		t.Fatalf("readStateFile: %v", err)
	}
	if err := apiCall("POST", "/state/alice/prod", body); err != nil {
		t.Fatalf("state push: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/state/alice/prod" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
	if !strings.Contains(string(rec.Body), `"version":4`) {
		t.Errorf("body %q does not contain version field", rec.Body)
	}
}

func TestUserState_Delete(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	if err := apiCall("DELETE", "/state/alice/prod", nil); err != nil {
		t.Fatalf("state delete: %v", err)
	}
	if rec.Method != "DELETE" || rec.Path != "/state/alice/prod" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestUserState_Lock(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	lockBody := jsonBody(map[string]string{"ID": "lock-123", "Operation": "OperationTypePlan"})
	if err := apiCall("LOCK", "/state/alice/prod", lockBody); err != nil {
		t.Fatalf("state lock: %v", err)
	}
	if rec.Method != "LOCK" || rec.Path != "/state/alice/prod" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
	if !strings.Contains(string(rec.Body), "lock-123") {
		t.Errorf("lock body missing ID: %q", rec.Body)
	}
}

func TestUserState_Unlock(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	if err := apiCall("UNLOCK", "/state/alice/prod", nil); err != nil {
		t.Fatalf("state unlock: %v", err)
	}
	if rec.Method != "UNLOCK" || rec.Path != "/state/alice/prod" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

// ── Org-scoped state ──────────────────────────────────────────────────────────

func TestOrgState_Get(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"serial":1}`)
	setupCLI(t, srv)
	silenceStdout(t)

	if err := apiCall("GET", "/myorg/state/backend/staging", nil); err != nil {
		t.Fatalf("org-state get: %v", err)
	}
	if rec.Method != "GET" || rec.Path != "/myorg/state/backend/staging" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestOrgState_Push(t *testing.T) {
	f, err := os.CreateTemp("", "state-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(`{"serial":3}`)
	f.Close()

	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	body, _ := readStateFile(f.Name())
	if err := apiCall("POST", "/myorg/state/backend/staging", body); err != nil {
		t.Fatalf("org-state push: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/myorg/state/backend/staging" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestOrgState_Lock(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	if err := apiCall("LOCK", "/myorg/state/backend/staging", nil); err != nil {
		t.Fatalf("org-state lock: %v", err)
	}
	if rec.Method != "LOCK" || rec.Path != "/myorg/state/backend/staging" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestOrgState_Unlock(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	if err := apiCall("UNLOCK", "/myorg/state/backend/staging", nil); err != nil {
		t.Fatalf("org-state unlock: %v", err)
	}
	if rec.Method != "UNLOCK" || rec.Path != "/myorg/state/backend/staging" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

func TestOrgState_Delete(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	silenceStdout(t)

	if err := apiCall("DELETE", "/myorg/state/backend/staging", nil); err != nil {
		t.Fatalf("org-state delete: %v", err)
	}
	if rec.Method != "DELETE" || rec.Path != "/myorg/state/backend/staging" {
		t.Errorf("request = %s %s", rec.Method, rec.Path)
	}
}

// ── Auth header on state routes ───────────────────────────────────────────────

func TestStateRoutes_AuthHeaderPresent(t *testing.T) {
	routes := []struct{ method, path string }{
		{"GET", "/state/alice/prod"},
		{"POST", "/state/alice/prod"},
		{"DELETE", "/state/alice/prod"},
		{"LOCK", "/state/alice/prod"},
		{"UNLOCK", "/state/alice/prod"},
		{"GET", "/acme/state/ops/dev"},
	}
	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			srv, rec := recordingServer(t, http.StatusOK, `{}`)
			setupCLI(t, srv)
			silenceStdout(t)

			apiCall(r.method, r.path, nil) //nolint:errcheck
			if rec.Auth != "Bearer test-jwt" {
				t.Errorf("Authorization = %q, want Bearer test-jwt", rec.Auth)
			}
		})
	}
}
