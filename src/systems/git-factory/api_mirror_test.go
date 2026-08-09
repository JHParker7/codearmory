package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeUpstream builds a real git repo with one commit on `main` and returns a
// fetchable path for it (the mirror fetches by URL/argv, and a local path is a valid
// git remote). It returns the path and the commit SHA so a test can assert the mirror
// actually pulled that object.
func makeUpstream(t *testing.T) (path, sha string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello mirror\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "initial")
	sha = run("rev-parse", "HEAD")
	return dir, sha
}

// postMirror drives handleEnsureMirror directly with an X-Internal-Key header.
func postMirror(t *testing.T, body map[string]any, key string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/internal/mirrors", bytes.NewReader(raw))
	if key != "" {
		req.Header.Set("X-Internal-Key", key)
	}
	rec := httptest.NewRecorder()
	handleEnsureMirror(rec, req)
	return rec
}

const testInternalKey = "test-internal-key"

// The surface is closed without the shared key: no key configured rejects all, and a
// wrong key is a 401 — a mirror must never be creatable by an unauthenticated caller.
func TestEnsureMirror_RequiresInternalKey(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	up, _ := makeUpstream(t)
	body := map[string]any{"upstream_url": up, "namespace": "acme", "name": "widgets", "owner": "ci-user"}

	// No key configured at all → closed.
	t.Setenv("GIT_FACTORY_INTERNAL_KEY", "")
	if rec := postMirror(t, body, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-key configured: status = %d, want 401", rec.Code)
	}

	// Key configured, wrong key presented → 401.
	t.Setenv("GIT_FACTORY_INTERNAL_KEY", testInternalKey)
	if rec := postMirror(t, body, "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status = %d, want 401", rec.Code)
	}
}

// The happy path: a mirror row is created, upstream is fetched into its bare repo, the
// row is stamped kind=mirror with a mirror_at and a derived clone URL, and the commit
// is actually present on disk.
func TestEnsureMirror_CreatesAndFetches(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	t.Setenv("GIT_FACTORY_INTERNAL_KEY", testInternalKey)
	t.Setenv("GIT_HTTP_BASE_URL", "https://git.example.com")
	up, sha := makeUpstream(t)

	rec := postMirror(t, map[string]any{
		"upstream_url": up, "namespace": "acme", "name": "widgets", "owner": "ci-user",
	}, testInternalKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got Repo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != kindMirror {
		t.Errorf("kind = %q, want mirror", got.Kind)
	}
	if got.Owner != "" { // Owner is json:"-" — must never be serialized
		t.Errorf("owner leaked into JSON: %q", got.Owner)
	}
	if got.MirrorAt == nil {
		t.Error("mirror_at not stamped")
	}
	if got.HttpUrl != "https://git.example.com/acme/widgets.git" {
		t.Errorf("http_url = %q, want the derived clone URL", got.HttpUrl)
	}
	// The upstream commit must be resolvable in the mirror's bare repo.
	if !refExists(context.Background(), got.ID, sha) {
		t.Errorf("commit %s not present in mirror after fetch", sha)
	}
	if !refExists(context.Background(), got.ID, "main") {
		t.Error("branch main not present in mirror after fetch")
	}

	// Idempotent: a second ensure against the same path refreshes rather than 409s.
	if rec := postMirror(t, map[string]any{
		"upstream_url": up, "namespace": "acme", "name": "widgets", "owner": "ci-user",
	}, testInternalKey); rec.Code != http.StatusOK {
		t.Fatalf("second ensure: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// The refresh-before-clone guarantee: a ref present upstream passes; a ref that isn't
// there after the fetch is a 404 — the caller must not then clone expecting it.
func TestEnsureMirror_RefGuarantee(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	t.Setenv("GIT_FACTORY_INTERNAL_KEY", testInternalKey)
	up, sha := makeUpstream(t)

	if rec := postMirror(t, map[string]any{
		"upstream_url": up, "namespace": "acme", "name": "w", "owner": "ci-user", "ref": sha,
	}, testInternalKey); rec.Code != http.StatusOK {
		t.Fatalf("present ref: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if rec := postMirror(t, map[string]any{
		"upstream_url": up, "namespace": "acme", "name": "w", "owner": "ci-user", "ref": "no-such-branch",
	}, testInternalKey); rec.Code != http.StatusNotFound {
		t.Fatalf("absent ref: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// A mirror is read-only over the wire: an authorized caller (gatekeeper grants the
// owner) is still refused a push with 403, because upstream — not a client — is the
// source of truth.
func TestEnsureMirror_MirrorIsReadOnly(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	t.Setenv("GIT_FACTORY_INTERNAL_KEY", testInternalKey)
	up, _ := makeUpstream(t)

	rec := postMirror(t, map[string]any{
		"upstream_url": up, "namespace": "ci-user", "name": "w", "owner": "ci-user",
	}, testInternalKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("ensure: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	newGatekeeperStub(t, "ci-user") // grants the owner full rights over its namespace
	req := httptest.NewRequest(http.MethodPost, "/ci-user/w.git/"+string(svcReceivePack), strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer tok")
	wr := httptest.NewRecorder()
	gitMux().ServeHTTP(wr, req)
	if wr.Code != http.StatusForbidden {
		t.Fatalf("push to mirror: status = %d, want 403 (read-only mirror)", wr.Code)
	}
}

// upstream_url lands in the argv of `git fetch`, where git parses options positionally:
// a value starting with "-" is read as an OPTION, and "--upload-pack=<cmd>" names a
// program git EXECUTES. An unscreened URL here was arbitrary command execution in the
// service that holds every repo, so this is a security regression test, not a
// validation-shape one — it must keep failing loudly if the guard is removed.
func TestEnsureMirror_RejectsOptionLikeUpstreamURL(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	t.Setenv("GIT_FACTORY_INTERNAL_KEY", testInternalKey)

	marker := filepath.Join(t.TempDir(), "pwned")
	for _, bad := range []string{
		"--upload-pack=touch " + marker, // the executing option
		"-u",                            // any leading dash at all
		"ext::sh -c touch " + marker,    // git's command-running transport helper
	} {
		rec := postMirror(t, map[string]any{
			"upstream_url": bad, "namespace": "acme", "name": "widgets", "owner": "ci-user",
		}, testInternalKey)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("upstream_url %q: status = %d, want 400", bad, rec.Code)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("REGRESSION: the injected command executed — git ran the upstream_url as an option")
	}
}

// The same argv injection on the replication path, which takes primary_url from a
// request body and hands it to the same `git fetch`.
func TestReplicate_RejectsOptionLikePrimaryURL(t *testing.T) {
	setupTestDB(t)
	t.Setenv("GIT_NODE_FORWARD_KEY", "node-key")

	marker := filepath.Join(t.TempDir(), "pwned")
	body, _ := json.Marshal(map[string]any{
		"repo_id": "11111111-1111-1111-1111-111111111111", "version": 1,
		"primary_url": "--upload-pack=touch " + marker,
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/replicate", bytes.NewReader(body))
	req.Header.Set("X-Git-Node-Forward", "node-key")
	rec := httptest.NewRecorder()
	handleReplicate(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("REGRESSION: the injected command executed on the replicate path")
	}
}

func TestValidateFetchURL(t *testing.T) {
	ok := []string{
		"https://git.example.com/a/b.git",
		"http://user:tok@host:3000/o/r.git",
		"/tmp/some/local/repo",   // a filesystem path is a valid git remote
		"file:///tmp/local/repo", // as is file://
		"C:\\repos\\thing",       // not mistaken for a transport helper
	}
	for _, in := range ok {
		if err := validateFetchURL(in); err != nil {
			t.Errorf("validateFetchURL(%q) = %v, want nil", in, err)
		}
	}
	bad := []string{
		"",
		"   ",
		"-u",
		"--upload-pack=touch /tmp/x",
		"--exec=whoami",
		"ext::sh -c whoami",
	}
	for _, in := range bad {
		if err := validateFetchURL(in); err == nil {
			t.Errorf("validateFetchURL(%q) = nil, want rejection", in)
		}
	}
}

func TestSanitizeUpstreamURL(t *testing.T) {
	cases := map[string]string{
		"https://user:tok@git.example.com/a/b.git": "https://git.example.com/a/b.git",
		"http://x-token:secret@host:3000/o/r.git":  "http://host:3000/o/r.git",
		"https://git.example.com/a/b.git":          "https://git.example.com/a/b.git",
	}
	for in, want := range cases {
		if got := sanitizeUpstreamURL(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
