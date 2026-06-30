package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidRegistryURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://registry.example.com", true},
		{"http://localhost:5000", true},
		{"https://registry.example.com/v2", true},
		{"ftp://registry.example.com", false},
		{"registry.example.com", false}, // no scheme
		{"https://", false},             // no host
		{"", false},
	}
	for _, c := range cases {
		if got := validRegistryURL(c.in); got != c.want {
			t.Errorf("validRegistryURL(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRegistryNamePattern(t *testing.T) {
	valid := []string{"default", "my-registry", "reg_1", "Reg.2", "a", "ghcr-io"}
	for _, n := range valid {
		if !registryNamePattern.MatchString(n) {
			t.Errorf("expected %q to be a valid registry name", n)
		}
	}
	invalid := []string{"", "-leading", ".dot-lead", "has space", "has/slash", "with:colon"}
	for _, n := range invalid {
		if registryNamePattern.MatchString(n) {
			t.Errorf("expected %q to be an invalid registry name", n)
		}
	}
	// Over the 63-char limit.
	long := make([]byte, 64)
	for i := range long {
		long[i] = 'a'
	}
	if registryNamePattern.MatchString(string(long)) {
		t.Error("expected a 64-char name to be rejected")
	}
}

func TestNewRegistryClient_TrimsTrailingSlash(t *testing.T) {
	c := newRegistryClient(Registry{URL: "https://registry.example.com/", Username: "u", Password: "p"})
	if c.baseURL != "https://registry.example.com" {
		t.Fatalf("baseURL = %q, want trailing slash trimmed", c.baseURL)
	}
	if c.username != "u" || c.password != "p" {
		t.Fatalf("creds not carried through: user=%q pass=%q", c.username, c.password)
	}
	if c.http != httpClient {
		t.Fatal("client should share the shared httpClient")
	}
}

func TestResolveDefaultRegistry_CacheHit(t *testing.T) {
	want := &registryClient{baseURL: "https://cached.example.com", http: httpClient}
	useTestRegistry(t, want)
	got, err := resolveDefaultRegistry(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("resolveDefaultRegistry returned %v, want cached client %v", got, want)
	}
}

func TestInvalidateRegistryCache(t *testing.T) {
	defRegMu.Lock()
	defRegCache = &registryClient{baseURL: "x"}
	defRegExpires = time.Now().Add(time.Hour)
	defRegMu.Unlock()

	invalidateRegistryCache()

	defRegMu.RLock()
	defer defRegMu.RUnlock()
	if defRegCache != nil {
		t.Fatal("expected cache to be cleared")
	}
}

// ── admin API auth + validation (pre-DB paths) ──────────────────────────────

func TestHandleListRegistries_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/registries", nil)
	w := httptest.NewRecorder()
	handleListRegistries(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleCreateRegistry_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/registries", nil)
	w := httptest.NewRecorder()
	handleCreateRegistry(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleCreateRegistry_InvalidName(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":""}`)
	r := httptest.NewRequest(http.MethodPost, "/registries",
		strings.NewReader(`{"name":"bad name","url":"https://ok.example.com"}`))
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleCreateRegistry(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for invalid name", w.Code)
	}
}

func TestHandleCreateRegistry_InvalidURL(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":""}`)
	r := httptest.NewRequest(http.MethodPost, "/registries",
		strings.NewReader(`{"name":"ok","url":"not-a-url"}`))
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleCreateRegistry(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for invalid url", w.Code)
	}
}
