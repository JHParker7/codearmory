package main

import "testing"

func TestInterpolate(t *testing.T) {
	evMap, _ := sampleEvent().asMap()
	cases := []struct{ in, want string }{
		{"{{ data.ref }}", "dev"},
		{"ref={{data.ref}} sha={{ data.commit }}", "ref=dev sha=9af3"},
		{"{{ subject }}", "jhparker7/codearmory_git_factory"},
		{"{{ actor.org_id }}", "org1"},
		{"{{ data.missing }}", ""},          // unknown path renders empty, never errors
		{"no templates here", "no templates here"},
	}
	for _, c := range cases {
		if got := interpolate(c.in, evMap); got != c.want {
			t.Errorf("interpolate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidateEventDefaultsAndErrors(t *testing.T) {
	// Fills id/spec_version/occurred_at when valid.
	e := Event{Type: "repo.push", Source: "git", Subject: "a/b", Actor: Actor{UserID: "u1"}}
	if msg := validateEvent(&e); msg != "" {
		t.Fatalf("expected valid, got %q", msg)
	}
	if e.ID == "" || e.SpecVersion != SpecVersion || e.OccurredAt == "" {
		t.Fatalf("defaults not filled: %+v", e)
	}

	bad := []Event{
		{Source: "git", Subject: "a/b", Actor: Actor{UserID: "u"}},   // no type
		{Type: "t", Subject: "a/b", Actor: Actor{UserID: "u"}},       // no source
		{Type: "t", Source: "git", Actor: Actor{UserID: "u"}},        // no subject
		{Type: "t", Source: "git", Subject: "a/b"},                   // no actor
	}
	for i, b := range bad {
		if msg := validateEvent(&b); msg == "" {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

func TestEventHMACRoundTrip(t *testing.T) {
	old := eventsTriggerKey
	eventsTriggerKey = "test-shared-key"
	defer func() { eventsTriggerKey = old }()

	e := sampleEvent()
	ts := "1700000000"
	tok := signEvent(e, ts)
	if !verifyEventToken(e, tok, ts) {
		t.Fatal("a freshly-signed token should verify")
	}
	if verifyEventToken(e, tok, "1700000001") {
		t.Fatal("a different timestamp must not verify")
	}
	if verifyEventToken(e, "deadbeef", ts) {
		t.Fatal("a bogus token must not verify")
	}
	eventsTriggerKey = ""
	if verifyEventToken(e, tok, ts) {
		t.Fatal("verification must fail closed when the key is unset")
	}
}
