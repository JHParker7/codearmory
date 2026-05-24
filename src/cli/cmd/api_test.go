package cmd

import (
	"net/http"
	"strings"
	"testing"
)

// apiRouteTest describes one expected HTTP interaction for an apiCall invocation.
type apiRouteTest struct {
	name       string
	callFn     func() error    // the function under test
	wantMethod string
	wantPath   string
	wantBody   string // substring that must appear in the request body (empty = skip)
}

// allRouteTests covers every resource endpoint that routes through apiCall.
// For each test a fresh recording server is spun up, so the pointer in rec is
// valid only for that sub-test.
var allRouteTests = []apiRouteTest{
	// ── Users ────────────────────────────────────────────────────────────────
	{
		name: "users/list",
		callFn: func() error {
			return apiCall("GET", "/users", nil)
		},
		wantMethod: "GET", wantPath: "/users",
	},
	{
		name: "users/get",
		callFn: func() error {
			return apiCall("GET", "/users/user-id-123", nil)
		},
		wantMethod: "GET", wantPath: "/users/user-id-123",
	},
	{
		name: "users/update",
		callFn: func() error {
			return apiCall("PUT", "/users/user-id-123", jsonBody(map[string]string{"username": "newname"}))
		},
		wantMethod: "PUT", wantPath: "/users/user-id-123", wantBody: `"username"`,
	},
	{
		name: "users/delete",
		callFn: func() error {
			return apiCall("DELETE", "/users/user-id-123", nil)
		},
		wantMethod: "DELETE", wantPath: "/users/user-id-123",
	},

	// ── Orgs ─────────────────────────────────────────────────────────────────
	{
		name: "orgs/list",
		callFn: func() error {
			return apiCall("GET", "/orgs", nil)
		},
		wantMethod: "GET", wantPath: "/orgs",
	},
	{
		name: "orgs/create",
		callFn: func() error {
			return apiCall("POST", "/orgs", jsonBody(map[string]string{"name": "myorg"}))
		},
		wantMethod: "POST", wantPath: "/orgs", wantBody: `"myorg"`,
	},
	{
		name: "orgs/get",
		callFn: func() error {
			return apiCall("GET", "/orgs/org-id", nil)
		},
		wantMethod: "GET", wantPath: "/orgs/org-id",
	},
	{
		name: "orgs/update",
		callFn: func() error {
			return apiCall("PUT", "/orgs/org-id", jsonBody(map[string]string{"name": "renamed"}))
		},
		wantMethod: "PUT", wantPath: "/orgs/org-id", wantBody: `"renamed"`,
	},
	{
		name: "orgs/delete",
		callFn: func() error {
			return apiCall("DELETE", "/orgs/org-id", nil)
		},
		wantMethod: "DELETE", wantPath: "/orgs/org-id",
	},
	{
		name: "orgs/invite",
		callFn: func() error {
			return apiCall("POST", "/orgs/org-id/invites", jsonBody(map[string]string{"user_id": "u1"}))
		},
		wantMethod: "POST", wantPath: "/orgs/org-id/invites", wantBody: `"user_id"`,
	},

	// ── Teams ─────────────────────────────────────────────────────────────────
	{
		name: "teams/list",
		callFn: func() error {
			return apiCall("GET", "/teams", nil)
		},
		wantMethod: "GET", wantPath: "/teams",
	},
	{
		name: "teams/create",
		callFn: func() error {
			return apiCall("POST", "/teams", jsonBody(map[string]string{"name": "backend"}))
		},
		wantMethod: "POST", wantPath: "/teams", wantBody: `"backend"`,
	},
	{
		name: "teams/get",
		callFn: func() error {
			return apiCall("GET", "/teams/team-id", nil)
		},
		wantMethod: "GET", wantPath: "/teams/team-id",
	},
	{
		name: "teams/update",
		callFn: func() error {
			return apiCall("PUT", "/teams/team-id", jsonBody(map[string]string{"name": "frontend"}))
		},
		wantMethod: "PUT", wantPath: "/teams/team-id",
	},
	{
		name: "teams/delete",
		callFn: func() error {
			return apiCall("DELETE", "/teams/team-id", nil)
		},
		wantMethod: "DELETE", wantPath: "/teams/team-id",
	},
	{
		name: "teams/invite",
		callFn: func() error {
			return apiCall("POST", "/teams/team-id/invites", jsonBody(map[string]string{"user_id": "u2"}))
		},
		wantMethod: "POST", wantPath: "/teams/team-id/invites",
	},

	// ── Roles ─────────────────────────────────────────────────────────────────
	{
		name: "roles/create",
		callFn: func() error {
			return apiCall("POST", "/roles", jsonBody(map[string]string{"name": "admin"}))
		},
		wantMethod: "POST", wantPath: "/roles", wantBody: `"admin"`,
	},
	{
		name: "roles/get",
		callFn: func() error {
			return apiCall("GET", "/roles/role-id", nil)
		},
		wantMethod: "GET", wantPath: "/roles/role-id",
	},
	{
		name: "roles/update",
		callFn: func() error {
			return apiCall("PUT", "/roles/role-id", jsonBody(map[string]string{"name": "viewer"}))
		},
		wantMethod: "PUT", wantPath: "/roles/role-id",
	},
	{
		name: "roles/delete",
		callFn: func() error {
			return apiCall("DELETE", "/roles/role-id", nil)
		},
		wantMethod: "DELETE", wantPath: "/roles/role-id",
	},

	// ── Permissions ──────────────────────────────────────────────────────────
	{
		name: "permissions/create",
		callFn: func() error {
			return apiCall("POST", "/permissions", jsonBody(map[string]string{"action": "read", "resource": "repos"}))
		},
		wantMethod: "POST", wantPath: "/permissions", wantBody: `"read"`,
	},
	{
		name: "permissions/get",
		callFn: func() error {
			return apiCall("GET", "/permissions/perm-id", nil)
		},
		wantMethod: "GET", wantPath: "/permissions/perm-id",
	},
	{
		name: "permissions/update",
		callFn: func() error {
			return apiCall("PUT", "/permissions/perm-id", jsonBody(map[string]string{"action": "write"}))
		},
		wantMethod: "PUT", wantPath: "/permissions/perm-id",
	},
	{
		name: "permissions/delete",
		callFn: func() error {
			return apiCall("DELETE", "/permissions/perm-id", nil)
		},
		wantMethod: "DELETE", wantPath: "/permissions/perm-id",
	},

	// ── Sessions ─────────────────────────────────────────────────────────────
	{
		name: "sessions/get",
		callFn: func() error {
			return apiCall("GET", "/sessions/sess-id", nil)
		},
		wantMethod: "GET", wantPath: "/sessions/sess-id",
	},
	{
		name: "sessions/delete",
		callFn: func() error {
			return apiCall("DELETE", "/sessions/sess-id", nil)
		},
		wantMethod: "DELETE", wantPath: "/sessions/sess-id",
	},

	// ── Invites ──────────────────────────────────────────────────────────────
	{
		name: "invites/list",
		callFn: func() error {
			return apiCall("GET", "/invites", nil)
		},
		wantMethod: "GET", wantPath: "/invites",
	},
	{
		name: "invites/get",
		callFn: func() error {
			return apiCall("GET", "/invites/inv-id", nil)
		},
		wantMethod: "GET", wantPath: "/invites/inv-id",
	},
	{
		name: "invites/accept",
		callFn: func() error {
			return apiCall("POST", "/invites/inv-id/accept", nil)
		},
		wantMethod: "POST", wantPath: "/invites/inv-id/accept",
	},
	{
		name: "invites/decline",
		callFn: func() error {
			return apiCall("POST", "/invites/inv-id/decline", nil)
		},
		wantMethod: "POST", wantPath: "/invites/inv-id/decline",
	},
	{
		name: "invites/delete",
		callFn: func() error {
			return apiCall("DELETE", "/invites/inv-id", nil)
		},
		wantMethod: "DELETE", wantPath: "/invites/inv-id",
	},
}

func TestAPIRoutes(t *testing.T) {
	for _, tt := range allRouteTests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := recordingServer(t, http.StatusOK, `{}`)
			setupCLI(t, srv)
			silenceStdout(t)

			if err := tt.callFn(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if rec.Method != tt.wantMethod {
				t.Errorf("method = %q, want %q", rec.Method, tt.wantMethod)
			}
			if rec.Path != tt.wantPath {
				t.Errorf("path = %q, want %q", rec.Path, tt.wantPath)
			}
			if rec.Auth != "Bearer test-jwt" {
				t.Errorf("Authorization = %q, want Bearer test-jwt", rec.Auth)
			}
			if tt.wantBody != "" && !strings.Contains(string(rec.Body), tt.wantBody) {
				t.Errorf("body %q does not contain %q", rec.Body, tt.wantBody)
			}
		})
	}
}

func TestAPIError_4xx(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, "forbidden")
	setupCLI(t, srv)
	silenceStdout(t)

	err := apiCall("GET", "/users", nil)
	if err == nil {
		t.Fatal("expected error for 403, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q should mention status 403", err.Error())
	}
}

func TestAPIError_5xx(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusInternalServerError, "internal error")
	setupCLI(t, srv)
	silenceStdout(t)

	err := apiCall("GET", "/orgs", nil)
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q should mention status 500", err.Error())
	}
}

func TestAPIError_ServerDown(t *testing.T) {
	// Point at a port nothing is listening on
	flagURL = "http://127.0.0.1:19999"
	flagToken = "tok"
	t.Cleanup(func() { flagURL = ""; flagToken = "" })

	err := apiCall("GET", "/users", nil)
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
}

func TestBearerTokenSentOnAllRequests(t *testing.T) {
	// Verify the Authorization header is attached regardless of HTTP method.
	methods := []string{"GET", "POST", "PUT", "DELETE"}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			srv, rec := recordingServer(t, http.StatusOK, `{}`)
			setupCLI(t, srv)
			silenceStdout(t)

			apiCall(method, "/users", jsonBody(map[string]string{})) //nolint:errcheck
			if rec.Auth != "Bearer test-jwt" {
				t.Errorf("%s: Authorization = %q, want Bearer test-jwt", method, rec.Auth)
			}
		})
	}
}

func TestNoTokenOnPublicEndpoints(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"token":"x"}`)
	setupCLINoToken(t, srv)
	silenceStdout(t)

	doRequest("POST", "/login", jsonBody(map[string]string{"email": "a@b.com", "password": "pw"})) //nolint:errcheck
	if rec.Auth != "" {
		t.Errorf("Authorization header should be empty for public endpoints, got %q", rec.Auth)
	}
}
