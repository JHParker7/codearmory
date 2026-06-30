package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestResolveUsername_ResolvesAndFailsOpen checks that the approval audit line's
// approver is shown by username when gatekeeper resolves it, and falls back to the
// raw user id on any error (bad status, no username, empty inputs) so an audit
// line is never lost.
func TestResolveUsername_ResolvesAndFailsOpen(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users/u-alice":
			w.Write([]byte(`{"user_id":"u-alice","username":"alice"}`)) //nolint:errcheck
		case "/users/u-noname":
			w.Write([]byte(`{"user_id":"u-noname"}`)) //nolint:errcheck — no username field
		default:
			http.Error(w, "forbidden", http.StatusForbidden)
		}
	}))
	defer srv.Close()
	orig := gatekeeperURL
	gatekeeperURL = srv.URL
	t.Cleanup(func() { gatekeeperURL = orig })

	if got := resolveUsername(context.Background(), "Bearer jwt", "u-alice"); got != "alice" {
		t.Errorf("resolveUsername(u-alice) = %q, want alice", got)
	}
	if gotAuth != "Bearer jwt" || gotPath != "/users/u-alice" {
		t.Errorf("forwarded auth=%q path=%q, want Bearer jwt /users/u-alice", gotAuth, gotPath)
	}
	// Fail-open paths all return the input id unchanged.
	if got := resolveUsername(context.Background(), "Bearer jwt", "u-noname"); got != "u-noname" {
		t.Errorf("no username field: got %q, want u-noname", got)
	}
	if got := resolveUsername(context.Background(), "Bearer jwt", "u-forbidden"); got != "u-forbidden" {
		t.Errorf("403: got %q, want u-forbidden", got)
	}
	if got := resolveUsername(context.Background(), "", "u-alice"); got != "u-alice" {
		t.Errorf("empty bearer: got %q, want u-alice (no lookup)", got)
	}
	if got := resolveUsername(context.Background(), "Bearer jwt", ""); got != "" {
		t.Errorf("empty id: got %q, want empty", got)
	}
}
