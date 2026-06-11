package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

// signInternalEvent reproduces the token the tickets service sends.
func signInternalEvent(key, source, event, ts string) string {
	mac := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(mac, "event:%s:%s:%s", source, event, ts)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyInternalEvent_NoKey(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = ""
	defer func() { hooksTriggerKey = orig }()
	if verifyInternalEvent("tickets", "ticket.status_changed", "tok", "123") {
		t.Fatal("expected false when key is empty")
	}
}

func TestVerifyInternalEvent_InvalidTimestamp(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = "secret"
	defer func() { hooksTriggerKey = orig }()
	if verifyInternalEvent("tickets", "ticket.created", "tok", "not-a-number") {
		t.Fatal("expected false for non-numeric timestamp")
	}
}

func TestVerifyInternalEvent_Expired(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = "secret"
	defer func() { hooksTriggerKey = orig }()
	oldTS := fmt.Sprintf("%d", time.Now().Unix()-60)
	tok := signInternalEvent("secret", "tickets", "ticket.created", oldTS)
	if verifyInternalEvent("tickets", "ticket.created", tok, oldTS) {
		t.Fatal("expected false for timestamp older than 30 s")
	}
}

func TestVerifyInternalEvent_WrongSignature(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = "secret"
	defer func() { hooksTriggerKey = orig }()
	ts := fmt.Sprintf("%d", time.Now().Unix())
	if verifyInternalEvent("tickets", "ticket.created", "deadbeef", ts) {
		t.Fatal("expected false for wrong signature")
	}
}

func TestVerifyInternalEvent_Valid(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = "test-key-123"
	defer func() { hooksTriggerKey = orig }()
	ts := fmt.Sprintf("%d", time.Now().Unix())
	tok := signInternalEvent(hooksTriggerKey, "tickets", "ticket.status_changed", ts)
	if !verifyInternalEvent("tickets", "ticket.status_changed", tok, ts) {
		t.Fatal("expected true for valid signature")
	}
}

// A token signed for one event must not validate a different event.
func TestVerifyInternalEvent_EventBound(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = "test-key-123"
	defer func() { hooksTriggerKey = orig }()
	ts := fmt.Sprintf("%d", time.Now().Unix())
	tok := signInternalEvent(hooksTriggerKey, "tickets", "ticket.created", ts)
	if verifyInternalEvent("tickets", "ticket.deleted", tok, ts) {
		t.Fatal("expected false when token's event differs from the request event")
	}
}

func TestRuleInScope(t *testing.T) {
	orgRule := PipelineRule{OrgID: "org-1", CreatedBy: "alice"}
	personalRule := PipelineRule{OrgID: "", CreatedBy: "alice"}

	cases := []struct {
		name   string
		rule   PipelineRule
		orgID  string
		userID string
		want   bool
	}{
		{"org event matches same-org rule", orgRule, "org-1", "anyone", true},
		{"org event rejects other-org rule", orgRule, "org-2", "anyone", false},
		{"org event rejects personal rule", personalRule, "org-1", "alice", false},
		{"personal event matches owner's personal rule", personalRule, "", "alice", true},
		{"personal event rejects other user's personal rule", personalRule, "", "bob", false},
		{"personal event rejects org rule", orgRule, "", "alice", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ruleInScope(tc.rule, tc.orgID, tc.userID); got != tc.want {
				t.Fatalf("ruleInScope(%+v, %q, %q) = %v, want %v", tc.rule, tc.orgID, tc.userID, got, tc.want)
			}
		})
	}
}
