package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// gitSecret is the core git service's Secret, whose git-internal-key builder reads to
// authenticate the registration call.
func gitSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "codearmory-git", Namespace: "codearmory"},
		Data:       map[string][]byte{"git-internal-key": []byte("git-internal")},
	}
}

type linkRecorder struct {
	mu     sync.Mutex
	calls  int
	body   map[string]string
	keyHdr string
}

func newStubGitConnector(t *testing.T, rec *linkRecorder, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/backends/platform" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		rec.mu.Lock()
		rec.calls++
		rec.body = body
		rec.keyHdr = r.Header.Get("X-Internal-Key")
		rec.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestEnsureGitConnectorBackend_RegistersInClusterURL(t *testing.T) {
	httpClient = initHTTPClient()
	rec := &linkRecorder{}
	prev := gitConnectorURL
	gitConnectorURL = newStubGitConnector(t, rec, http.StatusOK)
	defer func() { gitConnectorURL = prev }()

	b := newTestBackend(t, &registerRecorder{}, gitSecret())
	if err := b.ensureGitConnectorBackend(context.Background(), "codearmory_git_factory", svcGitBackend{Name: "git-factory"}); err != nil {
		t.Fatalf("ensureGitConnectorBackend: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("calls=%d want 1", rec.calls)
	}
	if rec.keyHdr != "git-internal" {
		t.Fatalf("internal key header=%q — must come from git's own Secret", rec.keyHdr)
	}
	if rec.body["name"] != "git-factory" {
		t.Fatalf("name=%q", rec.body["name"])
	}
	// The k8s Service name and the port from the embedded def — never an external
	// ingress, which a sandboxed runner's egress policy would block.
	if got := rec.body["base_url"]; got != "http://codearmory-git-factory:9002" {
		t.Fatalf("base_url=%q", got)
	}
	// Nothing resembling a credential is ever sent: the backend has none.
	for k := range rec.body {
		if k != "name" && k != "base_url" {
			t.Fatalf("unexpected field %q in registration body", k)
		}
	}
}

// Repeated reconcile passes must keep converging on the same registration.
func TestEnsureGitConnectorBackend_Idempotent(t *testing.T) {
	httpClient = initHTTPClient()
	rec := &linkRecorder{}
	prev := gitConnectorURL
	gitConnectorURL = newStubGitConnector(t, rec, http.StatusOK)
	defer func() { gitConnectorURL = prev }()

	b := newTestBackend(t, &registerRecorder{}, gitSecret())
	for i := 0; i < 3; i++ {
		if err := b.ensureGitConnectorBackend(context.Background(), "codearmory_git_factory", svcGitBackend{Name: "git-factory"}); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if rec.calls != 3 {
		t.Fatalf("calls=%d want 3 (upsert, not create-once)", rec.calls)
	}
}

// With no git Secret there is no git_connector to link to; that is a no-op, not an
// error, so a deployment without the git service still reconciles cleanly.
func TestEnsureGitConnectorBackend_NoGitSecret(t *testing.T) {
	httpClient = initHTTPClient()
	rec := &linkRecorder{}
	prev := gitConnectorURL
	gitConnectorURL = newStubGitConnector(t, rec, http.StatusOK)
	defer func() { gitConnectorURL = prev }()

	b := newTestBackend(t, &registerRecorder{})
	if err := b.ensureGitConnectorBackend(context.Background(), "codearmory_git_factory", svcGitBackend{Name: "git-factory"}); err != nil {
		t.Fatalf("expected a clean no-op, got %v", err)
	}
	if rec.calls != 0 {
		t.Fatalf("calls=%d want 0", rec.calls)
	}
}

// A git_connector error surfaces so ensureInfra can log it and retry next pass.
func TestEnsureGitConnectorBackend_PropagatesError(t *testing.T) {
	httpClient = initHTTPClient()
	rec := &linkRecorder{}
	prev := gitConnectorURL
	gitConnectorURL = newStubGitConnector(t, rec, http.StatusInternalServerError)
	defer func() { gitConnectorURL = prev }()

	b := newTestBackend(t, &registerRecorder{}, gitSecret())
	if err := b.ensureGitConnectorBackend(context.Background(), "codearmory_git_factory", svcGitBackend{Name: "git-factory"}); err == nil {
		t.Fatal("expected an error so the reconciler retries")
	}
}

// The embedded def is what drives the whole thing — if the declaration is dropped,
// builder silently stops linking git-factory.
func TestGitFactoryDefDeclaresGitConnectorBackend(t *testing.T) {
	d, ok := embeddedServiceDef("codearmory_git_factory")
	if !ok {
		t.Fatal("git_factory def missing")
	}
	if d.GitConnectorBackend == nil {
		t.Fatal("git_factory must declare gitConnectorBackend so builder links it automatically")
	}
	if d.GitConnectorBackend.Name != "git-factory" {
		t.Fatalf("backend name=%q", d.GitConnectorBackend.Name)
	}
}
