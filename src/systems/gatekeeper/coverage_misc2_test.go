package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandleRotateServiceKey(t *testing.T) {
	key := seedNamedSvc(t, "rotate-svc")
	r := httptest.NewRequest(http.MethodPost, "/service-accounts/rotate-key", nil)
	r.Header.Set("X-Service-Key", "rotate-svc:"+key)
	w := httptest.NewRecorder()
	handleRotateServiceKey(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate got %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["key"] == "" {
		t.Error("expected a new key in response")
	}
	// Unauthenticated rotate → 401.
	w2 := httptest.NewRecorder()
	handleRotateServiceKey(w2, httptest.NewRequest(http.MethodPost, "/service-accounts/rotate-key", nil))
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("no key got %d, want 401", w2.Code)
	}
}

func TestHandleAuthValidate(t *testing.T) {
	u := seedTestUser(t)
	r := withUserID(httptest.NewRequest(http.MethodGet, "/auth/validate", nil), u.UserID)
	w := httptest.NewRecorder()
	handleAuthValidate(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("validate got %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestFetchDefaultGrants(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`)) //nolint:errcheck
	}))
	defer srv.Close()
	if _, err := fetchDefaultGrants(context.Background(), srv.URL, "gatekeeper:key"); err != nil {
		t.Fatalf("fetchDefaultGrants: %v", err)
	}
	// Non-200 → error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	if _, err := fetchDefaultGrants(context.Background(), bad.URL, "gatekeeper:key"); err == nil {
		t.Error("expected error on non-200")
	}
}

func TestCacheFunctions_NoRedis(t *testing.T) {
	t.Setenv("REDIS_URL", "")
	initCache()
	ctx := context.Background()
	// Without Redis these are in-process/no-op; just exercise them.
	cacheSet(ctx, "k1", "v1", time.Minute)
	_, _ = cacheGet[string](ctx, "k1")
	cacheTrackUserSession(ctx, "u1", "s1")
	cacheDelUserSessions(ctx, "u1")
}
