package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestMain wires the package globals tests rely on: a fixed encryption key, a real
// HTTP client, an in-memory sqlite DB, and a fake gatekeeper that treats the bearer
// token as the user id.
func TestMain(m *testing.M) {
	sum := sha256.Sum256([]byte("test-key"))
	encKey = sum[:]
	httpClient = initHTTPClient()

	conn, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		panic(err)
	}
	if err := conn.AutoMigrate(&GitBackend{}, &GitRepo{}); err != nil {
		panic(err)
	}
	gormDB = conn
	gormDBRead = conn

	gk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"user_id": tok}) //nolint:errcheck
	}))
	gatekeeperURL = gk.URL

	code := m.Run()
	gk.Close()
	os.Exit(code)
}

func req(method, target, bearer string, body any) *http.Request {
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, target, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

func TestBackendCRUD(t *testing.T) {
	// Create
	rec := httptest.NewRecorder()
	handleCreateBackend(rec, req("POST", "/backends", "alice", createBackendRequest{
		Name: "gh", Type: backendGitHub, BaseURL: "https://github.com",
		Auth: authConfig{Mode: modePAT, Token: "ghp_x"},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var created backendView
	json.Unmarshal(rec.Body.Bytes(), &created) //nolint:errcheck
	if created.ID == "" || created.Host != "github.com" {
		t.Fatalf("unexpected created backend: %+v", created)
	}
	// Secret must never appear in the response.
	if strings.Contains(rec.Body.String(), "ghp_x") {
		t.Fatal("create response leaked the token")
	}

	// List (alice sees it)
	rec = httptest.NewRecorder()
	handleListBackends(rec, req("GET", "/backends", "alice", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("list: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Another user must NOT see alice's backend.
	rec = httptest.NewRecorder()
	handleListBackends(rec, req("GET", "/backends", "bob", nil))
	if strings.Contains(rec.Body.String(), created.ID) {
		t.Fatal("cross-user leak: bob saw alice's backend")
	}

	// Get by id (bob denied → not found)
	rec = httptest.NewRecorder()
	r := req("GET", "/backends/"+created.ID, "bob", nil)
	r.SetPathValue("id", created.ID)
	handleGetBackend(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob get: expected 404, got %d", rec.Code)
	}

	// Unauthorized (no bearer)
	rec = httptest.NewRecorder()
	handleListBackends(rec, req("GET", "/backends", "", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	// Delete (alice)
	rec = httptest.NewRecorder()
	r = req("DELETE", "/backends/"+created.ID, "alice", nil)
	r.SetPathValue("id", created.ID)
	handleDeleteBackend(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: code=%d", rec.Code)
	}
}

func TestCreateBackendValidation(t *testing.T) {
	rec := httptest.NewRecorder()
	handleCreateBackend(rec, req("POST", "/backends", "carol", createBackendRequest{
		Name: "bad", Type: "svn", BaseURL: "https://x.com", Auth: authConfig{Mode: modePAT},
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad type: expected 400, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	handleCreateBackend(rec, req("POST", "/backends", "carol", createBackendRequest{
		Name: "badmode", Type: backendGitHub, BaseURL: "https://x.com", Auth: authConfig{Mode: modeBasic},
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: expected 400, got %d", rec.Code)
	}
}

func TestMintCredentialEndpoint(t *testing.T) {
	// Link a generic backend for dave.
	rec := httptest.NewRecorder()
	handleCreateBackend(rec, req("POST", "/backends", "dave", createBackendRequest{
		Name: "internal", Type: backendGeneric, BaseURL: "https://git.internal",
		Auth: authConfig{Mode: modeBasic, Username: "dave", Password: "s3cret"},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}

	// Mint for a repo on that host.
	rec = httptest.NewRecorder()
	handleMintCredential(rec, req("POST", "/credentials", "dave", mintRequest{RepoURL: "https://git.internal/team/app.git"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("mint: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var cred credential
	json.Unmarshal(rec.Body.Bytes(), &cred) //nolint:errcheck
	if !strings.Contains(cred.CloneURL, "dave:s3cret@git.internal") {
		t.Fatalf("clone url missing creds: %s", cred.CloneURL)
	}

	// Unknown host → 404.
	rec = httptest.NewRecorder()
	handleMintCredential(rec, req("POST", "/credentials", "dave", mintRequest{RepoURL: "https://unknown.host/x/y.git"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown host: expected 404, got %d", rec.Code)
	}
}

func TestInternalCloneToken(t *testing.T) {
	internalKey = "ik-test"

	// Link a backend for erin.
	rec := httptest.NewRecorder()
	handleCreateBackend(rec, req("POST", "/backends", "erin", createBackendRequest{
		Name: "ghe", Type: backendGitHub, BaseURL: "https://ghe.internal",
		Auth: authConfig{Mode: modePAT, Token: "ghp_erin", Username: "x-access-token"},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}

	// Wrong key rejected.
	rec = httptest.NewRecorder()
	r := req("POST", "/internal/clone-token", "", internalMintRequest{UserID: "erin", RepoURL: "https://ghe.internal/o/r.git"})
	r.Header.Set("X-Internal-Key", "wrong")
	handleInternalCloneToken(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: expected 401, got %d", rec.Code)
	}

	// Correct key mints a clone URL.
	rec = httptest.NewRecorder()
	r = req("POST", "/internal/clone-token", "", internalMintRequest{UserID: "erin", RepoURL: "https://ghe.internal/o/r.git"})
	r.Header.Set("X-Internal-Key", "ik-test")
	handleInternalCloneToken(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("clone-token: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		CloneURL string `json:"clone_url"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out) //nolint:errcheck
	if !strings.Contains(out.CloneURL, "x-access-token:ghp_erin@ghe.internal") {
		t.Fatalf("unexpected clone url: %s", out.CloneURL)
	}
}
