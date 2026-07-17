package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// workflowRolePermsVersion is the version of the permission-derivation logic
// below. The scoped run role is provisioned once at workflow create/update and
// reused for every run, so a change here would otherwise never reach workflows
// created earlier. Bump it whenever collectWorkflowPermissions changes what it
// grants; handleTriggerRun re-provisions any workflow whose stored role predates
// the current version. v1 added the async-poll read grant (getExecution) that
// stops forge steps from hanging at "running". v2 added the deleteVolume companion
// grant so a run that creates shared workspace volumes can tear them down at the end.
// v3 made forge/create-volume async, so the generic async-poll rule now also grants
// getVolume on forge/volumes/* — without the bump, volume workflows created earlier
// would 403 every create-volume status poll and hang until timeout.
// v4 added the workflows/trigger sub-pipeline step, whose run role needs triggerRun
// + getRun on workflows/runs/* to create and poll the sub-run.
// v5 added map regions: a region with a volume clones the workspace per iteration and
// gathers outputs back, so — exactly like a scatter step — its run role needs forge's
// create-volume and volume-copy on top of the body's own actions. Without the bump, a
// workflow created earlier would 403 its first clone and hang.
const workflowRolePermsVersion = 6

// collectWorkflowPermissions returns the deduplicated set of gatekeeper
// permissions declared by the workflow's step actions in the current catalog.
func collectWorkflowPermissions(steps []WorkflowStep, maps []MapDef, ticket *TicketConfig) []PermissionSpec {
	seen := map[string]struct{}{}
	var out []PermissionSpec
	actionCatalogMu.RLock()
	defer actionCatalogMu.RUnlock()

	// addAction grants the permissions a single catalog action needs: its required
	// permission, the async-poll companion, and the deleteVolume companion for a
	// create-volume. Factored out so a scatter step can also grant the forge actions it
	// drives internally (resolve-paths, create-volume, volume-copy).
	addAction := func(action string) {
		def, ok := actionCatalog[action]
		if !ok || def.RequiredPermission == nil {
			return
		}
		p := def.RequiredPermission
		key := p.Service + ":" + p.Action + ":" + p.Resource
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, PermissionSpec{
			Service:  p.Service,
			Action:   p.Action,
			Resource: p.Resource,
		})

		// Async actions submit a job and then POLL it to a terminal state (forge:
		// POST /executions then GET /executions/{id}). The scoped run role grants
		// only the submit permission above, so every poll is 403 and the step never
		// observes completion — it hangs to the timeout. Also grant the read
		// permission for the submitted item. The submit permission is a "create"
		// (createExecution/createSync/…) and the poll reads the same resource, so
		// the read is its "get" counterpart on the item resource; the owner already
		// holds it for jobs they create (gatekeeper drops it otherwise).
		if def.Async != nil && strings.HasPrefix(p.Action, "create") {
			pollSpec := PermissionSpec{
				Service:  p.Service,
				Action:   "get" + strings.TrimPrefix(p.Action, "create"),
				Resource: strings.TrimRight(p.Resource, "/") + "/*",
			}
			pollKey := pollSpec.Service + ":" + pollSpec.Action + ":" + pollSpec.Resource
			if _, dup := seen[pollKey]; !dup {
				seen[pollKey] = struct{}{}
				out = append(out, pollSpec)
			}
		}

		// A create-volume step's run tears its volumes down when it finishes (DELETE
		// /volumes?workflow_id=...). Grant the matching deleteVolume on the same
		// resource so teardown isn't 403'd and volumes linger until the age reaper.
		if p.Action == "createVolume" {
			delSpec := PermissionSpec{Service: p.Service, Action: "deleteVolume", Resource: p.Resource}
			delKey := delSpec.Service + ":" + delSpec.Action + ":" + delSpec.Resource
			if _, dup := seen[delKey]; !dup {
				seen[delKey] = struct{}{}
				out = append(out, delSpec)
			}
		}

		// A workflows/trigger step creates a sub-run (triggerRun) and then polls it to
		// a terminal state (getRun). Both checks are item-scoped (workflows/runs/<id>),
		// which the generic async companion above does not cover (it only fires for
		// "create*" actions and grants a collection-scoped poll). Grant both on the
		// wildcard run space so the step can trigger and observe the sub-run.
		if p.Action == "triggerRun" {
			for _, act := range []string{"triggerRun", "getRun"} {
				spec := PermissionSpec{Service: p.Service, Action: act, Resource: "workflows/runs/*"}
				k := spec.Service + ":" + spec.Action + ":" + spec.Resource
				if _, dup := seen[k]; !dup {
					seen[k] = struct{}{}
					out = append(out, spec)
				}
			}
		}
	}

	for _, ws := range steps {
		if ws.Action == ActionHTTP {
			continue // ActionHTTP permissions are runtime-dynamic; can't enumerate statically
		}
		addAction(ws.Action)
		// A scatter step drives forge itself to resolve paths, clone a volume per leg,
		// and gather results — grant those forge actions on top of the leg action.
		if ws.Scatter != nil {
			addAction(actionForgeResolvePaths)
			addAction(ActionForgeCreateVolume)
			addAction(actionForgeVolumeCopy)
		}
	}
	// A map region with a volume clones the base workspace per iteration and gathers
	// owned outputs back, driving the same forge actions a scatter step does.
	for _, d := range maps {
		if d.Volume != "" {
			addAction(ActionForgeCreateVolume)
			addAction(actionForgeVolumeCopy)
		}
	}
	// Mirroring a run into a ticket is done with the RUN TOKEN, so the workflow's role
	// must carry the ticket permissions — but only when the workflow opted in. A
	// workflow with no ticket config grants nothing extra, which is why enabling this
	// cannot widen what any existing run can do.
	if ticket != nil && ticket.Enabled {
		for _, p := range ticketPermissions() {
			key := p.Service + ":" + p.Action + ":" + p.Resource
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, p)
		}
	}
	return out
} // provisionWorkflowRole asks gatekeeper to create a minimal-permission role for
// workflowID. Returns the new role_id, or "" when the key is unconfigured or the
// permission list is empty (runs will use the user's full session permissions).
func provisionWorkflowRole(ctx context.Context, workflowID, userID, orgID string, steps []WorkflowStep, maps []MapDef, ticket *TicketConfig) string {
	perms := collectWorkflowPermissions(steps, maps, ticket)
	if len(perms) == 0 {
		return ""
	}
	key := gatekeeperKey()
	if key == "" {
		return ""
	}

	payload, _ := json.Marshal(map[string]any{
		"workflow_id": workflowID,
		"user_id":     userID,
		"org_id":      orgID,
		"permissions": perms,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gatekeeperURL+"/internal/workflow-roles", bytes.NewReader(payload))
	if err != nil {
		slog.WarnContext(ctx, "provisionWorkflowRole: build request", "workflow_id", workflowID, "error", err)
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", "workflows:"+key)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "provisionWorkflowRole: request failed", "workflow_id", workflowID, "error", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		slog.WarnContext(ctx, "provisionWorkflowRole: unexpected status", "workflow_id", workflowID,
			"status", resp.StatusCode, "body", strings.TrimSpace(string(raw)))
		return ""
	}
	var result struct {
		RoleID string `json:"role_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		slog.WarnContext(ctx, "provisionWorkflowRole: decode response", "workflow_id", workflowID, "error", err)
		return ""
	}
	return result.RoleID
}

// deleteWorkflowRole removes the role that was provisioned at workflow creation.
// Failures are logged but never propagated — a missing cleanup is not fatal.
func deleteWorkflowRole(ctx context.Context, roleID string) {
	if roleID == "" {
		return
	}
	key := gatekeeperKey()
	if key == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		gatekeeperURL+"/internal/workflow-roles/"+roleID, nil)
	if err != nil {
		slog.WarnContext(ctx, "deleteWorkflowRole: build request", "role_id", roleID, "error", err)
		return
	}
	req.Header.Set("X-Service-Key", "workflows:"+key)
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "deleteWorkflowRole: request failed", "role_id", roleID, "error", err)
		return
	}
	resp.Body.Close()
}

var validMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// handleListActions returns the current in-memory action catalog loaded from the registry.
func handleListActions(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleListActions")
	defer span.End()

	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listAction", "workflows/actions"); !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	actionCatalogMu.RLock()
	catalog := make([]ActionDef, 0, len(actionCatalog))
	for _, def := range actionCatalog {
		catalog = append(catalog, def)
	}
	actionCatalogMu.RUnlock()

	sort.Slice(catalog, func(i, j int) bool { return catalog[i].Name < catalog[j].Name })

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(catalog) //nolint:errcheck
}

func canAccessWorkflow(wf Workflow, userID, orgID string) bool {
	return wf.CreatedBy == userID || (orgID != "" && wf.OrgID == orgID)
}

func bearerToken(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token
}

type createWorkflowRequest struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Project     string              `json:"project,omitempty"`
	Steps       []WorkflowStepRef   `json:"steps,omitempty"`
	Inputs      []WorkflowInputDef  `json:"inputs,omitempty"`
	Outputs     []WorkflowOutputDef `json:"outputs,omitempty"`
	// Routes are the explicit edges between steps, and the only way to express a
	// fork or a join. Omit them for a plain sequence: the edges are then derived as
	// a chain in array order at run time.
	Routes []WorkflowRoute `json:"routes,omitempty"`
	// Maps declare the map regions steps join via map_id — see MapDef.
	Maps []MapDef `json:"maps,omitempty"`
	// Ticket opts every run of this workflow into being mirrored to a ticket — see
	// TicketConfig. Omit it and nothing changes.
	Ticket *TicketConfig `json:"ticket,omitempty"`
}

// validateWorkflowIO checks the declared inputs/outputs: unique, named, and every
// output carries a value template. Returns "" when valid.
func validateWorkflowIO(inputs []WorkflowInputDef, outputs []WorkflowOutputDef) string {
	seenIn := map[string]bool{}
	for _, in := range inputs {
		if strings.TrimSpace(in.Name) == "" {
			return "each declared input requires a name"
		}
		if seenIn[in.Name] {
			return "duplicate input name: " + in.Name
		}
		seenIn[in.Name] = true
	}
	seenOut := map[string]bool{}
	for _, o := range outputs {
		if strings.TrimSpace(o.Name) == "" {
			return "each declared output requires a name"
		}
		if seenOut[o.Name] {
			return "duplicate output name: " + o.Name
		}
		seenOut[o.Name] = true
		if strings.TrimSpace(o.Value) == "" {
			return "output " + o.Name + " requires a value expression"
		}
	}
	return ""
}

func handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleCreateWorkflow")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createWorkflow", "workflows/pipelines")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	var req createWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.SetStatus(codes.Error, "invalid body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if msg := validateResourceName(req.Name); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if len(req.Steps) == 0 {
		http.Error(w, "at least one step is required", http.StatusBadRequest)
		return
	}
	if len(req.Steps) > maxSteps {
		http.Error(w, fmt.Sprintf("maximum %d steps allowed", maxSteps), http.StatusBadRequest)
		return
	}
	for i, ref := range req.Steps {
		if msg := validateStepRefShape(i, ref); msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
	}
	if msg := validateWorkflowIO(req.Inputs, req.Outputs); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// Name is the workflow's resource identifier, so it must be unique per caller.
	if exists, cerr := workflowNameExists(ctx, req.Name, userID, orgID); cerr != nil {
		span.RecordError(cerr)
		span.SetStatus(codes.Error, "db error")
		http.Error(w, "failed to create workflow", http.StatusInternalServerError)
		return
	} else if exists {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "a workflow with that name already exists", http.StatusConflict)
		return
	}

	// Validate all referenced steps exist and are accessible.
	if err := validateStepRefs(ctx, req.Steps, userID, orgID, w); err != nil {
		return // response already written
	}

	refs := req.Steps
	if refs == nil {
		refs = []WorkflowStepRef{}
	}
	wf := Workflow{
		WorkflowID:  uuid.New().String(),
		Name:        req.Name,
		Description: req.Description,
		Project:     req.Project,
		CreatedBy:   userID,
		OrgID:       orgID,
		Active:      true,
		Inputs:      req.Inputs,
		Outputs:     req.Outputs,
		Routes:      req.Routes,
		Maps:        req.Maps,
		Ticket:      req.Ticket,
		StepRefs:    refs,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	steps, err := enrichStepRefs(ctx, refs)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "enrich steps failed")
		slog.ErrorContext(ctx, "create workflow: enrich steps", "error", err)
		http.Error(w, "failed to create workflow", http.StatusInternalServerError)
		return
	}
	wf.Steps = steps

	// Routes are validated against the ENRICHED steps, since a route names a step
	// by the name it actually runs under (a stored-step reference may override it).
	if msg := validateTicket(wf.Ticket); msg != "" {
		span.SetStatus(codes.Ok, "")
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if msg := validateGraph(wf.Steps, wf.Routes, wf.Maps); msg != "" {
		span.SetStatus(codes.Ok, "")
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// Provision a scoped service role before persisting so the role_id is stored atomically.
	wf.RoleID = provisionWorkflowRole(ctx, wf.WorkflowID, userID, orgID, wf.Steps, wf.Maps, wf.Ticket)
	wf.RolePermsVersion = workflowRolePermsVersion

	if err := wf.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "create workflow: db error", "error", err)
		deleteWorkflowRole(ctx, wf.RoleID)
		http.Error(w, "failed to create workflow", http.StatusInternalServerError)
		return
	}

	span.SetAttributes(attribute.String("workflow.id", wf.WorkflowID))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "workflow created", "workflow_id", wf.WorkflowID, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(wf) //nolint:errcheck
}

func handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleListWorkflows")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listWorkflow", "workflows/pipelines")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	wfs, err := listWorkflows(ctx, userID, orgID, r.URL.Query().Get("project"))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.ErrorContext(ctx, "list workflows: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list workflows", http.StatusInternalServerError)
		return
	}
	for i := range wfs {
		wfs[i].Steps = []WorkflowStep{}
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(wfs) //nolint:errcheck
}

func handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleGetWorkflow")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getWorkflow", "workflows/pipelines/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	wf, err := resolveWorkflowRef(ctx, id, userID, orgID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "get workflow: db error", "workflow_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get workflow", http.StatusInternalServerError)
		return
	}
	if !canAccessWorkflow(wf, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	// ?raw=true additionally exposes the stored, unenriched step refs (normally hidden:
	// Workflow.StepRefs is json:"-"). A client that wants to mutate a pipeline (e.g. the
	// CLI convert/localize) needs the raw refs so it can re-PUT them faithfully — the
	// default `steps` are enriched with each stored step's merged With, which would bake
	// a referenced step's definition into its per-occurrence override on round-trip.
	if r.URL.Query().Get("raw") == "true" {
		json.NewEncoder(w).Encode(struct { //nolint:errcheck
			Workflow
			StepRefs []WorkflowStepRef `json:"step_refs"`
		}{Workflow: wf, StepRefs: wf.StepRefs})
		return
	}
	json.NewEncoder(w).Encode(wf) //nolint:errcheck
}

func handleUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleUpdateWorkflow")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateWorkflow", "workflows/pipelines/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	existing, err := resolveWorkflowRef(ctx, id, userID, orgID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "update workflow: fetch error", "workflow_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get workflow", http.StatusInternalServerError)
		return
	}
	if !canAccessWorkflow(existing, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	var req createWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	// Validate the name charset only on an actual rename, so editing a workflow whose
	// name predates this rule isn't blocked unless the name itself is changed.
	if req.Name != existing.Name {
		if msg := validateResourceName(req.Name); msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
	}
	// A rename must not collide with another of the caller's workflows; keeping its
	// own name is allowed (excludes existing.WorkflowID).
	if conflict, cerr := workflowNameConflict(ctx, req.Name, existing.WorkflowID, userID, orgID); cerr != nil {
		span.RecordError(cerr)
		span.SetStatus(codes.Error, "db error")
		http.Error(w, "failed to update workflow", http.StatusInternalServerError)
		return
	} else if conflict {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "a workflow with that name already exists", http.StatusConflict)
		return
	}
	if len(req.Steps) == 0 {
		http.Error(w, "at least one step is required", http.StatusBadRequest)
		return
	}
	if len(req.Steps) > maxSteps {
		http.Error(w, fmt.Sprintf("maximum %d steps allowed", maxSteps), http.StatusBadRequest)
		return
	}
	for i, ref := range req.Steps {
		if msg := validateStepRefShape(i, ref); msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
	}
	if msg := validateWorkflowIO(req.Inputs, req.Outputs); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if err := validateStepRefs(ctx, req.Steps, userID, orgID, w); err != nil {
		return
	}

	refs := req.Steps
	if refs == nil {
		refs = []WorkflowStepRef{}
	}
	newSteps, err := enrichStepRefs(ctx, refs)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "enrich steps failed")
		slog.ErrorContext(ctx, "update workflow: enrich steps", "workflow_id", id, "error", err)
		http.Error(w, "failed to update workflow", http.StatusInternalServerError)
		return
	}

	if msg := validateTicket(req.Ticket); msg != "" {
		span.SetStatus(codes.Ok, "")
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if msg := validateGraph(newSteps, req.Routes, req.Maps); msg != "" {
		span.SetStatus(codes.Ok, "")
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	oldRoleID := existing.RoleID
	existing.Name = req.Name
	existing.Description = req.Description
	existing.Routes = req.Routes
	existing.Maps = req.Maps
	existing.Ticket = req.Ticket
	// Guard like tickets: a partial PUT that omits project must not silently
	// wipe the stored label (the CLI/TUI update payloads don't send project).
	if req.Project != "" {
		existing.Project = req.Project
	}
	existing.Inputs = req.Inputs
	existing.Outputs = req.Outputs
	existing.StepRefs = refs
	existing.Steps = newSteps
	existing.UpdatedAt = time.Now().UTC()

	// Re-provision the role with the updated step set.
	existing.RoleID = provisionWorkflowRole(ctx, existing.WorkflowID, userID, orgID, newSteps, req.Maps, req.Ticket)
	existing.RolePermsVersion = workflowRolePermsVersion

	if err := existing.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "update workflow: db error", "workflow_id", existing.WorkflowID, "user_id", userID, "error", err)
		deleteWorkflowRole(ctx, existing.RoleID)
		http.Error(w, "failed to update workflow", http.StatusInternalServerError)
		return
	}
	// Old role is now superseded; clean it up after the DB write succeeds.
	deleteWorkflowRole(ctx, oldRoleID)

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "workflow updated", "workflow_id", existing.WorkflowID, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(existing) //nolint:errcheck
}

func handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleDeleteWorkflow")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteWorkflow", "workflows/pipelines/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	wf, err := resolveWorkflowRef(ctx, id, userID, orgID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "delete workflow: fetch error", "workflow_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete workflow", http.StatusInternalServerError)
		return
	}
	if !canAccessWorkflow(wf, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	roleID := wf.RoleID
	if err := wf.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "delete workflow: db error", "workflow_id", wf.WorkflowID, "user_id", userID, "error", err)
		http.Error(w, "failed to delete workflow", http.StatusInternalServerError)
		return
	}
	deleteWorkflowRole(ctx, roleID)

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "workflow deleted", "workflow_id", wf.WorkflowID, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}
