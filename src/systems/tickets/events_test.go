package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkevents "github.com/code-armory-app/codearmory_sdk/events"
)

func TestEventsEnabled(t *testing.T) {
	orig := eventEmitter
	defer func() { eventEmitter = orig }()

	cases := []struct {
		url, key string
		want     bool
	}{
		{"", "", false},
		{"http://events:8087", "", false},
		{"", "k", false},
		{"http://events:8087", "k", true},
	}
	for _, tc := range cases {
		eventEmitter = sdkevents.New(tc.url, tc.key, ticketsEventSource, nil)
		if got := eventsEnabled(); got != tc.want {
			t.Fatalf("eventsEnabled(url=%q,key=%q) = %v, want %v", tc.url, tc.key, got, tc.want)
		}
	}
}

func TestTicketEventFields(t *testing.T) {
	assignee := "bob"
	wf := "wf-1"
	tk := Ticket{
		TicketID:   "t-1",
		Title:      "Fix login",
		Status:     StatusInProgress,
		Priority:   PriorityHigh,
		Timescale:  "this_week",
		CreatedBy:  "alice",
		OrgID:      "org-1",
		AssigneeID: &assignee,
		WorkflowID: &wf,
	}
	f := ticketEventFields(tk)

	want := map[string]any{
		"ticket_id":   "t-1",
		"title":       "Fix login",
		"status":      StatusInProgress,
		"priority":    PriorityHigh,
		"timescale":   "this_week",
		"created_by":  "alice",
		"org_id":      "org-1",
		"assignee_id": "bob",
		"workflow_id": "wf-1",
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("field %q = %v, want %v", k, f[k], v)
		}
	}
	// run_id was nil, so it must be absent.
	if _, ok := f["run_id"]; ok {
		t.Error("run_id should be absent when nil")
	}
}

// The emitted token must match the scheme the events service verifies, or every ticket event
// is silently rejected with a 401.
func TestSignEvent_MatchesEventsVerification(t *testing.T) {
	ev := sdkevents.Event{
		ID: "evt-1", Type: eventTicketStatus, Source: ticketsEventSource, Subject: "t-1",
		Actor: sdkevents.Actor{OrgID: "org-1", UserID: "alice"},
		Data:  map[string]any{"status": StatusInProgress},
	}
	ts := "1700000000"

	payload, err := json.Marshal(ev.Data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	digest := sha256.Sum256(payload)
	mac := hmac.New(sha256.New, []byte("shared-secret"))
	fmt.Fprintf(mac, "event:%s:%s:%s:%s:%s:%s:%s:%s",
		ev.ID, ev.Type, ev.Source, ev.Subject, "org-1", "alice", hex.EncodeToString(digest[:]), ts)

	if want, got := hex.EncodeToString(mac.Sum(nil)), sdkevents.Sign("shared-secret", ev, ts); got != want {
		t.Fatalf("token = %q, want %q", got, want)
	}
}

// The ticket's tenant must survive onto the envelope: a trigger only ever sees events of its
// own tenant, so a dropped org_id would make the event unmatchable.
func TestNotifyEvents_CarriesTenantAndPayload(t *testing.T) {
	received := make(chan sdkevents.Event, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev sdkevents.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		received <- ev
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	orig := eventEmitter
	eventEmitter = sdkevents.New(srv.URL, "k", ticketsEventSource, srv.Client())
	defer func() { eventEmitter = orig }()

	tk := Ticket{TicketID: "t-9", Status: StatusOpen, CreatedBy: "alice", OrgID: "org-1"}
	notifyEvents(t.Context(), eventTicketStatus, tk, map[string]any{"old_status": StatusInProgress})

	ev := <-received
	if ev.Type != eventTicketStatus {
		t.Errorf("type = %q, want %q", ev.Type, eventTicketStatus)
	}
	if ev.Subject != "t-9" {
		t.Errorf("subject = %q, want the ticket id", ev.Subject)
	}
	if ev.Actor.OrgID != "org-1" || ev.Actor.UserID != "alice" {
		t.Errorf("actor = %+v, want org-1/alice", ev.Actor)
	}
	// The status a caller used to pass as the `ref` discriminator is now just a payload field
	// a filter addresses directly.
	if ev.Data["status"] != StatusOpen {
		t.Errorf("data.status = %v, want %q", ev.Data["status"], StatusOpen)
	}
	if ev.Data["old_status"] != StatusInProgress {
		t.Errorf("data.old_status = %v, want %q", ev.Data["old_status"], StatusInProgress)
	}
}

// notifyEvents must be a silent no-op when the integration is unconfigured — notably it must
// not attempt any network call.
func TestNotifyEvents_DisabledNoop(t *testing.T) {
	orig := eventEmitter
	eventEmitter = sdkevents.New("", "", ticketsEventSource, nil)
	defer func() { eventEmitter = orig }()

	// Should return immediately without panicking or dialing anywhere.
	notifyEvents(t.Context(), eventTicketCreated, Ticket{TicketID: "t-1"}, nil)
}
