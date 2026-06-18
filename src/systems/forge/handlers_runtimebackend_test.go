package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ── pre-DB paths (no database required) ─────────────────────────────────────────

func TestHandleListRuntimeBackends_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/runtime-backends", nil)
	w := httptest.NewRecorder()
	handleListRuntimeBackends(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleCreateRuntimeBackend_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/runtime-backends", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	handleCreateRuntimeBackend(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleCreateRuntimeBackend_MissingName(t *testing.T) {
	authAs(t, "user-1")
	r := httptest.NewRequest(http.MethodPost, "/runtime-backends",
		bytes.NewBufferString(`{"type":"docker"}`))
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleCreateRuntimeBackend(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing name, got %d", w.Code)
	}
}

func TestHandleCreateRuntimeBackend_InvalidType(t *testing.T) {
	authAs(t, "user-1")
	r := httptest.NewRequest(http.MethodPost, "/runtime-backends",
		bytes.NewBufferString(`{"name":"x","type":"bogus"}`))
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleCreateRuntimeBackend(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid type, got %d", w.Code)
	}
}

func TestHandleDeleteRuntimeBackend_RefusesDefault(t *testing.T) {
	authAs(t, "user-1")
	reg := newRuntimeRegistry()
	r := httptest.NewRequest(http.MethodDelete, "/runtime-backends/default", nil)
	r.SetPathValue("name", "default")
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleDeleteRuntimeBackend(reg)(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 refusing to delete default, got %d", w.Code)
	}
}

func TestValidateRuntimeBackendBody(t *testing.T) {
	if err := validateRuntimeBackendBody(runtimeBackendBody{Type: "docker"}); err != nil {
		t.Errorf("docker should be valid: %v", err)
	}
	if err := validateRuntimeBackendBody(runtimeBackendBody{Type: "kubernetes"}); err != nil {
		t.Errorf("kubernetes should be valid: %v", err)
	}
	if err := validateRuntimeBackendBody(runtimeBackendBody{Type: "proxmox"}); err == nil {
		t.Error("proxmox should be rejected until the runtime is wired in")
	}
	if err := validateRuntimeBackendBody(runtimeBackendBody{Type: "kata"}); err == nil {
		t.Error("kata without a runtime_class should be rejected (it would silently run as runc, no VM isolation)")
	}
	if err := validateRuntimeBackendBody(runtimeBackendBody{Type: "kata", Config: map[string]string{"runtime_class": "kata-qemu"}}); err != nil {
		t.Errorf("kata with a runtime_class should be valid: %v", err)
	}
	if err := validateRuntimeBackendBody(runtimeBackendBody{Type: ""}); err == nil {
		t.Error("empty type should be rejected")
	}
}

// ── DB-backed CRUD roundtrip ────────────────────────────────────────────────────

func TestHandleRuntimeBackend_CRUD_DB(t *testing.T) {
	requireForgeDB(t)
	name := "be-" + uuid.New().String()
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM runtime_backends WHERE name = ?`, name) //nolint:errcheck
	})

	// Create with a secret_ref: the ref is a NAME, never a secret value.
	authAs(t, "admin")
	createBody := `{"name":"` + name + `","type":"docker","enabled":true,` +
		`"config":{"node":"pve1"},"secret_refs":{"token":"PROXMOX_TOKEN"}}`
	r := httptest.NewRequest(http.MethodPost, "/runtime-backends", bytes.NewBufferString(createBody))
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleCreateRuntimeBackend(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201: %s", w.Code, w.Body.String())
	}
	var created RuntimeBackend
	json.NewDecoder(w.Body).Decode(&created) //nolint:errcheck
	if created.SecretRefs["token"] != "PROXMOX_TOKEN" {
		t.Errorf("secret_refs = %v, want the env var NAME echoed back", created.SecretRefs)
	}
	// No secret VALUE must ever appear in the response.
	if strings.Contains(w.Body.String(), "actual-secret-value") {
		t.Error("response leaked a secret value")
	}

	// Get
	authAs(t, "admin")
	r = httptest.NewRequest(http.MethodGet, "/runtime-backends/"+name, nil)
	r.SetPathValue("name", name)
	r.Header.Set("Authorization", "Bearer t")
	w = httptest.NewRecorder()
	handleGetRuntimeBackend(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("get: got %d, want 200: %s", w.Code, w.Body.String())
	}
	var got RuntimeBackend
	json.NewDecoder(w.Body).Decode(&got) //nolint:errcheck
	if got.Config["node"] != "pve1" {
		t.Errorf("config = %v, want node=pve1 persisted", got.Config)
	}

	// Update — flip enabled and change config; registry must evict.
	reg := newRuntimeRegistry()
	reg.build = func(RuntimeBackend) (Runtime, error) { return &fakeRuntime{}, nil }
	if _, err := reg.Get(t.Context(), name); err != nil {
		t.Fatalf("warm cache: %v", err)
	}
	authAs(t, "admin")
	updBody := `{"type":"kubernetes","enabled":false,"config":{"namespace":"forge"}}`
	r = httptest.NewRequest(http.MethodPut, "/runtime-backends/"+name, bytes.NewBufferString(updBody))
	r.SetPathValue("name", name)
	r.Header.Set("Authorization", "Bearer t")
	w = httptest.NewRecorder()
	handleUpdateRuntimeBackend(reg)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("update: got %d, want 200: %s", w.Code, w.Body.String())
	}
	if _, ok := reg.cache[name]; ok {
		t.Error("expected the registry to evict the cached runtime after update")
	}

	// List includes it.
	authAs(t, "admin")
	r = httptest.NewRequest(http.MethodGet, "/runtime-backends", nil)
	r.Header.Set("Authorization", "Bearer t")
	w = httptest.NewRecorder()
	handleListRuntimeBackends(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list: got %d, want 200", w.Code)
	}
	var list []RuntimeBackend
	json.NewDecoder(w.Body).Decode(&list) //nolint:errcheck
	found := false
	for _, b := range list {
		if b.Name == name {
			found = true
		}
	}
	if !found {
		t.Errorf("list did not include %q", name)
	}

	// Delete
	authAs(t, "admin")
	r = httptest.NewRequest(http.MethodDelete, "/runtime-backends/"+name, nil)
	r.SetPathValue("name", name)
	r.Header.Set("Authorization", "Bearer t")
	w = httptest.NewRecorder()
	handleDeleteRuntimeBackend(reg)(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204: %s", w.Code, w.Body.String())
	}
}

func TestHandleGetRuntimeBackend_NotFound_DB(t *testing.T) {
	requireForgeDB(t)
	authAs(t, "admin")
	r := httptest.NewRequest(http.MethodGet, "/runtime-backends/no-such", nil)
	r.SetPathValue("name", "no-such-"+uuid.New().String())
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleGetRuntimeBackend(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}
