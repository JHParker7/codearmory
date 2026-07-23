package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeriveRepoPath(t *testing.T) {
	ok := map[string][2]string{
		"https://host:3000/jp01/codearmory.git":   {"jp01", "codearmory"},
		"http://h/owner/repo":                     {"owner", "repo"},
		"https://gl.example.com/grp/sub/proj.git": {"grp", "proj"}, // nested: first + last
	}
	for in, want := range ok {
		ns, name, err := deriveRepoPath(in)
		if err != nil || ns != want[0] || name != want[1] {
			t.Errorf("deriveRepoPath(%q) = (%q,%q,%v), want (%q,%q,nil)", in, ns, name, err, want[0], want[1])
		}
	}
	for _, bad := range []string{"https://host/onlyowner", "https://host/", "not a url\n"} {
		if _, _, err := deriveRepoPath(bad); err == nil {
			t.Errorf("deriveRepoPath(%q) = nil error, want error", bad)
		}
	}
}

// gitFactoryStub records what the broker sent and answers the two internal endpoints.
type gitFactoryStub struct {
	server      *httptest.Server
	gotKey      string
	gotUpstream string
	mirrorCalls int
	tokenCalls  int
}

func newGitFactoryStub(t *testing.T, cloneURL string) *gitFactoryStub {
	t.Helper()
	s := &gitFactoryStub{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.gotKey = r.Header.Get("X-Internal-Key")
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		switch r.URL.Path {
		case "/internal/mirrors":
			s.mirrorCalls++
			if u, ok := m["upstream_url"].(string); ok {
				s.gotUpstream = u
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": "mirror-1"})
		case "/internal/clone-token":
			s.tokenCalls++
			writeJSON(w, http.StatusOK, map[string]any{"clone_url": cloneURL})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

// withGitFactory points the package globals at the stub and restores them after.
func withGitFactory(t *testing.T, url, key string) {
	t.Helper()
	pu, pk := gitFactoryURL, gitFactoryKey
	gitFactoryURL, gitFactoryKey = url, key
	t.Cleanup(func() { gitFactoryURL, gitFactoryKey = pu, pk })
}

func seedMirrorBackend(t *testing.T, owner, host string, prefer bool) {
	t.Helper()
	b := GitBackend{
		ID: owner + "-" + host, Owner: owner, Name: "be-" + host, Type: "generic",
		BaseURL: "https://" + host, Host: host, AuthMode: "basic", PreferMirror: prefer,
	}
	if err := b.Add(context.Background()); err != nil {
		t.Fatalf("seed backend: %v", err)
	}
}

// A PreferMirror backend routes the internal clone through git-factory: the upstream
// (authenticated) URL is handed to /internal/mirrors, and the returned URL is the mirror
// clone token, not upstream.
func TestMaybeMirrorCloneURL_PreferMirror(t *testing.T) {
	stub := newGitFactoryStub(t, "https://git:cgfct1.tok@factory.svc/jp01/repo.git")
	withGitFactory(t, stub.server.URL, "factory-key")
	seedMirrorBackend(t, "ci-a", "mirror-a.example.com", true)

	upstream := credential{CloneURL: "https://x-token:secret@mirror-a.example.com/jp01/repo.git"}
	got, _ := maybeMirrorCloneURL(context.Background(), "ci-a", "https://mirror-a.example.com/jp01/repo.git", upstream)

	if got != "https://git:cgfct1.tok@factory.svc/jp01/repo.git" {
		t.Fatalf("clone url = %q, want the git-factory mirror URL", got)
	}
	if stub.gotUpstream != upstream.CloneURL {
		t.Errorf("git-factory got upstream_url %q, want the authenticated upstream %q", stub.gotUpstream, upstream.CloneURL)
	}
	if stub.gotKey != "factory-key" {
		t.Errorf("git-factory got key %q, want factory-key", stub.gotKey)
	}
	if stub.mirrorCalls != 1 || stub.tokenCalls != 1 {
		t.Errorf("calls: mirrors=%d token=%d, want 1/1", stub.mirrorCalls, stub.tokenCalls)
	}
}

// A backend that did NOT opt in is never mirrored — upstream is used (result "").
func TestMaybeMirrorCloneURL_OptOutStaysUpstream(t *testing.T) {
	stub := newGitFactoryStub(t, "https://should/not/be/used.git")
	withGitFactory(t, stub.server.URL, "factory-key")
	seedMirrorBackend(t, "ci-b", "mirror-b.example.com", false)

	got, _ := maybeMirrorCloneURL(context.Background(), "ci-b", "https://mirror-b.example.com/jp01/repo.git",
		credential{CloneURL: "https://u:p@mirror-b.example.com/jp01/repo.git"})
	if got != "" {
		t.Fatalf("clone url = %q, want empty (opt-out uses upstream)", got)
	}
	if stub.mirrorCalls != 0 {
		t.Errorf("git-factory was called %d times for an opt-out backend, want 0", stub.mirrorCalls)
	}
}

// With git-factory unconfigured the feature is inert regardless of the opt-in flag.
func TestMaybeMirrorCloneURL_NotConfigured(t *testing.T) {
	withGitFactory(t, "", "")
	seedMirrorBackend(t, "ci-c", "mirror-c.example.com", true)
	got, _ := maybeMirrorCloneURL(context.Background(), "ci-c", "https://mirror-c.example.com/jp01/repo.git",
		credential{CloneURL: "https://u:p@mirror-c.example.com/jp01/repo.git"})
	if got != "" {
		t.Fatalf("clone url = %q, want empty when git-factory is unconfigured", got)
	}
}
