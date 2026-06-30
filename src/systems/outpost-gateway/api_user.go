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
var knownModules = map[string]bool{"chaos": true, "argo": true}

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
