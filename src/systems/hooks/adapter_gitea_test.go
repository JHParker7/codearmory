package main

import "testing"

// A real Forgejo/Gitea push payload (trimmed) must flatten to the normalized fields the
// rule engine matches on. The shape mirrors GitHub's, with pusher as a User object.
func TestNormalizeGiteaPayload_Push(t *testing.T) {
	body := []byte(`{
		"ref": "refs/heads/main",
		"head_commit": {"id": "abc123def456", "message": "fix: the thing"},
		"pusher": {"username": "jp01", "login": "jp01"},
		"repository": {"full_name": "jp01/codearmory"}
	}`)
	p, ok := normalizeGiteaPayload("push", body)
	if !ok {
		t.Fatal("push payload should parse")
	}
	if p.Repo != "jp01/codearmory" {
		t.Errorf("repo = %q", p.Repo)
	}
	// The ref is stripped to the bare branch so a rule ref_filter of "main" matches.
	if p.Ref != "main" {
		t.Errorf("ref = %q, want main (refs/heads/ stripped)", p.Ref)
	}
	if p.Commit != "abc123def456" {
		t.Errorf("commit = %q, want the head_commit id (checkout builds this)", p.Commit)
	}
	if p.Event != "push" || p.Pusher != "jp01" {
		t.Errorf("event=%q pusher=%q", p.Event, p.Pusher)
	}
}

// Gitea sends pusher as {username}; if only login is present it must still populate.
func TestNormalizeGiteaPayload_PusherLoginFallback(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/x","repository":{"full_name":"a/b"},"pusher":{"login":"only-login"}}`)
	p, ok := normalizeGiteaPayload("push", body)
	if !ok || p.Pusher != "only-login" {
		t.Errorf("pusher fallback failed: ok=%v pusher=%q", ok, p.Pusher)
	}
}

// A tag push strips refs/tags/ too, so it doesn't leak the full ref into a branch filter.
func TestNormalizeGiteaPayload_TagRef(t *testing.T) {
	body := []byte(`{"ref":"refs/tags/v1.2.3","repository":{"full_name":"a/b"}}`)
	p, _ := normalizeGiteaPayload("push", body)
	if p.Ref != "v1.2.3" {
		t.Errorf("ref = %q, want v1.2.3", p.Ref)
	}
}

// An event we don't translate is ignored (ok=false), so the handler acknowledges it
// rather than failing the delivery and making Forgejo retry.
func TestNormalizeGiteaPayload_UnsupportedEvent(t *testing.T) {
	if _, ok := normalizeGiteaPayload("release", []byte(`{}`)); ok {
		t.Error("release must not be translated")
	}
}

func TestStripRef(t *testing.T) {
	for in, want := range map[string]string{
		"refs/heads/main": "main", "refs/tags/v1": "v1", "main": "main",
	} {
		if got := stripRef(in); got != want {
			t.Errorf("stripRef(%q) = %q, want %q", in, got, want)
		}
	}
}
