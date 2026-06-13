package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	initMetrics()
	httpClient = initHTTPClient()
	gatekeeperClient = newGatekeeperClient()
	os.Exit(m.Run())
}

// ── outpost-facing auth ───────────────────────────────────────────────────────

func TestAuthenticateOutpost_MissingCreds(t *testing.T) {
	for _, h := range []struct {
		name string
		fn   http.HandlerFunc
		req  *http.Request
	}{
		{"commands", handleCommands, httptest.NewRequest(http.MethodGet, "/outpost/commands", nil)},
		{"events", handleEvents, httptest.NewRequest(http.MethodPost, "/outpost/events", nil)},
		{"heartbeat", handleHeartbeat, httptest.NewRequest(http.MethodPost, "/outpost/heartbeat", nil)},
	} {
		w := httptest.NewRecorder()
		h.fn(w, h.req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with no creds: got %d, want 401", h.name, w.Code)
		}
	}
}

func TestHandleRegister_MalformedToken(t *testing.T) {
	body := `{"enrollment_token":"noseparator"}`
	r := httptest.NewRequest(http.MethodPost, "/outpost/register", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleRegister(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleRegister_InvalidBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/outpost/register", bytes.NewBufferString("not-json"))
	w := httptest.NewRecorder()
	handleRegister(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

// ── internal command enqueue ──────────────────────────────────────────────────

func TestHandleEnqueueCommand_MissingFields(t *testing.T) {
	prev := outpostInternalKey
	outpostInternalKey = "k"
	t.Cleanup(func() { outpostInternalKey = prev })
	// A validly-authenticated request with missing fields must still 400. Auth is
	// verified before the body is parsed, so the token must be valid to reach the
	// field-validation path.
	body := `{"outpost_id":"o1"}` // no integration/type
	token, ts := signInternal("command", []byte(body))
	r := httptest.NewRequest(http.MethodPost, "/internal/commands", bytes.NewBufferString(body))
	r.Header.Set("X-Internal-Token", token)
	r.Header.Set("X-Internal-Timestamp", ts)
	w := httptest.NewRecorder()
	handleEnqueueCommand(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleEnqueueCommand_BadHMAC(t *testing.T) {
	prev := outpostInternalKey
	outpostInternalKey = "k"
	t.Cleanup(func() { outpostInternalKey = prev })
	body := `{"outpost_id":"o1","integration":"chaos","type":"run-experiment","payload":{}}`
	r := httptest.NewRequest(http.MethodPost, "/internal/commands", bytes.NewBufferString(body))
	r.Header.Set("X-Internal-Token", "wrong")
	r.Header.Set("X-Internal-Timestamp", "0")
	w := httptest.NewRecorder()
	handleEnqueueCommand(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── user-facing ───────────────────────────────────────────────────────────────

func TestHandleCreateOutpost_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/outposts", nil)
	w := httptest.NewRecorder()
	handleCreateOutpost(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── credential helpers ────────────────────────────────────────────────────────

func TestEnrollmentTokenRoundTrip(t *testing.T) {
	token, hash, err := mintEnrollmentToken("outpost-7")
	if err != nil {
		t.Fatal(err)
	}
	id, secretPart, ok := splitEnrollmentToken(token)
	if !ok || id != "outpost-7" {
		t.Fatalf("split: id=%q ok=%v", id, ok)
	}
	if !checkBcrypt(hash, secretPart) {
		t.Error("bcrypt should verify the minted secret")
	}
	if checkBcrypt(hash, "wrong") {
		t.Error("bcrypt should reject a wrong secret")
	}
}

func TestOutpostKeyRoundTrip(t *testing.T) {
	key, hash, err := mintOutpostKey()
	if err != nil {
		t.Fatal(err)
	}
	if !checkBcrypt(hash, key) {
		t.Error("bcrypt should verify the minted key")
	}
	if checkBcrypt(hash, key+"x") {
		t.Error("bcrypt should reject a tampered key")
	}
}

func TestBearerToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer abc123")
	if got := bearerToken(r); got != "abc123" {
		t.Fatalf("got %q, want abc123", got)
	}
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := bearerToken(r2); got != "" {
		t.Fatalf("missing header should yield empty, got %q", got)
	}
}

func TestOutpostHasModule(t *testing.T) {
	o := Outpost{Modules: "chaos,argo"}
	if !outpostHasModule(o, "chaos") || !outpostHasModule(o, "argo") {
		t.Error("expected both modules present")
	}
	if outpostHasModule(o, "foo") {
		t.Error("foo should not be present")
	}
	if outpostHasModule(Outpost{Modules: ""}, "chaos") {
		t.Error("empty modules should not match")
	}
}

// TestCanAccessOutpost locks in the tenant check handleEnqueueCommand relies on
// to stop one tenant driving another tenant's outpost by id.
func TestCanAccessOutpost(t *testing.T) {
	o := Outpost{UserID: "u1", OrgID: "o1"}
	if !canAccessOutpost(o, "u1", "") {
		t.Error("owner should have access")
	}
	if !canAccessOutpost(o, "u2", "o1") {
		t.Error("same-org user should have access")
	}
	if canAccessOutpost(o, "u2", "o2") {
		t.Error("different-org user must not have access")
	}
	if canAccessOutpost(Outpost{UserID: "u1"}, "u2", "o1") {
		t.Error("org membership must not grant access to a no-org outpost")
	}
}

func TestMarkStale(t *testing.T) {
	out := []Outpost{{Status: OutpostConnected, LastSeenAt: nil}}
	markStale(out)
	if out[0].Status != OutpostStale {
		t.Errorf("connected outpost never seen should be stale, got %q", out[0].Status)
	}
}
