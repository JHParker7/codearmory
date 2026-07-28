package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Owner-first (namespace-first) per-record resources.
//
// The problem this addresses: every per-record resource used to be declared as
// "tickets/tickets/{id}", and gatekeeper prefixes the CALLER's username to an
// unscoped resource. The evaluated string was therefore identical whether the
// record belonged to the caller or to someone else, so gatekeeper returned
// authorized for ANY id and the only thing standing between a user and another
// user's ticket was this service remembering to filter by owner in its own query.
//
// A per-record resource is now "<ns>/tickets/tickets/{id}", where ns is the owner's
// namespace. gatekeeper leaves an owner-qualified resource alone (scopeResource /
// ownerQualified), so the check is evaluated against the OWNER's namespace and a
// grant must actually exist for the caller to pass.
//
// Collection endpoints (GET/POST /tickets) stay caller-scoped: "show me mine" is
// exactly the caller-relative question, and there is no single owner to name.

// ticketResource is the gatekeeper resource for one ticket.
//
// An empty ns means a row created before the namespace column existed. Those keep
// the legacy caller-scoped form — the alternative is a backfill that would have to
// resolve a username for every distinct historical creator, and a wrong guess there
// locks a user out of their own tickets. New rows always carry a namespace, so the
// legacy form drains naturally.
func ticketResource(ns, id string) string {
	if ns == "" {
		return "tickets/tickets/" + id
	}
	return ns + "/tickets/tickets/" + id
}

// commentResource is the gatekeeper resource for one comment on a ticket.
func commentResource(ns, ticketID, commentID string) string {
	if ns == "" {
		return "tickets/tickets/" + ticketID + "/comments/" + commentID
	}
	return ns + "/tickets/tickets/" + ticketID + "/comments/" + commentID
}

// namespaceMatches guards the namespace-first routes.
//
// This is the check that makes the scheme safe rather than decorative. The ns comes
// from the URL, so a caller could otherwise name THEIR OWN namespace while asking
// for someone else's ticket id — gatekeeper would then authorize it, because the
// caller genuinely holds permission over their own namespace. Requiring the URL's
// namespace to equal the record's closes that.
//
// An empty fromURL means the request arrived on the LEGACY route, which is accepted
// whatever the record's namespace. That is not a hole, it is the migration: the
// legacy route still evaluates the old caller-scoped resource and still runs
// canAccessTicket, so it behaves exactly as it did before this field existed.
// Rejecting it instead would 404 every existing client the moment a ticket gained a
// namespace, since the CLI and portal still address tickets by bare id. The
// tightening happens when those clients move to the namespaced route and the legacy
// one is removed — at which point this function is a plain equality again.
//
// The reverse is still refused: a namespace supplied in the URL must be the
// record's, so a legacy row cannot be reached by inventing one for it.
func namespaceMatches(recorded, fromURL string) bool {
	if fromURL == "" {
		return true
	}
	return recorded == fromURL
}

// userinfo is the subset of gatekeeper's /oauth/userinfo response we need.
type userinfo struct {
	PreferredUsername string `json:"preferred_username"`
}

// callerNamespace resolves the calling user's namespace — their username, which is
// what gatekeeper scopes resources by. It forwards the caller's own bearer, the
// same pattern the project helpers use, because the SDK's CheckPermissions reports
// a user ID and gatekeeper's resources are keyed by username.
//
// Returns "" when it cannot be determined. Callers must treat that as "record no
// namespace" rather than as an error: failing ticket creation because a lookup
// hiccuped would be a worse outcome than storing a row that keeps the legacy
// caller-scoped resource.
func callerNamespace(ctx context.Context, bearer string) string {
	if bearer == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatekeeperURL+"/oauth/userinfo", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", bearer)
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return ""
	}
	var out userinfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}
	// A namespace segment must not contain "/" — it is one path segment and one
	// resource segment, and a value carrying a slash would let a crafted username
	// forge a different resource string.
	if strings.Contains(out.PreferredUsername, "/") {
		return ""
	}
	return out.PreferredUsername
}
