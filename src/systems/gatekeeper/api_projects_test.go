package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
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

// projectOwner creates a user holding the default createProject grant over their own
// namespace — the gate handleCreateProject checks before it touches anything.
func projectOwner(t *testing.T) User {
	t.Helper()
	ctx := context.Background()
	owner := createTestUser(t)

	perm := Permissions{
		PermissionsID: uuid.New().String(), Name: "own-projects-" + owner.UserID, Service: "gatekeeper",
		Actions: []string{"createProject"}, Resources: []string{owner.Username + "/gatekeeper/projects"},
		OwnerID: owner.UserID, Active: true,
	}
	if err := perm.Add(ctx); err != nil {
		t.Fatalf("add permission: %v", err)
	}
	t.Cleanup(func() { perm.Remove(ctx) }) //nolint:errcheck

	role := Role{RoleID: uuid.New().String(), Name: "own-projects-" + owner.UserID,
		PermissionsIDs: []string{perm.PermissionsID}, OwnerID: owner.UserID, Active: true}
	if err := role.Add(ctx); err != nil {
		t.Fatalf("add role: %v", err)
	}
	t.Cleanup(func() { role.Remove(ctx) }) //nolint:errcheck

	row, err := (User{UserID: owner.UserID}).Get(ctx)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	u := row.(User)
	u.RoleID = &role.RoleID
	if err := u.Update(ctx); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	return owner
}

func postProject(t *testing.T, owner User, slug string) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader(`{"slug":"` + slug + `","name":"` + slug + `"}`)
	r := withUserID(httptest.NewRequest(http.MethodPost, "/projects", body), owner.UserID)
	w := httptest.NewRecorder()
	handleCreateProject(w, r)
	return w
}

// tierObjectCount reports how many RBAC objects exist for a project slug, counting
// inactive ones too — an orphan is still an orphan after a soft delete, and the point of
// these tests is that a rejected creation leaves NOTHING behind.
func tierObjectCount(t *testing.T, slug string) (perms, roles int64) {
	t.Helper()
	gormDB.Model(&Permissions{}).Where("name LIKE ?", "project:"+slug+":%").Count(&perms)
	gormDB.Model(&Role{}).Where("name LIKE ?", "project/"+slug+"/%").Count(&roles)
	return perms, roles
}

// A slug is unique across ALL rows, so a second project claiming a live slug must be
// refused before any RBAC object is minted. Provisioning first meant the insert was the
// thing that failed — after three Permissions and three Roles already existed, one of
// them a Service "*" / Actions ["*"] wildcard over the namespace, with no rollback.
func TestCreateProject_DuplicateSlugLeavesNoOrphanedRBAC(t *testing.T) {
	owner := projectOwner(t)
	slug := "dup-" + uuid.New().String()[:8]
	t.Cleanup(func() { gormDB.Where("slug = ?", slug).Delete(&Project{}) }) //nolint:errcheck

	if w := postProject(t, owner, slug); w.Code != http.StatusCreated {
		t.Fatalf("first create: got %d, want 201 (%s)", w.Code, w.Body.String())
	}
	perms, roles := tierObjectCount(t, slug)
	if perms != 3 || roles != 3 {
		t.Fatalf("after one create: %d permissions / %d roles, want 3 / 3", perms, roles)
	}

	for i := 0; i < 3; i++ {
		w := postProject(t, owner, slug)
		if w.Code != http.StatusConflict {
			t.Fatalf("duplicate create %d: got %d, want 409 (%s)", i, w.Code, w.Body.String())
		}
	}
	if p, r := tierObjectCount(t, slug); p != perms || r != roles {
		t.Errorf("three refused creations left %d permissions / %d roles, want %d / %d", p, r, perms, roles)
	}
}

// Deleting a project is a SOFT delete, but the unique index on slug spans inactive rows.
// The conflict has to be reported as one — otherwise the check sails past, provisioning
// runs, and the insert fails with a 500 that orphans six RBAC objects per attempt.
func TestCreateProject_DeletedSlugIsRefusedNotOrphaned(t *testing.T) {
	ctx := context.Background()
	owner := projectOwner(t)
	slug := "gone-" + uuid.New().String()[:8]
	t.Cleanup(func() { gormDB.Where("slug = ?", slug).Delete(&Project{}) }) //nolint:errcheck

	if w := postProject(t, owner, slug); w.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (%s)", w.Code, w.Body.String())
	}
	p, err := getProjectBySlug(ctx, slug)
	if err != nil {
		t.Fatalf("lookup created project: %v", err)
	}
	dr := withUserID(httptest.NewRequest(http.MethodDelete, "/projects/"+p.ProjectID, nil), owner.UserID)
	dr.SetPathValue("id", p.ProjectID)
	dw := httptest.NewRecorder()
	handleDeleteProject(dw, dr)
	if dw.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204 (%s)", dw.Code, dw.Body.String())
	}
	perms, roles := tierObjectCount(t, slug)

	w := postProject(t, owner, slug)
	if w.Code != http.StatusConflict {
		t.Fatalf("recreate of a deleted slug: got %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "deleted project") {
		t.Errorf("conflict message %q does not explain that the slug is held by a deleted project", strings.TrimSpace(w.Body.String()))
	}
	if p, r := tierObjectCount(t, slug); p != perms || r != roles {
		t.Errorf("the refused recreate left %d permissions / %d roles, want %d / %d", p, r, perms, roles)
	}
	var rows int64
	gormDB.Model(&Project{}).Where("slug = ?", slug).Count(&rows)
	if rows != 1 {
		t.Errorf("%d project rows hold slug %q, want 1", rows, slug)
	}
}
