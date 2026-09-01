package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestInterpolate(t *testing.T) {
	evMap, _ := sampleEvent().asMap()
	cases := []struct{ in, want string }{
		{"{{ data.ref }}", "dev"},
		{"ref={{data.ref}} sha={{ data.commit }}", "ref=dev sha=9af3"},
		{"{{ subject }}", "jhparker7/codearmory_git_factory"},
		{"{{ actor.org_id }}", "org1"},
		{"{{ data.missing }}", ""}, // unknown path renders empty, never errors
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
		{Source: "git", Subject: "a/b", Actor: Actor{UserID: "u"}}, // no type
		{Type: "t", Subject: "a/b", Actor: Actor{UserID: "u"}},     // no source
		{Type: "t", Source: "git", Actor: Actor{UserID: "u"}},      // no subject
		{Type: "t", Source: "git", Subject: "a/b"},                 // no actor
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
	now := time.Now().Unix()
	ts := strconv.FormatInt(now, 10)
	tok := signEvent(e, ts)
	if !verifyEventToken(e, tok, ts) {
		t.Fatal("a freshly-signed token should verify")
	}
	if verifyEventToken(e, tok, strconv.FormatInt(now+1, 10)) {
		t.Fatal("a different timestamp must not verify")
	}
	if verifyEventToken(e, "deadbeef", ts) {
		t.Fatal("a bogus token must not verify")
	}

	// Outside the freshness window a valid MAC is still refused, so a captured token
	// cannot be replayed later.
	stale := strconv.FormatInt(now-int64(eventTokenWindow/time.Second)-1, 10)
	if verifyEventToken(e, signEvent(e, stale), stale) {
		t.Fatal("a token older than the window must not verify")
	}
	future := strconv.FormatInt(now+int64(eventTokenWindow/time.Second)+1, 10)
	if verifyEventToken(e, signEvent(e, future), future) {
		t.Fatal("a token from beyond the window must not verify")
	}
	if verifyEventToken(e, tok, "not-a-timestamp") {
		t.Fatal("an unparseable timestamp must not verify")
	}

	eventsTriggerKey = ""
	if verifyEventToken(e, tok, ts) {
		t.Fatal("verification must fail closed when the key is unset")
	}
}

// The MAC must cover everything that decides what the event does — tenant, subject and
// payload — so a captured token cannot be re-aimed at another tenant or trigger.
func TestEventHMACBindsTenantSubjectAndData(t *testing.T) {
	old := eventsTriggerKey
	eventsTriggerKey = "test-shared-key"
	defer func() { eventsTriggerKey = old }()

	e := sampleEvent()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	tok := signEvent(e, ts)

	forgedOrg := sampleEvent()
	forgedOrg.Actor.OrgID = "org2"
	forgedUser := sampleEvent()
	forgedUser.Actor.UserID = "u2"
	forgedSubject := sampleEvent()
	forgedSubject.Subject = "jhparker7/other-repo"
	forgedData := sampleEvent()
	forgedData.Data["ref"] = "main"
	addedData := sampleEvent()
	addedData.Data["extra"] = "x"

	for name, forged := range map[string]Event{
		"org_id":  forgedOrg,
		"user_id": forgedUser,
		"subject": forgedSubject,
		"data":    forgedData,
		"data+":   addedData,
	} {
		if verifyEventToken(forged, tok, ts) {
			t.Errorf("a forged %s must not verify under the original token", name)
		}
	}

	// The data digest is order-independent: the same map re-created in another order signs
	// identically, so an emitter and events always agree.
	same := sampleEvent()
	same.Data = map[string]any{
		"pusher": "jhparker7", "commit": "9af3", "attempt": float64(3),
		"labels": []any{"ci", "backend"}, "ref": "dev",
	}
	if !verifyEventToken(same, tok, ts) {
		t.Error("the same data in a different map order must verify")
	}
}

func TestVerifyGitSignature(t *testing.T) {
	old := eventsWebhookSecret
	eventsWebhookSecret = "webhook-secret"
	defer func() { eventsWebhookSecret = old }()

	body := []byte(`{"repo":"a/b","event":"push","org_id":"org1"}`)
	mac := hmac.New(sha256.New, []byte(eventsWebhookSecret))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	github := http.Header{"X-Hub-Signature-256": {"sha256=" + sig}}
	if !verifyGitSignature(body, github) {
		t.Error("a GitHub-shaped signature should verify")
	}
	gitea := http.Header{"X-Gitea-Signature": {sig}}
	if !verifyGitSignature(body, gitea) {
		t.Error("a Gitea-shaped signature should verify")
	}
	if verifyGitSignature([]byte(`{"repo":"a/b","event":"push","org_id":"org2"}`), github) {
		t.Error("a tampered body must not verify")
	}
	if verifyGitSignature(body, http.Header{}) {
		t.Error("an unsigned request must not verify")
	}
	if verifyGitSignature(body, http.Header{"X-Gitea-Signature": {"deadbeef"}}) {
		t.Error("a bogus signature must not verify")
	}

	// Fail closed: with no secret configured the endpoint accepts nothing, signed or not.
	eventsWebhookSecret = ""
	if verifyGitSignature(body, github) {
		t.Error("verification must fail closed when no webhook secret is configured")
	}
}

func TestResolveSignSecretRejectsNonReferences(t *testing.T) {
	tr := Trigger{OrgID: "org1", CreatedBy: "u1"}
	// A plain string is a configuration error, never the signing key itself.
	for _, ref := range []string{"my-raw-secret", "secret:", "vault:name", ":name"} {
		if _, err := resolveSignSecret(context.Background(), tr, ref); err == nil {
			t.Errorf("sign_secret %q should be rejected", ref)
		}
	}
	// A well-formed reference is looked up — which fails here because no service key is
	// wired — but it must never fall back to using the reference as the key.
	if _, err := resolveSignSecret(context.Background(), tr, "secret:webhook-signing"); err == nil {
		t.Error("an unresolvable secret: reference must fail the action")
	}
}
