package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
)

// Audit ingest for backend services.
//
// The audit trail lives here, but most of what is worth auditing does not: a git
// push, a sandbox execution, a secret read. Those services already authenticate to
// gatekeeper with their rotated service key, so this endpoint lets them contribute
// entries to the one trail instead of each growing a private log nobody reads.
//
// The trust boundary is the reason this is not just "insert whatever you are sent".
// A service may only describe ITS OWN actions: gatekeeper stamps the caller's name
// onto the action ("codearmory_git_factory.push"), so a compromised service can add
// noise under its own name but cannot forge "role.update" or backdate an entry
// attributed to another service. Everything else about the entry is the caller's to
// state, since only it knows what happened.

// auditActionRe constrains the action a service may name. It is a short token, not a
// sentence: the action is what a reader filters on, so it has to be stable and
// machine-comparable. The service prefix gatekeeper adds is what makes it unique.
var auditActionRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// maxAuditDetailLen caps the free-text field. Detail is attacker-influenced (a ref
// name, a repo path) and this is an append-only table nobody prunes, so it is
// truncated rather than rejected — losing the tail of one entry beats losing the
// entry, and beats letting a caller write a megabyte per request.
const maxAuditDetailLen = 2048

type auditIngestRequest struct {
	// Action, without the service prefix — e.g. "push". Required.
	Action string `json:"action"`
	// ResourceID is whatever the service considers the subject: a repo id, an
	// execution id. Required, since an audit entry about nothing in particular is
	// not worth storing.
	ResourceID string `json:"resource_id"`
	// ActorID is the user the service acted on behalf of. Empty means the service
	// acted on its own (a scheduled sweep) or the caller was anonymous, and the
	// entry is attributed to the service itself.
	ActorID string `json:"actor_id"`
	Detail  string `json:"detail"`
}

// handleIngestAuditLog records an audit entry on behalf of a registered service.
func handleIngestAuditLog(w http.ResponseWriter, r *http.Request) {
	svc, ok := requireServiceAuth(w, r)
	if !ok {
		return
	}

	var req auditIngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Action = strings.TrimSpace(req.Action)
	req.ResourceID = strings.TrimSpace(req.ResourceID)
	if !auditActionRe.MatchString(req.Action) {
		http.Error(w, "action must be a short lowercase token", http.StatusBadRequest)
		return
	}
	if req.ResourceID == "" {
		http.Error(w, "resource_id is required", http.StatusBadRequest)
		return
	}
	if len(req.Detail) > maxAuditDetailLen {
		req.Detail = req.Detail[:maxAuditDetailLen]
	}

	// The actor is the user when the service names one, and the service itself
	// otherwise. The service name is never taken from the body — it is the
	// authenticated identity, which is the whole point of stamping it here.
	actorID, actorType := svc.ServiceName, "service"
	if req.ActorID != "" {
		actorID, actorType = req.ActorID, "user"
	}
	writeAudit(r.Context(), actorID, actorType, svc.ServiceName+"."+req.Action, req.ResourceID, req.Detail)

	slog.DebugContext(r.Context(), "audit entry ingested", "service", svc.ServiceName, "action", req.Action, "resource_id", req.ResourceID)
	w.WriteHeader(http.StatusAccepted)
}
