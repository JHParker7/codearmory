package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

type enqueueCommandRequest struct {
	OutpostID   string `json:"outpost_id"`
	Integration string `json:"integration"`
	Type        string `json:"type"`
	// OrgID/UserID identify the tenant the calling service authorized this command
	// for. They are covered by the request HMAC and checked against the target
	// outpost's owner, so one tenant cannot drive another tenant's outpost by id.
	OrgID   string         `json:"org_id"`
	UserID  string         `json:"user_id"`
	Payload map[string]any `json:"payload"`
}

// handleEnqueueCommand is the internal endpoint control-plane services call to
// drive an outpost. Authenticated by the shared-key HMAC over the full request
// body, and authorized by matching the asserted tenant to the outpost's owner.
func handleEnqueueCommand(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("outpost-gateway").Start(r.Context(), "handleEnqueueCommand")
	defer span.End()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	// Authenticate over the raw bytes before parsing, so an unauthenticated caller
	// can neither probe JSON validity nor make us do parse work pre-auth.
	if !verifyInternal("command", body,
		r.Header.Get("X-Internal-Token"), r.Header.Get("X-Internal-Timestamp")) {
		span.SetStatus(codes.Error, "invalid internal token")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req enqueueCommandRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.OutpostID == "" || req.Integration == "" || req.Type == "" {
		http.Error(w, "outpost_id, integration and type are required", http.StatusBadRequest)
		return
	}

	// Validate the target outpost exists and has the module enabled.
	o, err := getOutpost(ctx, req.OutpostID)
	if err != nil {
		if isNotFound(err) {
			http.Error(w, "unknown outpost", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		http.Error(w, "failed to look up outpost", http.StatusInternalServerError)
		return
	}
	// Enforce tenant ownership: the calling service must have authorized this
	// command for a tenant that owns the target outpost. Without this an
	// authenticated user of one tenant could drive another tenant's outpost by id.
	if !canAccessOutpost(o, req.UserID, req.OrgID) {
		span.SetStatus(codes.Error, "outpost ownership mismatch")
		http.Error(w, "outpost does not belong to the requesting tenant", http.StatusForbidden)
		return
	}
	if !outpostHasModule(o, req.Integration) {
		http.Error(w, "outpost does not have the "+req.Integration+" module enabled", http.StatusConflict)
		return
	}

	cmd := OutpostCommand{
		ID:          uuid.New().String(),
		OutpostID:   req.OutpostID,
		Integration: req.Integration,
		Type:        req.Type,
		Payload:     req.Payload,
		Status:      CmdPending,
		CreatedAt:   time.Now().UTC(),
	}
	if err := enqueueCommandDB(ctx, cmd); err != nil {
		span.RecordError(err)
		http.Error(w, "failed to enqueue command", http.StatusInternalServerError)
		return
	}
	meterCommandsEnqueued.Add(ctx, 1)
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "command enqueued", "command_id", cmd.ID, "outpost_id", cmd.OutpostID, "integration", cmd.Integration, "type", cmd.Type)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"command_id": cmd.ID}) //nolint:errcheck
}

func outpostHasModule(o Outpost, module string) bool {
	for _, m := range splitCSV(o.Modules) {
		if m == module {
			return true
		}
	}
	return false
}
