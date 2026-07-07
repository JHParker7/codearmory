package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxy_ForwardsMethodAuthBodyAndMirrors(t *testing.T) {
	var gotMethod, gotAuth, gotBody string
	ts := withConductor(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	_ = ts

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/runs", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	if err := proxyToUpstream(rr, req, "/workflows/runs"); err != nil {
		t.Fatalf("proxy error: %v", err)
	}
	if gotMethod != http.MethodPost || gotAuth != "Bearer tok" || gotBody != `{"name":"x"}` {
		t.Fatalf("upstream did not receive forwarded request: method=%s auth=%s body=%s", gotMethod, gotAuth, gotBody)
	}
	if rr.Code != http.StatusCreated || rr.Body.String() != `{"ok":true}` {
		t.Fatalf("response not mirrored: %d %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected json content type, got %q", ct)
	}
}

func TestProxy_EmptyBodyNoContentType(t *testing.T) {
	withConductor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rr := httptest.NewRecorder()
	if err := proxyToUpstream(rr, req, "/health"); err != nil {
		t.Fatalf("proxy error: %v", err)
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "" {
		t.Fatalf("expected no content type on empty body, got %q", ct)
	}
}
