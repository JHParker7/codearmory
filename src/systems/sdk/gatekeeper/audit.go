package gatekeeper

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Audit ingest for backend services.
//
// The platform has ONE audit trail and it lives in gatekeeper. Most of what is worth
// auditing does not: a git push, a sandbox execution, a credential mint, a service
// enabled. Those services already authenticate to gatekeeper with their rotated
// service key, so they contribute to the shared trail rather than each growing a
// private log nobody reads.
//
// Two rules shape this code, and both are the reason it is worth having in the SDK
// instead of hand-rolled per service:
//
//   - Auditing NEVER fails the operation it describes. The entry is written out of
//     band, after the caller has been served, with its own timeout. A gatekeeper that
//     is slow or down must not turn a successful push into a failed one — an audit
//     trail that can break the thing it audits is the first thing an operator
//     switches off.
//   - The action is the caller's claim, but the SERVICE is not. Gatekeeper stamps the
//     authenticated service name onto every entry, so a call from forge lands as
//     "forge.exec" and cannot masquerade as "gatekeeper.role.update". A compromised
//     service can add noise under its own name; it cannot forge another's history.

// auditTimeout bounds the out-of-band write. Deliberately short: this runs after the
// caller's request has finished, so a slow gatekeeper should cost a dropped entry
// rather than a goroutine that outlives the thing it was describing.
const auditTimeout = 5 * time.Second

// maxAuditDetailLen mirrors gatekeeper's own cap. Truncating here keeps an
// over-long detail from being silently cut server-side, and avoids shipping a
// payload that will only be trimmed on arrival.
const maxAuditDetailLen = 2048

// auditEntry is the ingest body. The service name is deliberately absent — gatekeeper
// derives it from the authenticated key, which is what makes the entry trustworthy.
type auditEntry struct {
	Action     string `json:"action"`
	ResourceID string `json:"resource_id"`
	ActorID    string `json:"actor_id,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// Audit records one entry in the platform audit trail, asynchronously.
//
// action is a short lowercase token WITHOUT the service prefix — "exec", "repo.create",
// "image.push". Gatekeeper enforces ^[a-z0-9][a-z0-9._-]{0,63}$ and prepends the
// service name, so pick something a reader will filter on rather than a sentence.
//
// resourceID is whatever this service considers the subject: an execution id, a repo
// id, an image reference. It is required — an audit entry about nothing in particular
// is not worth storing.
//
// actorID is the user the service acted for. Empty means the service acted on its own
// behalf (a scheduled sweep) or the caller was anonymous, and gatekeeper attributes
// the entry to the service itself — which is the honest answer: nobody identified
// themselves.
//
// detail is free text for the human reading the trail later. Keep it to what an
// investigator would ask: which ref, which image tag, whether it succeeded.
//
// Audit returns immediately and never blocks the caller. It is a no-op when the client
// has no ServiceKey or URL configured, which keeps it inert in tests rather than
// panicking or emitting noise.
func (c *Client) Audit(ctx context.Context, action, resourceID, actorID, detail string) {
	if c == nil || c.URL == "" || c.ServiceKey == nil {
		return
	}
	action, resourceID = strings.TrimSpace(action), strings.TrimSpace(resourceID)
	if action == "" || resourceID == "" {
		// Gatekeeper would reject these with a 400. Dropping them here keeps a
		// programming error from showing up as recurring warning noise in the logs.
		slog.WarnContext(ctx, "audit: action and resource_id are required", "service", c.Service, "action", action)
		return
	}
	if len(detail) > maxAuditDetailLen {
		detail = detail[:maxAuditDetailLen]
	}
	body, err := json.Marshal(auditEntry{Action: action, ResourceID: resourceID, ActorID: actorID, Detail: detail})
	if err != nil {
		return
	}

	// Detached from the caller's context on purpose: that context is usually about to
	// be cancelled as the handler returns, which would cancel this write with it.
	// WithoutCancel keeps the trace/span lineage for correlation while dropping the
	// cancellation.
	go func() {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(writeCtx, http.MethodPost,
			strings.TrimRight(c.URL, "/")+"/internal/audit-logs", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Service-Key", c.Service+":"+c.ServiceKey())

		resp, err := c.client().Do(req)
		if err != nil {
			slog.WarnContext(writeCtx, "audit: could not record event",
				"service", c.Service, "action", action, "resource_id", resourceID, "error", err)
			return
		}
		defer resp.Body.Close() //nolint:errcheck
		if resp.StatusCode >= 300 {
			slog.WarnContext(writeCtx, "audit: gatekeeper rejected the event",
				"service", c.Service, "action", action, "resource_id", resourceID, "status", resp.StatusCode)
		}
	}()
}
