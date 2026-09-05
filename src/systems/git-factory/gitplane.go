package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// The git plane: the ONLY place that touches git-on-disk (ARCHITECTURE §1). Every
// entry point here is scoped to a single repo id and takes plain io.Reader/io.Writer
// for the wire bytes, so the day this moves to a remote git-node the control plane
// changes not at all — the same three calls become HTTP to that node.
//
// v1 shells out to git with --stateless-rpc, which is exactly what Smart HTTP is
// specified in terms of: the transport hands git a request body and streams back what
// git writes. No pkt-line parsing of our own beyond the advertisement header below.

// gitService is one of the two Smart-HTTP services. It is a closed set on purpose:
// the value arrives from a query parameter or a URL suffix and is used to build an
// argv, so it is matched against these constants and never interpolated.
type gitService string

const (
	svcUploadPack  gitService = "git-upload-pack"  // fetch/clone — read
	svcReceivePack gitService = "git-receive-pack" // push — write
)

// errQuotaExceeded is returned instead of running receive-pack when the repo has no
// headroom left at all. A push that could only take it further over is refused before
// a byte of it is written.
var errQuotaExceeded = errors.New("repository is at its storage quota")

// parseGitService maps the wire name to the closed set, rejecting anything else.
func parseGitService(s string) (gitService, bool) {
	switch gitService(s) {
	case svcUploadPack:
		return svcUploadPack, true
	case svcReceivePack:
		return svcReceivePack, true
	}
	return "", false
}

// subcommand is the git subcommand for a service ("git-upload-pack" → "upload-pack").
func (s gitService) subcommand() string { return string(s)[len("git-"):] }

// action is the RBAC action a service requires: fetching reads, pushing writes.
func (s gitService) action() string {
	if s == svcReceivePack {
		return "writeRepo"
	}
	return "readRepo"
}

// gitBinary is the git executable; a variable so tests can point it elsewhere.
var gitBinary = "git"

// shardPrefixLen is how many leading characters of a repo id form its shard key: the
// 2-char fan-out ARCHITECTURE §4 specifies. It serves two purposes that must not
// diverge — the on-disk directory (keeping one directory from holding millions of
// entries) and, from §5-Step 2 onward, the placement key deciding WHICH node holds the
// bytes. Both derive from the stable id, so a rename never moves data.
//
// 2 hex characters gives 256 buckets. Changing this value relocates every repository
// on disk, so it is a migration, not a tunable.
const shardPrefixLen = 2

// Commit-history page sizes. The default keeps a repo card cheap to render; the cap
// stops a caller asking for a million-commit page and holding a subprocess open.
const (
	defaultCommitPage = 50
	maxCommitPage     = 200
)

// shardOf returns the shard key for a repo id. A canonical uuid is always long enough;
// a short or malformed id (which repoDiskPath rejects outright) returns itself rather
// than panicking on the slice.
func shardOf(id string) string {
	if len(id) < shardPrefixLen {
		return id
	}
	return id[:shardPrefixLen]
}

// pktLine encodes a payload as a Smart-HTTP pkt-line: a 4-byte hex length covering
// the length prefix itself, then the payload. "0000" (flush-pkt) is written literally
// by callers rather than through here, since it has no payload.
func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

// localDirFor resolves where a repo's bytes live and returns the on-disk path when the
// answer is this node. This is the single choke point the routing table (§5 Step 2)
// feeds, and the single place Step 3 changes: when a shard resolves elsewhere, the
// remote branch becomes a reverse proxy instead of the error it is today.
func localDirFor(ctx context.Context, repoID string) (string, error) {
	node, err := resolveNode(ctx, repoID, "")
	if err != nil {
		return "", err
	}
	if !node.Local {
		slog.ErrorContext(ctx, "gitplane: repo is placed on another node",
			"Repo_id", repoID, "shard", node.Shard, "address", node.Address)
		return "", errRemoteNodeUnsupported
	}
	return repoDiskPath(repoID)
}

// advertiseRefs writes the ref advertisement for a service: the magic first pkt-line
// naming the service, a flush-pkt, then git's own advertisement. Getting these two
// header lines wrong is the classic Smart-HTTP failure — git reports an opaque error
// and gives no hint that the framing is at fault (ARCHITECTURE §2b).
func advertiseRefs(ctx context.Context, repoID string, svc gitService, w io.Writer) error {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "gitplane.advertiseRefs")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", repoID), attribute.String("git.service", string(svc)))

	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	if _, err := io.WriteString(w, pktLine("# service="+string(svc)+"\n")+"0000"); err != nil {
		return err
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, svc.subcommand(), "--stateless-rpc", "--advertise-refs", dir)
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "gitplane: advertise refs", "Repo_id", repoID, "service", svc, "error", err, "stderr", stderr.String())
		return fmt.Errorf("advertise refs: %w", err)
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// reconcileHEAD repoints a repo's HEAD when it references a branch that does not
// exist, which is the state every repo starts in: `git init --bare --initial-branch=X`
// writes HEAD before any branch exists, and if the first push never creates X, HEAD
// stays dangling. A clone of such a repo transfers every object and then checks out
// NOTHING — "remote HEAD refers to nonexistent ref" and an empty working tree, which
// reads as data loss even though the data is all there.
//
// Preference order: the repo's declared default branch, then the conventional names,
// then whatever exists (sorted, so the choice is deterministic rather than dependent on
// ref iteration order). A HEAD that already resolves is left alone — this must never
// override a deliberate choice.
func reconcileHEAD(ctx context.Context, repoID, preferred string) error {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return err
	}
	// Already valid? Nothing to do.
	if err := exec.CommandContext(ctx, gitBinary, "-C", dir, "symbolic-ref", "-q", "HEAD").Run(); err == nil {
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "symbolic-ref", "HEAD")
		cmd.Stdout = &out
		if cmd.Run() == nil {
			ref := strings.TrimSpace(out.String())
			if exec.CommandContext(ctx, gitBinary, "-C", dir, "show-ref", "--verify", "--quiet", ref).Run() == nil {
				return nil
			}
		}
	}

	var refsOut bytes.Buffer
	list := exec.CommandContext(ctx, gitBinary, "-C", dir, "for-each-ref", "--format=%(refname)", "--sort=refname", "refs/heads/")
	list.Stdout = &refsOut
	if err := list.Run(); err != nil {
		return fmt.Errorf("list refs: %w", err)
	}
	refs := strings.Fields(refsOut.String())
	if len(refs) == 0 {
		return nil // still empty — HEAD stays as initialized
	}

	has := func(ref string) bool {
		for _, r := range refs {
			if r == ref {
				return true
			}
		}
		return false
	}
	target := refs[0]
	for _, candidate := range append([]string{preferred}, defaultBranchName, "master", "trunk") {
		if candidate == "" {
			continue
		}
		if ref := "refs/heads/" + candidate; has(ref) {
			target = ref
			break
		}
	}
	if err := exec.CommandContext(ctx, gitBinary, "-C", dir, "symbolic-ref", "HEAD", target).Run(); err != nil {
		return fmt.Errorf("set HEAD to %s: %w", target, err)
	}
	slog.InfoContext(ctx, "gitplane: HEAD reconciled", "Repo_id", repoID, "head", target)
	return nil
}

// runPack streams a Smart-HTTP request through git and streams the result back.
//
// Both directions are unbounded byte streams — a clone of a large repo writes for as
// long as it takes, and a push body is the whole packfile — so body and w are wired
// straight to the subprocess rather than buffered. The caller is responsible for
// having cleared the server's read/write deadlines (see the wire handlers).
func runPack(ctx context.Context, repoID string, svc gitService, body io.Reader, w io.Writer) error {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "gitplane.runPack")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", repoID), attribute.String("git.service", string(svc)))

	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	// Hold the repo shared for the duration of the transfer so a maintenance sweep
	// cannot repack underneath it. Shared, not exclusive: concurrent pushes are safe
	// (git locks refs itself) and concurrent fetches obviously so.
	lock := repoLocks.get(repoID)
	lock.RLock()
	defer lock.RUnlock()

	// Quota, enforced by git rather than by us. receive.maxInputSize makes
	// receive-pack refuse a packfile larger than the headroom left, and the client is
	// told why — the alternative, accepting the objects and deleting them afterwards,
	// spends the disk we are trying to protect.
	args := []string{svc.subcommand(), "--stateless-rpc", dir}
	if svc == svcReceivePack {
		headroom, limited := quotaHeadroom(ctx, repoID)
		if limited && headroom <= 0 {
			span.SetStatus(codes.Error, "quota exhausted")
			slog.WarnContext(ctx, "gitplane: push refused, repo is at its quota", "Repo_id", repoID)
			return errQuotaExceeded
		}
		if limited {
			args = append([]string{"-c", "receive.maxInputSize=" + strconv.FormatInt(headroom, 10)}, args...)
		}
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, args...)
	cmd.Stdin = body
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "gitplane: run pack", "Repo_id", repoID, "service", svc, "error", err, "stderr", stderr.String())
		return fmt.Errorf("%s: %w", svc.subcommand(), err)
	}
	if stderr.Len() > 0 {
		// git writes progress and hook output to stderr on a perfectly successful run.
		slog.DebugContext(ctx, "gitplane: git stderr", "Repo_id", repoID, "service", svc, "stderr", stderr.String())
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// commit is one entry of a repo's history, as rendered for the API.
type commit struct {
	SHA     string `json:"sha"`
	Short   string `json:"short"`
	Author  string `json:"author"`
	// AuthorEmail lets a client tell an automated agent's commit from a human's by the
	// author identity itself (blacksmith authors agent commits under an agents.* domain),
	// rather than parsing the display name.
	AuthorEmail string `json:"author_email"`
	Date        string `json:"date"` // RFC3339, straight from git
	Subject     string `json:"subject"`
}

// commitLogSep is an ASCII unit separator: it cannot appear in any of the fields git
// substitutes, so splitting on it is safe where splitting on a space or a pipe is not
// (subjects and author names contain both).
const commitLogSep = "\x1f"

// commitQuery is one page of history, optionally filtered. Zero values mean "first
// page, unfiltered", so a bare query still reads naturally.
type commitQuery struct {
	Ref    string
	Skip   int
	Limit  int
	Grep   string // substring of the commit message, case-insensitive
	Author string // substring of the author name/email, case-insensitive
}

// filterArgs are the arguments shared by listCommits and countCommits, so a page and
// its total can never be computed over different filters — the bug that makes a pager
// claim more results than it can show.
//
// -i --fixed-strings matters: without it a user's search string is interpreted as a
// regular expression, so a stray "(" is an error and a crafted pattern is a CPU sink.
// Searches are literal substrings, which is also what a search box implies.
func (q commitQuery) filterArgs() []string {
	args := []string{}
	if q.Grep != "" {
		args = append(args, "--grep="+q.Grep)
	}
	if q.Author != "" {
		args = append(args, "--author="+q.Author)
	}
	if len(args) > 0 {
		args = append(args, "--regexp-ignore-case", "--fixed-strings")
	}
	return args
}

func (q commitQuery) ref() string {
	if q.Ref == "" {
		return "HEAD"
	}
	return q.Ref
}

// hasCommits reports whether ref resolves. A repo created but never pushed to has no
// commits and git exits non-zero with wording that varies by repo shape, so ask
// rev-parse directly rather than matching English on stderr.
func hasCommits(ctx context.Context, dir, ref string) bool {
	return exec.CommandContext(ctx, gitBinary, "-C", dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run() == nil
}

// countCommits returns how many commits match a query, for the pager.
func countCommits(ctx context.Context, repoID string, q commitQuery) (int, error) {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return 0, err
	}
	if !hasCommits(ctx, dir, q.ref()) {
		return 0, nil
	}
	args := append([]string{"-C", dir, "rev-list", "--count"}, q.filterArgs()...)
	args = append(args, q.ref(), "--")

	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, args...)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("count commits: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		return 0, fmt.Errorf("parse count %q: %w", out.String(), err)
	}
	return n, nil
}

// listCommits returns one page of history, newest first.
//
// An empty repository is not an error: a freshly created repo that has never been
// pushed to is the state the UI renders most often, and it returns an empty page.
func listCommits(ctx context.Context, repoID string, q commitQuery) ([]commit, error) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "gitplane.listCommits")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", repoID))

	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if q.Limit <= 0 || q.Limit > maxCommitPage {
		q.Limit = defaultCommitPage
	}
	if q.Skip < 0 {
		q.Skip = 0
	}
	if !hasCommits(ctx, dir, q.ref()) {
		return []commit{}, nil
	}

	args := []string{"-C", dir, "log",
		"--max-count=" + strconv.Itoa(q.Limit),
		"--skip=" + strconv.Itoa(q.Skip),
		"--format=%H" + commitLogSep + "%an" + commitLogSep + "%aI" + commitLogSep + "%s" + commitLogSep + "%ae",
	}
	args = append(args, q.filterArgs()...)
	args = append(args, q.ref(), "--")

	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, args...)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.ErrorContext(ctx, "gitplane: list commits", "Repo_id", repoID, "ref", q.ref(), "error", err, "stderr", stderr.String())
		return nil, fmt.Errorf("list commits: %w", err)
	}

	commits := []commit{}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, commitLogSep, 5)
		if len(f) < 4 {
			continue
		}
		short := f[0]
		if len(short) > 7 {
			short = short[:7]
		}
		email := ""
		if len(f) == 5 {
			email = f[4]
		}
		commits = append(commits, commit{SHA: f[0], Short: short, Author: f[1], Date: f[2], Subject: f[3], AuthorEmail: email})
	}
	return commits, nil
}

// readmeNames are the top-level files treated as a repo's readme, in preference order.
// The match is case-insensitive because "README.md", "Readme.md" and "readme.md" are
// all common and all mean the same thing to a human.
var readmeNames = []string{"readme.md", "readme.markdown", "readme.txt", "readme", "readme.rst"}

// maxReadmeBytes caps what is read into memory and shipped to a browser. A readme is
// prose; anything past this is a data file that happens to be named like one.
const maxReadmeBytes = 512 * 1024

// readme returns a repo's readme at ref: its actual path (so the UI can name what it
// rendered) and its contents. Returns ok=false when the repo has no commits or no
// readme at the top level — both ordinary states, not errors.
func readme(ctx context.Context, repoID, ref string) (path string, content string, ok bool, err error) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "gitplane.readme")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", repoID))

	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return "", "", false, err
	}
	if ref == "" {
		ref = "HEAD"
	}
	if !hasCommits(ctx, dir, ref) {
		return "", "", false, nil
	}

	// Top level only (no -r): a readme nested in a subdirectory is not the repo's
	// readme, and listing the whole tree of a large repo to find one file is wasteful.
	var tree bytes.Buffer
	ls := exec.CommandContext(ctx, gitBinary, "-C", dir, "ls-tree", "--name-only", ref)
	ls.Stdout = &tree
	if err := ls.Run(); err != nil {
		return "", "", false, fmt.Errorf("list tree: %w", err)
	}
	entries := strings.Split(strings.TrimRight(tree.String(), "\n"), "\n")

	for _, want := range readmeNames {
		for _, e := range entries {
			if strings.ToLower(e) != want {
				continue
			}
			var out bytes.Buffer
			// The path comes from ls-tree, not from the caller, so it cannot be
			// attacker-shaped; "--" still separates it from anything option-like.
			show := exec.CommandContext(ctx, gitBinary, "-C", dir, "show", ref+":"+e, "--")
			show.Stdout = &out
			if err := show.Run(); err != nil {
				return "", "", false, fmt.Errorf("read %s: %w", e, err)
			}
			body := out.String()
			if len(body) > maxReadmeBytes {
				body = body[:maxReadmeBytes]
			}
			return e, body, true, nil
		}
	}
	return "", "", false, nil
}

// ── refs ────────────────────────────────────────────────────────────────────

// ref is a branch or tag with the commit it points at.
type gitRef struct {
	Name    string `json:"name"`
	SHA     string `json:"sha"`
	Date    string `json:"date"`    // committer date of the target, RFC3339
	Subject string `json:"subject"` // target's subject line, for context in a picker
	Default bool   `json:"default,omitempty"`
}

// listRefs enumerates refs under prefix ("refs/heads/" or "refs/tags/"), newest first.
// Tags are dereferenced with ^{} so an annotated tag reports the COMMIT it points at
// rather than the tag object, which is what a caller means by "where is this tag".
func listRefs(ctx context.Context, repoID, prefix string) ([]gitRef, error) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "gitplane.listRefs")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", repoID), attribute.String("git.ref_prefix", prefix))

	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "for-each-ref",
		"--sort=-committerdate",
		"--format=%(refname:short)"+commitLogSep+"%(objectname)"+commitLogSep+
			"%(committerdate:iso-strict)"+commitLogSep+"%(contents:subject)",
		prefix)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}

	refs := []gitRef{}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, commitLogSep, 4)
		if len(f) != 4 {
			continue
		}
		refs = append(refs, gitRef{Name: f[0], SHA: f[1], Date: f[2], Subject: f[3]})
	}
	return refs, nil
}

// headBranch reports the branch HEAD points at, or "" when HEAD is unborn.
func headBranch(ctx context.Context, repoID string) string {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return ""
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "symbolic-ref", "--short", "HEAD")
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// setHEAD points HEAD at a branch that must already exist. Pointing it at a missing
// branch is what produces a clone that transfers everything and checks out nothing
// (see reconcileHEAD), so the existence check is the whole job.
func setHEAD(ctx context.Context, repoID, branch string) error {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return err
	}
	if !branchNameRe.MatchString(branch) {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	ref := "refs/heads/" + branch
	if exec.CommandContext(ctx, gitBinary, "-C", dir, "show-ref", "--verify", "--quiet", ref).Run() != nil {
		return fmt.Errorf("branch %q does not exist", branch)
	}
	if err := exec.CommandContext(ctx, gitBinary, "-C", dir, "symbolic-ref", "HEAD", ref).Run(); err != nil {
		return fmt.Errorf("set HEAD: %w", err)
	}
	slog.InfoContext(ctx, "default branch set", "Repo_id", repoID, "branch", branch)
	return nil
}

// ── tree / blob ─────────────────────────────────────────────────────────────

// treeEntry is one item in a directory listing.
type treeEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"` // "file" | "dir" | "submodule" | "symlink"
	Size int64  `json:"size"`
	Mode string `json:"mode"`
}

// maxBlobBytes caps what is read into memory and returned as text.
const maxBlobBytes = 1 << 20 // 1 MiB

// listTree returns ONE directory level at ref:path. One level, not recursive: a
// recursive listing of a large repo is enormous and the UI renders a directory at a
// time anyway.
//
// path is never joined onto the filesystem — it is handed to git as part of a
// "ref:path" revision, so git resolves it inside the object database and a "../"
// simply fails to resolve rather than escaping anywhere.
func listTree(ctx context.Context, repoID, ref, path string) ([]treeEntry, error) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "gitplane.listTree")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", repoID))

	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if ref == "" {
		ref = "HEAD"
	}
	if !hasCommits(ctx, dir, ref) {
		return []treeEntry{}, nil
	}
	target := ref
	if path != "" {
		target = ref + ":" + strings.Trim(path, "/")
	}

	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "ls-tree", "--long", "--full-name", target)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "Not a valid object name") || strings.Contains(stderr.String(), "not a tree") {
			return nil, errPathNotFound
		}
		return nil, fmt.Errorf("list tree: %w", err)
	}

	entries := []treeEntry{}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		// "<mode> <type> <object> <size>\t<name>" — size is "-" for trees.
		meta, name, found := strings.Cut(line, "\t")
		if !found {
			continue
		}
		f := strings.Fields(meta)
		if len(f) < 4 {
			continue
		}
		e := treeEntry{Name: name, Path: strings.TrimPrefix(strings.Trim(path, "/")+"/"+name, "/"), Mode: f[0]}
		switch {
		case f[1] == "tree":
			e.Type = "dir"
		case f[1] == "commit":
			e.Type = "submodule"
		case f[0] == "120000":
			e.Type = "symlink"
		default:
			e.Type = "file"
		}
		if n, err := strconv.ParseInt(f[3], 10, 64); err == nil {
			e.Size = n
		}
		entries = append(entries, e)
	}
	// Directories first, then files, each alphabetical — the ordering every file
	// browser uses, and git's own is by name only.
	sort.SliceStable(entries, func(i, j int) bool {
		if (entries[i].Type == "dir") != (entries[j].Type == "dir") {
			return entries[i].Type == "dir"
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

// errPathNotFound distinguishes "no such path in this tree" from a git failure.
var errPathNotFound = errors.New("path not found")

// readBlob returns a file's contents at ref:path. Binary files are reported rather
// than returned: shipping arbitrary bytes into a browser as JSON helps nobody.
func readBlob(ctx context.Context, repoID, ref, path string) (content string, size int64, binary bool, err error) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "gitplane.readBlob")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", repoID))

	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return "", 0, false, err
	}
	if ref == "" {
		ref = "HEAD"
	}
	target := ref + ":" + strings.Trim(path, "/")

	var sizeOut bytes.Buffer
	sizeCmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "cat-file", "-s", target)
	sizeCmd.Stdout = &sizeOut
	if err := sizeCmd.Run(); err != nil {
		return "", 0, false, errPathNotFound
	}
	size, _ = strconv.ParseInt(strings.TrimSpace(sizeOut.String()), 10, 64)

	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "cat-file", "blob", target)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", size, false, errPathNotFound
	}
	raw := out.Bytes()
	// A NUL in the first 8k is git's own heuristic for "binary".
	probe := raw
	if len(probe) > 8000 {
		probe = probe[:8000]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return "", size, true, nil
	}
	if len(raw) > maxBlobBytes {
		raw = raw[:maxBlobBytes]
	}
	return string(raw), size, false, nil
}

// writeArchive streams a tar.gz of ref straight to w. Streaming rather than buffering:
// an archive of a large repo is large, and it is exactly the kind of response the
// wire-route deadline handling exists for.
func writeArchive(ctx context.Context, repoID, ref, prefix string, w io.Writer) error {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "gitplane.writeArchive")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", repoID))

	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return err
	}
	if ref == "" {
		ref = "HEAD"
	}
	if !hasCommits(ctx, dir, ref) {
		return errPathNotFound
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "archive",
		"--format=tar.gz", "--prefix="+prefix+"/", ref)
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.ErrorContext(ctx, "gitplane: archive", "Repo_id", repoID, "ref", ref, "error", err, "stderr", stderr.String())
		return fmt.Errorf("archive: %w", err)
	}
	return nil
}

// refSnapshot maps branch name → sha, for diffing what a push changed.
func refSnapshot(ctx context.Context, repoID string) map[string]string {
	refs, err := listRefs(ctx, repoID, "refs/heads/")
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(refs))
	for _, r := range refs {
		out[r.Name] = r.SHA
	}
	return out
}

// changedRefs returns the branches that were created or updated between two
// snapshots. Deletions are deliberately not reported as changes here — a push that
// only deletes a branch has no new content for a listener to build.
func changedRefs(before, after map[string]string) []string {
	var changed []string
	for name, sha := range after {
		if before[name] != sha {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	return changed
}

// ── merging ─────────────────────────────────────────────────────────────────

// fileChange is one file's diffstat between two commits.
type fileChange struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
}

// diffStat summarises what merging head into base would bring: the three-dot form,
// so it describes head's own commits rather than every difference between the two
// branches (base moving on must not inflate the diff).
func diffStat(ctx context.Context, repoID, base, head string) ([]fileChange, error) {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "diff", "--numstat", base+"..."+head)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("diff: %w", err)
	}
	changes := []fileChange{}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		c := fileChange{Path: f[2]}
		// numstat writes "-" for both counts on a binary file.
		if f[0] == "-" || f[1] == "-" {
			c.Binary = true
		} else {
			c.Additions, _ = strconv.Atoi(f[0])
			c.Deletions, _ = strconv.Atoi(f[1])
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// diffPatch returns the full unified diff for base...head (the symmetric range git
// uses for reviews: everything on head since it forked from base). base/head become git
// argv elements, so callers must charset-restrict them (branchNameRe).
func diffPatch(ctx context.Context, repoID, base, head string) (string, error) {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "diff", "--no-color", base+"..."+head)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("diff: %w", err)
	}
	return out.String(), nil
}

// parseNumstat turns `git ... --numstat` output into fileChange rows (path + line
// counts; "-" counts mean a binary file).
func parseNumstat(s string) []fileChange {
	changes := []fileChange{}
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		c := fileChange{Path: f[2]}
		if f[0] == "-" || f[1] == "-" {
			c.Binary = true
		} else {
			c.Additions, _ = strconv.Atoi(f[0])
			c.Deletions, _ = strconv.Atoi(f[1])
		}
		changes = append(changes, c)
	}
	return changes
}

// commitDetail is a single commit with its per-file summary and full unified diff, for
// the portal's commit view.
type commitDetail struct {
	commit
	Body   string       `json:"body"`
	Parent string       `json:"parent"`
	Files  []fileChange `json:"files"`
	Diff   string       `json:"diff"`
}

// commitDetailFor resolves rev (a sha or ref) to its metadata, numstat summary and the
// full unified diff. rev becomes a git argv element, so the caller MUST charset-restrict
// it (branchNameRe) before calling. An unknown rev yields errPathNotFound.
func commitDetailFor(ctx context.Context, repoID, rev string) (*commitDetail, error) {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return nil, err
	}
	const sep = "\x1f"
	// Metadata in one read. %P is the space-separated parents; the first is the base.
	format := "--format=%H" + sep + "%h" + sep + "%an" + sep + "%aI" + sep + "%s" + sep + "%P" + sep + "%b"
	var meta bytes.Buffer
	mc := exec.CommandContext(ctx, gitBinary, "-C", dir, "show", "-s", format, rev, "--")
	mc.Stdout = &meta
	if err := mc.Run(); err != nil {
		return nil, errPathNotFound
	}
	f := strings.SplitN(strings.TrimRight(meta.String(), "\n"), sep, 7)
	if len(f) < 6 {
		return nil, errPathNotFound
	}
	d := &commitDetail{commit: commit{SHA: f[0], Short: f[1], Author: f[2], Date: f[3], Subject: f[4]}}
	if p := strings.Fields(f[5]); len(p) > 0 {
		d.Parent = p[0]
	}
	if len(f) == 7 {
		d.Body = strings.TrimRight(f[6], "\n")
	}
	// Per-file summary.
	var ns bytes.Buffer
	nc := exec.CommandContext(ctx, gitBinary, "-C", dir, "show", "--numstat", "--format=", rev, "--")
	nc.Stdout = &ns
	if err := nc.Run(); err == nil {
		d.Files = parseNumstat(ns.String())
	}
	// The unified diff itself. --first-parent keeps a merge commit's diff single-sided.
	var patch bytes.Buffer
	pc := exec.CommandContext(ctx, gitBinary, "-C", dir, "show", "--patch", "--no-color", "--first-parent", "--format=", rev, "--")
	pc.Stdout = &patch
	if err := pc.Run(); err == nil {
		d.Diff = patch.String()
	}
	return d, nil
}

// errNoChange means an edit's content matched the file already there — no commit made.
var errNoChange = errors.New("file is unchanged")

// commitFileChange writes content to path on branch as a new commit — a normal
// fast-forward, so branch protections that only block force-push/delete never apply. It
// builds the tree in a throwaway index (the bare repo is only touched by the final CAS
// ref update) and attributes the commit to author. Returns the new commit sha, or
// errNoChange if content was identical.
func commitFileChange(ctx context.Context, repoID, branch, path, content, message, author string) (string, error) {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return "", err
	}
	var tipBuf bytes.Buffer
	tipCmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	tipCmd.Stdout = &tipBuf
	if err := tipCmd.Run(); err != nil {
		return "", fmt.Errorf("branch %q not found", branch)
	}
	tip := strings.TrimSpace(tipBuf.String())

	// Preserve the existing file mode; a new path defaults to a regular file.
	mode := "100644"
	var lsBuf bytes.Buffer
	ls := exec.CommandContext(ctx, gitBinary, "-C", dir, "ls-tree", tip, "--", path)
	ls.Stdout = &lsBuf
	if ls.Run() == nil {
		if f := strings.Fields(lsBuf.String()); len(f) >= 2 {
			if f[1] == "tree" {
				return "", fmt.Errorf("%q is a directory", path)
			}
			mode = f[0]
		}
	}

	var blobBuf bytes.Buffer
	ho := exec.CommandContext(ctx, gitBinary, "-C", dir, "hash-object", "-w", "--stdin")
	ho.Stdin = strings.NewReader(content)
	ho.Stdout = &blobBuf
	if err := ho.Run(); err != nil {
		return "", fmt.Errorf("hash-object: %w", err)
	}
	blob := strings.TrimSpace(blobBuf.String())

	idx, err := os.CreateTemp("", "gf-idx-*")
	if err != nil {
		return "", err
	}
	idxPath := idx.Name()
	idx.Close()
	defer os.Remove(idxPath)
	idxEnv := append(os.Environ(), "GIT_INDEX_FILE="+idxPath)

	rt := exec.CommandContext(ctx, gitBinary, "-C", dir, "read-tree", tip)
	rt.Env = idxEnv
	if err := rt.Run(); err != nil {
		return "", fmt.Errorf("read-tree: %w", err)
	}
	ui := exec.CommandContext(ctx, gitBinary, "-C", dir, "update-index", "--add", "--cacheinfo", mode+","+blob+","+path)
	ui.Env = idxEnv
	if err := ui.Run(); err != nil {
		return "", fmt.Errorf("update-index: %w", err)
	}
	var treeBuf bytes.Buffer
	wt := exec.CommandContext(ctx, gitBinary, "-C", dir, "write-tree")
	wt.Env = idxEnv
	wt.Stdout = &treeBuf
	if err := wt.Run(); err != nil {
		return "", fmt.Errorf("write-tree: %w", err)
	}
	tree := strings.TrimSpace(treeBuf.String())

	var tipTreeBuf bytes.Buffer
	tt := exec.CommandContext(ctx, gitBinary, "-C", dir, "rev-parse", tip+"^{tree}")
	tt.Stdout = &tipTreeBuf
	if tt.Run() == nil && strings.TrimSpace(tipTreeBuf.String()) == tree {
		return "", errNoChange
	}

	var commitBuf bytes.Buffer
	ct := exec.CommandContext(ctx, gitBinary, "-C", dir, "commit-tree", tree, "-p", tip, "-m", message)
	ct.Stdout = &commitBuf
	ct.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+author, "GIT_AUTHOR_EMAIL="+author+"@codearmory.local",
		"GIT_COMMITTER_NAME="+author, "GIT_COMMITTER_EMAIL="+author+"@codearmory.local")
	if err := ct.Run(); err != nil {
		return "", fmt.Errorf("commit-tree: %w", err)
	}
	commit := strings.TrimSpace(commitBuf.String())
	if err := exec.CommandContext(ctx, gitBinary, "-C", dir, "update-ref", "refs/heads/"+branch, commit, tip).Run(); err != nil {
		return "", fmt.Errorf("update %s (it moved — reload and retry): %w", branch, err)
	}
	slog.InfoContext(ctx, "file committed", "Repo_id", repoID, "branch", branch, "path", path, "commit", commit)
	return commit, nil
}

// commitsBetween counts how many commits head is ahead of base.
func commitsBetween(ctx context.Context, repoID, base, head string) int {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return 0
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "rev-list", "--count", base+".."+head)
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out.String()))
	return n
}

// mergeResult describes whether a merge can proceed, and what it would produce.
type mergeResult struct {
	Mergeable bool     `json:"mergeable"`
	Conflicts []string `json:"conflicts,omitempty"`
	Tree      string   `json:"-"` // the merged tree, when mergeable
	AlreadyIn bool     `json:"already_merged"`
	// Reason explains an unmergeable state that is not a file conflict — unrelated
	// histories, most often. Without it the UI can only say "cannot merge" and leave
	// the user guessing at which of several quite different problems they have.
	Reason string `json:"reason,omitempty"`
}

// tryMerge computes the merge of head into base WITHOUT a worktree, using
// `git merge-tree --write-tree`. This matters for a bare repo on a single-writer
// volume: checking out a worktree to test a merge would serialise every mergeability
// check against every other, and would need writable space per check.
//
// The command writes the merged tree to the object database and exits non-zero on
// conflict, listing the conflicted paths — so one call answers both "can this merge"
// and "what would it produce".
func tryMerge(ctx context.Context, repoID, base, head string) (mergeResult, error) {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return mergeResult{}, err
	}
	// Already merged: head adds nothing, so there is nothing to do and a merge commit
	// would be noise.
	if commitsBetween(ctx, repoID, base, head) == 0 {
		return mergeResult{Mergeable: false, AlreadyIn: true}, nil
	}

	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "merge-tree", "--write-tree", "--name-only", base, head)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = cmd.Run()
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if err != nil {
		// Conflict: the first line is the (partial) tree, the rest are conflicted paths.
		conflicts := []string{}
		if len(lines) > 1 {
			for _, l := range lines[1:] {
				if l = strings.TrimSpace(l); l != "" {
					conflicts = append(conflicts, l)
				}
			}
		}
		if len(conflicts) == 0 {
			// Not a content conflict: the two branches may have no common ancestor at
			// all (a branch pushed from an unrelated repository). That is a legitimate
			// answer to "can this merge", not a server failure.
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return mergeResult{Mergeable: false, Reason: strings.TrimPrefix(msg, "fatal: ")}, nil
			}
			return mergeResult{}, fmt.Errorf("merge-tree: %w", err)
		}
		return mergeResult{Mergeable: false, Conflicts: conflicts}, nil
	}
	if len(lines) == 0 || lines[0] == "" {
		return mergeResult{}, fmt.Errorf("merge-tree produced no tree")
	}
	return mergeResult{Mergeable: true, Tree: strings.TrimSpace(lines[0])}, nil
}

// mergeBranches performs the merge, writing a real merge commit with both parents and
// advancing base to it. Returns the new commit sha.
//
// The ref update is guarded with the expected old value, so a push that lands between
// the mergeability check and this update fails the merge rather than silently
// discarding the pushed commit.
func mergeBranches(ctx context.Context, repoID, base, head, message, author string) (string, error) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "gitplane.mergeBranches")
	defer span.End()

	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return "", err
	}
	res, err := tryMerge(ctx, repoID, base, head)
	if err != nil {
		return "", err
	}
	if !res.Mergeable {
		if res.AlreadyIn {
			return "", fmt.Errorf("nothing to merge: %s is already contained in %s", head, base)
		}
		return "", fmt.Errorf("cannot merge: conflicts in %s", strings.Join(res.Conflicts, ", "))
	}

	rev := func(ref string) (string, error) {
		var o bytes.Buffer
		c := exec.CommandContext(ctx, gitBinary, "-C", dir, "rev-parse", "--verify", ref+"^{commit}")
		c.Stdout = &o
		if err := c.Run(); err != nil {
			return "", fmt.Errorf("resolve %s: %w", ref, err)
		}
		return strings.TrimSpace(o.String()), nil
	}
	baseSHA, err := rev(base)
	if err != nil {
		return "", err
	}
	headSHA, err := rev(head)
	if err != nil {
		return "", err
	}

	var out bytes.Buffer
	commit := exec.CommandContext(ctx, gitBinary, "-C", dir, "commit-tree", res.Tree,
		"-p", baseSHA, "-p", headSHA, "-m", message)
	commit.Stdout = &out
	// The merge is attributed to whoever pressed the button. A bare repo has no
	// configured identity, so it must be supplied or commit-tree fails outright.
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+author, "GIT_AUTHOR_EMAIL="+author+"@codearmory.local",
		"GIT_COMMITTER_NAME="+author, "GIT_COMMITTER_EMAIL="+author+"@codearmory.local")
	if err := commit.Run(); err != nil {
		return "", fmt.Errorf("commit-tree: %w", err)
	}
	mergeSHA := strings.TrimSpace(out.String())

	// Compare-and-swap on the ref: baseSHA is what we merged against.
	if err := exec.CommandContext(ctx, gitBinary, "-C", dir, "update-ref",
		"refs/heads/"+base, mergeSHA, baseSHA).Run(); err != nil {
		return "", fmt.Errorf("update %s (it moved during the merge): %w", base, err)
	}
	slog.InfoContext(ctx, "merged", "Repo_id", repoID, "base", base, "head", head, "commit", mergeSHA)
	return mergeSHA, nil
}

// branchExists reports whether a branch is present, so a caller can reject a bad ref
// up front rather than at merge time.
func branchExists(ctx context.Context, repoID, branch string) bool {
	dir, err := localDirFor(ctx, repoID)
	if err != nil || !branchNameRe.MatchString(branch) {
		return false
	}
	return exec.CommandContext(ctx, gitBinary, "-C", dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

// branchTip resolves a branch to its tip commit SHA, so a commit status can be keyed
// to the head of a PR's source. Returns ("", false) if the branch does not exist.
func branchTip(ctx context.Context, repoID, branch string) (string, bool) {
	dir, err := localDirFor(ctx, repoID)
	if err != nil || !branchNameRe.MatchString(branch) {
		return "", false
	}
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	cmd.Stdout = &buf
	if cmd.Run() != nil {
		return "", false
	}
	return strings.TrimSpace(buf.String()), true
}
