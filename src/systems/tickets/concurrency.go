package main

import (
	"net/http"
	"strconv"
	"strings"
)

// Optimistic concurrency for tickets.
//
// PUT /tickets/{id} was last-write-wins: two callers could both read a ticket,
// both write, and both believe they won. That is invisible when two people edit
// the same ticket in the portal — the second save silently discards the first —
// and it is a correctness problem when several agent hosts race to claim the
// same work.
//
// The fix is the standard HTTP one rather than a bespoke field: GET returns an
// ETag, and PUT accepts If-Match. It is OPTIONAL — a request without If-Match
// behaves exactly as before — so existing clients need no change and there is no
// flag day.

// etagFor renders a ticket version as a strong ETag.
func etagFor(version int64) string {
	return `"` + strconv.FormatInt(version, 10) + `"`
}

// setTicketETag stamps the response with the ticket's current version. Callers
// set it on every response that carries a whole ticket, so a client always has a
// fresh token to send back.
func setTicketETag(w http.ResponseWriter, t Ticket) {
	w.Header().Set("ETag", etagFor(t.Version))
}

// ifMatch is a parsed If-Match header.
type ifMatch struct {
	// present is false when the header was absent: the request is unconditional.
	present bool
	// any is true for "If-Match: *", which per RFC 9110 means "the resource must
	// exist" without pinning a version. The handler has already loaded the
	// ticket by that point, so existence is established and the write proceeds.
	any bool
	// version is the pinned version when present && !any.
	version int64
}

// parseIfMatch reads the If-Match header.
//
// A malformed value is reported as invalid rather than quietly ignored: silently
// dropping the condition would turn a request that asked for safety into a
// last-write-wins overwrite, which is the opposite of what the caller wanted.
// A multi-valued header takes the first entry that parses, matching the
// "any of these" semantics of the spec.
func parseIfMatch(h http.Header) (ifMatch, bool) {
	raw := strings.TrimSpace(h.Get("If-Match"))
	if raw == "" {
		return ifMatch{}, true
	}
	if raw == "*" {
		return ifMatch{present: true, any: true}, true
	}
	for _, part := range strings.Split(raw, ",") {
		tag := strings.TrimSpace(part)
		// Weak validators (W/"…") cannot be used for If-Match; skip them.
		if strings.HasPrefix(tag, "W/") {
			continue
		}
		tag = strings.Trim(tag, `"`)
		v, err := strconv.ParseInt(tag, 10, 64)
		if err != nil {
			continue
		}
		return ifMatch{present: true, version: v}, true
	}
	return ifMatch{}, false
}

// writePreconditionFailed reports a lost race. 412 is the correct code and the
// message names the versions, because "your update was rejected" is not
// actionable on its own — a client needs to know it should re-read.
func writePreconditionFailed(w http.ResponseWriter, current int64) {
	w.Header().Set("ETag", etagFor(current))
	http.Error(w,
		"ticket was modified by someone else (current version "+strconv.FormatInt(current, 10)+"); re-read it and retry",
		http.StatusPreconditionFailed)
}
