package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMiddlewareAndDocs(t *testing.T) {
	// handleOpenAPIYAML
	w := httptest.NewRecorder()
	handleOpenAPIYAML(w, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatalf("openapi: code=%d len=%d", w.Code, w.Body.Len())
	}

	// limitBody passes through to the next handler.
	served := false
	limitBody(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("body")))
	if !served {
		t.Error("limitBody did not call next")
	}

	// NewLogger + Logger.ServeHTTP + statusResponseWriter.WriteHeader
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	lw := httptest.NewRecorder()
	NewLogger(inner).ServeHTTP(lw, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if lw.Code != http.StatusTeapot {
		t.Errorf("logger status = %d, want 418", lw.Code)
	}
}

func TestPermissionsCheck_CRUD(t *testing.T) {
	ctx := context.Background()
	pc := PermissionsCheck{
		PermissionsCheckID: uuid.NewString(), Service: "forge", Action: "createExecution",
		Resource: "forge/executions", UserID: "u-" + uuid.NewString(), CreatedAt: time.Now().UTC(),
	}
	if err := pc.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { gormDB.Exec(`DELETE FROM permissions_checks WHERE permissions_check_id = ?`, pc.PermissionsCheckID) }) //nolint:errcheck
	if _, err := (PermissionsCheck{PermissionsCheckID: pc.PermissionsCheckID}).Get(ctx); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := (PermissionsCheck{}).List(ctx, 10, 0); err != nil {
		t.Fatalf("List: %v", err)
	}
	_ = pc.Update(ctx)
	_ = pc.Remove(ctx)
}

func TestOAuthClientManagement(t *testing.T) {
	key := seedNamedSvc(t, "oauth-mgr")
	svcHdr := "oauth-mgr:" + key

	// Create.
	body, _ := json.Marshal(map[string]any{"name": "Test App", "redirect_uris": []string{"https://app.example/cb"}})
	cr := httptest.NewRequest(http.MethodPost, "/internal/oauth/clients", strings.NewReader(string(body)))
	cr.Header.Set("X-Service-Key", svcHdr)
	cw := httptest.NewRecorder()
	handleCreateOAuthClient(cw, cr)
	if cw.Code != http.StatusCreated {
		t.Fatalf("create client got %d, want 201: %s", cw.Code, cw.Body.String())
	}
	var created map[string]any
	json.Unmarshal(cw.Body.Bytes(), &created) //nolint:errcheck
	clientID, _ := created["client_id"].(string)
	if clientID == "" {
		t.Fatal("no client_id returned")
	}
	t.Cleanup(func() { gormDB.Unscoped().Where("client_id = ?", clientID).Delete(&OAuthClient{}) }) //nolint:errcheck

	// List.
	lr := httptest.NewRequest(http.MethodGet, "/internal/oauth/clients", nil)
	lr.Header.Set("X-Service-Key", svcHdr)
	lw := httptest.NewRecorder()
	handleListOAuthClients(lw, lr)
	if lw.Code != http.StatusOK {
		t.Fatalf("list clients got %d, want 200", lw.Code)
	}

	// Delete.
	dr := httptest.NewRequest(http.MethodDelete, "/internal/oauth/clients/"+clientID, nil)
	dr.SetPathValue("id", clientID)
	dr.Header.Set("X-Service-Key", svcHdr)
	dw := httptest.NewRecorder()
	handleDeleteOAuthClient(dw, dr)
	if dw.Code < 200 || dw.Code >= 300 {
		t.Fatalf("delete client got %d, want 2xx", dw.Code)
	}
}
