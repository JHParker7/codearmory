package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// newTestGithubApp builds a githubApp backed by a freshly generated RSA key.
func newTestGithubApp(t *testing.T, secret string) *githubApp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	app, err := newGithubApp(12345, string(pemBytes), secret)
	if err != nil {
		t.Fatalf("newGithubApp: %v", err)
	}
	return app
}

func TestNewGithubApp_BadPEM(t *testing.T) {
	if _, err := newGithubApp(1, "not-a-pem", "s"); err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestNewGithubApp_Valid(t *testing.T) {
	app := newTestGithubApp(t, "s")
	if app.appID != 12345 || app.privateKey == nil {
		t.Fatal("app not initialized")
	}
}

func TestB64URL(t *testing.T) {
	// URL-safe, no padding.
	got := b64url([]byte{0xff, 0xfe, 0xfd})
	if strings.ContainsAny(got, "=+/") {
		t.Errorf("b64url leaked padding/std chars: %q", got)
	}
}

func TestMakeJWT(t *testing.T) {
	app := newTestGithubApp(t, "s")
	jwt, err := app.makeJWT()
	if err != nil {
		t.Fatalf("makeJWT: %v", err)
	}
	if parts := strings.Split(jwt, "."); len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		t.Errorf("jwt not a 3-part token: %q", jwt)
	}
}

func TestVerifySignature(t *testing.T) {
	app := newTestGithubApp(t, "webhook-secret")
	body := []byte(`{"hello":"world"}`)
	good := "sha256=" + computeHMAC("webhook-secret", body)
	if !app.verifySignature(body, good) {
		t.Error("valid signature rejected")
	}
	if app.verifySignature(body, "sha256=deadbeef") {
		t.Error("invalid signature accepted")
	}
}

func TestNormalizeGitHubPayload(t *testing.T) {
	push := `{"ref":"refs/heads/main","head_commit":{"id":"abc","message":"m"},"pusher":{"name":"alice"},"repository":{"full_name":"org/repo"},"installation":{"id":99}}`
	p, inst, ok := normalizeGitHubPayload("push", []byte(push))
	if !ok || p.Repo != "org/repo" || p.Ref != "main" || p.Commit != "abc" || inst != 99 {
		t.Fatalf("push normalize wrong: %+v inst=%d ok=%v", p, inst, ok)
	}

	pr := `{"action":"opened","pull_request":{"head":{"ref":"feat","sha":"sha1"},"user":{"login":"bob"},"title":"t"},"repository":{"full_name":"org/repo"}}`
	p2, _, ok2 := normalizeGitHubPayload("pull_request", []byte(pr))
	if !ok2 || p2.Event != "pull_request.opened" || p2.Ref != "feat" {
		t.Fatalf("pr normalize wrong: %+v ok=%v", p2, ok2)
	}

	if _, _, ok3 := normalizeGitHubPayload("issues", []byte(`{}`)); ok3 {
		t.Error("unsupported event should be ok=false")
	}
	if _, _, ok4 := normalizeGitHubPayload("push", []byte(`{not json`)); ok4 {
		t.Error("bad json should be ok=false")
	}
}

func TestHandleGitHubWebhook_PingAndBadSig(t *testing.T) {
	app := newTestGithubApp(t, "whsec")
	h := handleGitHubWebhook(app)

	// Ping with valid signature → 200.
	body := []byte(`{"zen":"hi"}`)
	r := httptest.NewRequest(http.MethodPost, "/hooks/github", bytes.NewReader(body))
	r.Header.Set("X-GitHub-Event", "ping")
	r.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC("whsec", body))
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("ping got %d, want 200", w.Code)
	}

	// Bad signature → 401.
	r2 := httptest.NewRequest(http.MethodPost, "/hooks/github", bytes.NewReader(body))
	r2.Header.Set("X-GitHub-Event", "ping")
	r2.Header.Set("X-Hub-Signature-256", "sha256=wrong")
	w2 := httptest.NewRecorder()
	h(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig got %d, want 401", w2.Code)
	}
}

func TestHandleGitHubWebhook_UnknownEventIgnored(t *testing.T) {
	app := newTestGithubApp(t, "whsec")
	h := handleGitHubWebhook(app)
	body := []byte(`{"action":"x"}`)
	r := httptest.NewRequest(http.MethodPost, "/hooks/github", bytes.NewReader(body))
	r.Header.Set("X-GitHub-Event", "issues") // unsupported → normalize ok=false
	r.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC("whsec", body))
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unknown event got %d, want 200", w.Code)
	}
}

func TestHandleGitHubWebhook_PushDispatches(t *testing.T) {
	requireDB(t)
	repo := "org/" + uuid.New().String()
	insertRule(t, PipelineRule{Name: "gh", Source: repo, Events: []string{"push"}, WorkflowID: "w1", Secret: secretPtr("anything"), CreatedBy: "u1", OrgID: "o1", Active: true})
	t.Cleanup(func() { connect().Exec(`DELETE FROM hook_triggers WHERE workflow_id = 'w1'`) }) //nolint:errcheck
	fakeWorkflows(t, map[string]string{"w1": "o1"}, "run-gh")

	app := newTestGithubApp(t, "whsec")
	h := handleGitHubWebhook(app)
	// No "installation" key → installationID 0 → no GitHub API call (check-run skipped).
	body := []byte(`{"ref":"refs/heads/main","head_commit":{"id":"c1","message":"m"},"pusher":{"name":"alice"},"repository":{"full_name":"` + repo + `"}}`)
	r := httptest.NewRequest(http.MethodPost, "/hooks/github", bytes.NewReader(body))
	r.Header.Set("X-GitHub-Event", "push")
	r.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC("whsec", body))
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("push got %d, want 200: %s", w.Code, w.Body.String())
	}
}

// ── git adapter (POST /hooks/git) ──────────────────────────────────────────────

func TestHandleGitWebhook_Dispatches(t *testing.T) {
	requireDB(t)
	repo := "git/" + uuid.New().String()
	insertRule(t, PipelineRule{Name: "gw", Source: repo, Events: []string{"push"}, RefFilter: "refs/heads/*", WorkflowID: "w1", Secret: secretPtr("sek"), CreatedBy: "u1", OrgID: "o1", Active: true})
	t.Cleanup(func() { connect().Exec(`DELETE FROM hook_triggers WHERE workflow_id = 'w1'`) }) //nolint:errcheck
	fakeWorkflows(t, map[string]string{"w1": "o1"}, "run-git")

	body, _ := json.Marshal(map[string]any{"repo": repo, "event": "push", "ref": "refs/heads/main", "commit": "c1"})
	r := httptest.NewRequest(http.MethodPost, "/hooks/git", bytes.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC("sek", body))
	w := httptest.NewRecorder()
	handleGitWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("git webhook got %d, want 200: %s", w.Code, w.Body.String())
	}
	var ev HookEvent
	json.Unmarshal(w.Body.Bytes(), &ev) //nolint:errcheck
	if ev.Status != "triggered" {
		t.Errorf("status = %q, want triggered (matched=%d)", ev.Status, ev.RulesMatched)
	}
}

func TestHandleGitWebhook_MissingRepoOrEvent(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"event": "push"})
	w := httptest.NewRecorder()
	handleGitWebhook(w, httptest.NewRequest(http.MethodPost, "/hooks/git", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing repo got %d, want 400", w.Code)
	}
}

func TestGitBaseInputs(t *testing.T) {
	in := gitBaseInputs(gitPayload{Repo: "r", Ref: "main", Commit: "abc"})
	if in["HOOK_REPO"] != "r" || in["HOOK_REF"] != "main" || in["HOOK_COMMIT"] != "abc" {
		t.Errorf("git base inputs wrong: %+v", in)
	}
}

// ── internal (ticket) event + middleware ───────────────────────────────────────

func TestHandleInternalEvent_Unauthorized(t *testing.T) {
	withTriggerKey(t, "k")
	body, _ := json.Marshal(map[string]any{"source": "tickets", "event": "created", "org_id": "o1"})
	r := httptest.NewRequest(http.MethodPost, "/internal/events", bytes.NewReader(body))
	// no/invalid X-Hooks-Token → 401
	w := httptest.NewRecorder()
	handleInternalEvent(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleInternalEvent_BadJSON(t *testing.T) {
	w := httptest.NewRecorder()
	handleInternalEvent(w, httptest.NewRequest(http.MethodPost, "/internal/events", bytes.NewBufferString("{bad")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestRequestLogger_ServeHTTP(t *testing.T) {
	for _, path := range []string{"/rules", "/healthz"} {
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
		w := httptest.NewRecorder()
		(&requestLogger{handler: inner}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusCreated {
			t.Errorf("%s: status %d, want 201", path, w.Code)
		}
	}
}

func TestLimitBody_Hooks(t *testing.T) {
	var readErr error
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	})
	big := strings.NewReader(strings.Repeat("a", int(maxBodyBytes)+512))
	limitBody(inner).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", big))
	if readErr == nil {
		t.Error("expected oversized body to error")
	}
}
