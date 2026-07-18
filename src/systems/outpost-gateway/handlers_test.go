package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// testDBReady is set in TestMain when the in-memory test DB is migrated. The
// SKIP LOCKED claim paths are dialect-guarded (skipLocked), so they run on
// hermetic sqlite here and use FOR UPDATE SKIP LOCKED on Postgres in production.
var testDBReady bool

func TestMain(m *testing.M) {
	initMetrics()
	httpClient = initHTTPClient()
	gatekeeperClient = newGatekeeperClient()

	// Hermetic in-memory sqlite (mirrors gatekeeper) — no external Postgres.
	conn, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err == nil {
		if migrateErr := conn.AutoMigrate(&Outpost{}, &OutpostCommand{}, &OutpostEvent{}); migrateErr == nil {
			dbInitMu.Lock()
			gormDB = conn
			gormDBRead = conn
			dbInitMu.Unlock()
			testDBReady = true
		}
	}

	os.Exit(m.Run())
}

func requireDB(t *testing.T) {
	t.Helper()
	if !testDBReady {
		t.Skip("outpost-gateway test database not available")
	}
}

// stubGatekeeper points gatekeeperClient at an httptest server that authorizes
// every request as the given user/org. It restores the previous URL on cleanup.
// authorized=false makes /check_permissions return 200 with authorized:false so
// the SDK writes 403.
func stubGatekeeper(t *testing.T, userID, orgID string, authorized bool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"authorized": authorized, "user_id": userID}
		if orgID != "" {
			resp["org_id"] = orgID
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	prev := gatekeeperClient.URL
	gatekeeperClient.URL = srv.URL
	t.Cleanup(func() {
		gatekeeperClient.URL = prev
		srv.Close()
	})
}

// bearerReq builds a request carrying a (dummy) bearer token so the gatekeeper
// SDK accepts the Authorization header and calls the stub.
func bearerReq(method, target string, body []byte) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Header.Set("Authorization", "Bearer test-jwt")
	return r
}

// jsonBody marshals v to JSON bytes for a request body.
func jsonBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
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
