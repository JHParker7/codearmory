package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withBuilderInternalKey(t *testing.T, key string) {
	t.Helper()
	orig := builderInternalKey
	builderInternalKey = key
	t.Cleanup(func() { builderInternalKey = orig })
}

func TestRegisterServiceAccount_RegistersAndAuthenticates(t *testing.T) {
	withBuilderInternalKey(t, "internal-key")

	body := `{"service_name":"dyn-svc","key":"svc-bootstrap-key"}`
	r := httptest.NewRequest(http.MethodPost, "/internal/service-accounts", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer internal-key")
	w := httptest.NewRecorder()
	handleRegisterServiceAccount(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("register: got %d, want 204", w.Code)
	}

	// The registered service must now authenticate with its bootstrap key, proving
	// it can come online without being in GATEKEEPER_SERVICES.
	ar := httptest.NewRequest(http.MethodPost, "/x", nil)
	ar.Header.Set("X-Service-Key", "dyn-svc:svc-bootstrap-key")
	aw := httptest.NewRecorder()
	if _, ok := requireServiceAuth(aw, ar); !ok {
		t.Fatalf("registered service failed to authenticate (status %d)", aw.Code)
	}
}

func TestRegisterServiceAccount_Unauthorized(t *testing.T) {
	withBuilderInternalKey(t, "internal-key")
	r := httptest.NewRequest(http.MethodPost, "/internal/service-accounts", strings.NewReader(`{"service_name":"x","key":"y"}`))
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	handleRegisterServiceAccount(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestRegisterServiceAccount_DisabledWhenNoKey(t *testing.T) {
	withBuilderInternalKey(t, "")
	r := httptest.NewRequest(http.MethodPost, "/internal/service-accounts", strings.NewReader(`{"service_name":"x","key":"y"}`))
	r.Header.Set("Authorization", "Bearer anything")
	w := httptest.NewRecorder()
	handleRegisterServiceAccount(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 (gate inert)", w.Code)
	}
}

func TestRegisterServiceAccount_MissingFields(t *testing.T) {
	withBuilderInternalKey(t, "internal-key")
	r := httptest.NewRequest(http.MethodPost, "/internal/service-accounts", strings.NewReader(`{"service_name":"x"}`))
	r.Header.Set("Authorization", "Bearer internal-key")
	w := httptest.NewRecorder()
	handleRegisterServiceAccount(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestDeregisterServiceAccount(t *testing.T) {
	withBuilderInternalKey(t, "internal-key")
	// register first
	r := httptest.NewRequest(http.MethodPost, "/internal/service-accounts", strings.NewReader(`{"service_name":"temp-svc","key":"k"}`))
	r.Header.Set("Authorization", "Bearer internal-key")
	handleRegisterServiceAccount(httptest.NewRecorder(), r)

	dr := httptest.NewRequest(http.MethodDelete, "/internal/service-accounts/temp-svc", nil)
	dr.Header.Set("Authorization", "Bearer internal-key")
	dr.SetPathValue("name", "temp-svc")
	dw := httptest.NewRecorder()
	handleDeregisterServiceAccount(dw, dr)
	if dw.Code != http.StatusNoContent {
		t.Fatalf("deregister: got %d, want 204", dw.Code)
	}
	// the account should no longer be retrievable as active
	if _, err := (ServiceAccount{ServiceName: "temp-svc"}).Get(context.Background()); err == nil {
		t.Fatal("expected the deregistered account to be inactive")
	}
}
