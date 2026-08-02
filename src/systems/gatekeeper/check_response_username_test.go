package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The /check_permissions response carries the caller's USERNAME, not just their user id.
//
// It matters because gatekeeper keys resources by username (scopeResource), while a
// service only ever learned the user id from this response. Without the username a
// service cannot build an owner-first resource naming its own caller, which is what
// per-record authorisation needs — tickets worked around it with a second round trip to
// /oauth/userinfo plus a stored column, purely because this field was missing from a
// response that had already loaded the row.

func TestDenialResponse_CarriesUsername(t *testing.T) {
	org := "org-1"
	out := denialResponse(true, "u-1", "alice", &org, permissionDenial{})

	if out["user_id"] != "u-1" {
		t.Errorf("user_id = %v", out["user_id"])
	}
	if out["username"] != "alice" {
		t.Errorf("username = %v, want alice — a service cannot name an owner without it", out["username"])
	}
	if out["authorized"] != true {
		t.Errorf("authorized = %v", out["authorized"])
	}
}

// Omitted rather than sent blank, so presence means "this subject has a namespace you
// can name". A client-credentials subject is not a user and owns no namespace.
func TestDenialResponse_OmitsAnEmptyUsername(t *testing.T) {
	out := denialResponse(true, "client-1", "", nil, permissionDenial{})

	if _, present := out["username"]; present {
		t.Errorf("username was emitted as %v for a subject with none; an empty namespace would build the resource \"/service/collection\" and match nothing", out["username"])
	}
	if out["user_id"] != "client-1" {
		t.Errorf("user_id = %v", out["user_id"])
	}
}

// The denial fields still arrive alongside it — adding the username must not disturb
// the reason a 403 can explain itself.
func TestDenialResponse_UsernameDoesNotDisplaceDenialDetail(t *testing.T) {
	out := denialResponse(false, "u-1", "alice", nil, permissionDenial{
		Roles: []string{"dev"}, Service: "tickets", Action: "getTicket", Resource: "alice/tickets/tickets/x",
	})

	if out["username"] != "alice" {
		t.Errorf("username = %v", out["username"])
	}
	for _, k := range []string{"role", "service", "action", "resource", "reason"} {
		if _, present := out[k]; !present {
			t.Errorf("denial detail %q missing from an unauthorized response", k)
		}
	}
}

// An authorized response stays lean — the denial detail is only for explaining a 403.
func TestDenialResponse_AuthorizedOmitsDenialDetail(t *testing.T) {
	out := denialResponse(true, "u-1", "alice", nil, permissionDenial{
		Roles: []string{"dev"}, Service: "tickets", Action: "getTicket", Resource: "r",
	})
	for _, k := range []string{"role", "reason"} {
		if _, present := out[k]; present {
			t.Errorf("%q leaked into an authorized response", k)
		}
	}
}

// End-to-end through the real handler: the payload builder is only half the wiring, and
// a username that never reaches denialResponse would still pass every test above.
func TestHandleCheckPermissions_ResponseCarriesTheCallersUsername(t *testing.T) {
	u := createTestUser(t)

	b, _ := json.Marshal(checkPermissionsRequest{Service: "svc", Resource: "svc/things", Action: "read"})
	r := withUserID(httptest.NewRequest(http.MethodGet, "/check_permissions", bytes.NewReader(b)), u.UserID)
	w := httptest.NewRecorder()
	handleCheckPermissions(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// Asserted even though this user holds no grant: the username identifies the CALLER
	// and is needed to build the resource for the next attempt, so it must not depend on
	// the outcome of the check.
	if resp["username"] != u.Username {
		t.Errorf("username = %v, want %q (user_id was %v)", resp["username"], u.Username, resp["user_id"])
	}
	if resp["user_id"] != u.UserID {
		t.Errorf("user_id = %v, want %q", resp["user_id"], u.UserID)
	}
}
