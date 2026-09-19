package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// putTicket issues a PUT as the ticket's owner, optionally conditional.
func putTicket(t *testing.T, tk Ticket, body string, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"`+tk.CreatedBy+`","org_id":""}`)
	r := httptest.NewRequest(http.MethodPut, "/tickets/"+tk.TicketID, bytes.NewBufferString(body))
	r.SetPathValue("id", tk.TicketID)
	r.Header.Set("Authorization", "Bearer t")
	r.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	w := httptest.NewRecorder()
	handleUpdateTicket(w, r)
	return w
}

func getTicketHTTP(t *testing.T, tk Ticket) *httptest.ResponseRecorder {
	t.Helper()
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"`+tk.CreatedBy+`","org_id":""}`)
	r := httptest.NewRequest(http.MethodGet, "/tickets/"+tk.TicketID, nil)
	r.SetPathValue("id", tk.TicketID)
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleGetTicket(w, r)
	return w
}

func currentVersion(t *testing.T, id string) int64 {
	t.Helper()
	tk, err := getTicket(context.Background(), id)
	if err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	return tk.Version
}

// A client needs a token to send back, so every response carrying a whole
// ticket must supply one.
func TestGetReturnsETag(t *testing.T) {
	requireDB(t)
	tk := seedTicketFor(t, "alice", "", "alice")

	w := getTicketHTTP(t, tk)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", w.Code, w.Body.String())
	}
	if got, want := w.Header().Get("ETag"), etagFor(tk.Version); got != want {
		t.Errorf("ETag = %q, want %q", got, want)
	}
}

// Every write advances the version, including unconditional ones — otherwise a
// conditional client would not notice a legacy client's overwrite.
func TestVersionAdvancesOnEveryWrite(t *testing.T) {
	requireDB(t)
	tk := seedTicketFor(t, "alice", "", "alice")
	start := currentVersion(t, tk.TicketID)

	for i := range 3 {
		if w := putTicket(t, tk, `{"title":"v"}`, ""); w.Code != http.StatusOK {
			t.Fatalf("unconditional PUT %d = %d: %s", i, w.Code, w.Body.String())
		}
	}
	if got := currentVersion(t, tk.TicketID); got != start+3 {
		t.Errorf("version = %d after 3 writes, want %d", got, start+3)
	}
}

// The whole point: a conditional write on a stale version must be refused
// rather than silently overwriting the other writer.
func TestIfMatchRejectsStaleWrite(t *testing.T) {
	requireDB(t)
	tk := seedTicketFor(t, "alice", "", "alice")
	stale := currentVersion(t, tk.TicketID)

	// Someone else writes first.
	if w := putTicket(t, tk, `{"title":"theirs"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("first write = %d", w.Code)
	}

	w := putTicket(t, tk, `{"title":"mine"}`, etagFor(stale))
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale conditional PUT = %d, want 412: %s", w.Code, w.Body.String())
	}

	// The other writer's value must survive — that is the data loss this prevents.
	after, _ := getTicket(context.Background(), tk.TicketID)
	if after.Title != "theirs" {
		t.Errorf("title = %q, want %q: the rejected write was applied anyway", after.Title, "theirs")
	}
	// And the client must be told what to rebase onto, not merely that it lost.
	if got, want := w.Header().Get("ETag"), etagFor(after.Version); got != want {
		t.Errorf("412 ETag = %q, want the current version %q", got, want)
	}
}

func TestIfMatchAcceptsCurrentVersion(t *testing.T) {
	requireDB(t)
	tk := seedTicketFor(t, "alice", "", "alice")
	v := currentVersion(t, tk.TicketID)

	w := putTicket(t, tk, `{"title":"mine"}`, etagFor(v))
	if w.Code != http.StatusOK {
		t.Fatalf("conditional PUT on the current version = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got, want := w.Header().Get("ETag"), etagFor(v+1); got != want {
		t.Errorf("response ETag = %q, want the new version %q", got, want)
	}
}

// Backwards compatibility is the reason this is safe to ship: every existing
// client omits If-Match and must be unaffected.
func TestUnconditionalWritesStillWin(t *testing.T) {
	requireDB(t)
	tk := seedTicketFor(t, "alice", "", "alice")
	stale := currentVersion(t, tk.TicketID)

	if w := putTicket(t, tk, `{"title":"first"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d", w.Code)
	}
	// The same stale caller, now unconditional, still overwrites — last-write-wins.
	if w := putTicket(t, tk, `{"title":"second"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("stale unconditional PUT = %d, want 200 (behaviour must be unchanged)", w.Code)
	}
	after, _ := getTicket(context.Background(), tk.TicketID)
	if after.Title != "second" {
		t.Errorf("title = %q, want second", after.Title)
	}
	_ = stale
}

// "If-Match: *" means "the resource must exist" and does not pin a version.
func TestIfMatchStarSucceeds(t *testing.T) {
	requireDB(t)
	tk := seedTicketFor(t, "alice", "", "alice")
	if w := putTicket(t, tk, `{"title":"x"}`, "*"); w.Code != http.StatusOK {
		t.Fatalf("If-Match: * = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// A malformed condition must be rejected, never ignored: silently dropping it
// turns a request that asked for safety into a last-write-wins overwrite.
func TestMalformedIfMatchIsRejected(t *testing.T) {
	requireDB(t)
	tk := seedTicketFor(t, "alice", "", "alice")
	before, _ := getTicket(context.Background(), tk.TicketID)

	for _, bad := range []string{`"abc"`, `garbage`, `W/"3"`} {
		w := putTicket(t, tk, `{"title":"sneaky"}`, bad)
		if w.Code != http.StatusBadRequest {
			t.Errorf("If-Match: %s = %d, want 400", bad, w.Code)
		}
	}
	after, _ := getTicket(context.Background(), tk.TicketID)
	if after.Version != before.Version {
		t.Error("a malformed If-Match still wrote to the ticket")
	}
}

func TestParseIfMatch(t *testing.T) {
	mk := func(v string) http.Header {
		h := http.Header{}
		if v != "" {
			h.Set("If-Match", v)
		}
		return h
	}
	if got, ok := parseIfMatch(mk("")); !ok || got.present {
		t.Errorf("absent header = %+v, ok=%v; want an unconditional request", got, ok)
	}
	if got, ok := parseIfMatch(mk("*")); !ok || !got.any {
		t.Errorf(`"*" = %+v, ok=%v; want any=true`, got, ok)
	}
	if got, ok := parseIfMatch(mk(`"7"`)); !ok || got.version != 7 {
		t.Errorf(`"7" = %+v, ok=%v; want version 7`, got, ok)
	}
	// Unquoted is tolerated: clients hand-rolling the header get it wrong often
	// enough that failing them buys nothing.
	if got, ok := parseIfMatch(mk("7")); !ok || got.version != 7 {
		t.Errorf("unquoted 7 = %+v, ok=%v", got, ok)
	}
	// A list picks the first usable entry.
	if got, ok := parseIfMatch(mk(`W/"1", "9"`)); !ok || got.version != 9 {
		t.Errorf("list = %+v, ok=%v; want 9", got, ok)
	}
	for _, bad := range []string{"abc", `"x"`, `W/"3"`} {
		if _, ok := parseIfMatch(mk(bad)); ok {
			t.Errorf("parseIfMatch(%q) accepted a value it cannot honour", bad)
		}
	}
}

// The property that matters under real contention: many concurrent conditional
// writers on one version, and exactly one may win.
func TestConcurrentConditionalWritesHaveOneWinner(t *testing.T) {
	requireDB(t)
	tk := seedTicketFor(t, "alice", "", "alice")
	v := currentVersion(t, tk.TicketID)

	const writers = 8
	var wg sync.WaitGroup
	results := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cp := tk
			cp.Title = "writer"
			results[i] = cp.UpdateIfVersion(context.Background(), v)
		}()
	}
	wg.Wait()

	won := 0
	for _, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrVersionConflict):
			// Lost the race, which is the outcome under test.
		case strings.Contains(err.Error(), "locked"):
			// The suite runs on in-memory sqlite, which serialises writers and
			// returns "table is locked" instead of letting them contend. That is
			// a harness limitation, not a behaviour: on Postgres these arrive as
			// version conflicts. Either way the writer did not win, so the
			// invariant below still holds.
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d of %d concurrent conditional writes succeeded, want exactly 1", won, writers)
	}
	// The invariant that actually matters, and the one sqlite cannot weaken:
	// exactly one write landed, so the version moved by exactly one.
	if got := currentVersion(t, tk.TicketID); got != v+1 {
		t.Errorf("version = %d, want %d: more than one write landed", got, v+1)
	}
}

// The update path must not be able to move fields that identify or own the row.
func TestUpdateCannotMoveImmutableFields(t *testing.T) {
	requireDB(t)
	tk := seedTicketFor(t, "alice", "org-1", "alice")

	body, _ := json.Marshal(map[string]any{
		"title": "renamed", "created_by": "mallory", "org_id": "org-evil", "namespace": "mallory",
	})
	if w := putTicket(t, tk, string(body), ""); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}

	after, _ := getTicket(context.Background(), tk.TicketID)
	if after.CreatedBy != "alice" || after.OrgID != "org-1" || after.Namespace != "alice" {
		t.Errorf("ownership moved: created_by=%q org=%q ns=%q", after.CreatedBy, after.OrgID, after.Namespace)
	}
	if after.Title != "renamed" {
		t.Errorf("title = %q, want the legitimate change applied", after.Title)
	}
}
