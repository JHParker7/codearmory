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
	"regexp"
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

// resourcePathParamRe matches a "{param}" segment of an endpoint's resource template.
var resourcePathParamRe = regexp.MustCompile(`\{[^}]+\}`)

// wildcardPathParams turns an endpoint's resource TEMPLATE into a pattern a role can
// match: "tickets/tickets/{id}" -> "tickets/tickets/*".
//
// A run cannot know the id it will act on when its role is provisioned — the ticket it
// updates is created by an earlier step of the same run — so the grant has to span the
// id space. It is still scoped to that one service and action.
func wildcardPathParams(resource string) string {
	return resourcePathParamRe.ReplaceAllString(resource, "*")
}

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
		// The registry DERIVES an action's permission from its endpoint, whose resource
		// is a TEMPLATE ("tickets/tickets/{id}", "forge/executions/{id}"). A role carries
		// this string verbatim and gatekeeper knows nothing about "{id}" — it matches
		// exact strings, "*", "foo/*" and "foo/*/bar" — so granting the literal would
		// 403 every call on a real id. Rewrite each path param to the wildcard the role
		// can actually match.
		resource := wildcardPathParams(p.Resource)
		key := p.Service + ":" + p.Action + ":" + resource
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, PermissionSpec{
			Service:  p.Service,
			Action:   p.Action,
			Resource: resource,
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
				Resource: strings.TrimRight(resource, "/") + "/*",
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
			delSpec := PermissionSpec{Service: p.Service, Action: "deleteVolume", Resource: resource}
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

		// An outpost-gateway/enqueueCommand step enqueues a command (POST
		// /outposts/<id>/commands) and then polls it to a terminal state (GET
		// /outpost-commands/<cmdId>). The poll reads a DIFFERENT resource space than the
		// submit (a globally-unique command id, not scoped under the outpost), so the
		// generic create→get companion above cannot cover it. Grant getCommand on the
		// command space explicitly, mirroring the triggerRun case.
		if p.Action == "enqueueCommand" {
			spec := PermissionSpec{Service: p.Service, Action: "getCommand", Resource: "outpost-gateway/outpost-commands/*"}
			k := spec.Service + ":" + spec.Action + ":" + spec.Resource
			if _, dup := seen[k]; !dup {
				seen[k] = struct{}{}
				out = append(out, spec)
			}
		}
	}

	// addSpec grants a permission the step DECLARED, rather than one derived from the
	// action catalog. Same dedup and path-param wildcarding as addAction.
	addSpec := func(p PermissionSpec) {
		if p.Service == "" || p.Action == "" || p.Resource == "" {
			return
		}
		resource := wildcardPathParams(p.Resource)
		key := p.Service + ":" + p.Action + ":" + resource
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, PermissionSpec{Service: p.Service, Action: p.Action, Resource: resource})
	}

	for _, ws := range steps {
		// A step may declare the grants it needs. This is the ONLY way an http
		// escape-hatch step gets any: it has no catalog entry, so nothing can be
		// inferred from its action, and without this its calls are simply 403'd.
		// Safe to honour verbatim — gatekeeper mints only permissions the pipeline's
		// owner already holds, and scopes each to that owner.
		for _, p := range ws.Permissions {
			addSpec(p)
		}
		if ws.Action == ActionHTTP {
			continue // no catalog entry to derive from — see the declared grants above
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
// workflowID.
//
// ("", nil) means the workflow declares NO permissions and legitimately needs no
// role — the ONLY case in which an empty role id is a final answer. Every failure
// to provision one returns a non-nil error, and the two must never be conflated:
// createRunToken omits role_id when the role is empty, so a run whose workflow
// stored "" carries the OWNER'S FULL SESSION PERMISSIONS instead of the minimal
// step set. Persisting that alongside a stamped-current RolePermsVersion also stops
// the trigger-time heal from ever retrying (it only compares versions), which made
// the downgrade permanent and invisible.
func provisionWorkflowRole(ctx context.Context, workflowID, userID, orgID string, steps []WorkflowStep, maps []MapDef, ticket *TicketConfig) (string, error) {
	perms := collectWorkflowPermissions(steps, maps, ticket)
	if len(perms) == 0 {
		return "", nil
	}
	key := gatekeeperKey()
	if key == "" {
		// Not "no role needed": without a service key no run token can be minted either
		// (see createRunToken), so this is a misconfiguration to surface, not absorb.
		return "", errors.New("gatekeeper service key not available")
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
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", "workflows:"+key)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("gatekeeper returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var result struct {
		RoleID string `json:"role_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.RoleID == "" {
		// A 201 that names no role is as unusable as a failure — the permissions were
		// requested, so an empty id here would silently mean "no role needed".
		return "", errors.New("gatekeeper returned an empty role_id")
	}
	return result.RoleID, nil
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

// deleteWorkflowRoleIfUnused deletes a superseded workflow role, but only once no
// in-flight run still depends on it. keepRoleID is the workflow's current role: a
// role equal to it is never deleted (it is still the live one). A role an active run
// is still authenticating with is left in place — deleting it would 403 that run's
// every subsequent step (this is exactly the bug where updating a workflow mid-run
// killed the run). The role is instead garbage-collected when the last run using it
// finishes (see the run-completion GC in the worker). On a query error it errs toward
// keeping the role — a leaked role is harmless; a wrongly-deleted one breaks a run.
func deleteWorkflowRoleIfUnused(ctx context.Context, roleID, keepRoleID string) {
	if roleID == "" || roleID == keepRoleID {
		return
	}
	inUse, err := roleInUseByActiveRun(ctx, roleID)
	if err != nil {
		slog.WarnContext(ctx, "deleteWorkflowRoleIfUnused: active-run check failed, keeping role", "role_id", roleID, "error", err)
		return
	}
	if inUse {
		slog.InfoContext(ctx, "deleteWorkflowRoleIfUnused: role still used by an active run, deferring deletion", "role_id", roleID)
		return
	}
	deleteWorkflowRole(ctx, roleID)
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
	Name        string `json:"name"`
	Description string `json:"description"`
	Project     string `json:"project,omitempty"`
	// TimeoutSecs caps a single run's wall-clock duration (default 1800 / 30 min when
	// omitted). Prevents a hung run from lingering for hours.
	TimeoutSecs int64               `json:"timeout_secs,omitempty"`
	Steps       []WorkflowStepRef   `json:"steps,omitempty"`
	Inputs      []WorkflowInputDef  `json:"inputs,omitempty"`
	Outputs     []WorkflowOutputDef `json:"outputs,omitempty"`
	// Routes are the explicit edges between steps, and the only way to express a
	// fork or a join. Omit them for a plain sequence: the edges are then derived as
	// a chain in array order at run time.
	Routes []WorkflowRoute `json:"routes,omitempty"`
	// Maps declare the map regions steps join via map_id — see MapDef.
	Maps []MapDef `json:"maps,omitempty"`
	// Loops declare the loops steps join via loop_id — see LoopDef.
	Loops []LoopDef `json:"loops,omitempty"`
	// Ticket opts every run of this workflow into being mirrored to a ticket — see
	// TicketConfig. Omit it and nothing changes.
	Ticket *TicketConfig `json:"ticket,omitempty"`
	// StateMachine is the human-authored pipeline as a state machine (see smDoc). When
	// present it is expanded into Steps/Routes/Maps (and fills Name/Description/Inputs/
	// Outputs if those are unset) BEFORE the normal validation chain, so a client can
	// POST either shape and everything downstream is unchanged. Explicit top-level
	// fields still win over the document's.
	StateMachine *smDoc `json:"state_machine,omitempty"`
}

// applyStateMachine expands a state-machine document on the request into the
// (steps, routes, maps) triple the rest of the handler already knows how to
// validate and store. Name/description/inputs/outputs are filled from the document
// only when the request did not set them, so an explicit top-level field wins.
// Returns a user-facing message on a malformed document, "" otherwise (including
// when no document was sent).
func applyStateMachine(req *createWorkflowRequest) string {
	if req.StateMachine == nil {
		return ""
	}
	// Explicit steps WIN over the document, matching what this function already does
	// for name/description/inputs/outputs. Without this the rule held for every field
	// except the one that matters: every GET returns a computed state_machine, so the
	// natural read-edit-write round-trip silently discarded the caller's edited steps
	// — the request returned 200 and echoed the OLD steps back, so nothing indicated
	// the write had been thrown away. Worse, the steps it substituted lost anything the
	// document cannot express, which re-minted the run role without the step's declared
	// permissions and failed later as an unexplained 403.
	if len(req.Steps) > 0 {
		return ""
	}
	steps, routes, maps, err := smToModel(req.StateMachine)
	if err != nil {
		return err.Error()
	}
	req.Steps, req.Routes, req.Maps = steps, routes, maps
	if req.Name == "" {
		req.Name = req.StateMachine.Name
	}
	if req.Description == "" {
		req.Description = req.StateMachine.Description
	}
	if len(req.Inputs) == 0 {
		req.Inputs = req.StateMachine.Inputs
	}
	if len(req.Outputs) == 0 {
		req.Outputs = req.StateMachine.Outputs
	}
	return ""
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
	// A state-machine document is expanded into steps/routes/maps before any of the
	// checks below, so both request shapes take the identical validation path.
	if msg := applyStateMachine(&req); msg != "" {
		span.SetStatus(codes.Ok, "")
		http.Error(w, msg, http.StatusBadRequest)
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
		TimeoutSecs: req.TimeoutSecs, // 0 → the column default (1800 / 30 min) applies
		CreatedBy:   userID,
		OrgID:       orgID,
		Active:      true,
		Inputs:      req.Inputs,
		Outputs:     req.Outputs,
		Routes:      req.Routes,
		Maps:        req.Maps,
		Loops:       req.Loops,
		Ticket:      req.Ticket,
		StepRefs:    refs,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	// Every pipeline must belong to a project — no projectless pipelines. The slug
	// must resolve to a real gatekeeper project the caller may create within
	// (developer/admin/owner); an empty or unresolvable project is rejected (400)
	// rather than silently kept as a free-text label, and a view-only project is
	// refused (403).
	if req.Project == "" {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "project is required", http.StatusBadRequest)
		return
	}
	{
		bearer := r.Header.Get("Authorization")
		p := resolveProjectSlug(ctx, bearer, req.Project)
		if p == nil {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "unknown project "+req.Project, http.StatusBadRequest)
			return
		}
		if !checkProjectPermission(ctx, bearer, "createWorkflow", "pipelines", p.Slug, "") {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "you cannot create pipelines in project "+p.Slug, http.StatusForbidden)
			return
		}
		wf.ProjectID = p.ProjectID
		wf.ProjectNamespace = p.Namespace
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
	if msg := validateGraph(wf.Steps, wf.Routes, wf.Maps, wf.Loops); msg != "" {
		span.SetStatus(codes.Ok, "")
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// Provision a scoped service role before persisting so the role_id is stored atomically.
	// A provisioning failure fails the whole request rather than storing an empty role:
	// that would silently hand every run of this workflow the owner's full session
	// permissions, with the stamped version stopping the heal from ever retrying.
	roleID, perr := provisionWorkflowRole(ctx, wf.WorkflowID, userID, orgID, wf.Steps, wf.Maps, wf.Ticket)
	if perr != nil {
		span.RecordError(perr)
		span.SetStatus(codes.Error, "role provisioning failed")
		slog.ErrorContext(ctx, "create workflow: provision run role", "workflow_id", wf.WorkflowID, "error", perr)
		http.Error(w, "failed to provision the workflow's run permissions", http.StatusInternalServerError)
		return
	}
	wf.RoleID = roleID
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

	wfs, err := listWorkflows(ctx, userID, orgID, r.URL.Query().Get("project"), accessibleProjectIDs(ctx, r.Header.Get("Authorization")))
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
	if !authorizeWorkflow(ctx, r.Header.Get("Authorization"), "getWorkflow", wf, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	// Render the pipeline as a state machine too, so a client can display/edit it in
	// that shape. Uses effective routes — a route-less pipeline is a plain sequence,
	// whose chain is derived — so the document shows real transitions either way.
	smRoutes := wf.Routes
	if len(smRoutes) == 0 {
		smRoutes = deriveRoutes(wf.Steps)
	}
	wf.StateMachine = modelToSM(wf.Name, wf.Description, wf.Steps, smRoutes, wf.Maps, wf.Inputs, wf.Outputs)
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
	if !authorizeWorkflow(ctx, r.Header.Get("Authorization"), "updateWorkflow", existing, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	var req createWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if msg := applyStateMachine(&req); msg != "" {
		span.SetStatus(codes.Ok, "")
		http.Error(w, msg, http.StatusBadRequest)
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
	if msg := validateGraph(newSteps, req.Routes, req.Maps, req.Loops); msg != "" {
		span.SetStatus(codes.Ok, "")
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	oldRoleID := existing.RoleID
	existing.Name = req.Name
	existing.Description = req.Description
	existing.Routes = req.Routes
	existing.Maps = req.Maps
	existing.Loops = req.Loops
	// Guard like the timeout and project below: the state_machine document carries no
	// ticket field, and a state_machine PUT is the round-trippable edit shape (the
	// enriched steps array fails validateStepRefShape), so an unguarded assignment made
	// every such edit silently delete the workflow's ticket-mirroring config — and
	// re-provision the role without the ticket permissions. Disabling mirroring is still
	// expressible by sending the config with enabled=false.
	if req.Ticket != nil {
		existing.Ticket = req.Ticket
	}
	// A partial PUT that omits the timeout keeps the stored one, so an update never
	// silently drops a run's cap to the zero value.
	if req.TimeoutSecs > 0 {
		existing.TimeoutSecs = req.TimeoutSecs
	}
	// Guard like tickets: a partial PUT that omits project must not silently
	// wipe the stored label (the CLI/TUI update payloads don't send project).
	if req.Project != "" {
		existing.Project = req.Project
		// Re-resolve project membership: if the (possibly new) slug names a real
		// project the caller may write to, file it there; otherwise it reverts to a
		// plain label (clear the ids so a moved pipeline never keeps stale scope).
		existing.ProjectID, existing.ProjectNamespace = "", ""
		bearer := r.Header.Get("Authorization")
		if p := resolveProjectSlug(ctx, bearer, req.Project); p != nil &&
			checkProjectPermission(ctx, bearer, "updateWorkflow", "pipelines", p.Slug, "") {
			existing.ProjectID = p.ProjectID
			existing.ProjectNamespace = p.Namespace
		}
	}
	existing.Inputs = req.Inputs
	existing.Outputs = req.Outputs
	existing.StepRefs = refs
	existing.Steps = newSteps
	existing.UpdatedAt = time.Now().UTC()

	// Re-provision the role with the updated step set — against the EFFECTIVE ticket
	// config (which a partial PUT may have preserved), not the request's, so the role
	// keeps the ticket grants the workflow still uses. On failure keep the stored role
	// and version and fail the request: persisting an empty role id would downgrade
	// every future run to the owner's full session permissions, and deleting the old
	// role below would revoke the scoped one that is still correct.
	newRoleID, perr := provisionWorkflowRole(ctx, existing.WorkflowID, userID, orgID, newSteps, existing.Maps, existing.Ticket)
	if perr != nil {
		span.RecordError(perr)
		span.SetStatus(codes.Error, "role provisioning failed")
		slog.ErrorContext(ctx, "update workflow: provision run role", "workflow_id", existing.WorkflowID, "error", perr)
		http.Error(w, "failed to provision the workflow's run permissions", http.StatusInternalServerError)
		return
	}
	existing.RoleID = newRoleID
	existing.RolePermsVersion = workflowRolePermsVersion

	if err := existing.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "update workflow: db error", "workflow_id", existing.WorkflowID, "user_id", userID, "error", err)
		deleteWorkflowRole(ctx, existing.RoleID)
		http.Error(w, "failed to update workflow", http.StatusInternalServerError)
		return
	}
	// Old role is now superseded; delete it only if no in-flight run is still
	// authenticating with it — otherwise the run would 403 on its next step. A role
	// left behind for an active run is GC'd when that run finishes.
	deleteWorkflowRoleIfUnused(ctx, oldRoleID, existing.RoleID)

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
	if !authorizeWorkflow(ctx, r.Header.Get("Authorization"), "deleteWorkflow", wf, userID, orgID) {
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
	// Deleting the role outright would revoke it out from under any run still in flight:
	// its remaining steps would 403 and leave a half-applied deploy. Take the same
	// active-run-aware path the update flow uses — keepRoleID is "" because a deleted
	// workflow has no current role to preserve — and let the run-completion GC in the
	// worker reclaim the role once the last run using it finishes.
	deleteWorkflowRoleIfUnused(ctx, roleID, "")

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "workflow deleted", "workflow_id", wf.WorkflowID, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}
