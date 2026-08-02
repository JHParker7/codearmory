package main

// Executable specs for three known defects in the create path, each RED until the
// named fix lands. They exist so the bugs can't silently persist: the rest of the
// create tests pass only because the gatekeeper stub collapses user_id, username,
// and org_name to the same string, which hides these. Each test below deliberately
// makes those values differ (or inspects the derived URL) so the defect shows.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// newGatekeeperStubDistinct authorises as userID but resolves GET /users/{id} to a
// DIFFERENT username — the realistic case (user_id is a UUID, username is a handle).
// This is what separates the authz key from the display name so the ownership bug
// below becomes visible.
func newGatekeeperStubDistinct(t *testing.T, userID, username string) {
	t.Helper()
	withFullGatekeeper(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/check_permissions":
			writeJSON(w, http.StatusOK, map[string]any{"authorized": true, "user_id": userID})
		case strings.HasPrefix(r.URL.Path, "/users/"):
			writeJSON(w, http.StatusOK, map[string]any{"user_id": userID, "username": username, "active": true})
		default:
			http.NotFound(w, r)
		}
	})
}

// RED until the Owner/Namespace split: create must store Owner = the gatekeeper
// user_id (the authz filter every other handler uses) and put the resolved
// username in Namespace. Today the handler stores the username in Owner
// (api_repo.go:92), so when user_id != username the owner-scoped lookups can never
// find the row — a user creates a repo they can't list, get, or delete.
// Fix: api_repo.go handleCreateRepo (Owner=userID, Namespace=resolved name) and
// db.go (drop `re.Namespace = re.Owner`).
func TestHandleCreateRepo_OwnerIsUserIDNotUsername(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStubDistinct(t, "user-1", "alice") // user_id != username

	rec := postCreateRepo(t, map[string]any{"name": "widgets"}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, _ := got["id"].(string)

	// Authz: the row must be scoped to the gatekeeper user_id, so a userID-scoped
	// lookup finds it. With the bug, Owner holds "alice" and this returns not-found.
	if _, err := getRepo(context.Background(), "user-1", id); err != nil {
		t.Errorf("getRepo(user_id=user-1) = %v; the repo must be owned by the gatekeeper "+
			"user_id, but create stored the username in Owner instead", err)
	}
	// Display: the namespace should carry the resolved username.
	if got["namespace"] != "alice" {
		t.Errorf("namespace = %v, want alice (the resolved username)", got["namespace"])
	}
}

// RED until name validation uses the [A-Za-z0-9._-]+ allowlist (ARCHITECTURE §6):
// a dotted name like "my.repo" is valid, but the current ad-hoc Contains(".")
// check rejects it (api_repo.go:98). Fix: replace the Contains checks with a single
// anchored allowlist regex that permits dots yet still rejects a ".git" name.
func TestHandleCreateRepo_AllowsDottedName(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	rec := postCreateRepo(t, map[string]any{"name": "my.repo"}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d — a dotted name is valid per [A-Za-z0-9._-]+ (body %q)",
			rec.Code, http.StatusCreated, rec.Body.String())
	}
}

// RED until http_url is built from a real base: db.go hardcodes the literal
// "http://<host>/" placeholder (db.go:80), so every clone URL is unusable.
// Fix: db.go — build HttpUrl from a configured host (e.g. GIT_HTTP_BASE_URL).
func TestHandleCreateRepo_HTTPURLHasRealHost(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	rec := postCreateRepo(t, map[string]any{"name": "widgets"}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if httpURL, _ := got["http_url"].(string); strings.Contains(httpURL, "<host>") {
		t.Errorf("http_url = %q still contains the \"<host>\" placeholder — it must be a real base URL", httpURL)
	}
}
