package main

import (
	"testing"
	"time"
)

func TestRetryBackoff(t *testing.T) {
	want := []time.Duration{
		30 * time.Second,  // attempt 1
		60 * time.Second,  // 2
		120 * time.Second, // 3
		240 * time.Second, // 4
		480 * time.Second, // 5
		900 * time.Second, // 6 (cap)
		900 * time.Second, // 7 (still capped)
	}
	for i, w := range want {
		if got := retryBackoff(i + 1); got != w {
			t.Errorf("retryBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestParseConsumers(t *testing.T) {
	m := parseConsumers("chaos=http://chaos:8090, argo=http://argo:8091/ ,bad")
	if m["chaos"] != "http://chaos:8090" {
		t.Errorf("chaos consumer = %q", m["chaos"])
	}
	if m["argo"] != "http://argo:8091" { // trailing slash trimmed
		t.Errorf("argo consumer = %q", m["argo"])
	}
	if _, ok := m["bad"]; ok {
		t.Errorf("entry without '=' should be skipped")
	}
}

func TestSplitEnrollmentToken(t *testing.T) {
	id, secretPart, ok := splitEnrollmentToken("abc-123.deadbeef")
	if !ok || id != "abc-123" || secretPart != "deadbeef" {
		t.Errorf("split = (%q,%q,%v)", id, secretPart, ok)
	}
	for _, bad := range []string{"", "noseparator", ".leading", "trailing."} {
		if _, _, ok := splitEnrollmentToken(bad); ok {
			t.Errorf("splitEnrollmentToken(%q) should fail", bad)
		}
	}
}

func TestInternalHMACRoundTrip(t *testing.T) {
	prev := outpostInternalKey
	outpostInternalKey = "unit-test-shared-key"
	defer func() { outpostInternalKey = prev }()

	body := []byte(`{"outpost_id":"outpost-1","integration":"chaos","type":"run-experiment","org_id":"o1"}`)
	token, ts := signInternal("command", body)
	if !verifyInternal("command", body, token, ts) {
		t.Fatal("valid token failed verification")
	}
	if verifyInternal("event", body, token, ts) {
		t.Error("token verified against a different domain")
	}
	// Tampering with any body byte (e.g. swapping the org) must invalidate the MAC.
	tampered := []byte(`{"outpost_id":"outpost-1","integration":"chaos","type":"run-experiment","org_id":"o2"}`)
	if verifyInternal("command", tampered, token, ts) {
		t.Error("token verified against a tampered body")
	}
	if verifyInternal("command", body, token+"00", ts) {
		t.Error("tampered token verified")
	}
	// Stale timestamp outside the 30s window must fail.
	stale := time.Now().Add(-5 * time.Minute).Unix()
	staleToken, staleTS := signInternalAt("command", body, stale)
	if verifyInternal("command", body, staleToken, staleTS) {
		t.Error("stale token verified")
	}
}

// TestInternalHMACEmptyKeyRejects ensures an unconfigured key rejects everything.
func TestInternalHMACEmptyKeyRejects(t *testing.T) {
	prev := outpostInternalKey
	outpostInternalKey = ""
	defer func() { outpostInternalKey = prev }()
	if verifyInternal("command", []byte("x"), "anything", "0") {
		t.Error("empty key should reject all tokens")
	}
}
