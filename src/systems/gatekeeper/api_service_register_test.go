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

// builder re-registers every managed service on every reconcile tick, so an
// unchanged registration must write nothing: no row, and above all no audit entry.
// Otherwise the trail becomes a log of polling and buries the entries someone is
// actually looking for.
func TestRegisterServiceAccount_UnchangedKeyIsNotAudited(t *testing.T) {
	withBuilderInternalKey(t, "internal-key")

	register := func() int {
		body := `{"service_name":"quiet-svc","key":"quiet-bootstrap-key"}`
		r := httptest.NewRequest(http.MethodPost, "/internal/service-accounts", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer internal-key")
		w := httptest.NewRecorder()
		handleRegisterServiceAccount(w, r)
		return w.Code
	}
	countRegisterEntries := func() int {
		rows, err := AuditLog{ResourceID: "quiet-svc", Action: "service_account.register"}.List(context.Background(), 0, 0)
		if err != nil {
			t.Fatalf("list audit logs: %v", err)
		}
		return len(rows)
	}

	if code := register(); code != http.StatusNoContent {
		t.Fatalf("first register: got %d, want 204", code)
	}
	first := countRegisterEntries()
	if first != 1 {
		t.Fatalf("after one registration there are %d audit entries, want 1", first)
	}

	for i := 0; i < 3; i++ {
		if code := register(); code != http.StatusNoContent {
			t.Fatalf("repeat register %d: got %d, want 204", i, code)
		}
	}
	if got := countRegisterEntries(); got != first {
		t.Errorf("after 3 unchanged re-registrations there are %d audit entries, want %d", got, first)
	}

	// The service still authenticates — the no-op must not have disturbed anything.
	ar := httptest.NewRequest(http.MethodPost, "/x", nil)
	ar.Header.Set("X-Service-Key", "quiet-svc:quiet-bootstrap-key")
	aw := httptest.NewRecorder()
	if _, ok := requireServiceAuth(aw, ar); !ok {
		t.Fatalf("service failed to authenticate after re-registration (status %d)", aw.Code)
	}
}

// A CHANGED key is a real event and must still be recorded — that is the entry which
// says "this service's credential was replaced".
func TestRegisterServiceAccount_ChangedKeyIsAudited(t *testing.T) {
	withBuilderInternalKey(t, "internal-key")

	post := func(key string) {
		t.Helper()
		body := `{"service_name":"rekeyed-svc","key":"` + key + `"}`
		r := httptest.NewRequest(http.MethodPost, "/internal/service-accounts", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer internal-key")
		w := httptest.NewRecorder()
		handleRegisterServiceAccount(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("register: got %d, want 204", w.Code)
		}
	}
	post("first-key")
	post("second-key")

	rows, err := AuditLog{ResourceID: "rekeyed-svc", Action: "service_account.register"}.List(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("list audit logs: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("audit entries = %d, want 2 (one per real key change)", len(rows))
	}
}
