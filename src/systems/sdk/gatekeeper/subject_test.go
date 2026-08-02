package gatekeeper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubGatekeeper serves one fixed /check_permissions body.
func stubGatekeeper(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, Service: "svc", HTTPClient: srv.Client()}
}

func authedRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("Authorization", "Bearer tok")
	return r
}

// The whole point of the change: a service can learn the caller's NAMESPACE, which is
// the username, without a second round trip. user_id is not usable here — gatekeeper
// keys resources by username.
func TestCheck_ReportsTheCallersUsername(t *testing.T) {
	c := stubGatekeeper(t, `{"authorized":true,"user_id":"u-1","username":"alice","org_id":"org-1"}`)

	sub, ok := c.Check(context.Background(), httptest.NewRecorder(), authedRequest(), "getThing", "svc/things")
	if !ok {
		t.Fatal("expected authorised")
	}
	if sub.UserID != "u-1" || sub.Username != "alice" || sub.OrgID != "org-1" {
		t.Errorf("got %+v", sub)
	}
	ns, known := sub.Namespace()
	if !known || ns != "alice" {
		t.Errorf("Namespace() = (%q, %v), want (alice, true)", ns, known)
	}
}

// An older gatekeeper omits the field. The SDK must keep working against a control
// plane that has not been upgraded yet — the alternative is that rolling out the SDK
// first takes the platform down.
func TestCheck_ToleratesAGatekeeperWithoutUsername(t *testing.T) {
	c := stubGatekeeper(t, `{"authorized":true,"user_id":"u-1","org_id":"org-1"}`)

	sub, ok := c.Check(context.Background(), httptest.NewRecorder(), authedRequest(), "getThing", "svc/things")
	if !ok {
		t.Fatal("expected authorised")
	}
	if sub.UserID != "u-1" || sub.OrgID != "org-1" {
		t.Errorf("got %+v", sub)
	}
	if ns, known := sub.Namespace(); known {
		t.Errorf("Namespace() reported %q as known, but the response carried none — a caller would build \"%s/svc/things\" and match nothing", ns, ns)
	}
}

// A client-credentials subject owns no namespace, so the field is absent for it too.
func TestSubjectNamespace_UnknownWhenEmpty(t *testing.T) {
	if ns, known := (Subject{UserID: "client-1"}).Namespace(); known || ns != "" {
		t.Errorf("Namespace() = (%q, %v), want (\"\", false)", ns, known)
	}
}

// The old signature is unchanged and still delegates correctly — every existing call
// site depends on it.
func TestCheckPermissions_StillReturnsUserAndOrg(t *testing.T) {
	c := stubGatekeeper(t, `{"authorized":true,"user_id":"u-1","username":"alice","org_id":"org-1"}`)

	userID, orgID, ok := c.CheckPermissions(context.Background(), httptest.NewRecorder(), authedRequest(), "getThing", "svc/things")
	if !ok || userID != "u-1" || orgID != "org-1" {
		t.Errorf("got (%q, %q, %v), want (u-1, org-1, true)", userID, orgID, ok)
	}
}

// A missing org stays "" rather than becoming a literal "null".
func TestCheck_NullOrgBecomesEmpty(t *testing.T) {
	c := stubGatekeeper(t, `{"authorized":true,"user_id":"u-1","username":"alice","org_id":null}`)

	sub, ok := c.Check(context.Background(), httptest.NewRecorder(), authedRequest(), "getThing", "svc/things")
	if !ok || sub.OrgID != "" {
		t.Errorf("got %+v, want an empty OrgID", sub)
	}
}

// A denial yields the zero Subject, so a username can never leak out of an unauthorised
// check and be used to build a resource.
func TestCheck_DeniedYieldsZeroSubject(t *testing.T) {
	c := stubGatekeeper(t, `{"authorized":false,"user_id":"u-1","username":"alice"}`)

	w := httptest.NewRecorder()
	sub, ok := c.Check(context.Background(), w, authedRequest(), "getThing", "svc/things")
	if ok {
		t.Fatal("expected denial")
	}
	if sub != (Subject{}) {
		t.Errorf("got %+v, want the zero Subject", sub)
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403", w.Code)
	}
}

// No bearer is a 401 before any request is made.
func TestCheck_NoBearerIsUnauthorized(t *testing.T) {
	c := stubGatekeeper(t, `{"authorized":true,"user_id":"u-1"}`)

	w := httptest.NewRecorder()
	if _, ok := c.Check(context.Background(), w, httptest.NewRequest(http.MethodGet, "/x", nil), "getThing", "svc/things"); ok {
		t.Fatal("expected refusal with no bearer")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", w.Code)
	}
}
