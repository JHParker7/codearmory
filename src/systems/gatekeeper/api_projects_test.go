package main

import (
	"context"
	"testing"
)

// A project provisions three tier roles whose wildcards scope a whole project across
// services, and membership on a tier is what a check consults. These tests exercise the
// permission SHAPE directly (provisionTierRole + assignMembership + checkPermissions),
// independent of the HTTP layer, so they pin the security-relevant behaviour: what a
// tier can and cannot do, and that it is confined to its own project's namespace.

func projectCheck(t *testing.T, userID, service, action, resource string) bool {
	t.Helper()
	ok, err := checkPermissions(context.Background(), userID, service, action, resource)
	if err != nil {
		t.Fatalf("checkPermissions(%s,%s,%s): %v", service, action, resource, err)
	}
	return ok
}

func TestProject_DeveloperTierScopesAcrossServices(t *testing.T) {
	ctx := context.Background()
	owner := createTestUser(t)
	dev := createTestUser(t)

	devRole, err := provisionTierRole(ctx, owner.UserID, "core", "developer")
	if err != nil {
		t.Fatalf("provision developer role: %v", err)
	}
	if err := assignMembership(ctx, devRole, dev.UserID, owner.UserID); err != nil {
		t.Fatalf("assign membership: %v", err)
	}

	// A developer can run a pipeline and write a repo branch in project core, across
	// the different services, from one grant.
	if !projectCheck(t, dev.UserID, "workflows", "runWorkflow", "project/core/workflows/pipelines/42") {
		t.Error("developer should run pipelines in project core")
	}
	if !projectCheck(t, dev.UserID, "codearmory_git_factory", "writeRepo", "project/core/codearmory_git_factory/repos/r1/branches/dev") {
		t.Error("developer should write repo branches in project core")
	}
	if !projectCheck(t, dev.UserID, "tickets", "createTicket", "project/core/tickets/boards/b1") {
		t.Error("developer should create tickets in project core")
	}

	// But NOT a different project — the scope is per-slug.
	if projectCheck(t, dev.UserID, "workflows", "runWorkflow", "project/other/workflows/pipelines/42") {
		t.Error("developer must not reach a different project")
	}
	// And a non-member gets nothing.
	stranger := createTestUser(t)
	if projectCheck(t, stranger.UserID, "workflows", "runWorkflow", "project/core/workflows/pipelines/42") {
		t.Error("non-member must not reach the project")
	}
}

func TestProject_ViewerCannotWrite(t *testing.T) {
	ctx := context.Background()
	owner := createTestUser(t)
	viewer := createTestUser(t)

	viewerRole, err := provisionTierRole(ctx, owner.UserID, "core", "viewer")
	if err != nil {
		t.Fatalf("provision viewer role: %v", err)
	}
	if err := assignMembership(ctx, viewerRole, viewer.UserID, owner.UserID); err != nil {
		t.Fatalf("assign membership: %v", err)
	}

	if !projectCheck(t, viewer.UserID, "codearmory_git_factory", "readRepo", "project/core/codearmory_git_factory/repos/r1") {
		t.Error("viewer should read repos in project core")
	}
	if !projectCheck(t, viewer.UserID, "workflows", "listWorkflow", "project/core/workflows/pipelines/42") {
		t.Error("viewer should list pipelines in project core")
	}
	if projectCheck(t, viewer.UserID, "codearmory_git_factory", "writeRepo", "project/core/codearmory_git_factory/repos/r1/branches/dev") {
		t.Error("viewer must NOT write repos")
	}
	if projectCheck(t, viewer.UserID, "workflows", "deleteWorkflow", "project/core/workflows/pipelines/42") {
		t.Error("viewer must NOT delete pipelines")
	}
}
