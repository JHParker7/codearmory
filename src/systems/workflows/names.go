package main

import (
	"context"
	"errors"
	"regexp"

	"gorm.io/gorm"
)

// resourceNameRe constrains workflow and step names so a name is always a single
// safe URL/RBAC-resource segment. Conductor derives the gatekeeper resource from
// the raw URL path segment (e.g. `workflows/pipelines/<name>`), so a name must
// contain no slashes or whitespace; we also forbid an empty name and cap length.
var resourceNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// validateResourceName returns a user-facing error message if name is not a valid
// single-segment resource name, or "" when it is acceptable.
func validateResourceName(name string) string {
	if !resourceNameRe.MatchString(name) {
		return "name must be 1-100 chars of letters, digits, '.', '_' or '-' (no spaces or '/')"
	}
	return ""
}

// ownerScope is the WHERE predicate that limits a row to ones the caller owns
// directly or shares via their org — the same visibility rule used by listSteps
// and stepNameExists. Kept in one place so name lookups and access checks agree.
const ownerScope = "active=true AND (created_by=? OR (org_id!='' AND org_id=?))"

// workflowNameExists reports whether an active workflow with the given name is
// already visible to the caller (owned or shared via org). Used on create.
func workflowNameExists(ctx context.Context, name, userID, orgID string) (bool, error) {
	return workflowNameConflict(ctx, name, "", userID, orgID)
}

// workflowNameConflict reports whether an active workflow other than excludeID
// already uses name within the caller's scope. excludeID="" checks all rows
// (create); on rename pass the workflow's own id so keeping its name is allowed.
func workflowNameConflict(ctx context.Context, name, excludeID, userID, orgID string) (bool, error) {
	var existing Workflow
	err := connectRead().WithContext(ctx).
		Where("name=? AND workflow_id<>? AND "+ownerScope, name, excludeID, userID, orgID).
		First(&existing).Error
	if err == nil {
		return true, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return false, err
}

// stepNameConflict mirrors workflowNameConflict for steps — used on rename so a
// step can keep its own name (excludeID = its step_id) but not collide with
// another. stepNameExists covers the create path.
func stepNameConflict(ctx context.Context, name, excludeID, userID, orgID string) (bool, error) {
	var existing Step
	err := connectRead().WithContext(ctx).
		Where("name=? AND step_id<>? AND "+ownerScope, name, excludeID, userID, orgID).
		First(&existing).Error
	if err == nil {
		return true, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return false, err
}

// resolveWorkflowRef maps a URL ref to a workflow. A ref is either the workflow's
// unique name (scoped to the caller) or its workflow_id; the name is tried first
// so `/pipelines/deploy-prod` addresses the caller's own "deploy-prod", and any
// ref matching no owned name falls back to an id lookup. The returned workflow is
// enriched with its step definitions, exactly like getWorkflow.
func resolveWorkflowRef(ctx context.Context, ref, userID, orgID string) (Workflow, error) {
	var wf Workflow
	err := connectRead().WithContext(ctx).
		Where("name=? AND "+ownerScope, ref, userID, orgID).
		First(&wf).Error
	if err == nil {
		steps, serr := enrichStepRefs(ctx, wf.StepRefs)
		if serr != nil {
			return Workflow{}, serr
		}
		wf.Steps = steps
		return wf, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return Workflow{}, err
	}
	return getWorkflow(ctx, ref)
}

// resolveStepRef maps a URL ref to a step by unique name (scoped to the caller)
// first, falling back to a step_id lookup — the step twin of resolveWorkflowRef.
func resolveStepRef(ctx context.Context, ref, userID, orgID string) (Step, error) {
	var s Step
	err := connectRead().WithContext(ctx).
		Where("name=? AND "+ownerScope, ref, userID, orgID).
		First(&s).Error
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return Step{}, err
	}
	return getStep(ctx, ref)
}
