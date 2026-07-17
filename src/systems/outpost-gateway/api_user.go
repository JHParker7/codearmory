package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// knownModules are the integration modules an outpost can enable. Adding a new
// integration (e.g. argo) adds an entry here; the gateway core is otherwise
// integration-agnostic.
var knownModules = map[string]bool{"chaos": true, "argo": true, "deploy": true}

type createOutpostRequest struct {
	Name    string   `json:"name"`
	Modules []string `json:"modules"`
}

type createOutpostResponse struct {
	Outpost
	EnrollmentToken string `json:"enrollment_token"`
}

// handleCreateOutpost registers a new outpost and mints its single-use
// enrollment token (returned once). This is a user-facing route reached through
// conductor with a gatekeeper bearer token.
func handleCreateOutpost(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("outpost-gateway").Start(r.Context(), "handleCreateOutpost")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createOutpost", "outpost-gateway/outposts")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	var req createOutpostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	// Outpost names must be unique within their scope so an outpost can be
	// referenced by name rather than its UUID (enforced by uq_outposts_* indexes).
	if outpostNameTaken(ctx, orgID, userID, strings.TrimSpace(req.Name)) {
		http.Error(w, "outpost with that name already exists", http.StatusConflict)
		return
	}
	var modules []string
	for _, m := range req.Modules {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if !knownModules[m] {
			http.Error(w, "unknown module: "+m, http.StatusBadRequest)
			return
		}
		modules = append(modules, m)
	}

	outpostID := uuid.New().String()
	token, tokenHash, err := mintEnrollmentToken(outpostID)
	if err != nil {
		span.RecordError(err)
		http.Error(w, "failed to mint enrollment token", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	o := Outpost{
		OutpostID:       outpostID,
		OrgID:           orgID,
		UserID:          userID,
		Name:            strings.TrimSpace(req.Name),
		Modules:         strings.Join(modules, ","),
		Status:          OutpostPending,
		EnrollTokenHash: tokenHash,
		Active:          true,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := o.Add(ctx); err != nil {
		span.RecordError(err)
		http.Error(w, "failed to create outpost", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(attribute.String("outpost.id", o.OutpostID))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "outpost registered", "outpost_id", o.OutpostID, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(createOutpostResponse{Outpost: o, EnrollmentToken: token}) //nolint:errcheck
}

func handleListOutposts(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("outpost-gateway").Start(r.Context(), "handleListOutposts")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listOutpost", "outpost-gateway/outposts")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	out, err := listOutposts(ctx, orgID, userID)
	if err != nil {
		span.RecordError(err)
		http.Error(w, "failed to list outposts", http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []Outpost{}
	}
	markStale(out)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out) //nolint:errcheck
}

func handleGetOutpost(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("outpost-gateway").Start(r.Context(), "handleGetOutpost")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getOutpost", "outpost-gateway/outposts/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	o, err := getOutpost(ctx, id)
	if err != nil {
		if isNotFound(err) {
			http.Error(w, "outpost not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		http.Error(w, "failed to get outpost", http.StatusInternalServerError)
		return
	}
	if !canAccessOutpost(o, userID, orgID) {
		http.Error(w, "outpost not found", http.StatusNotFound)
		return
	}
	one := []Outpost{o}
	markStale(one)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(one[0]) //nolint:errcheck
}

func handleDeleteOutpost(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("outpost-gateway").Start(r.Context(), "handleDeleteOutpost")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteOutpost", "outpost-gateway/outposts/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	o, err := getOutpost(ctx, id)
	if err != nil {
		if isNotFound(err) {
			http.Error(w, "outpost not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		http.Error(w, "failed to get outpost", http.StatusInternalServerError)
		return
	}
	if !canAccessOutpost(o, userID, orgID) {
		http.Error(w, "outpost not found", http.StatusNotFound)
		return
	}
	if err := softDeleteOutpost(ctx, id); err != nil {
		span.RecordError(err)
		http.Error(w, "failed to delete outpost", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func canAccessOutpost(o Outpost, userID, orgID string) bool {
	return o.UserID == userID || (orgID != "" && o.OrgID == orgID)
}

// markStale flips connected outposts that have not been seen within the liveness
// window to stale for display. It does not persist; status is authoritative only
// for pending/connected via heartbeat.
func markStale(out []Outpost) {
	cutoff := time.Now().UTC().Add(-90 * time.Second)
	for i := range out {
		if out[i].Status == OutpostConnected && (out[i].LastSeenAt == nil || out[i].LastSeenAt.Before(cutoff)) {
			out[i].Status = OutpostStale
		}
	}
}

// userEnqueueRequest is the body of POST /outposts/{id}/commands — the user-facing,
// gatekeeper-authed way to enqueue a command. Unlike the internal endpoint, the tenant
// is NOT in the body: it is taken from the authenticated caller, so a workflow step can
// trigger a redeploy with its run token and never assert someone else's identity.
type userEnqueueRequest struct {
	Integration string         `json:"integration"`
	Type        string         `json:"type"`
	Payload     map[string]any `json:"payload"`
}

// handleEnqueueOutpostCommand lets an authenticated caller (e.g. a workflow step)
// enqueue a command for an outpost it owns. This is what lets CI trigger a redeploy
// via outpost: the pipeline calls it as a catalog action with its run token, and the
// gateway relays the command to the in-cluster outpost — the control plane still holds
// no cluster credentials.
func handleEnqueueOutpostCommand(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("outpost-gateway").Start(r.Context(), "handleEnqueueOutpostCommand")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "enqueueCommand", "outpost-gateway/outposts/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	var req userEnqueueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Integration == "" || req.Type == "" {
		http.Error(w, "integration and type are required", http.StatusBadRequest)
		return
	}

	o, err := getOutpost(ctx, id)
	if err != nil {
		if isNotFound(err) {
			http.Error(w, "unknown outpost", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		http.Error(w, "failed to look up outpost", http.StatusInternalServerError)
		return
	}
	// The tenant is the authenticated caller — never a body field — so one tenant
	// cannot drive another's outpost.
	if !canAccessOutpost(o, userID, orgID) {
		http.Error(w, "outpost does not belong to you", http.StatusForbidden)
		return
	}
	if !outpostHasModule(o, req.Integration) {
		http.Error(w, "outpost does not have the "+req.Integration+" module enabled", http.StatusConflict)
		return
	}

	cmd := OutpostCommand{
		ID:          uuid.New().String(),
		OutpostID:   id,
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
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(cmd) //nolint:errcheck
}
