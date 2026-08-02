package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Who owns a global field definition?
//
// The seeded status and priority catalogs are inserted with OrgID "" — they belong to
// the instance, not to any org or user. The ownership test for a non-board-scoped def
// used to be `f.OrgID == orgID`, which reads as "same org" and is correct for a real
// org, since "" never equals a real org id. It is not correct for a caller who has NO
// org: "" == "" authorized them over every global def, and deleting those removes the
// default statuses for everyone.
//
// The empty string was standing for two different things — "owned by the platform" and
// "this caller has no org" — and comparing them was never a statement about ownership.
// These tests pin the distinction.

// stubGatekeeperHTTP points the raw forward-the-bearer helpers (checkPlatformPermission,
// checkProjectPermission) at a stub. Distinct from fakeGatekeeper, which only redirects
// the SDK client — these helpers use the package-level gatekeeperURL and httpClient.
func stubGatekeeperHTTP(t *testing.T, authorized bool) *[]string {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Resource string `json:"resource"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		seen = append(seen, body.Resource)
		w.Header().Set("Content-Type", "application/json")
		if authorized {
			w.Write([]byte(`{"authorized":true}`)) //nolint:errcheck
		} else {
			w.Write([]byte(`{"authorized":false}`)) //nolint:errcheck
		}
	}))
	origURL, origClient := gatekeeperURL, httpClient
	gatekeeperURL = srv.URL
	httpClient = srv.Client()
	t.Cleanup(func() {
		gatekeeperURL, httpClient = origURL, origClient
		srv.Close()
	})
	return &seen
}

// A global seeded def is not modifiable by a caller who simply has no org.
func TestCanModifyFieldDef_GlobalDefIsNotOwnedByAnOrglessCaller(t *testing.T) {
	seen := stubGatekeeperHTTP(t, false)
	global := TicketFieldDef{FieldDefID: "fd-1", Kind: FieldKindStatus, Value: StatusOpen, OrgID: ""}

	if canModifyFieldDef(context.Background(), "Bearer nobody", "deleteFieldDef", global, "u-nobody", "") {
		t.Error("an org-less caller was authorized over a global seeded def; that caller can delete the platform's default statuses")
	}
	// And the question actually asked names the platform namespace, so an ordinary
	// user's own-namespace grants cannot match it.
	if len(*seen) != 1 || (*seen)[0] != "codearmory/tickets/field-defs" {
		t.Errorf("checked %v, want one check on codearmory/tickets/field-defs", *seen)
	}
}

// The same call succeeds when gatekeeper says yes — i.e. an admin wildcard. The gate is
// a real permission check, not a blanket refusal that would make the defs uneditable.
func TestCanModifyFieldDef_GlobalDefIsModifiableWithAPlatformGrant(t *testing.T) {
	stubGatekeeperHTTP(t, true)
	global := TicketFieldDef{FieldDefID: "fd-1", Kind: FieldKindStatus, Value: StatusOpen, OrgID: ""}

	if !canModifyFieldDef(context.Background(), "Bearer admin", "updateFieldDef", global, "u-admin", "") {
		t.Error("a caller holding the platform grant was refused; the seeded catalogs must stay editable by an admin")
	}
}

// No credential is a refusal, not a fallthrough to the org comparison.
func TestCanModifyFieldDef_GlobalDefRefusedWithoutABearer(t *testing.T) {
	stubGatekeeperHTTP(t, true)
	global := TicketFieldDef{FieldDefID: "fd-1", OrgID: ""}

	if canModifyFieldDef(context.Background(), "", "deleteFieldDef", global, "u", "") {
		t.Error("authorized with no bearer to present")
	}
}

// An unreachable gatekeeper denies. A platform-owned resource must not become
// world-writable because a dependency is down.
func TestCanModifyFieldDef_GlobalDefFailsClosedWhenGatekeeperIsDown(t *testing.T) {
	origURL, origClient := gatekeeperURL, httpClient
	gatekeeperURL = "http://127.0.0.1:1" // nothing listens here
	httpClient = &http.Client{}
	t.Cleanup(func() { gatekeeperURL, httpClient = origURL, origClient })

	global := TicketFieldDef{FieldDefID: "fd-1", OrgID: ""}
	if canModifyFieldDef(context.Background(), "Bearer x", "deleteFieldDef", global, "u", "") {
		t.Error("authorized while gatekeeper was unreachable — this must fail closed")
	}
}

// An org's own def is still its own, and gatekeeper is not consulted for it: the org
// comparison is a complete answer, and a needless round-trip on every edit would be a
// cost with no decision attached.
func TestCanModifyFieldDef_OrgDefStillMatchesItsOwnOrg(t *testing.T) {
	seen := stubGatekeeperHTTP(t, false)
	orgDef := TicketFieldDef{FieldDefID: "fd-2", OrgID: "org-a"}

	if !canModifyFieldDef(context.Background(), "Bearer a", "updateFieldDef", orgDef, "u-a", "org-a") {
		t.Error("an org was refused its own field def")
	}
	if len(*seen) != 0 {
		t.Errorf("consulted gatekeeper %v for a plain org-owned def", *seen)
	}
}

// A different org is still refused, and an org-less caller does not slip through here
// either — the case that used to work by string coincidence.
func TestCanModifyFieldDef_OrgDefRefusesOthers(t *testing.T) {
	stubGatekeeperHTTP(t, false)
	orgDef := TicketFieldDef{FieldDefID: "fd-2", OrgID: "org-a"}

	if canModifyFieldDef(context.Background(), "Bearer b", "updateFieldDef", orgDef, "u-b", "org-b") {
		t.Error("another org modified org-a's field def")
	}
	if canModifyFieldDef(context.Background(), "Bearer c", "updateFieldDef", orgDef, "u-c", "") {
		t.Error("an org-less caller modified org-a's field def")
	}
}
