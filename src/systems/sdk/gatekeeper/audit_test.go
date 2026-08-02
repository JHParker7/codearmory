package gatekeeper

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitFor polls cond until it returns true or the timeout elapses. Audit writes are
// deliberately asynchronous, so every assertion about what reached the server has to
// wait for it rather than assume it already arrived.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// auditRecorder is a stand-in gatekeeper that captures what the client posted.
type auditRecorder struct {
	mu      sync.Mutex
	calls   int
	path    string
	key     string
	entry   auditEntry
	status  int
	gotBody []byte
}

func newAuditRecorder(status int) (*auditRecorder, *httptest.Server) {
	rec := &auditRecorder{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.calls++
		rec.path = r.URL.Path
		rec.key = r.Header.Get("X-Service-Key")
		rec.gotBody = body
		_ = json.Unmarshal(body, &rec.entry)
		rec.mu.Unlock()
		w.WriteHeader(rec.status)
	}))
	return rec, srv
}

func (r *auditRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestAudit_PostsEntryWithServiceKey is the happy path: the entry lands on the ingest
// route, authenticated with "<service>:<rotated key>", and the body carries exactly
// what the caller stated — with no service field, since gatekeeper derives that from
// the key rather than trusting the body.
func TestAudit_PostsEntryWithServiceKey(t *testing.T) {
	rec, srv := newAuditRecorder(http.StatusAccepted)
	defer srv.Close()

	c := &Client{URL: srv.URL, Service: "forge", ServiceKey: func() string { return "rotated-key" }}
	c.Audit(context.Background(), "exec", "exec-123", "user-7", "ran alpine:3.19")

	if !waitFor(2*time.Second, func() bool { return rec.count() == 1 }) {
		t.Fatalf("audit entry never reached the server (calls=%d)", rec.count())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.path != "/internal/audit-logs" {
		t.Errorf("path = %q, want /internal/audit-logs", rec.path)
	}
	if rec.key != "forge:rotated-key" {
		t.Errorf("X-Service-Key = %q, want forge:rotated-key", rec.key)
	}
	if rec.entry.Action != "exec" || rec.entry.ResourceID != "exec-123" ||
		rec.entry.ActorID != "user-7" || rec.entry.Detail != "ran alpine:3.19" {
		t.Errorf("entry = %+v, want the values passed to Audit", rec.entry)
	}
	if strings.Contains(string(rec.gotBody), `"service"`) {
		t.Errorf("body must not carry a service field (gatekeeper stamps it): %s", rec.gotBody)
	}
}

// TestAudit_UsesTheCurrentKey pins the reason ServiceKey is a func: the east-west key
// rotates, and a client that captured a copy at construction would authenticate with
// a stale key after the first rotation.
func TestAudit_UsesTheCurrentKey(t *testing.T) {
	rec, srv := newAuditRecorder(http.StatusAccepted)
	defer srv.Close()

	key := "first"
	c := &Client{URL: srv.URL, Service: "forge", ServiceKey: func() string { return key }}
	key = "rotated"
	c.Audit(context.Background(), "exec", "exec-1", "", "")

	if !waitFor(2*time.Second, func() bool { return rec.count() == 1 }) {
		t.Fatal("audit entry never arrived")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.key != "forge:rotated" {
		t.Errorf("X-Service-Key = %q, want the key at call time (forge:rotated)", rec.key)
	}
}

// TestAudit_SurvivesCallerCancellation is the property the whole design exists for:
// the handler returns (cancelling its context) the instant after Audit is called, and
// the entry must still be written. Without context.WithoutCancel this silently drops
// every entry under real traffic while passing any test that keeps its context alive.
func TestAudit_SurvivesCallerCancellation(t *testing.T) {
	rec, srv := newAuditRecorder(http.StatusAccepted)
	defer srv.Close()

	c := &Client{URL: srv.URL, Service: "forge", ServiceKey: func() string { return "k" }}
	ctx, cancel := context.WithCancel(context.Background())
	c.Audit(ctx, "exec", "exec-9", "", "")
	cancel() // the handler returns here

	if !waitFor(2*time.Second, func() bool { return rec.count() == 1 }) {
		t.Fatal("entry was dropped when the caller's context was cancelled")
	}
}

// TestAudit_NoOpWithoutConfig covers the inert cases. A service that never wired a
// key (or a test that never set a URL) must not panic, block, or emit requests.
func TestAudit_NoOpWithoutConfig(t *testing.T) {
	rec, srv := newAuditRecorder(http.StatusAccepted)
	defer srv.Close()

	for name, c := range map[string]*Client{
		"no service key": {URL: srv.URL, Service: "forge"},
		"no url":         {Service: "forge", ServiceKey: func() string { return "k" }},
		"nil client":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			c.Audit(context.Background(), "exec", "exec-1", "", "")
		})
	}
	// Give any (incorrectly) spawned goroutine time to land before asserting silence.
	time.Sleep(100 * time.Millisecond)
	if n := rec.count(); n != 0 {
		t.Errorf("unconfigured clients sent %d requests, want 0", n)
	}
}

// TestAudit_DropsIncompleteEntries — gatekeeper 400s on these, so sending them would
// turn a programming error into recurring warning noise rather than a fixed bug.
func TestAudit_DropsIncompleteEntries(t *testing.T) {
	rec, srv := newAuditRecorder(http.StatusAccepted)
	defer srv.Close()

	c := &Client{URL: srv.URL, Service: "forge", ServiceKey: func() string { return "k" }}
	c.Audit(context.Background(), "", "exec-1", "", "") // no action
	c.Audit(context.Background(), "exec", "", "", "")   // no resource id
	c.Audit(context.Background(), "  ", "  ", "", "")   // whitespace only

	time.Sleep(100 * time.Millisecond)
	if n := rec.count(); n != 0 {
		t.Errorf("sent %d incomplete entries, want 0", n)
	}
}

// TestAudit_TruncatesLongDetail keeps an over-long detail from being trimmed
// server-side, which would otherwise ship a payload only to have it cut on arrival.
func TestAudit_TruncatesLongDetail(t *testing.T) {
	rec, srv := newAuditRecorder(http.StatusAccepted)
	defer srv.Close()

	c := &Client{URL: srv.URL, Service: "forge", ServiceKey: func() string { return "k" }}
	c.Audit(context.Background(), "exec", "exec-1", "", strings.Repeat("x", maxAuditDetailLen+500))

	if !waitFor(2*time.Second, func() bool { return rec.count() == 1 }) {
		t.Fatal("audit entry never arrived")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.entry.Detail) != maxAuditDetailLen {
		t.Errorf("detail length = %d, want %d", len(rec.entry.Detail), maxAuditDetailLen)
	}
}

// TestAudit_RejectionDoesNotPanic — a gatekeeper that is down or rejects the entry
// must cost a dropped record and a log line, never the caller's operation.
func TestAudit_RejectionDoesNotPanic(t *testing.T) {
	rec, srv := newAuditRecorder(http.StatusInternalServerError)
	defer srv.Close()

	c := &Client{URL: srv.URL, Service: "forge", ServiceKey: func() string { return "k" }}
	c.Audit(context.Background(), "exec", "exec-1", "", "")

	if !waitFor(2*time.Second, func() bool { return rec.count() == 1 }) {
		t.Fatal("audit entry never arrived")
	}
	// Nothing to assert beyond "we got here": the caller is unaffected by the 500.
}
