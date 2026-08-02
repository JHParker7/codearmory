package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Audit for git operations.
//
// The platform has one audit trail and it lives in gatekeeper, so the git plane
// contributes to it rather than keeping a log of its own. What is recorded is what a
// reader of that trail asks after the fact: who fetched or rewrote what, on which
// refs, and whether it worked.
//
// Two rules shape the code below:
//
//   - Auditing NEVER fails the operation. The entry is written after the bytes are
//     safely transferred, out of band, with its own timeout. A gatekeeper that is
//     slow or down must not turn a successful push into a failed one — an audit trail
//     that can break pushes will be the first thing switched off.
//   - The action is the caller's claim, but the SERVICE is not: gatekeeper stamps the
//     authenticated service name onto every entry, so these actions land as
//     "codearmory_git_factory.push" and cannot masquerade as anything else.

// serviceKey returns the current rotated service key, set in main from the value
// StartKeyRotation hands back. Nil until then (and in tests), which turns auditing
// into a no-op rather than a panic.
var serviceKey func() string

// auditTimeout bounds the out-of-band write. Short on purpose: this runs after the
// user's request is finished, so a slow gatekeeper should cost a dropped entry, not a
// goroutine that lives forever.
const auditTimeout = 5 * time.Second

// Actions recorded by the git plane. Short lowercase tokens — gatekeeper requires it,
// because the action is what a reader filters on.
const (
	auditActionFetch      = "fetch"
	auditActionPush       = "push"
	auditActionRepoCreate = "repo.create"
	auditActionRepoDelete = "repo.delete"
)

// auditEvent records one entry, asynchronously. actorID is the authenticated user, or
// empty for an anonymous fetch of a public repo — gatekeeper then attributes the entry
// to this service, which is the honest answer: nobody identified themselves.
func auditEvent(ctx context.Context, actorID, action, repoID, detail string) {
	if serviceKey == nil || gatekeeperURL == "" {
		return
	}
	body, err := json.Marshal(map[string]string{
		"action":      action,
		"resource_id": repoID,
		"actor_id":    actorID,
		"detail":      detail,
	})
	if err != nil {
		return
	}
	// Detached from the request context on purpose: the client has been served and
	// its context is about to be cancelled, which would cancel this write with it.
	go func() {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(writeCtx, http.MethodPost, strings.TrimRight(gatekeeperURL, "/")+"/internal/audit-logs", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Service-Key", serviceName+":"+serviceKey())
		resp, err := httpClient.Do(req)
		if err != nil {
			slog.WarnContext(writeCtx, "audit: could not record event", "action", action, "Repo_id", repoID, "error", err)
			return
		}
		defer resp.Body.Close() //nolint:errcheck
		if resp.StatusCode >= 300 {
			slog.WarnContext(writeCtx, "audit: gatekeeper rejected the event", "action", action, "Repo_id", repoID, "status", resp.StatusCode)
		}
	}()
}

// auditDetail renders the human-readable half of an entry: the repo by its clone path
// (an id alone is unreadable in a log), then whatever the operation is worth saying.
func auditDetail(re Repo, parts ...string) string {
	all := append([]string{re.Namespace + "/" + re.Name}, parts...)
	kept := all[:0]
	for _, p := range all {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}
