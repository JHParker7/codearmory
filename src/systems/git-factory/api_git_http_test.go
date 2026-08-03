package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	sdkevents "github.com/code-armory-app/codearmory_sdk/events"
	"github.com/google/uuid"
)

// The Go-side contract for the git wire surface (ARCHITECTURE §2b). tests/test_git_http.py
// covers the same ground against a live stack with the real git CLI; these run in
// milliseconds and pin the parts that are easy to regress silently — the auth challenge,
// the advertisement framing, and the ownership 404.

// gitMux builds a mux with only the wire routes, matching main().
func gitMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{ns}/{repo}/info/refs", handleInfoRefs)
	mux.HandleFunc("POST /{ns}/{repo}/"+string(svcUploadPack), handleUploadPack)
	mux.HandleFunc("POST /{ns}/{repo}/"+string(svcReceivePack), handleReceivePack)
	return mux
}

func infoRefs(t *testing.T, path, service, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path+"/info/refs?service="+service, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	gitMux().ServeHTTP(rec, req)
	return rec
}

// Without the challenge header git neither prompts nor retries — it just fails, so the
// header's presence is as load-bearing as the status code.
func TestInfoRefs_UnauthenticatedIsChallenged(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")
	seedRepo(t, uuid.New().String(), "user-1", "admin", "repo-a", "")

	rec := infoRefs(t, "/admin/repo-a.git", string(svcUploadPack), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(strings.ToLower(got), "basic") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
	}
}

func TestInfoRefs_AdvertisementFraming(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")
	seedRepo(t, uuid.New().String(), "user-1", "admin", "repo-a", "")

	for _, svc := range []gitService{svcUploadPack, svcReceivePack} {
		rec := infoRefs(t, "/admin/repo-a.git", string(svc), "Bearer tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (%s)", svc, rec.Code, rec.Body.String())
		}
		if got, want := rec.Header().Get("Content-Type"), "application/x-"+string(svc)+"-advertisement"; got != want {
			t.Errorf("%s: Content-Type = %q, want %q", svc, got, want)
		}
		// "<4-hex len># service=<svc>\n" then a flush-pkt. git rejects anything else
		// with an opaque error, so assert the exact bytes.
		body := rec.Body.String()
		want := pktLine("# service="+string(svc)+"\n") + "0000"
		if !strings.HasPrefix(body, want) {
			t.Errorf("%s: advertisement starts %q, want prefix %q", svc, body[:min(len(body), 40)], want)
		}
	}
}

// git clones /{ns}/{repo} and /{ns}/{repo}.git interchangeably.
func TestInfoRefs_DotGitSuffixOptional(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")
	seedRepo(t, uuid.New().String(), "user-1", "admin", "repo-a", "")

	for _, path := range []string{"/admin/repo-a", "/admin/repo-a.git"} {
		if rec := infoRefs(t, path, string(svcUploadPack), "Bearer tok"); rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
	}
}

// Basic with the token in the password field is what git actually sends once a
// credential is stored; the username is ignored.
func TestInfoRefs_BasicCredentialsAccepted(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")
	seedRepo(t, uuid.New().String(), "user-1", "admin", "repo-a", "")

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("anyuser:the-token"))
	if rec := infoRefs(t, "/admin/repo-a.git", string(svcUploadPack), basic); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for Basic credentials", rec.Code)
	}
}

func TestInfoRefs_UnknownRepoIs404(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	if rec := infoRefs(t, "/admin/nope.git", string(svcUploadPack), "Bearer tok"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// The heart of ARCHITECTURE §3: gatekeeper's grant is username-scoped and every user
// holds it over their own namespace, so passing the permission check does NOT mean
// "may touch this repo". The owner comparison is the real gate, and it answers 404 —
// not 403 — so a probe cannot enumerate other people's repositories.
func TestWireRoutes_OtherUserGets404NotTheRepo(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	// The intruder holds full rights in their OWN namespace and none in "admin" — which
	// is exactly what gatekeeper answers now that the resource names the repo's owner.
	newGatekeeperStubForNamespace(t, "intruder", "intruder")
	seedRepo(t, uuid.New().String(), "owner-1", "admin", "private-repo", "")

	rec := infoRefs(t, "/admin/private-repo.git", string(svcUploadPack), "Bearer tok")
	if rec.Code != http.StatusNotFound {
		t.Errorf("info/refs status = %d, want 404 for a non-owner", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "private-repo") {
		t.Error("404 body leaks the repo name")
	}

	for _, svc := range []gitService{svcUploadPack, svcReceivePack} {
		req := httptest.NewRequest(http.MethodPost, "/admin/private-repo.git/"+string(svc), strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		gitMux().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404 for a non-owner", svc, rec.Code)
		}
	}
}

// A path segment that is not in the allowlist must be rejected before any lookup —
// nothing here ever reaches the filesystem (paths derive from the repo id), but the
// check is the cheap guard ARCHITECTURE §6 asks for.
func TestWireRoutes_RejectsTraversalishSegments(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	for _, ns := range []string{"..", "bad name", "a/b"} {
		req := httptest.NewRequest(http.MethodGet, "/x/y/info/refs?service="+string(svcUploadPack), nil)
		req.SetPathValue("ns", ns)
		req.SetPathValue("repo", "repo-a")
		if _, _, ok := repoPathParts(req); ok {
			t.Errorf("namespace %q accepted, want rejected", ns)
		}
	}
	for _, name := range []string{"..", ".", "x y"} {
		req := httptest.NewRequest(http.MethodGet, "/x/y/info/refs", nil)
		req.SetPathValue("ns", "admin")
		req.SetPathValue("repo", name)
		if _, _, ok := repoPathParts(req); ok {
			t.Errorf("repo name %q accepted, want rejected", name)
		}
	}
}

// Only the two real services are servable; anything else is refused rather than being
// handed to a subprocess.
func TestParseGitService_ClosedSet(t *testing.T) {
	for _, ok := range []string{"git-upload-pack", "git-receive-pack"} {
		if _, valid := parseGitService(ok); !valid {
			t.Errorf("%q rejected, want accepted", ok)
		}
	}
	for _, bad := range []string{"", "git-upload-archive", "upload-pack", "rm", "git-upload-pack;rm -rf /"} {
		if _, valid := parseGitService(bad); valid {
			t.Errorf("%q accepted, want rejected", bad)
		}
	}
	if got := svcUploadPack.subcommand(); got != "upload-pack" {
		t.Errorf("subcommand = %q, want upload-pack", got)
	}
	if got := svcReceivePack.action(); got != "writeRepo" {
		t.Errorf("receive-pack action = %q, want writeRepo", got)
	}
	if got := svcUploadPack.action(); got != "readRepo" {
		t.Errorf("upload-pack action = %q, want readRepo", got)
	}
}

// A dumb-HTTP client asking for something we do not serve gets a clear answer.
func TestInfoRefs_NonSmartServiceRefused(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	if rec := infoRefs(t, "/admin/repo-a.git", "git-upload-archive", "Bearer tok"); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// The wire routes must not inherit the JSON API's 1 MiB body cap — a push body is the
// whole packfile.
func TestIsGitWirePath(t *testing.T) {
	wire := []string{
		"/admin/repo.git/info/refs",
		"/admin/repo/info/refs",
		"/admin/repo.git/git-upload-pack",
		"/admin/repo.git/git-receive-pack",
	}
	for _, p := range wire {
		if !isGitWirePath(p) {
			t.Errorf("%q not recognised as a git wire path (body cap would break pushes)", p)
		}
	}
	for _, p := range []string{"/repos", "/repos/abc", "/healthz", "/openapi.yaml"} {
		if isGitWirePath(p) {
			t.Errorf("%q treated as a git wire path (would lose the body cap)", p)
		}
	}
}

// HEAD must point at the branch the API advertises, or a client that pushes to
// default_branch and clones back gets an empty working tree.
func TestInitialBranch(t *testing.T) {
	if got := initialBranch(Repo{}); got != defaultBranchName {
		t.Errorf("unset = %q, want %q", got, defaultBranchName)
	}
	if got := initialBranch(Repo{DefaultBranch: "trunk"}); got != "trunk" {
		t.Errorf("trunk = %q", got)
	}
	for _, bad := range []string{"--upload-pack=evil", "-x", "a b", ""} {
		if got := initialBranch(Repo{DefaultBranch: bad}); got != defaultBranchName {
			t.Errorf("%q = %q, want the safe default", bad, got)
		}
	}
}

// A repo whose first push never creates the branch HEAD was initialized at must still
// clone into a populated working tree. Left unreconciled, git transfers every object
// and checks out nothing — indistinguishable from data loss at the client.
func TestReconcileHEAD_PointsAtAnExistingBranch(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	ctx := context.Background()
	re := seedRepo(t, uuid.New().String(), "user-1", "admin", "repo-head", "")
	dir, err := repoDiskPath(re.ID)
	if err != nil {
		t.Fatalf("disk path: %v", err)
	}

	// HEAD starts on the initial branch, which does not exist yet.
	if out, _ := exec.Command("git", "-C", dir, "symbolic-ref", "HEAD").Output(); strings.TrimSpace(string(out)) != "refs/heads/"+defaultBranchName {
		t.Fatalf("initial HEAD = %q", strings.TrimSpace(string(out)))
	}
	// Simulate a push that created "dev" only — the monorepo case, which has no main.
	writeBranch(t, dir, "dev")

	if err := reconcileHEAD(ctx, re.ID, re.DefaultBranch); err != nil {
		t.Fatalf("reconcileHEAD: %v", err)
	}
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "refs/heads/dev" {
		t.Errorf("HEAD = %q, want refs/heads/dev", got)
	}
}

// Reconciling must never override a HEAD that already resolves.
func TestReconcileHEAD_LeavesAValidHeadAlone(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	ctx := context.Background()
	re := seedRepo(t, uuid.New().String(), "user-1", "admin", "repo-head2", "")
	dir, _ := repoDiskPath(re.ID)

	writeBranch(t, dir, defaultBranchName) // HEAD's target now exists
	writeBranch(t, dir, "other")
	if err := reconcileHEAD(ctx, re.ID, re.DefaultBranch); err != nil {
		t.Fatalf("reconcileHEAD: %v", err)
	}
	out, _ := exec.Command("git", "-C", dir, "symbolic-ref", "HEAD").Output()
	if got := strings.TrimSpace(string(out)); got != "refs/heads/"+defaultBranchName {
		t.Errorf("HEAD = %q, want it left on %s", got, defaultBranchName)
	}
}

// writeBranch creates a branch with one empty commit in a bare repo.
func writeBranch(t *testing.T, dir, branch string) {
	t.Helper()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	cmd := exec.Command("git", "-C", dir, "commit-tree", "-m", "seed", "4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	cmd.Env = env
	sha, err := cmd.Output()
	if err != nil {
		t.Fatalf("commit-tree: %v", err)
	}
	up := exec.Command("git", "-C", dir, "update-ref", "refs/heads/"+branch, strings.TrimSpace(string(sha)))
	up.Env = env
	if err := up.Run(); err != nil {
		t.Fatalf("update-ref %s: %v", branch, err)
	}
}

// The shard key is the 2-char fan-out of ARCHITECTURE §4 and, from §5-Step 2, the
// placement key. It must derive from the stable id — deriving it from the name would
// move a repo's bytes on rename, the exact property the id-keyed layout preserves.
func TestShardOf(t *testing.T) {
	id := "a2830c85-b846-4374-8b71-84081e3eae65"
	if got := shardOf(id); got != "a2" {
		t.Errorf("shardOf(%s) = %q, want a2", id, got)
	}
	if got := shardOf("x"); got != "x" {
		t.Errorf("short id = %q, want it returned unsliced rather than panicking", got)
	}
	dir, err := repoDiskPath(id)
	if err != nil {
		t.Fatalf("repoDiskPath: %v", err)
	}
	if !strings.Contains(dir, "/a2/") {
		t.Errorf("disk path %q does not use the 2-char fan-out", dir)
	}
}

func TestRepoAdd_ShardIsKeyedByIDNotName(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepo(t, "c7000000-0000-0000-0000-000000000000", "user-1", "alice", "zzz-name", "")
	var got Repo
	if err := connectRead().WithContext(ctx).Where("id = ?", re.ID).First(&got).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Shard != "c7" {
		t.Errorf("Shard = %q, want c7 (the id prefix, not the name)", got.Shard)
	}
}

// §5 Step 2: the routing table must be pure indirection. With no rows, every shard
// resolves to this node and behaviour is identical to having no table at all.
func TestResolveNode_UnmappedShardIsLocal(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	node, err := resolveNode(ctx, "a2830c85-b846-4374-8b71-84081e3eae65", "")
	if err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	if !node.Local || node.Shard != "a2" {
		t.Errorf("node = %+v, want local with shard a2", node)
	}
}

// A row with an empty address, or one naming this node, is still local — the second
// case is what keeps a node from proxying to itself once placement is written down.
func TestResolveNode_EmptyAddressAndSelfAreLocal(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	id := "a2830c85-b846-4374-8b71-84081e3eae65"

	if err := connect().WithContext(ctx).Create(&ShardNode{Shard: "a2"}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if node, _ := resolveNode(ctx, id, ""); !node.Local {
		t.Error("empty address should resolve local")
	}

	t.Setenv("GIT_NODE_ADDRESS", "http://git-node-1:9002/")
	if err := connect().WithContext(ctx).Model(&ShardNode{}).Where("shard = ?", "a2").
		Update("address", "http://git-node-1:9002").Error; err != nil {
		t.Fatalf("update: %v", err)
	}
	if node, _ := resolveNode(ctx, id, ""); !node.Local {
		t.Error("a row naming this node should resolve local, not proxy to itself")
	}
}

// A shard placed elsewhere must fail loudly rather than fall through to a local path
// that holds no such repository — until Step 3's reverse proxy exists.
func TestGitPlane_RemoteShardIsRefusedNotServedLocally(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	id := "a2830c85-b846-4374-8b71-84081e3eae65"
	if err := connect().WithContext(ctx).Create(&ShardNode{Shard: "a2", Address: "http://git-node-2:9002"}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := localDirFor(ctx, id); !errors.Is(err, errRemoteNodeUnsupported) {
		t.Errorf("err = %v, want errRemoteNodeUnsupported", err)
	}
	if err := advertiseRefs(ctx, id, svcUploadPack, io.Discard); !errors.Is(err, errRemoteNodeUnsupported) {
		t.Errorf("advertiseRefs err = %v, want errRemoteNodeUnsupported", err)
	}
	if err := runPack(ctx, id, svcUploadPack, strings.NewReader(""), io.Discard); !errors.Is(err, errRemoteNodeUnsupported) {
		t.Errorf("runPack err = %v, want errRemoteNodeUnsupported", err)
	}
}

// The full route table must register without panicking. ServeMux rejects ambiguous
// patterns at registration time, in main(), which no other test reaches — a conflict
// therefore crash-loops the pod instead of failing a build. "/ui/" and the wire routes
// collided exactly this way: "/ui/x/info/refs" matched both.
func TestRouteTable_RegistersWithoutConflict(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("route registration panicked: %v", p)
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /openapi.yaml", handleOpenAPIYAML)
	mux.HandleFunc("GET /ui", handleUI)
	mux.HandleFunc("GET /ui/{$}", handleUI)
	mux.HandleFunc("GET /repos", handleListRepos)
	mux.HandleFunc("POST /repos", handleCreateRepo)
	mux.HandleFunc("GET /repos/{id}", handleGetRepo)
	mux.HandleFunc("PATCH /repos/{id}", handleUpdateRepo)
	mux.HandleFunc("DELETE /repos/{id}", handleDeleteRepo)
	mux.HandleFunc("GET /{ns}/{repo}/info/refs", handleInfoRefs)
	mux.HandleFunc("POST /{ns}/{repo}/"+string(svcUploadPack), handleUploadPack)
	mux.HandleFunc("POST /{ns}/{repo}/"+string(svcReceivePack), handleReceivePack)

	// The frame is loaded at "<ui_path>/", so that exact path must serve the page.
	for _, path := range []string{"/ui", "/ui/"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "git_factory") {
			t.Errorf("GET %s did not serve the mini-portal", path)
		}
	}
}

// Commit history: an empty repo is the state the UI renders most often (created, never
// pushed), and git log exits non-zero there — that must read as "no history", not as an
// error the page shows as a failure.
func TestListCommits_EmptyRepoIsNotAnError(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepo(t, uuid.New().String(), "user-1", "admin", "fresh", "")
	got, err := listCommits(ctx, re.ID, commitQuery{})
	if err != nil {
		t.Fatalf("listCommits on an empty repo: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d commits, want none", len(got))
	}
}

func TestListCommits_ReturnsHistoryNewestFirst(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepo(t, uuid.New().String(), "user-1", "admin", "hist", "")
	dir, _ := repoDiskPath(re.ID)
	writeBranch(t, dir, defaultBranchName)

	got, err := listCommits(ctx, re.ID, commitQuery{Ref: defaultBranchName})
	if err != nil {
		t.Fatalf("listCommits: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d commits, want 1", len(got))
	}
	c := got[0]
	if c.Subject != "seed" || c.Author != "t" {
		t.Errorf("commit = %+v, want subject seed by t", c)
	}
	if len(c.Short) != 7 || !strings.HasPrefix(c.SHA, c.Short) {
		t.Errorf("short sha %q is not a prefix of %q", c.Short, c.SHA)
	}
	if _, err := time.Parse(time.RFC3339, c.Date); err != nil {
		t.Errorf("date %q is not RFC3339: %v", c.Date, err)
	}
}

// A push must bump updated_at, or "last updated" reports the creation date forever and
// a repo pushed to daily looks abandoned.
func TestTouchRepo_BumpsUpdatedAt(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepo(t, uuid.New().String(), "user-1", "admin", "touched", "")
	before, err := getRepo(ctx, "user-1", re.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := touchRepo(ctx, re.ID); err != nil {
		t.Fatalf("touchRepo: %v", err)
	}
	after, _ := getRepo(ctx, "user-1", re.ID)
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at %v not advanced past %v", after.UpdatedAt, before.UpdatedAt)
	}
}

// The readme is the repo home page, so the interesting cases are the ordinary ones: no
// commits, no readme, and a name whose case differs from README.md.
func TestReadme_MissingCasesAreNotErrors(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	empty := seedRepo(t, uuid.New().String(), "user-1", "admin", "no-commits", "")
	if _, _, found, err := readme(ctx, empty.ID, ""); err != nil || found {
		t.Errorf("empty repo: found=%v err=%v, want found=false err=nil", found, err)
	}

	noReadme := seedRepo(t, uuid.New().String(), "user-1", "admin", "no-readme", "")
	dir, _ := repoDiskPath(noReadme.ID)
	writeBranch(t, dir, defaultBranchName) // a commit with an empty tree
	if _, _, found, err := readme(ctx, noReadme.ID, ""); err != nil || found {
		t.Errorf("repo without a readme: found=%v err=%v, want found=false err=nil", found, err)
	}
}

func TestReadme_FindsItCaseInsensitively(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepo(t, uuid.New().String(), "user-1", "admin", "with-readme", "")
	dir, _ := repoDiskPath(re.ID)
	commitFile(t, dir, defaultBranchName, "Readme.md", "# Title\n\nhello\n")

	path, content, found, err := readme(ctx, re.ID, "")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v, want found=true", found, err)
	}
	if path != "Readme.md" {
		t.Errorf("path = %q, want the real on-disk casing Readme.md", path)
	}
	if !strings.Contains(content, "# Title") {
		t.Errorf("content = %q, want the file body", content)
	}
}

// commitFile writes one file into a bare repo on branch, via the index-less plumbing
// path (hash-object → mktree → commit-tree → update-ref).
func commitFile(t *testing.T, dir, branch, name, body string) {
	t.Helper()
	run := func(stdin string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	blob := run(body, "hash-object", "-w", "--stdin")
	tree := run("100644 blob "+blob+"\t"+name+"\n", "mktree")
	sha := run("", "commit-tree", tree, "-m", "add "+name)
	run("", "update-ref", "refs/heads/"+branch, sha)
}

// Paging and filtering must agree: the total is what drives the pager, so it has to be
// computed over the same filters as the page it labels.
func TestListCommits_PagesAndFilters(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepo(t, uuid.New().String(), "user-1", "admin", "many", "")
	dir, _ := repoDiskPath(re.ID)
	// five commits: three by alice mentioning "fix", two by bob
	for i, c := range []struct{ author, subject string }{
		{"alice", "fix one"}, {"bob", "feat two"}, {"alice", "fix three"},
		{"bob", "chore four"}, {"alice", "fix five"},
	} {
		commitOn(t, dir, defaultBranchName, fmt.Sprintf("f%d", i), c.author, c.subject)
	}

	all, err := listCommits(ctx, re.ID, commitQuery{Ref: defaultBranchName})
	if err != nil || len(all) != 5 {
		t.Fatalf("got %d commits (err %v), want 5", len(all), err)
	}
	if n, _ := countCommits(ctx, re.ID, commitQuery{Ref: defaultBranchName}); n != 5 {
		t.Errorf("count = %d, want 5", n)
	}

	// Page two of two-per-page is the middle pair, and pages must not overlap.
	p1, _ := listCommits(ctx, re.ID, commitQuery{Ref: defaultBranchName, Limit: 2})
	p2, _ := listCommits(ctx, re.ID, commitQuery{Ref: defaultBranchName, Limit: 2, Skip: 2})
	if len(p1) != 2 || len(p2) != 2 {
		t.Fatalf("page sizes = %d/%d, want 2/2", len(p1), len(p2))
	}
	if p1[0].SHA == p2[0].SHA {
		t.Error("skip did not advance the page")
	}
	if p1[1].SHA != all[1].SHA || p2[0].SHA != all[2].SHA {
		t.Error("pages do not tile the full history in order")
	}

	// Message filter, and its count.
	fixes, _ := listCommits(ctx, re.ID, commitQuery{Ref: defaultBranchName, Grep: "fix"})
	if len(fixes) != 3 {
		t.Errorf("grep=fix returned %d, want 3", len(fixes))
	}
	if n, _ := countCommits(ctx, re.ID, commitQuery{Ref: defaultBranchName, Grep: "fix"}); n != 3 {
		t.Errorf("filtered count = %d, want 3", n)
	}
	// Case-insensitive.
	if up, _ := listCommits(ctx, re.ID, commitQuery{Ref: defaultBranchName, Grep: "FIX"}); len(up) != 3 {
		t.Errorf("grep=FIX returned %d, want 3 (search is case-insensitive)", len(up))
	}
	// Author filter, and the two combined.
	if byBob, _ := listCommits(ctx, re.ID, commitQuery{Ref: defaultBranchName, Author: "bob"}); len(byBob) != 2 {
		t.Errorf("author=bob returned %d, want 2", len(byBob))
	}
	if both, _ := listCommits(ctx, re.ID, commitQuery{Ref: defaultBranchName, Author: "alice", Grep: "fix"}); len(both) != 3 {
		t.Errorf("author+grep returned %d, want 3", len(both))
	}
}

// A search string is a literal, not a regex: "(" must return nothing rather than
// erroring, and a pathological pattern must not become a CPU sink.
func TestListCommits_SearchIsLiteralNotRegex(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepo(t, uuid.New().String(), "user-1", "admin", "literal", "")
	dir, _ := repoDiskPath(re.ID)
	commitOn(t, dir, defaultBranchName, "a", "alice", "add thing (parenthesised)")

	if got, err := listCommits(ctx, re.ID, commitQuery{Ref: defaultBranchName, Grep: "("}); err != nil {
		t.Errorf("unbalanced paren errored: %v — the search must be literal", err)
	} else if len(got) != 1 {
		t.Errorf("literal '(' matched %d, want 1", len(got))
	}
	if got, _ := listCommits(ctx, re.ID, commitQuery{Ref: defaultBranchName, Grep: ".*"}); len(got) != 0 {
		t.Errorf("'.*' matched %d, want 0 — it is a literal, not a wildcard", len(got))
	}
}

// commitOn appends a commit authored by author onto branch, PRESERVING the files
// already there. Building a single-entry tree instead would make every commit replace
// the whole tree, so a diff between two branches would show unrelated files deleted —
// a fixture that quietly misrepresents what the code under test is doing.
func commitOn(t *testing.T, dir, branch, file, author, subject string) {
	t.Helper()
	commitContentOn(t, dir, branch, file, subject+"\n", author, subject)
}

// commitContentOn is commitOn with explicit content, so two branches can be made to
// conflict on the same path.
func commitContentOn(t *testing.T, dir, branch, file, body, author, subject string) {
	t.Helper()
	run := func(stdin string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME="+author, "GIT_AUTHOR_EMAIL="+author+"@example.com",
			"GIT_COMMITTER_NAME="+author, "GIT_COMMITTER_EMAIL="+author+"@example.com")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}

	parent := ""
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Output(); err == nil {
		parent = strings.TrimSpace(string(out))
	}

	// Start from the parent's tree so existing files survive.
	entries := map[string]string{}
	if parent != "" {
		for _, line := range strings.Split(gitOut(t, dir, "ls-tree", parent), "\n") {
			if meta, name, ok := strings.Cut(line, "\t"); ok && name != "" {
				entries[name] = meta + "\t" + name
			}
		}
	}
	blob := run(body, "hash-object", "-w", "--stdin")
	entries[file] = "100644 blob " + blob + "\t" + file

	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var spec strings.Builder
	for _, n := range names {
		spec.WriteString(entries[n] + "\n")
	}
	tree := run(spec.String(), "mktree")

	args := []string{"commit-tree", tree, "-m", subject}
	if parent != "" {
		args = []string{"commit-tree", tree, "-p", parent, "-m", subject}
	}
	run("", "update-ref", "refs/heads/"+branch, run("", args...))
}

// Branch/tag listing, tree, blob and archive — the code-browsing surface.
func TestBrowsing_RefsTreeBlobArchive(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepo(t, uuid.New().String(), "user-1", "admin", "browse", "")
	dir, _ := repoDiskPath(re.ID)
	commitOn(t, dir, defaultBranchName, "README.md", "alice", "first")
	commitOn(t, dir, "feature", "README.md", "bob", "second")

	// Branches, newest first, with their target and subject.
	branches, err := listRefs(ctx, re.ID, "refs/heads/")
	if err != nil {
		t.Fatalf("listRefs: %v", err)
	}
	names := map[string]bool{}
	for _, b := range branches {
		names[b.Name] = true
		if b.SHA == "" || b.Date == "" {
			t.Errorf("branch %s missing sha/date: %+v", b.Name, b)
		}
	}
	if !names[defaultBranchName] || !names["feature"] {
		t.Errorf("branches = %v, want both %s and feature", names, defaultBranchName)
	}
	// Tags are a separate namespace and must not leak into branches.
	if tags, _ := listRefs(ctx, re.ID, "refs/tags/"); len(tags) != 0 {
		t.Errorf("tags = %v, want none", tags)
	}

	// HEAD is the authority on the default branch.
	if got := headBranch(ctx, re.ID); got != defaultBranchName {
		t.Errorf("headBranch = %q, want %q", got, defaultBranchName)
	}
	if err := setHEAD(ctx, re.ID, "feature"); err != nil {
		t.Fatalf("setHEAD: %v", err)
	}
	if got := headBranch(ctx, re.ID); got != "feature" {
		t.Errorf("headBranch after set = %q, want feature", got)
	}
	// Pointing HEAD at a branch that does not exist is what produces a clone that
	// checks out nothing, so it must be refused.
	if err := setHEAD(ctx, re.ID, "no-such-branch"); err == nil {
		t.Error("setHEAD accepted a nonexistent branch")
	}

	// Tree.
	entries, err := listTree(ctx, re.ID, defaultBranchName, "")
	if err != nil {
		t.Fatalf("listTree: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "README.md" || entries[0].Type != "file" {
		t.Errorf("tree = %+v, want one file README.md", entries)
	}
	if entries[0].Size == 0 {
		t.Error("tree entry has no size")
	}
	if _, err := listTree(ctx, re.ID, defaultBranchName, "nope"); !errors.Is(err, errPathNotFound) {
		t.Errorf("missing path err = %v, want errPathNotFound", err)
	}

	// Blob.
	content, size, binary, err := readBlob(ctx, re.ID, defaultBranchName, "README.md")
	if err != nil {
		t.Fatalf("readBlob: %v", err)
	}
	if binary || !strings.Contains(content, "first") || size == 0 {
		t.Errorf("blob = %q size=%d binary=%v", content, size, binary)
	}
	if _, _, _, err := readBlob(ctx, re.ID, defaultBranchName, "missing.txt"); !errors.Is(err, errPathNotFound) {
		t.Errorf("missing blob err = %v, want errPathNotFound", err)
	}

	// Archive streams a real gzip.
	var buf bytes.Buffer
	if err := writeArchive(ctx, re.ID, defaultBranchName, "browse", &buf); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}
	if b := buf.Bytes(); len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		t.Errorf("archive is not gzip (first bytes %x)", buf.Bytes()[:min(2, buf.Len())])
	}
}

// A binary file must be reported, not shipped as JSON text.
func TestReadBlob_BinaryIsReportedNotReturned(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepo(t, uuid.New().String(), "user-1", "admin", "bin", "")
	dir, _ := repoDiskPath(re.ID)
	commitOn(t, dir, defaultBranchName, "logo.png", "alice", "add binary")
	// Overwrite the blob with real NUL-bearing content.
	commitBinary(t, dir, defaultBranchName, "data.bin", []byte{0x89, 0x50, 0x00, 0x01, 0x02})

	_, size, binary, err := readBlob(ctx, re.ID, defaultBranchName, "data.bin")
	if err != nil {
		t.Fatalf("readBlob: %v", err)
	}
	if !binary {
		t.Error("NUL-bearing content was not reported as binary")
	}
	if size == 0 {
		t.Error("binary blob reported zero size")
	}
}

func commitBinary(t *testing.T, dir, branch, name string, body []byte) {
	t.Helper()
	run := func(stdin string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	blob := run(string(body), "hash-object", "-w", "--stdin")
	tree := run("100644 blob "+blob+"\t"+name+"\n", "mktree")
	parent := ""
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Output(); err == nil {
		parent = strings.TrimSpace(string(out))
	}
	args := []string{"commit-tree", tree, "-m", "binary"}
	if parent != "" {
		args = []string{"commit-tree", tree, "-p", parent, "-m", "binary"}
	}
	run("", "update-ref", "refs/heads/"+branch, run("", args...))
}

// The push event must describe what actually changed, and must never be able to fail
// the push that produced it.
func TestPushEvent_ChangedRefsAndSafety(t *testing.T) {
	before := map[string]string{"main": "aaa", "old": "bbb"}
	after := map[string]string{"main": "ccc", "old": "bbb", "new": "ddd"}
	got := changedRefs(before, after)
	if len(got) != 2 || got[0] != "main" || got[1] != "new" {
		t.Errorf("changedRefs = %v, want [main new] — updated and created only", got)
	}
	// An unchanged repo reports nothing.
	if n := changedRefs(after, after); len(n) != 0 {
		t.Errorf("changedRefs on an unchanged snapshot = %v, want none", n)
	}
	// A deleted branch is not "new content"; it must not be announced as a change.
	if d := changedRefs(map[string]string{"gone": "aaa"}, map[string]string{}); len(d) != 0 {
		t.Errorf("deletion reported as a change: %v", d)
	}

	// With events unconfigured the integration is inert — the platform runs without it.
	prev := eventEmitter
	eventEmitter = sdkevents.New("", "", gitEventSource, nil)
	t.Cleanup(func() { eventEmitter = prev })
	if eventsEnabled() {
		t.Error("events reported enabled with no URL or key")
	}
	// Must not panic or block when disabled.
	notifyPush(context.Background(), Repo{ID: "x", Namespace: "admin", Name: "r"}, "user-1", []string{"main"}, nil)
}

// A push must be attributed to the repo's OWNER, not whoever pushed. Triggers are matched per
// tenant, so scoping by the pusher would run a collaborator's triggers on someone else's repo
// and never the owner's — which is what the former hooks service did.
func TestNotifyPush_TenantIsRepoOwner(t *testing.T) {
	var got sdkevents.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	prev := eventEmitter
	eventEmitter = sdkevents.New(srv.URL, "test-key", gitEventSource, srv.Client())
	t.Cleanup(func() { eventEmitter = prev })

	re := Repo{ID: "r1", Owner: "owner-user", Namespace: "acme", Name: "app"}
	ev := sdkevents.Event{
		Type:    eventPush,
		Source:  gitEventSource,
		Subject: re.Namespace + "/" + re.Name,
		Actor:   sdkevents.Actor{UserID: re.Owner},
	}
	ev.Data = map[string]any{"pusher": "collaborator-user"}
	if err := eventEmitter.Emit(context.Background(), ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got.Actor.UserID != "owner-user" {
		t.Errorf("actor.user_id = %q, want the repo owner", got.Actor.UserID)
	}
	if got.Data["pusher"] != "collaborator-user" {
		t.Errorf("data.pusher = %v, want the pusher preserved on the payload", got.Data["pusher"])
	}
	if got.Subject != "acme/app" {
		t.Errorf("subject = %q, want namespace/name", got.Subject)
	}
}

// The HMAC must match what the events service verifies, or every event is silently rejected.
func TestSignEvent_MatchesEventsVerification(t *testing.T) {
	ev := sdkevents.Event{
		ID: "evt-1", Type: eventPush, Source: gitEventSource, Subject: "acme/app",
		Actor: sdkevents.Actor{UserID: "user-1"},
		Data:  map[string]any{"ref": "main"},
	}
	ts := "1700000000"
	digest := sha256.Sum256(mustJSON(t, ev.Data))
	mac := hmac.New(sha256.New, []byte("test-key"))
	fmt.Fprintf(mac, "event:%s:%s:%s:%s:%s:%s:%s:%s",
		ev.ID, ev.Type, ev.Source, ev.Subject, "", "user-1", hex.EncodeToString(digest[:]), ts)
	if want, got := hex.EncodeToString(mac.Sum(nil)), sdkevents.Sign("test-key", ev, ts); got != want {
		t.Errorf("token = %s, want %s — events would reject every emit", got, want)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// Merging is the part with real consequences, so the cases that matter are the ones
// that must NOT merge: a conflict, and a branch that is already contained.
func TestMerge_CleanConflictAndAlreadyMerged(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepo(t, uuid.New().String(), "user-1", "admin", "merge", "")
	dir, _ := repoDiskPath(re.ID)

	// main: one file. feature: branches off main and adds a second file — a clean merge.
	commitOn(t, dir, defaultBranchName, "a.txt", "alice", "add a")
	forkBranch(t, dir, defaultBranchName, "feature")
	commitOn(t, dir, "feature", "b.txt", "bob", "add b")

	res, err := tryMerge(ctx, re.ID, defaultBranchName, "feature")
	if err != nil {
		t.Fatalf("tryMerge: %v", err)
	}
	if !res.Mergeable || res.Tree == "" {
		t.Fatalf("clean merge reported unmergeable: %+v", res)
	}

	// The diff describes the SOURCE's commits, not every difference between branches.
	changes, err := diffStat(ctx, re.ID, defaultBranchName, "feature")
	if err != nil {
		t.Fatalf("diffStat: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "b.txt" {
		t.Errorf("diff = %+v, want just b.txt", changes)
	}
	if n := commitsBetween(ctx, re.ID, defaultBranchName, "feature"); n != 1 {
		t.Errorf("commits ahead = %d, want 1", n)
	}

	// Merge it, and confirm a real merge commit with TWO parents landed on main.
	sha, err := mergeBranches(ctx, re.ID, defaultBranchName, "feature", "merge it", "user-1")
	if err != nil {
		t.Fatalf("mergeBranches: %v", err)
	}
	parents := gitOut(t, dir, "rev-list", "--parents", "-n", "1", sha)
	if len(strings.Fields(parents)) != 3 { // commit + 2 parents
		t.Errorf("merge commit has parents %q, want two", parents)
	}
	if head := gitOut(t, dir, "rev-parse", "refs/heads/"+defaultBranchName); head != sha {
		t.Errorf("%s = %s, want the merge commit %s", defaultBranchName, head, sha)
	}

	// Now already merged: nothing to do, and it must say so rather than making an
	// empty merge commit.
	if res, err := tryMerge(ctx, re.ID, defaultBranchName, "feature"); err != nil || res.Mergeable || !res.AlreadyIn {
		t.Errorf("second merge = %+v (err %v), want already_merged", res, err)
	}
	if _, err := mergeBranches(ctx, re.ID, defaultBranchName, "feature", "again", "user-1"); err == nil {
		t.Error("merging an already-merged branch succeeded")
	}

	// Conflict: two branches editing the same file differently.
	forkBranch(t, dir, defaultBranchName, "left")
	forkBranch(t, dir, defaultBranchName, "right")
	commitFileOn(t, dir, "left", "conflict.txt", "left side\n", "alice")
	commitFileOn(t, dir, "right", "conflict.txt", "right side\n", "bob")
	res, err = tryMerge(ctx, re.ID, "left", "right")
	if err != nil {
		t.Fatalf("tryMerge (conflict): %v", err)
	}
	if res.Mergeable {
		t.Error("conflicting merge reported as mergeable")
	}
	if len(res.Conflicts) == 0 || res.Conflicts[0] != "conflict.txt" {
		t.Errorf("conflicts = %v, want [conflict.txt]", res.Conflicts)
	}
	if _, err := mergeBranches(ctx, re.ID, "left", "right", "nope", "user-1"); err == nil {
		t.Error("a conflicting merge was allowed to proceed")
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// forkBranch points a new branch at another branch's tip.
func forkBranch(t *testing.T, dir, from, to string) {
	t.Helper()
	sha := gitOut(t, dir, "rev-parse", "refs/heads/"+from)
	if err := exec.Command("git", "-C", dir, "update-ref", "refs/heads/"+to, sha).Run(); err != nil {
		t.Fatalf("fork %s: %v", to, err)
	}
}

func commitFileOn(t *testing.T, dir, branch, name, body, author string) {
	t.Helper()
	commitContentOn(t, dir, branch, name, body, author, "edit "+name)
}

// Branch protection is enforced by a shell hook, so it is tested by RUNNING the hook
// the way git does — feeding it "<old> <new> <ref>" on stdin and checking the exit
// code. Asserting on the Go that writes the file would prove nothing about the file.
func TestBranchProtection_HookRejectsForceAndDelete(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepo(t, uuid.New().String(), "user-1", "admin", "protected", "")
	dir, _ := repoDiskPath(re.ID)

	// main gets two commits; "rewritten" forks from the FIRST, so pushing it over main
	// would discard the second — exactly what a force-push does.
	commitOn(t, dir, defaultBranchName, "a.txt", "alice", "first")
	first := gitOut(t, dir, "rev-parse", "refs/heads/"+defaultBranchName)
	commitOn(t, dir, defaultBranchName, "b.txt", "alice", "second")
	second := gitOut(t, dir, "rev-parse", "refs/heads/"+defaultBranchName)
	if err := exec.Command("git", "-C", dir, "update-ref", "refs/heads/rewritten", first).Run(); err != nil {
		t.Fatalf("fork: %v", err)
	}

	if err := writeProtection(ctx, re.ID, []BranchProtection{
		{RepoID: re.ID, Pattern: defaultBranchName, NoForce: true, NoDelete: true},
		{RepoID: re.ID, Pattern: "release/*", NoForce: true, NoDelete: true},
	}); err != nil {
		t.Fatalf("writeProtection: %v", err)
	}

	const zero = "0000000000000000000000000000000000000000"
	run := func(stdin string) (int, string) {
		cmd := exec.Command("sh", filepath.Join(dir, "hooks", "pre-receive"))
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		var errOut bytes.Buffer
		cmd.Stderr = &errOut
		cmd.Env = append(os.Environ(), "GIT_DIR="+dir)
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("hook: %v", err)
		}
		return code, errOut.String()
	}

	// Fast-forward on a protected branch: allowed. Protection is not read-only.
	if code, out := run(first + " " + second + " refs/heads/" + defaultBranchName + "\n"); code != 0 {
		t.Errorf("fast-forward rejected (exit %d): %s", code, out)
	}
	// Force-push (old is not an ancestor of new): rejected, with a reason.
	code, out := run(second + " " + first + " refs/heads/" + defaultBranchName + "\n")
	if code == 0 {
		t.Error("force-push to a protected branch was allowed")
	}
	if !strings.Contains(out, "force-push") {
		t.Errorf("rejection did not explain itself: %q", out)
	}
	// Deletion: rejected.
	if code, out := run(second + " " + zero + " refs/heads/" + defaultBranchName + "\n"); code == 0 {
		t.Errorf("deletion of a protected branch was allowed: %s", out)
	}
	// An unprotected branch is untouched by any of this.
	if code, out := run(second + " " + first + " refs/heads/scratch\n"); code != 0 {
		t.Errorf("force-push to an UNPROTECTED branch was rejected (exit %d): %s", code, out)
	}
	// The glob applies to the branches it names, and only those.
	if code, _ := run(second + " " + first + " refs/heads/release/1.0\n"); code == 0 {
		t.Error("force-push to release/1.0 allowed despite the release/* rule")
	}
	if code, _ := run(second + " " + first + " refs/heads/releases-old\n"); code != 0 {
		t.Error("release/* wrongly matched releases-old")
	}
	// Tags are not branches; the rules must not reach them.
	if code, _ := run(second + " " + zero + " refs/tags/v1\n"); code != 0 {
		t.Error("a tag update was blocked by a branch rule")
	}

	// No rules at all: the hook exits immediately and blocks nothing.
	if err := writeProtection(ctx, re.ID, nil); err != nil {
		t.Fatalf("clear protection: %v", err)
	}
	if code, out := run(second + " " + zero + " refs/heads/" + defaultBranchName + "\n"); code != 0 {
		t.Errorf("deletion blocked with no rules configured (exit %d): %s", code, out)
	}
}
