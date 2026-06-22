package main

import (
	"net/http/httptest"
	"testing"
)

func TestAuthedCloneURL(t *testing.T) {
	got, err := authedCloneURL("http://forgejo:3000", "alice", "tok123", "acme", "widgets")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "http://alice:tok123@forgejo:3000/acme/widgets.git"
	if got != want {
		t.Errorf("authedCloneURL = %q, want %q", got, want)
	}
}

func TestInternalKeyOK(t *testing.T) {
	old := giteaInternalKey
	defer func() { giteaInternalKey = old }()

	// Unset key rejects everything (deny-by-default).
	giteaInternalKey = ""
	r := httptest.NewRequest("POST", "/internal/clone-token", nil)
	r.Header.Set("X-Internal-Key", "anything")
	if internalKeyOK(r) {
		t.Error("internalKeyOK should be false when key is unset")
	}

	giteaInternalKey = "s3cret"
	match := httptest.NewRequest("POST", "/internal/clone-token", nil)
	match.Header.Set("X-Internal-Key", "s3cret")
	if !internalKeyOK(match) {
		t.Error("internalKeyOK should be true on exact match")
	}

	mismatch := httptest.NewRequest("POST", "/internal/clone-token", nil)
	mismatch.Header.Set("X-Internal-Key", "wrong")
	if internalKeyOK(mismatch) {
		t.Error("internalKeyOK should be false on mismatch")
	}
}
