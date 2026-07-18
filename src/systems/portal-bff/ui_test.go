package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServiceUI_StreamsVerbatimContentType(t *testing.T) {
	withConductor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blueprints/ui/app.js" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = w.Write([]byte("console.log(1)"))
	})
	req := httptest.NewRequest(http.MethodGet, "/api/blueprints/ui/app.js", nil)
	rr := httptest.NewRecorder()
	handleServiceUI(rr, req)
	if ct := rr.Header().Get("Content-Type"); ct != "application/javascript" {
		t.Fatalf("content type not passed through: %q", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "max-age=3600" {
		t.Fatalf("cache-control not passed through: %q", cc)
	}
	if rr.Body.String() != "console.log(1)" {
		t.Fatalf("body not streamed verbatim: %q", rr.Body.String())
	}
}

func TestServiceUI_InvalidServiceRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/bad$svc/ui/x", nil)
	rr := httptest.NewRecorder()
	handleServiceUI(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid service, got %d", rr.Code)
	}
}

func TestIsServiceUIPath(t *testing.T) {
	cases := map[string]bool{
		"/blueprints/ui":        true,
		"/blueprints/ui/app.js": true,
		"/blueprints/runs":      false,
		"/state/alice/prod":     false,
		"/ui":                   false,
	}
	for path, want := range cases {
		if got := isServiceUIPath(path); got != want {
			t.Errorf("isServiceUIPath(%q)=%v want %v", path, got, want)
		}
	}
}
