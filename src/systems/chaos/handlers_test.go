package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func fakeGatekeeper(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body)) //nolint:errcheck
	}))
	orig := gatekeeperClient.URL
	gatekeeperClient.URL = srv.URL
	t.Cleanup(func() {
		gatekeeperClient.URL = orig
		srv.Close()
	})
}

func TestMain(m *testing.M) {
	initMetrics()
	httpClient = initHTTPClient()
	gatekeeperClient = newGatekeeperClient()
	os.Exit(m.Run())
}

// ── CheckPermissions paths ────────────────────────────────────────────────────

func TestCheckGatekeeper_NoToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/experiments", nil)
	w := httptest.NewRecorder()
	if _, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listExperiment", "chaos/experiments"); ok {
		t.Fatal("expected ok=false with no Bearer token")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestCheckGatekeeper_Authorized(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":"o1"}`)
	r := httptest.NewRequest(http.MethodGet, "/experiments", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	id, org, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listExperiment", "chaos/experiments")
	if !ok || id != "u1" || org != "o1" {
		t.Fatalf("got (%q,%q,%v), want (u1,o1,true)", id, org, ok)
	}
}

// ── Handler auth gates ────────────────────────────────────────────────────────

func TestHandleCreateExperiment_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/experiments", nil)
	w := httptest.NewRecorder()
	handleCreateExperiment(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleListExperiments_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/experiments", nil)
	w := httptest.NewRecorder()
	handleListExperiments(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleGetExperiment_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/experiments/x", nil)
	r.SetPathValue("id", "x")
	w := httptest.NewRecorder()
	handleGetExperiment(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleInternalEvent_BadToken(t *testing.T) {
	// No/invalid HMAC token → 401 regardless of body.
	body := `{"event_id":"e1","integration":"chaos","type":"verdict","payload":{"experiment_id":"x","verdict":"Pass"}}`
	r := httptest.NewRequest(http.MethodPost, "/internal/events", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleInternalEvent(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleInternalEvent_ValidTokenNoExperiment(t *testing.T) {
	// A correctly-signed event with no experiment_id is acked (200) so the
	// dispatcher does not retry it forever.
	prev := outpostInternalKey
	outpostInternalKey = "unit-key"
	t.Cleanup(func() { outpostInternalKey = prev })

	body := `{"event_id":"e1","integration":"chaos","type":"verdict","payload":{}}`
	token, ts := signInternal("event", []byte(body))
	r := httptest.NewRequest(http.MethodPost, "/internal/events", bytes.NewBufferString(body))
	r.Header.Set("X-Internal-Token", token)
	r.Header.Set("X-Internal-Timestamp", ts)
	w := httptest.NewRecorder()
	handleInternalEvent(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}

// ── Pure helpers ──────────────────────────────────────────────────────────────

func TestLookupExperimentType(t *testing.T) {
	if et, ok := lookupExperimentType("pod-delete"); !ok || et.Name != "pod-delete" {
		t.Fatalf("pod-delete lookup failed: %v %v", et, ok)
	}
	if _, ok := lookupExperimentType("nope"); ok {
		t.Fatal("unknown type should not resolve")
	}
}

func TestEngineNameDeterministic(t *testing.T) {
	if got := engineName("abc"); got != "exp-abc" {
		t.Fatalf("engineName(abc) = %q, want exp-abc", got)
	}
}

func TestCanAccess(t *testing.T) {
	e := Experiment{UserID: "u1", OrgID: "o1"}
	if !canAccess(e, "u1", "") {
		t.Error("owner should have access")
	}
	if !canAccess(e, "u2", "o1") {
		t.Error("same-org user should have access")
	}
	if canAccess(e, "u2", "o2") {
		t.Error("different-org user should not have access")
	}
	if canAccess(Experiment{UserID: "u1"}, "u2", "o1") {
		t.Error("org membership should not grant access to a no-org experiment")
	}
}

func TestExperimentTypesHaveParams(t *testing.T) {
	for _, et := range experimentTypes {
		if et.Name == "" || len(et.Params) == 0 {
			t.Errorf("experiment type %q has no params", et.Name)
		}
	}
}
