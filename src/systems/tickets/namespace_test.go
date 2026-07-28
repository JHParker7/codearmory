package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTicketResource_NamespacedVsLegacy(t *testing.T) {
	if got := ticketResource("alice", "T1"); got != "alice/tickets/tickets/T1" {
		t.Errorf("got %q, want the owner-first form", got)
	}
	// A legacy row must keep the caller-scoped form, or gatekeeper would scope it to
	// the caller anyway and the check would silently mean something else.
	if got := ticketResource("", "T1"); got != "tickets/tickets/T1" {
		t.Errorf("got %q, want the legacy caller-scoped form", got)
	}
}

func TestCommentResource_NamespacedVsLegacy(t *testing.T) {
	if got := commentResource("alice", "T1", "C1"); got != "alice/tickets/tickets/T1/comments/C1" {
		t.Errorf("got %q, want the owner-first form", got)
	}
	if got := commentResource("", "T1", "C1"); got != "tickets/tickets/T1/comments/C1" {
		t.Errorf("got %q, want the legacy caller-scoped form", got)
	}
}

// This is the check that stops the namespace in the URL from being a free choice.
// Without it a caller could name their OWN namespace against someone else's ticket
// id: gatekeeper would authorize it, because they really do hold permission over
// their own namespace.
func TestNamespaceMatches_RejectsAForgedNamespace(t *testing.T) {
	if namespaceMatches("bob", "alice") {
		t.Error("a caller must not reach bob's ticket by naming their own namespace")
	}
	if !namespaceMatches("bob", "bob") {
		t.Error("the owner's own namespace must be accepted")
	}
	if !namespaceMatches("", "") {
		t.Error("legacy row on the legacy route must be accepted")
	}
	if namespaceMatches("", "alice") {
		t.Error("a legacy row must not be reachable by inventing a namespace for it")
	}
	// The legacy route stays open to a namespaced row on purpose. Rejecting it would
	// 404 every existing client the moment a ticket gained a namespace, because the
	// CLI and portal still address tickets by bare id. That route still evaluates the
	// old caller-scoped resource and still runs canAccessTicket, so nothing is weaker
	// than before the column existed.
	if !namespaceMatches("alice", "") {
		t.Error("the legacy route must keep working for a namespaced row during migration")
	}
}

func TestCallerNamespace_ResolvesPreferredUsername(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"email":"a@b.c","preferred_username":"alice"}`))
	}))
	defer srv.Close()

	oldURL, oldClient := gatekeeperURL, httpClient
	gatekeeperURL, httpClient = srv.URL, srv.Client()
	defer func() { gatekeeperURL, httpClient = oldURL, oldClient }()

	if got := callerNamespace(context.Background(), "Bearer tok"); got != "alice" {
		t.Errorf("got %q, want alice", got)
	}
}

// Every failure path must yield "" rather than an error: an unresolvable namespace
// means "record a legacy row", which is strictly no worse than before the column
// existed. Failing ticket creation over a gatekeeper hiccup would be worse.
func TestCallerNamespace_FailsSoftToEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	oldURL, oldClient := gatekeeperURL, httpClient
	gatekeeperURL, httpClient = srv.URL, srv.Client()
	defer func() { gatekeeperURL, httpClient = oldURL, oldClient }()

	if got := callerNamespace(context.Background(), "Bearer tok"); got != "" {
		t.Errorf("non-200 should yield %q, got %q", "", got)
	}
	if got := callerNamespace(context.Background(), ""); got != "" {
		t.Errorf("no bearer should yield %q, got %q", "", got)
	}
}

// A username containing "/" would let a crafted account forge extra resource
// segments, since the namespace is interpolated straight into the resource string.
func TestCallerNamespace_RejectsUsernameWithSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"preferred_username":"alice/tickets/tickets"}`))
	}))
	defer srv.Close()

	oldURL, oldClient := gatekeeperURL, httpClient
	gatekeeperURL, httpClient = srv.URL, srv.Client()
	defer func() { gatekeeperURL, httpClient = oldURL, oldClient }()

	if got := callerNamespace(context.Background(), "Bearer tok"); got != "" {
		t.Errorf("a username with a slash must be refused, got %q", got)
	}
}

// The whole two-route design rests on one behaviour: a handler shared by the legacy
// and namespaced patterns sees ns=="" when it was reached through the legacy one.
// This also asserts the pattern set does not panic — http.ServeMux rejects
// conflicting patterns at registration, which would take the service down at boot.
func TestRoutePatterns_LegacyAndNamespacedCoexist(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("route patterns conflict, the service would panic at startup: %v", p)
		}
	}()

	var gotNS, gotID, gotComment string
	record := func(w http.ResponseWriter, r *http.Request) {
		gotNS, gotID, gotComment = r.PathValue("ns"), r.PathValue("id"), r.PathValue("comment_id")
	}

	mux := http.NewServeMux()
	for _, p := range []string{
		"GET /tickets/{id}", "PUT /tickets/{id}", "DELETE /tickets/{id}",
		"GET /tickets/{ns}/{id}", "PUT /tickets/{ns}/{id}", "DELETE /tickets/{ns}/{id}",
		"POST /tickets/{id}/comments", "DELETE /tickets/{id}/comments/{comment_id}",
		"POST /tickets/{ns}/{id}/comments", "DELETE /tickets/{ns}/{id}/comments/{comment_id}",
	} {
		mux.HandleFunc(p, record)
	}

	cases := []struct{ method, path, ns, id, comment string }{
		{"GET", "/tickets/T1", "", "T1", ""},
		{"GET", "/tickets/alice/T1", "alice", "T1", ""},
		{"PUT", "/tickets/alice/T1", "alice", "T1", ""},
		{"DELETE", "/tickets/T1", "", "T1", ""},
		{"DELETE", "/tickets/alice/T1", "alice", "T1", ""},
		{"POST", "/tickets/T1/comments", "", "T1", ""},
		{"POST", "/tickets/alice/T1/comments", "alice", "T1", ""},
		{"DELETE", "/tickets/T1/comments/C1", "", "T1", "C1"},
		{"DELETE", "/tickets/alice/T1/comments/C1", "alice", "T1", "C1"},
	}
	for _, c := range cases {
		gotNS, gotID, gotComment = "unset", "unset", "unset"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s %s did not match any pattern", c.method, c.path)
			continue
		}
		if gotNS != c.ns || gotID != c.id || gotComment != c.comment {
			t.Errorf("%s %s -> ns=%q id=%q comment=%q, want ns=%q id=%q comment=%q",
				c.method, c.path, gotNS, gotID, gotComment, c.ns, c.id, c.comment)
		}
	}
}
