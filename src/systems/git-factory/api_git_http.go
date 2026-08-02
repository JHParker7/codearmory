package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// The git wire surface (ARCHITECTURE §2b). Same authorization as the management API —
// gatekeeper for coarse access, the owner filter here for per-record ownership — but
// different auth MECHANICS, because the client is git rather than our own UI:
//
//   - credentials arrive as HTTP Basic with the token in the PASSWORD field, since
//     that is what git does with a stored credential;
//   - an unauthenticated request must be answered 401 WITH a WWW-Authenticate header,
//     or git neither prompts nor retries — it just fails;
//   - the request and response are unbounded byte streams, so the JSON API's body cap
//     and the server's write deadline have to be lifted per-route.

// gitAuthRealm is sent on every wire 401. The realm string is opaque to git but the
// header's presence is what makes it retry with credentials.
const gitAuthRealm = `Basic realm="git"`

// challengeWriter adds the WWW-Authenticate header to any 401 written through it,
// including the ones the gatekeeper SDK writes itself, so every unauthorized answer on
// this surface is one git can act on.
type challengeWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (c *challengeWriter) WriteHeader(status int) {
	if !c.wroteHeader {
		c.wroteHeader = true
		if status == http.StatusUnauthorized {
			c.Header().Set("WWW-Authenticate", gitAuthRealm)
		}
	}
	c.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the underlying writer for Flush and the
// deadline calls below.
func (c *challengeWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// gitToken extracts the caller's token from either credential shape git can send.
// Basic is the norm (password field holds a PAT, username is ignored); Bearer covers
// programmatic clients using http.extraHeader.
func gitToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if user, pass, ok := r.BasicAuth(); ok {
		// Token in the password field is the documented shape; fall back to the
		// username so `git clone https://<token>@host/...` also works.
		if pass != "" {
			return pass
		}
		return user
	}
	return ""
}

// repoPathParts pulls the namespace and repo name out of the URL, accepting the name
// with or without the .git suffix — git clones /{ns}/{repo} and /{ns}/{repo}.git
// interchangeably. Both segments are validated against the same allowlist the
// management API uses BEFORE they reach any lookup, per ARCHITECTURE §6; note that
// nothing here ever becomes a filesystem path (that is derived from the repo id).
func repoPathParts(r *http.Request) (namespace, name string, ok bool) {
	namespace = r.PathValue("ns")
	name = strings.TrimSuffix(r.PathValue("repo"), ".git")
	// Both segments go through validRepoName, not just the bare pattern: the pattern
	// alone admits "." and "..", which are exactly the traversal-shaped names the guard
	// exists for.
	if !validRepoName(namespace) || !validRepoName(name) {
		return "", "", false
	}
	return namespace, name, true
}

// challengeUnauthorized writes the 401 that makes git prompt for credentials and
// retry. It always returns false, so callers can `return Repo{}, "", challenge…(w)`.
func challengeUnauthorized(w http.ResponseWriter) bool {
	w.Header().Set("WWW-Authenticate", gitAuthRealm)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// authorizeGitRepo resolves the repo addressed by the URL and authorizes the caller
// for svc, writing the failure response itself and returning ok=false when it does.
//
// Order matters. The 401 challenge comes first, so an anonymous client is told how to
// authenticate rather than being told the repo does not exist. After that a missing repo
// and a repo the caller may not touch are BOTH 404, so a probe cannot enumerate other
// people's repositories (ARCHITECTURE §3). The permission check names the REPO's
// namespace rather than the caller's, which is what makes it answer "may this caller act
// on THIS repo" instead of the vacuous "may this caller act on repos".
func authorizeGitRepo(w http.ResponseWriter, r *http.Request, svc gitService) (Repo, string, bool) {
	ctx := r.Context()

	namespace, name, ok := repoPathParts(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return Repo{}, "", false
	}

	// A trusted forward from a peer node was already authorized at the entry node
	// (proxy.go). We load the repo and serve from local disk without a second gatekeeper
	// round-trip. The mirror read-only guard below still applies as defence in depth.
	if isTrustedForward(r) {
		re, err := getRepoByPath(ctx, namespace, name)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return Repo{}, "", false
		}
		if svc == svcReceivePack && re.Kind == kindMirror {
			http.Error(w, "repository is a read-only mirror", http.StatusForbidden)
			return Repo{}, "", false
		}
		return re, re.Owner, true
	}

	token := gitToken(r)

	// The repo is loaded before the challenge because visibility is a property of the
	// record: an anonymous fetch of a PUBLIC repo must succeed, and there is no way to
	// know it is public without looking. The 401-before-404 property still holds — an
	// anonymous caller is told to authenticate whether the repo is missing or merely
	// private, so the reordering leaks nothing.
	re, err := getRepoByPath(ctx, namespace, name)
	switch {
	case err == errRepoNotFound:
		if token == "" {
			return Repo{}, "", challengeUnauthorized(w)
		}
		http.Error(w, "not found", http.StatusNotFound)
		return Repo{}, "", false
	case err != nil:
		slog.ErrorContext(ctx, "git http: lookup repo", "namespace", namespace, "name", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return Repo{}, "", false
	}

	// A git-factory clone token (service-minted, read-only, repo-scoped — clonetoken.go)
	// lets a workflow runner clone a mirror as the CI identity with no platform JWT. It
	// is honoured ONLY for the exact repo it was minted for and ONLY for reads; anything
	// else falls through to the gatekeeper path below, so a JWT/PAT is unaffected.
	if token != "" {
		if tokenRepoID, ok := verifyCloneToken(token); ok {
			if tokenRepoID != re.ID {
				http.Error(w, "not found", http.StatusNotFound) // wrong repo — no enumeration
				return Repo{}, "", false
			}
			if svc != svcUploadPack {
				http.Error(w, "clone token is read-only", http.StatusForbidden)
				return Repo{}, "", false
			}
			return re, re.Owner, true
		}
	}

	// Anonymous clone: visibility alone authorizes a fetch from a public repo, so no
	// credential is asked for and none is used. Push is never reachable this way —
	// receive-pack's action is not in publicReadActions — so a public repo is
	// world-readable and still writable only by grant.
	if token == "" && allowedByVisibility(re, svc.action()) {
		return re, "", true
	}
	if token == "" {
		return Repo{}, "", challengeUnauthorized(w)
	}
	// Normalize to Bearer so the gatekeeper SDK — and therefore the authorization
	// logic itself — is shared verbatim with the management API. Only the credential
	// extraction above differs between the two surfaces.
	r.Header.Set("Authorization", "Bearer "+token)

	// The resource names the REPO's namespace, so gatekeeper genuinely answers "may
	// this caller act on THIS repo" — which is what makes a shared repo fetchable. It
	// replaces the old owner-equality check: that check was the per-record gate while
	// the resource was caller-scoped, and keeping it would deny every collaborator.
	aw := &notFoundOnDeny{ResponseWriter: w}
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, aw, r, svc.action(), resRepoOf(re))
	if !ok {
		// Everything below overrides a DENIAL only; a 401/500 the SDK already answered
		// with passes through untouched.
		if !aw.swallowed {
			return Repo{}, "", false
		}
		// Three fallbacks, narrowest first. Each sets the caller id it resolved and
		// falls through to the mirror guard below — a fallback that authorizes a WRITE
		// must not skip it, or a mirror could be pushed to via a project role.
		//
		//  1. the repo's own owner, for a namespace no grant can name — an org repo, or
		//     a renamed user (callerOwnsRepo). The management API applies the same
		//     fallback, so a clone and a browse agree on who may reach the repository.
		//  2. a repo filed into a project, reachable by anyone holding the matching
		//     project role. Tried after the per-repo grant so an explicit share wins.
		//  3. a public repo, which authorizes its own reads with no grant at all — the
		//     same rule the anonymous path above uses, for a caller who sent a token.
		uid, granted := callerOwnsRepo(ctx, token, svc.action(), re)
		if !granted {
			uid, granted = repoProjectAuthorizes(ctx, token, svc.action(), re)
		}
		if !granted && allowedByVisibility(re, svc.action()) {
			uid, granted = "", true
		}
		if !granted {
			// Same rule as the management API: no access reads as not-found, so a probe
			// cannot enumerate other people's repositories.
			http.Error(w, "not found", http.StatusNotFound)
			return Repo{}, "", false
		}
		userID = uid
	}
	// A mirror is a read-only cache of its upstream: a client push would silently
	// diverge it from the source of truth. Refuse receive-pack even for a caller who is
	// otherwise authorized to write — and do it only AFTER auth so an unauthorized probe
	// still learns nothing (the 401/404 paths above are unchanged).
	if svc == svcReceivePack && re.Kind == kindMirror {
		http.Error(w, "repository is a read-only mirror", http.StatusForbidden)
		return Repo{}, "", false
	}
	return re, userID, true
}

// unboundedStream clears the read and write deadlines for a wire request. The server's
// 15s read / 60s write timeouts are right for JSON CRUD and fatal here: a clone of any
// real repo streams for longer than 60s, and a push body is the entire packfile. Both
// failures look like a network fault from the client, which is what makes them worth
// disabling explicitly rather than raising to some larger guess.
func unboundedStream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		slog.DebugContext(r.Context(), "git http: clear write deadline", "error", err)
	}
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		slog.DebugContext(r.Context(), "git http: clear read deadline", "error", err)
	}
}

// handleInfoRefs serves the ref advertisement that opens every clone, fetch and push.
func handleInfoRefs(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleInfoRefs")
	defer span.End()
	r = r.WithContext(ctx)

	cw := &challengeWriter{ResponseWriter: w}
	svc, ok := parseGitService(r.URL.Query().Get("service"))
	if !ok {
		// A dumb-HTTP client (or a probe) asking for something else. We serve only
		// Smart HTTP, and saying so beats a confusing 404.
		http.Error(cw, "only smart HTTP is supported", http.StatusForbidden)
		return
	}
	span.SetAttributes(attribute.String("git.service", string(svc)))

	re, _, ok := authorizeGitRepo(cw, r, svc)
	if !ok {
		return
	}
	// Bytes on another node → stream the advertisement from there (reads fan out to a
	// caught-up replica, writes to the primary — ARCHITECTURE §5 Steps 3–4).
	if maybeProxyToNode(cw, r, re, svc) {
		return
	}
	unboundedStream(cw, r)

	cw.Header().Set("Content-Type", "application/x-"+string(svc)+"-advertisement")
	cw.Header().Set("Cache-Control", "no-cache")
	cw.WriteHeader(http.StatusOK)
	if err := advertiseRefs(ctx, re.ID, svc, cw); err != nil {
		// The status is already sent, so the only honest signal left is to cut the
		// stream; git reports it as a broken connection.
		slog.ErrorContext(ctx, "git http: advertisement failed mid-stream", "Repo_id", re.ID, "error", err)
	}
}

// handleUploadPack serves fetch/clone; handleReceivePack serves push.
func handleUploadPack(w http.ResponseWriter, r *http.Request)  { servicePack(w, r, svcUploadPack) }
func handleReceivePack(w http.ResponseWriter, r *http.Request) { servicePack(w, r, svcReceivePack) }

func servicePack(w http.ResponseWriter, r *http.Request, svc gitService) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handle"+svc.subcommand())
	defer span.End()
	span.SetAttributes(attribute.String("git.service", string(svc)))
	r = r.WithContext(ctx)

	cw := &challengeWriter{ResponseWriter: w}
	re, userID, ok := authorizeGitRepo(cw, r, svc)
	if !ok {
		return
	}
	// Bytes on another node → the owning node runs the pack (and its quota/audit/HEAD
	// reconcile) locally; we just stream it through. Writes go to the primary, reads to
	// a caught-up replica (ARCHITECTURE §5 Steps 3–4).
	if maybeProxyToNode(cw, r, re, svc) {
		return
	}
	unboundedStream(cw, r)

	// Snapshot the refs so the event can say WHAT changed. Diffing before/after is
	// cheaper and far more robust than parsing the pkt-line command list out of the
	// request body, which is being streamed straight into git.
	var before map[string]string
	if svc == svcReceivePack {
		// Refuse an over-quota push HERE, while a real status code can still be sent.
		// Once the 200 below is written the only way to reject anything is to cut the
		// stream, which reaches the user as a network error rather than a reason.
		if headroom, limited := quotaHeadroom(ctx, re.ID); limited && headroom <= 0 {
			http.Error(cw, "repository is at its storage quota", http.StatusInsufficientStorage)
			return
		}
		before = refSnapshot(ctx, re.ID)
	}

	// git sends a large upload-pack request (many "have" lines on an incremental fetch)
	// gzip-compressed. Without decompressing it here the raw gzip bytes reach
	// git-upload-pack's stdin and it dies with "protocol error: bad line length
	// character" — the mid-stream pack failure that broke large clones. Decompress when
	// the client set Content-Encoding: gzip; the header write below can no longer send a
	// status, so a malformed body is rejected here first.
	var body io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(cw, "malformed gzip request body", http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}

	cw.Header().Set("Content-Type", "application/x-"+string(svc)+"-result")
	cw.Header().Set("Cache-Control", "no-cache")
	cw.WriteHeader(http.StatusOK)
	if err := runPack(ctx, re.ID, svc, body, cw); err != nil {
		slog.ErrorContext(ctx, "git http: pack failed mid-stream", "Repo_id", re.ID, "service", svc, "error", err)
		// Audited as a failure too: "someone tried and it broke" is exactly the kind
		// of thing a trail is read for, and a trail of successes only is misleading.
		auditEvent(ctx, userID, auditActionFor(svc), re.ID, auditDetail(re, "outcome=error"))
		return
	}
	// A push may have created the first branch, or a branch other than the one HEAD was
	// initialized at. Reconcile after the fact rather than guessing at create time: the
	// alternative is a repo that clones every object and checks out nothing. Failure
	// here is logged, never surfaced — the push itself already succeeded.
	if svc == svcReceivePack {
		if err := reconcileHEAD(ctx, re.ID, re.DefaultBranch); err != nil {
			slog.WarnContext(ctx, "git http: could not reconcile HEAD after push", "Repo_id", re.ID, "error", err)
		}
		if err := touchRepo(ctx, re.ID); err != nil {
			slog.WarnContext(ctx, "git http: could not bump updated_at after push", "Repo_id", re.ID, "error", err)
		}
		// A push is the only thing that grows a repo, so this is the moment its size
		// is worth measuring — and the number the next push's quota check reads.
		if err := recordRepoSize(ctx, re.ID); err != nil {
			slog.WarnContext(ctx, "git http: could not record repo size after push", "Repo_id", re.ID, "error", err)
		}
		// Bump the repo version and push the new packs to replicas (no-op on a single
		// node). The version gate then lets a replica serve reads only once it catches up.
		if v, err := bumpRepoVersion(ctx, re.ID); err != nil {
			slog.WarnContext(ctx, "git http: could not bump repo version after push", "Repo_id", re.ID, "error", err)
		} else {
			re.Version = v
			replicateRepo(ctx, re, v)
		}
		refs := changedRefs(before, refSnapshot(ctx, re.ID))
		// Which refs moved is the fact that makes a push entry useful: "someone
		// rewrote main" reads very differently from "someone pushed a topic branch".
		//
		// A successful RPC that moved no ref is NOT recorded. git sends more than one
		// receive-pack POST per push (the first negotiates), so auditing every
		// invocation writes two entries for one push and reads as a duplicate. A
		// failure is still recorded whether or not anything moved — that one is the
		// entry someone will go looking for.
		//
		// The push EVENT is gated on the same test, for the same reason and with a
		// sharper consequence. notifyPush falls back to the default branch when refs
		// is empty, so emitting on every invocation published a second event claiming
		// the default branch had moved. That matched any ref_filter on it and fired
		// every subscribed pipeline twice — and the two runs then contend on the
		// shared run caches rather than one being harmlessly redundant.
		//
		// Emitted last, once the bytes are safely on disk. A listener that is down
		// must never turn a successful push into a failed one.
		if len(refs) > 0 {
			auditEvent(ctx, userID, auditActionPush, re.ID,
				auditDetail(re, "refs="+refsSummary(refs), "outcome=ok"))
			notifyPush(ctx, re, userID, refs, before)
		}
		return
	}
	auditEvent(ctx, userID, auditActionFetch, re.ID, auditDetail(re, "outcome=ok"))
}

// auditActionFor maps a wire service to the action recorded for it.
func auditActionFor(svc gitService) string {
	if svc == svcReceivePack {
		return auditActionPush
	}
	return auditActionFetch
}

// refsSummary renders the changed refs compactly, capped so one enormous push cannot
// dominate the trail. The count is kept when the list is trimmed, because "42 refs"
// is still an answer.
func refsSummary(refs []string) string {
	const max = 10
	if len(refs) == 0 {
		return "none"
	}
	if len(refs) <= max {
		return strings.Join(refs, ",")
	}
	return strings.Join(refs[:max], ",") + fmt.Sprintf(",+%d more", len(refs)-max)
}
