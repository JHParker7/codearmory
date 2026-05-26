package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// createTestServiceAccount inserts an active ServiceAccount with a known plain-text key
// and returns both the record and the plain key so callers can build X-Service-Key headers.
func createTestServiceAccount(t *testing.T) (ServiceAccount, string) {
	t.Helper()
	plainKey := "test-key-" + uuid.New().String()[:8]
	hash, err := bcrypt.GenerateFromPassword([]byte(plainKey), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	svc := ServiceAccount{
		ServiceAccountID: uuid.New().String(),
		ServiceName:      "svc-" + uuid.New().String()[:8],
		HashedKey:        string(hash),
		Active:           true,
	}
	if err := svc.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Remove(context.Background()) })
	return svc, plainKey
}

// createPendingSPR inserts a pending ServicePermissionRequest for the given service account
// and returns it.
func createPendingSPR(t *testing.T, svc ServiceAccount) ServicePermissionRequest {
	t.Helper()
	spr := ServicePermissionRequest{
		RequestID:   uuid.New().String(),
		ServiceName: svc.ServiceName,
		Name:        "perm-" + uuid.New().String()[:8],
		Service:     svc.ServiceName,
		Actions:     []string{"read"},
		Resources:   []string{"some/resource"},
		Status:      "pending",
		Active:      true,
	}
	if err := spr.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spr.Remove(context.Background()) })
	return spr
}

// serviceKeyHeader returns the value for X-Service-Key ("name:plain-key").
func serviceKeyHeader(svc ServiceAccount, plainKey string) string {
	return svc.ServiceName + ":" + plainKey
}

// --- requireServiceAuth ---

func TestRequireServiceAuth_MissingHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	if _, ok := requireServiceAuth(w, r); ok {
		t.Fatal("expected false for missing X-Service-Key header")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRequireServiceAuth_WrongFormat(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Service-Key", "no-colon")
	w := httptest.NewRecorder()
	if _, ok := requireServiceAuth(w, r); ok {
		t.Fatal("expected false for header with no colon")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRequireServiceAuth_WrongKey(t *testing.T) {
	svc, _ := createTestServiceAccount(t)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Service-Key", svc.ServiceName+":wrong-key")
	w := httptest.NewRecorder()
	if _, ok := requireServiceAuth(w, r); ok {
		t.Fatal("expected false for wrong key")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRequireServiceAuth_Success(t *testing.T) {
	svc, plainKey := createTestServiceAccount(t)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Service-Key", serviceKeyHeader(svc, plainKey))
	w := httptest.NewRecorder()
	got, ok := requireServiceAuth(w, r)
	if !ok {
		t.Fatalf("expected ok=true for valid credentials, got body: %s", w.Body.String())
	}
	if got.ServiceName != svc.ServiceName {
		t.Fatalf("expected service name %q, got %q", svc.ServiceName, got.ServiceName)
	}
}

// --- handleCreateServicePermissionRequest ---

func TestHandleCreateServicePermissionRequest_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/service-permission-requests", nil)
	w := httptest.NewRecorder()
	handleCreateServicePermissionRequest(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleCreateServicePermissionRequest_InvalidBody(t *testing.T) {
	svc, plainKey := createTestServiceAccount(t)
	r := httptest.NewRequest(http.MethodPost, "/service-permission-requests", bytes.NewReader([]byte("not-json")))
	r.Header.Set("X-Service-Key", serviceKeyHeader(svc, plainKey))
	w := httptest.NewRecorder()
	handleCreateServicePermissionRequest(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleCreateServicePermissionRequest_MissingFields(t *testing.T) {
	svc, plainKey := createTestServiceAccount(t)
	body, _ := json.Marshal(map[string]interface{}{
		"name": "my-perm",
		// missing service, actions, resources
	})
	r := httptest.NewRequest(http.MethodPost, "/service-permission-requests", bytes.NewReader(body))
	r.Header.Set("X-Service-Key", serviceKeyHeader(svc, plainKey))
	w := httptest.NewRecorder()
	handleCreateServicePermissionRequest(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleCreateServicePermissionRequest_Success(t *testing.T) {
	svc, plainKey := createTestServiceAccount(t)
	body, _ := json.Marshal(servicePermissionRequestBody{
		Name:      "my-perm-" + uuid.New().String()[:8],
		Service:   svc.ServiceName,
		Actions:   []string{"read"},
		Resources: []string{"some/resource"},
	})
	r := httptest.NewRequest(http.MethodPost, "/service-permission-requests", bytes.NewReader(body))
	r.Header.Set("X-Service-Key", serviceKeyHeader(svc, plainKey))
	w := httptest.NewRecorder()
	handleCreateServicePermissionRequest(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp ServicePermissionRequest
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if resp.RequestID == "" {
		t.Fatal("expected non-empty request_id in response")
	}
	// cleanup
	t.Cleanup(func() { resp.Remove(context.Background()) })
}

func TestHandleCreateServicePermissionRequest_Duplicate(t *testing.T) {
	svc, plainKey := createTestServiceAccount(t)
	permName := "dup-perm-" + uuid.New().String()[:8]

	// Create the first request
	body, _ := json.Marshal(servicePermissionRequestBody{
		Name:      permName,
		Service:   svc.ServiceName,
		Actions:   []string{"read"},
		Resources: []string{"some/resource"},
	})
	r := httptest.NewRequest(http.MethodPost, "/service-permission-requests", bytes.NewReader(body))
	r.Header.Set("X-Service-Key", serviceKeyHeader(svc, plainKey))
	w := httptest.NewRecorder()
	handleCreateServicePermissionRequest(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("first create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var first ServicePermissionRequest
	json.Unmarshal(w.Body.Bytes(), &first)
	t.Cleanup(func() { first.Remove(context.Background()) })

	// Attempt a duplicate (same name, same service, still pending)
	body2, _ := json.Marshal(servicePermissionRequestBody{
		Name:      permName,
		Service:   svc.ServiceName,
		Actions:   []string{"write"},
		Resources: []string{"other/resource"},
	})
	r2 := httptest.NewRequest(http.MethodPost, "/service-permission-requests", bytes.NewReader(body2))
	r2.Header.Set("X-Service-Key", serviceKeyHeader(svc, plainKey))
	w2 := httptest.NewRecorder()
	handleCreateServicePermissionRequest(w2, r2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("duplicate create: expected 409, got %d: %s", w2.Code, w2.Body.String())
	}
}

// --- handleListServicePermissionRequests ---

func TestHandleListServicePermissionRequests_Forbidden(t *testing.T) {
	actor := createTestUser(t)
	r := withUserID(httptest.NewRequest(http.MethodGet, "/service-permission-requests", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListServicePermissionRequests(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestHandleListServicePermissionRequests_Success(t *testing.T) {
	actor := createAuthorizedUser(t, "listServicePermissionRequest", "gatekeeper/service-permission-requests")
	svc, _ := createTestServiceAccount(t)
	spr := createPendingSPR(t, svc)
	_ = spr

	r := withUserID(httptest.NewRequest(http.MethodGet, "/service-permission-requests", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListServicePermissionRequests(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result []ServicePermissionRequest
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

// --- handleGetServicePermissionRequest ---

func TestHandleGetServicePermissionRequest_Forbidden(t *testing.T) {
	actor := createTestUser(t)
	svc, _ := createTestServiceAccount(t)
	spr := createPendingSPR(t, svc)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/service-permission-requests/"+spr.RequestID, nil), actor.UserID)
	r.SetPathValue("id", spr.RequestID)
	w := httptest.NewRecorder()
	handleGetServicePermissionRequest(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestHandleGetServicePermissionRequest_NotFound(t *testing.T) {
	missing := uuid.New().String()
	actor := createAuthorizedUser(t, "getServicePermissionRequest", "gatekeeper/service-permission-requests/"+missing)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/service-permission-requests/"+missing, nil), actor.UserID)
	r.SetPathValue("id", missing)
	w := httptest.NewRecorder()
	handleGetServicePermissionRequest(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleGetServicePermissionRequest_Success(t *testing.T) {
	svc, _ := createTestServiceAccount(t)
	spr := createPendingSPR(t, svc)
	actor := createAuthorizedUser(t, "getServicePermissionRequest", "gatekeeper/service-permission-requests/"+spr.RequestID)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/service-permission-requests/"+spr.RequestID, nil), actor.UserID)
	r.SetPathValue("id", spr.RequestID)
	w := httptest.NewRecorder()
	handleGetServicePermissionRequest(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp ServicePermissionRequest
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.RequestID != spr.RequestID {
		t.Fatalf("expected request_id %q, got %q", spr.RequestID, resp.RequestID)
	}
}

// --- handleApproveServicePermissionRequest ---

func TestHandleApproveServicePermissionRequest_Forbidden(t *testing.T) {
	actor := createTestUser(t)
	svc, _ := createTestServiceAccount(t)
	spr := createPendingSPR(t, svc)

	r := withUserID(httptest.NewRequest(http.MethodPost, "/service-permission-requests/"+spr.RequestID+"/approve", nil), actor.UserID)
	r.SetPathValue("id", spr.RequestID)
	w := httptest.NewRecorder()
	handleApproveServicePermissionRequest(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestHandleApproveServicePermissionRequest_NotFound(t *testing.T) {
	missing := uuid.New().String()
	actor := createAuthorizedUser(t, "approveServicePermissionRequest", "gatekeeper/service-permission-requests/"+missing)

	r := withUserID(httptest.NewRequest(http.MethodPost, "/service-permission-requests/"+missing+"/approve", nil), actor.UserID)
	r.SetPathValue("id", missing)
	w := httptest.NewRecorder()
	handleApproveServicePermissionRequest(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleApproveServicePermissionRequest_Success(t *testing.T) {
	svc, _ := createTestServiceAccount(t)
	spr := createPendingSPR(t, svc)
	actor := createAuthorizedUser(t, "approveServicePermissionRequest", "gatekeeper/service-permission-requests/"+spr.RequestID)

	r := withUserID(httptest.NewRequest(http.MethodPost, "/service-permission-requests/"+spr.RequestID+"/approve", nil), actor.UserID)
	r.SetPathValue("id", spr.RequestID)
	w := httptest.NewRecorder()
	handleApproveServicePermissionRequest(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the SPR is now approved.
	row, err := (ServicePermissionRequest{RequestID: spr.RequestID}).Get(context.Background())
	if err != nil {
		t.Fatalf("Get after approve: %v", err)
	}
	updated := row.(ServicePermissionRequest)
	if updated.Status != "approved" {
		t.Fatalf("expected status=approved, got %q", updated.Status)
	}
}

// --- handleDeclineServicePermissionRequest ---

func TestHandleDeclineServicePermissionRequest_Forbidden(t *testing.T) {
	actor := createTestUser(t)
	svc, _ := createTestServiceAccount(t)
	spr := createPendingSPR(t, svc)

	r := withUserID(httptest.NewRequest(http.MethodPost, "/service-permission-requests/"+spr.RequestID+"/decline", nil), actor.UserID)
	r.SetPathValue("id", spr.RequestID)
	w := httptest.NewRecorder()
	handleDeclineServicePermissionRequest(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestHandleDeclineServicePermissionRequest_NotFound(t *testing.T) {
	missing := uuid.New().String()
	actor := createAuthorizedUser(t, "declineServicePermissionRequest", "gatekeeper/service-permission-requests/"+missing)

	r := withUserID(httptest.NewRequest(http.MethodPost, "/service-permission-requests/"+missing+"/decline", nil), actor.UserID)
	r.SetPathValue("id", missing)
	w := httptest.NewRecorder()
	handleDeclineServicePermissionRequest(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleDeclineServicePermissionRequest_Success(t *testing.T) {
	svc, _ := createTestServiceAccount(t)
	spr := createPendingSPR(t, svc)
	actor := createAuthorizedUser(t, "declineServicePermissionRequest", "gatekeeper/service-permission-requests/"+spr.RequestID)

	r := withUserID(httptest.NewRequest(http.MethodPost, "/service-permission-requests/"+spr.RequestID+"/decline", nil), actor.UserID)
	r.SetPathValue("id", spr.RequestID)
	w := httptest.NewRecorder()
	handleDeclineServicePermissionRequest(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the SPR is now declined.
	row, err := (ServicePermissionRequest{RequestID: spr.RequestID}).Get(context.Background())
	if err != nil {
		t.Fatalf("Get after decline: %v", err)
	}
	updated := row.(ServicePermissionRequest)
	if updated.Status != "declined" {
		t.Fatalf("expected status=declined, got %q", updated.Status)
	}
}

func TestHandleDeclineServicePermissionRequest_NotPending(t *testing.T) {
	svc, _ := createTestServiceAccount(t)
	spr := createPendingSPR(t, svc)

	// First decline succeeds
	actor := createAuthorizedUser(t, "declineServicePermissionRequest", "gatekeeper/service-permission-requests/"+spr.RequestID)
	r1 := withUserID(httptest.NewRequest(http.MethodPost, "/service-permission-requests/"+spr.RequestID+"/decline", nil), actor.UserID)
	r1.SetPathValue("id", spr.RequestID)
	w1 := httptest.NewRecorder()
	handleDeclineServicePermissionRequest(w1, r1)
	if w1.Code != http.StatusNoContent {
		t.Fatalf("first decline: expected 204, got %d: %s", w1.Code, w1.Body.String())
	}

	// Second decline should fail because request is no longer pending
	r2 := withUserID(httptest.NewRequest(http.MethodPost, "/service-permission-requests/"+spr.RequestID+"/decline", nil), actor.UserID)
	r2.SetPathValue("id", spr.RequestID)
	w2 := httptest.NewRecorder()
	handleDeclineServicePermissionRequest(w2, r2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("second decline: expected 409, got %d: %s", w2.Code, w2.Body.String())
	}
}

// --- handleListAuditLogs ---

func TestHandleListAuditLogs_Forbidden(t *testing.T) {
	actor := createTestUser(t)
	r := withUserID(httptest.NewRequest(http.MethodGet, "/audit-logs", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListAuditLogs(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestHandleListAuditLogs_Success(t *testing.T) {
	actor := createAuthorizedUser(t, "listAuditLog", "gatekeeper/audit-logs")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/audit-logs", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListAuditLogs(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result []AuditLog
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}
