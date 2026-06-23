package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedEvent inserts an outbox event and registers cleanup. status/nextRetry let a
// test place the event in the past (due) or future (not yet due).
func seedEvent(t *testing.T, integration, status string, nextRetry time.Time, attempt int) OutpostEvent {
	t.Helper()
	e := OutpostEvent{
		ID:          uuid.New().String(),
		OutpostID:   uuid.New().String(),
		OrgID:       "org-" + uuid.New().String(),
		UserID:      "user-1",
		Integration: integration,
		Type:        "demo.event",
		Payload:     map[string]any{"k": "v"},
		Status:      status,
		NextRetryAt: nextRetry,
		CreatedAt:   time.Now().UTC(),
	}
	if err := connect().Create(&e).Error; err != nil {
		t.Fatalf("seedEvent: %v", err)
	}
	if attempt != 0 {
		connect().Exec(`UPDATE outpost_events SET attempt = ? WHERE id = ?`, attempt, e.ID) //nolint:errcheck
		e.Attempt = attempt
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM outpost_events WHERE id = ?`, e.ID) }) //nolint:errcheck
	return e
}

func eventStatus(t *testing.T, id string) (status string, attempt int, nextRetry time.Time) {
	t.Helper()
	var e OutpostEvent
	if err := connect().Where("id = ?", id).First(&e).Error; err != nil {
		t.Fatalf("load event: %v", err)
	}
	return e.Status, e.Attempt, e.NextRetryAt
}

// withConsumer points the dispatcher at a stub consumer for one integration.
func withConsumer(t *testing.T, integration, url string) {
	t.Helper()
	prev, had := eventConsumers[integration]
	eventConsumers[integration] = strings.TrimRight(url, "/")
	t.Cleanup(func() {
		if had {
			eventConsumers[integration] = prev
		} else {
			delete(eventConsumers, integration)
		}
	})
}

func TestSplitCSV(t *testing.T) {
	got := splitCSV(" a, ,b ,, c")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestDeliverEvent_Success(t *testing.T) {
	requireDB(t)
	var gotToken, gotTS string
	var body map[string]any
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		gotToken = r.Header.Get("X-Internal-Token")
		gotTS = r.Header.Get("X-Internal-Timestamp")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withConsumer(t, "catest", srv.URL)

	e := seedEvent(t, "catest", EvPending, time.Now().UTC().Add(-time.Minute), 0)
	deliverEvent(context.Background(), e)

	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("consumer hit %d times, want 1", hits)
	}
	if gotToken == "" || gotTS == "" {
		t.Error("expected internal auth headers to be set")
	}
	if body["event_id"] != e.ID || body["integration"] != "catest" {
		t.Errorf("delivered body wrong: %+v", body)
	}
	if st, _, _ := eventStatus(t, e.ID); st != EvDelivered {
		t.Errorf("status = %q, want delivered", st)
	}
}

func TestDeliverEvent_NoConsumer_DeadLetters(t *testing.T) {
	requireDB(t)
	// integration with no configured consumer → dead-letter immediately.
	e := seedEvent(t, "no-such-integration-"+uuid.New().String(), EvPending, time.Now().UTC().Add(-time.Minute), 0)
	deliverEvent(context.Background(), e)
	if st, _, _ := eventStatus(t, e.ID); st != EvFailed {
		t.Errorf("status = %q, want failed (dead-lettered)", st)
	}
}

func TestDeliverEvent_Non2xx_SchedulesRetry(t *testing.T) {
	requireDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	withConsumer(t, "catest5xx", srv.URL)

	e := seedEvent(t, "catest5xx", EvPending, time.Now().UTC().Add(-time.Minute), 0)
	before := time.Now().UTC()
	deliverEvent(context.Background(), e)

	st, attempt, next := eventStatus(t, e.ID)
	if st != EvPending {
		t.Errorf("status = %q, want pending (retry)", st)
	}
	if attempt != 1 {
		t.Errorf("attempt = %d, want 1", attempt)
	}
	if !next.After(before) {
		t.Errorf("next_retry_at not pushed into the future: %v", next)
	}
}

func TestDeliverEvent_TransportError_SchedulesRetry(t *testing.T) {
	requireDB(t)
	// Point at a closed server to force a transport error.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	withConsumer(t, "catestdown", url)

	e := seedEvent(t, "catestdown", EvPending, time.Now().UTC().Add(-time.Minute), 0)
	deliverEvent(context.Background(), e)
	st, attempt, _ := eventStatus(t, e.ID)
	if st != EvPending || attempt != 1 {
		t.Errorf("transport error: status=%q attempt=%d, want pending/1", st, attempt)
	}
}

func TestScheduleEventRetry_DeadLettersAtMax(t *testing.T) {
	requireDB(t)
	e := seedEvent(t, "catestmax", EvPending, time.Now().UTC(), maxDeliveryAttempts-1)
	if err := scheduleEventRetry(context.Background(), e, "exhausted"); err != nil {
		t.Fatalf("scheduleEventRetry: %v", err)
	}
	st, attempt, _ := eventStatus(t, e.ID)
	if st != EvFailed {
		t.Errorf("status = %q, want failed at max attempts", st)
	}
	if attempt != maxDeliveryAttempts {
		t.Errorf("attempt = %d, want %d", attempt, maxDeliveryAttempts)
	}
}

func TestDispatchDue_DeliversPending(t *testing.T) {
	requireDB(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withConsumer(t, "catestdue", srv.URL)

	e := seedEvent(t, "catestdue", EvPending, time.Now().UTC().Add(-time.Minute), 0)
	dispatchDue(context.Background())
	if st, _, _ := eventStatus(t, e.ID); st != EvDelivered {
		t.Errorf("status = %q, want delivered after dispatchDue", st)
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Error("consumer not called by dispatchDue")
	}
}

func TestClaimDueEvents_ClaimsDueLeavesFuture(t *testing.T) {
	requireDB(t)
	due := seedEvent(t, "catestclaim", EvPending, time.Now().UTC().Add(-time.Minute), 0)
	future := seedEvent(t, "catestclaim", EvPending, time.Now().UTC().Add(time.Hour), 0)

	lease := 30 * time.Second
	claimed, err := claimDueEvents(context.Background(), 50, lease)
	if err != nil {
		t.Fatalf("claimDueEvents: %v", err)
	}
	ids := map[string]bool{}
	for _, e := range claimed {
		ids[e.ID] = true
	}
	if !ids[due.ID] {
		t.Error("due event was not claimed")
	}
	if ids[future.ID] {
		t.Error("future event must not be claimed")
	}
	// The claimed (due) event is leased into the future so a peer poller won't re-grab it.
	_, _, next := eventStatus(t, due.ID)
	if !next.After(time.Now().UTC().Add(lease - 5*time.Second)) {
		t.Errorf("due event was not leased forward: next=%v", next)
	}
}

func TestMarkEventDelivered(t *testing.T) {
	requireDB(t)
	e := seedEvent(t, "catestmark", EvPending, time.Now().UTC(), 0)
	if err := markEventDelivered(context.Background(), e.ID); err != nil {
		t.Fatalf("markEventDelivered: %v", err)
	}
	if st, _, _ := eventStatus(t, e.ID); st != EvDelivered {
		t.Errorf("status = %q, want delivered", st)
	}
}

// ── middleware / helpers ───────────────────────────────────────────────────────

func TestSecretOrDefault(t *testing.T) {
	t.Setenv("OG_TEST_VAR", "set-value")
	if got := secretOrDefault("OG_TEST_VAR", "def"); got != "set-value" {
		t.Errorf("got %q, want set-value", got)
	}
	if got := secretOrDefault("OG_TEST_VAR_MISSING", "def"); got != "def" {
		t.Errorf("got %q, want def", got)
	}
}

func TestRequestLogger_CapturesStatusAndServes(t *testing.T) {
	for _, path := range []string{"/anything", "/healthz"} {
		called := false
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusTeapot)
			w.Write([]byte("ok")) //nolint:errcheck
		})
		lg := &requestLogger{handler: inner}
		w := httptest.NewRecorder()
		lg.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if !called {
			t.Fatalf("%s: inner handler not called", path)
		}
		if w.Code != http.StatusTeapot {
			t.Errorf("%s: status = %d, want 418", path, w.Code)
		}
	}
}

func TestLimitBody(t *testing.T) {
	var readErr error
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	})
	h := limitBody(inner)
	// A body larger than maxBodyBytes must error on read.
	big := strings.NewReader(strings.Repeat("a", int(maxBodyBytes)+1024))
	r := httptest.NewRequest(http.MethodPost, "/x", big)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if readErr == nil {
		t.Error("expected oversized body to error under limitBody")
	}
}

func TestHandleOpenAPIYAML(t *testing.T) {
	w := httptest.NewRecorder()
	handleOpenAPIYAML(w, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("content-type = %q", ct)
	}
	if w.Body.Len() == 0 {
		t.Error("expected non-empty openapi body")
	}
}
