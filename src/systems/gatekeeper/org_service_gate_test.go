package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withBuilderGate points the disable-gate at a stub builder and restores the
// package vars afterwards. An empty url leaves the gate inert.
func withBuilderGate(t *testing.T, url, key string) {
	t.Helper()
	ou, ok := builderURL, builderInternalKey
	builderURL, builderInternalKey = url, key
	t.Cleanup(func() { builderURL, builderInternalKey = ou, ok })
}

func stubBuilder(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestServiceDisabledForOrg_InertWhenUnconfigured(t *testing.T) {
	withBuilderGate(t, "", "")
	if serviceDisabledForOrg(context.Background(), "org1", "forge") {
		t.Fatal("gate must be inert (allow) when BUILDER_URL is unset")
	}
}

func TestServiceDisabledForOrg_CoreNeverGated(t *testing.T) {
	// Even with a builder that would report it disabled, core services are allowed.
	url := stubBuilder(t, http.StatusOK, `{"disabled":["gatekeeper"]}`)
	withBuilderGate(t, url, "k")
	if serviceDisabledForOrg(context.Background(), "org1", "gatekeeper") {
		t.Fatal("core services must never be gated")
	}
}

func TestServiceDisabledForOrg_Disabled(t *testing.T) {
	url := stubBuilder(t, http.StatusOK, `{"disabled":["forge"]}`)
	withBuilderGate(t, url, "k")
	if !serviceDisabledForOrg(context.Background(), "org1", "forge") {
		t.Fatal("forge should be reported disabled")
	}
	if serviceDisabledForOrg(context.Background(), "org1", "workflows") {
		t.Fatal("workflows is not in the disabled set; must be allowed")
	}
}

func TestServiceDisabledForOrg_FailOpenOnError(t *testing.T) {
	url := stubBuilder(t, http.StatusInternalServerError, ``)
	withBuilderGate(t, url, "k")
	if serviceDisabledForOrg(context.Background(), "org1", "forge") {
		t.Fatal("a builder error must fail open (allow)")
	}
}
