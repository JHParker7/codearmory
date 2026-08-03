package main

import (
	"encoding/json"
	"testing"
)

// The adapters' whole job is turning provider-shaped payloads into one envelope. A field that
// silently normalizes to "" does not fail — it produces an event no filter matches, which
// looks identical to "nothing happened".

func TestStripRef(t *testing.T) {
	cases := map[string]string{
		"refs/heads/main":       "main",
		"refs/heads/feat/x":     "feat/x",
		"refs/tags/v1.2.3":      "v1.2.3",
		"main":                  "main", // already bare
		"":                      "",
		"refs/remotes/upstream": "refs/remotes/upstream", // not a branch or tag: left alone
	}
	for in, want := range cases {
		if got := stripRef(in); got != want {
			t.Errorf("stripRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeGiteaEvent_Push(t *testing.T) {
	body := []byte(`{
	  "ref": "refs/heads/main",
	  "head_commit": {"id": "abc123", "message": "fix the thing"},
	  "pusher": {"username": "alice"},
	  "repository": {"full_name": "acme/app"}
	}`)
	e, ok := normalizeGiteaEvent("push", body)
	if !ok {
		t.Fatal("push was not translated")
	}
	if e.Type != "repo.push" {
		t.Errorf("type = %q, want repo.push", e.Type)
	}
	if e.Subject != "acme/app" {
		t.Errorf("subject = %q, want acme/app", e.Subject)
	}
	if e.Data["ref"] != "main" {
		t.Errorf("data.ref = %v, want main (refs/heads/ stripped)", e.Data["ref"])
	}
	if e.Data["commit"] != "abc123" || e.Data["pusher"] != "alice" {
		t.Errorf("data = %v, want the head commit and pusher carried through", e.Data)
	}
}

// Gitea sends `pusher.username`; GitHub-compatible payloads may send `login` instead. Reading
// only one leaves the pusher empty on the other.
func TestNormalizeGiteaEvent_PusherLoginFallback(t *testing.T) {
	body := []byte(`{
	  "ref": "refs/heads/dev",
	  "head_commit": {"id": "d1"},
	  "pusher": {"login": "bob"},
	  "repository": {"full_name": "acme/app"}
	}`)
	e, ok := normalizeGiteaEvent("push", body)
	if !ok {
		t.Fatal("push was not translated")
	}
	if e.Data["pusher"] != "bob" {
		t.Errorf("data.pusher = %v, want bob from the login fallback", e.Data["pusher"])
	}
}

func TestNormalizeGiteaEvent_PullRequestCarriesAction(t *testing.T) {
	body := []byte(`{
	  "action": "opened",
	  "pull_request": {
	    "head": {"ref": "feature", "sha": "cafe01"},
	    "user": {"username": "carol"},
	    "title": "Add thing"
	  },
	  "repository": {"full_name": "acme/app"}
	}`)
	e, ok := normalizeGiteaEvent("pull_request", body)
	if !ok {
		t.Fatal("pull_request was not translated")
	}
	// The action is part of the type so a filter can target "opened" without a second condition.
	if e.Type != "repo.pull_request.opened" {
		t.Errorf("type = %q, want repo.pull_request.opened", e.Type)
	}
	if e.Data["ref"] != "feature" || e.Data["commit"] != "cafe01" {
		t.Errorf("data = %v, want the head ref and sha", e.Data)
	}
}

func TestNormalizeGiteaEvent_Rejects(t *testing.T) {
	cases := []struct {
		name, eventType string
		body            string
	}{
		{"unknown type", "release", `{"repository":{"full_name":"a/b"}}`},
		{"malformed json", "push", `{not json`},
		// Without a repo there is no subject, so the event would be unaddressable.
		{"missing repository", "push", `{"ref":"refs/heads/main"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := normalizeGiteaEvent(c.eventType, []byte(c.body)); ok {
				t.Error("expected the payload to be rejected")
			}
		})
	}
}

func TestNormalizeGitHubEvent_PushAndInstallation(t *testing.T) {
	body := []byte(`{
	  "installation": {"id": 4242},
	  "ref": "refs/tags/v2",
	  "head_commit": {"id": "beef01", "message": "release"},
	  "pusher": {"name": "dave"},
	  "repository": {"full_name": "acme/app"}
	}`)
	e, installID, ok := normalizeGitHubEvent("push", body)
	if !ok {
		t.Fatal("push was not translated")
	}
	// The installation id is what mints the token for the check run; losing it silently
	// disables reporting back to GitHub.
	if installID != 4242 {
		t.Errorf("installation id = %d, want 4242", installID)
	}
	if e.Source != "github" {
		t.Errorf("source = %q, want github", e.Source)
	}
	if e.Data["ref"] != "v2" {
		t.Errorf("data.ref = %v, want v2 (refs/tags/ stripped)", e.Data["ref"])
	}
	if e.Data["pusher"] != "dave" {
		t.Errorf("data.pusher = %v, want dave from GitHub's {name} shape", e.Data["pusher"])
	}
}

func TestNormalizeGitHubEvent_NoInstallation(t *testing.T) {
	body := []byte(`{
	  "ref": "refs/heads/main",
	  "head_commit": {"id": "a1"},
	  "repository": {"full_name": "acme/app"}
	}`)
	_, installID, ok := normalizeGitHubEvent("push", body)
	if !ok {
		t.Fatal("push was not translated")
	}
	if installID != 0 {
		t.Errorf("installation id = %d, want 0 when absent", installID)
	}
}

func TestNormalizeGitHubEvent_Rejects(t *testing.T) {
	if _, _, ok := normalizeGitHubEvent("issues", []byte(`{"repository":{"full_name":"a/b"}}`)); ok {
		t.Error("an untranslated event type was accepted")
	}
	if _, _, ok := normalizeGitHubEvent("push", []byte(`{"ref":"refs/heads/main"}`)); ok {
		t.Error("a payload with no repository was accepted")
	}
}

// tenantFromQuery is the only thing binding a provider webhook to a codearmory tenant, so an
// event with neither field must not silently pass validation.
func TestTenantFromQuery(t *testing.T) {
	e := Event{Type: "repo.push", Source: "gitea", Subject: "a/b"}
	if msg := validateEvent(&e); msg == "" {
		t.Error("an event with no actor was accepted; it would belong to no tenant")
	}
	e.Actor = Actor{UserID: "u-1"}
	if msg := validateEvent(&e); msg != "" {
		t.Errorf("validateEvent = %q, want valid once a tenant is set", msg)
	}
}

// The check-run observer must fire only for run_pipeline outcomes that produced a run id —
// otherwise it would open a check run pointing at nothing.
func TestCheckRunObserver_SkipsWithoutRunID(t *testing.T) {
	app := &githubApp{tokenCache: map[int64]instToken{}}
	obs := app.checkRunObserver("acme/app", "sha1", 0) // installation 0 = nothing to report to
	// Must not panic or dial anywhere.
	obs(t.Context(), Trigger{Name: "t"}, Event{}, []actionOutcome{{Kind: "run_pipeline", RunID: "r1"}})

	obs = app.checkRunObserver("acme/app", "", 42) // no head sha to attach to
	obs(t.Context(), Trigger{Name: "t"}, Event{}, []actionOutcome{{Kind: "run_pipeline", RunID: "r1"}})
}

// gitPushData is the shared payload shape: a filter written against data.ref must work the
// same whichever adapter produced the event.
func TestGitPushData_IsStableAcrossAdapters(t *testing.T) {
	gitea, _ := normalizeGiteaEvent("push", []byte(`{
	  "ref":"refs/heads/main","head_commit":{"id":"c1","message":"m"},
	  "pusher":{"username":"u"},"repository":{"full_name":"a/b"}}`))
	github, _, _ := normalizeGitHubEvent("push", []byte(`{
	  "ref":"refs/heads/main","head_commit":{"id":"c1","message":"m"},
	  "pusher":{"name":"u"},"repository":{"full_name":"a/b"}}`))

	g1, _ := json.Marshal(gitea.Data)
	g2, _ := json.Marshal(github.Data)
	if string(g1) != string(g2) {
		t.Errorf("payload shapes diverge:\n gitea:  %s\n github: %s", g1, g2)
	}
}
