package main

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeBody(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func handleCreateTrigger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createTrigger", "events/triggers")
	if !ok {
		return
	}
	var t Trigger
	if err := decodeBody(r, &t); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if t.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if len(t.Actions) == 0 {
		http.Error(w, "at least one action is required", http.StatusBadRequest)
		return
	}
	t.ID = uuid.NewString()
	t.OrgID = orgID
	t.CreatedBy = userID
	t.Enabled = true
	if err := t.Add(ctx); err != nil {
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func handleListTriggers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listTrigger", "events/triggers")
	if !ok {
		return
	}
	ts, err := listTriggers(ctx, orgID, userID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

func handleGetTrigger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getTrigger", "events/triggers/"+id)
	if !ok {
		return
	}
	t, err := getTrigger(ctx, id, orgID, userID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func handleUpdateTrigger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateTrigger", "events/triggers/"+id)
	if !ok {
		return
	}
	existing, err := getTrigger(ctx, id, orgID, userID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var in Trigger
	if err := decodeBody(r, &in); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	// Only the mutable fields; identity/tenant stay fixed.
	existing.Name = in.Name
	existing.Match = in.Match
	existing.Actions = in.Actions
	existing.Enabled = in.Enabled
	if err := existing.Save(ctx); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

func handleDeleteTrigger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteTrigger", "events/triggers/"+id)
	if !ok {
		return
	}
	if err := removeTrigger(ctx, id, orgID, userID); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestMatch dry-runs a filter against a supplied event body — the "does my trigger fire?"
// helper, no persistence.
func handleTestMatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listTrigger", "events/triggers"); !ok {
		return
	}
	var body struct {
		Match Match `json:"match"`
		Event Event `json:"event"`
	}
	if err := decodeBody(r, &body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	matched, err := EvalEvent(body.Match, body.Event)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"matched": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"matched": matched})
}

// ── event log queries ────────────────────────────────────────────────────────────────────
func handleListEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listEvent", "events")
	if !ok {
		return
	}
	evs, err := listEvents(ctx, orgID, userID, r.URL.Query().Get("type"), 0)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, evs)
}

// handleGetEvent returns one event from the log. An event belonging to another tenant is
// reported as "not found" rather than forbidden, so the endpoint never confirms that an id
// exists for someone else.
func handleGetEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	// The permission check names the COLLECTION, not the id. Conductor can only template a
	// resource from path params, so `events/{id}` would evaluate to `<caller>/events/<id>` —
	// identical in shape for every id, meaning gatekeeper would authorise any of them and only
	// this handler's own filter would protect the row. The honest resource is the collection;
	// the per-record decision is made below with the row loaded, and a miss is a 404 so the
	// log never confirms an id exists for another tenant.
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getEvent", "events")
	if !ok {
		return
	}
	e, err := getEventScoped(ctx, id, orgID, userID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, e)
}
