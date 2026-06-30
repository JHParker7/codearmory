package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"testing"
)

func TestHooksEnabled(t *testing.T) {
	origURL, origKey := hooksURL, hooksEventKey
	defer func() { hooksURL, hooksEventKey = origURL, origKey }()

	cases := []struct {
		url, key string
		want     bool
	}{
		{"", "", false},
		{"http://hooks:8087", "", false},
		{"", "k", false},
		{"http://hooks:8087", "k", true},
	}
	for _, tc := range cases {
		hooksURL, hooksEventKey = tc.url, tc.key
		if got := hooksEnabled(); got != tc.want {
			t.Fatalf("hooksEnabled(url=%q,key=%q) = %v, want %v", tc.url, tc.key, got, tc.want)
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

	want := map[string]string{
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
			t.Errorf("field %q = %q, want %q", k, f[k], v)
		}
	}
	// run_id was nil, so it must be absent.
	if _, ok := f["run_id"]; ok {
		t.Error("run_id should be absent when nil")
	}
}

// signHookEvent must produce a token that matches the hooks-side verification
// scheme: HMAC-SHA256 over "event:{source}:{event}:{timestamp}".
func TestSignHookEvent(t *testing.T) {
	orig := hooksEventKey
	hooksEventKey = "shared-secret"
	defer func() { hooksEventKey = orig }()

	token, ts := signHookEvent(ticketsHookSource, eventTicketStatus, "org-1", "alice")
	if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
		t.Fatalf("timestamp %q is not numeric: %v", ts, err)
	}

	mac := hmac.New(sha256.New, []byte(hooksEventKey))
	fmt.Fprintf(mac, "event:%s:%s:%s:%s:%s", ticketsHookSource, eventTicketStatus, "org-1", "alice", ts)
	want := hex.EncodeToString(mac.Sum(nil))
	if token != want {
		t.Fatalf("token = %q, want %q", token, want)
	}
}

// notifyHooks must be a silent no-op when the integration is unconfigured —
// notably it must not attempt any network call (httpClient is nil in tests).
func TestNotifyHooks_DisabledNoop(t *testing.T) {
	origURL, origKey := hooksURL, hooksEventKey
	hooksURL, hooksEventKey = "", ""
	defer func() { hooksURL, hooksEventKey = origURL, origKey }()

	// Should return immediately without panicking or dialing anywhere.
	notifyHooks(t.Context(), eventTicketCreated, StatusOpen, Ticket{TicketID: "t-1"}, nil)
}
