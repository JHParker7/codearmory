package main

// Unit spec for the gatekeeper identity resolver (gatekeeper_identity.go). These
// tests stand up a local httptest server, point the gatekeeperURL/httpClient
// globals at it, and exercise getOrg/getUser/gatekeeperGet directly — no DB, no
// full handler. They cover the happy path (request shape + decode) and every
// status-code branch gatekeeperGet maps onto an error.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gk "github.com/code-armory-app/codearmory_sdk/gatekeeper"
)

// withGatekeeperServer routes gatekeeperURL/httpClient at a stub serving h, and
// restores the previous globals on cleanup.
func withGatekeeperServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	prevURL, prevHTTP := gatekeeperURL, httpClient
	gatekeeperURL = srv.URL
	httpClient = srv.Client()
	t.Cleanup(func() {
		srv.Close()
		gatekeeperURL = prevURL
		httpClient = prevHTTP
	})
}

// getOrg must GET /orgs/{id} with the caller's Bearer token and decode every field.
func TestGetOrg_Success(t *testing.T) {
	var gotPath, gotAuth string
	withGatekeeperServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, map[string]any{
			"org_id": "org-9", "org_name": "acme", "owner_id": "user-1", "active": true,
		})
	})

	org, err := getOrg(context.Background(), "tok-123", "org-9")
	if err != nil {
		t.Fatalf("getOrg: %v", err)
	}
	if gotPath != "/orgs/org-9" {
		t.Errorf("requested path = %q, want /orgs/org-9", gotPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok-123")
	}
	if org.OrgID != "org-9" || org.OrgName != "acme" || org.OwnerID != "user-1" || !org.Active {
		t.Errorf("decoded org = %+v, want {OrgID:org-9 OrgName:acme OwnerID:user-1 Active:true}", org)
	}
}

// getUser must GET /users/{id} with the Bearer token and decode the safe user shape.
func TestGetUser_Success(t *testing.T) {
	var gotPath, gotAuth string
	withGatekeeperServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, map[string]any{
			"user_id": "user-1", "username": "alice", "active": true,
		})
	})

	user, err := getUser(context.Background(), "tok-123", "user-1")
	if err != nil {
		t.Fatalf("getUser: %v", err)
	}
	if gotPath != "/users/user-1" {
		t.Errorf("requested path = %q, want /users/user-1", gotPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok-123")
	}
	if user.UserID != "user-1" || user.Username != "alice" || !user.Active {
		t.Errorf("decoded user = %+v, want {UserID:user-1 Username:alice Active:true}", user)
	}
}

// gatekeeperGet maps non-200 statuses onto errors; the resolver returns a zero
// value alongside the error. One case per branch of the status switch.
func TestGatekeeperGet_MapsErrorStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"not found", http.StatusNotFound},
		{"unauthorized", http.StatusUnauthorized},
		{"forbidden", http.StatusForbidden},
		{"server error", http.StatusInternalServerError},
		{"unexpected", http.StatusTeapot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withGatekeeperServer(t, func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "nope", tc.status)
			})

			user, err := getUser(context.Background(), "tok", "user-1")
			if err == nil {
				t.Fatalf("status %d: expected an error, got nil", tc.status)
			}
			if user != (User{}) {
				t.Errorf("status %d: expected zero User on error, got %+v", tc.status, user)
			}
		})
	}
}

// A 200 with an undecodable body is an error, not a silently-zero result.
func TestGatekeeperGet_BadJSON(t *testing.T) {
	withGatekeeperServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("this is not json"))
	})

	_, err := getUser(context.Background(), "tok", "user-1")
	if err == nil {
		t.Fatal("expected a decode error for a non-JSON 200 body, got nil")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %q, want it to mention a decode failure", err)
	}
}

// A transport-level failure (connection refused) surfaces as an error rather than
// panicking or returning a zero value with nil error.
func TestGatekeeperGet_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	prevURL, prevHTTP := gatekeeperURL, httpClient
	gatekeeperURL = srv.URL
	httpClient = srv.Client()
	t.Cleanup(func() { gatekeeperURL = prevURL; httpClient = prevHTTP })
	srv.Close() // close before the call so the request cannot connect

	_, err := getOrg(context.Background(), "tok", "org-9")
	if err == nil {
		t.Fatal("expected a transport error against a closed server, got nil")
	}
}

// withFullGatekeeper points ALL three gatekeeper globals at a single stub serving
// h: the SDK client used by CheckPermissions, plus the raw gatekeeperURL/httpClient
// used by getUser/getOrg. Handler-level create tests need this because
// handleCreateRepo touches both paths. Globals are restored on cleanup.
func withFullGatekeeper(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	prevClient, prevURL, prevHTTP := gatekeeperClient, gatekeeperURL, httpClient
	gatekeeperClient = &gk.Client{URL: srv.URL, Service: serviceName, HTTPClient: srv.Client()}
	gatekeeperURL = srv.URL
	httpClient = srv.Client()
	t.Cleanup(func() {
		srv.Close()
		gatekeeperClient = prevClient
		gatekeeperURL = prevURL
		httpClient = prevHTTP
	})
}

// newGatekeeperStubWithOrg is like newGatekeeperStub but check_permissions also
// returns an org_id, and /orgs/{id} resolves to a fixed org name — so the org
// branch of handleCreateRepo (req.AssignToOrg) can be exercised end to end.
func newGatekeeperStubWithOrg(t *testing.T, userID, orgID, orgName string) {
	t.Helper()
	withFullGatekeeper(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/check_permissions":
			writeJSON(w, http.StatusOK, map[string]any{"authorized": true, "user_id": userID, "org_id": orgID})
		case strings.HasPrefix(r.URL.Path, "/orgs/"):
			writeJSON(w, http.StatusOK, map[string]any{
				"org_id": strings.TrimPrefix(r.URL.Path, "/orgs/"), "org_name": orgName, "active": true,
			})
		default:
			http.NotFound(w, r)
		}
	})
}

// The org branch: with org_repo=true the namespace is resolved from the org, so
// the created repo's namespace and clone URL carry the org name, not the caller's
// username. This is the path getOrg exists for and was previously untested.
func TestHandleCreateRepo_OrgRepo(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStubWithOrg(t, "user-1", "org-9", "acme")

	rec := postCreateRepo(t, map[string]any{"name": "widgets", "org_repo": true}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["namespace"] != "acme" {
		t.Errorf("namespace = %v, want acme (resolved from the org)", got["namespace"])
	}
	if httpURL, _ := got["http_url"].(string); !strings.HasSuffix(httpURL, "/acme/widgets.git") {
		t.Errorf("http_url = %q, want it to end in /acme/widgets.git", httpURL)
	}
}

// ── handleCreateRepo error paths ────────────────────────────────────────────

// A malformed JSON body is rejected with 400, before any identity resolution.
func TestHandleCreateRepo_InvalidBody(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	withFullGatekeeper(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/check_permissions" {
			writeJSON(w, http.StatusOK, map[string]any{"authorized": true, "user_id": "user-1"})
			return
		}
		http.NotFound(w, r)
	})

	req := httptest.NewRequest(http.MethodPost, "/repos", bytes.NewReader([]byte("{not valid json")))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	handleCreateRepo(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d for a malformed body", rec.Code, http.StatusBadRequest)
	}
}

// When gatekeeper cannot resolve the caller's user (5xx), create fails with 400
// instead of persisting a repo with an empty owner/namespace.
func TestHandleCreateRepo_UserResolveFails(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	withFullGatekeeper(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/check_permissions":
			writeJSON(w, http.StatusOK, map[string]any{"authorized": true, "user_id": "user-1"})
		case strings.HasPrefix(r.URL.Path, "/users/"):
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})

	rec := postCreateRepo(t, map[string]any{"name": "widgets"}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d when the user lookup fails", rec.Code, http.StatusBadRequest)
	}
}

// The org branch mirrors the user branch: an unresolvable org is a 400, not a
// partial create.
func TestHandleCreateRepo_OrgResolveFails(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	withFullGatekeeper(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/check_permissions":
			writeJSON(w, http.StatusOK, map[string]any{"authorized": true, "user_id": "user-1", "org_id": "org-9"})
		case strings.HasPrefix(r.URL.Path, "/orgs/"):
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})

	rec := postCreateRepo(t, map[string]any{"name": "widgets", "org_repo": true}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d when the org lookup fails", rec.Code, http.StatusBadRequest)
	}
}
