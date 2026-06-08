package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// testDBReady is set to true in TestMain when the registry DB is accessible.
var testDBReady bool

// Test-only service account credentials seeded into registry_service_accounts.
const (
	testReadKey  = "test-reader:testreadkey"
	testAdminKey = "test-admin:testadminkey"
)

func TestMain(m *testing.M) {
	// Stub DNS so tests don't need real hostname resolution.
	// Returns a public IP that passes SSRF validation.
	resolveHost = func(host string) ([]string, error) {
		return []string{"93.184.216.34"}, nil
	}

	// Bypass connect()'s os.Exit by opening GORM directly and only setting the
	// singleton on success.
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgresql://postgres:postgres@localhost:5432/registry"
	}
	conn, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err == nil {
		gormDBMu.Lock()
		gormDB = conn
		gormDBMu.Unlock()

		ctx := context.Background()
		migrateErr := conn.AutoMigrate(
			&ServiceModel{},
			&ServiceRoleModel{},
			&ServiceEndpointModel{},
			&ServiceActionModel{},
			&ServiceAccountModel{},
			&ServiceDefaultGrantModel{},
		)
		if migrateErr == nil {
			testDBReady = true
			seedServiceAccounts(ctx, "test-reader=testreadkey", "read")
			seedServiceAccounts(ctx, "test-admin=testadminkey", "admin")
		}
	}

	code := m.Run()
	if testDBReady {
		connect().Exec(`DELETE FROM registry_service_accounts WHERE name IN ('test-reader','test-admin')`) //nolint:errcheck
	}
	os.Exit(code)
}

func requireDB(t *testing.T) {
	t.Helper()
	if !testDBReady {
		t.Skip("registry database not available")
	}
}

// insertTestService creates an active service row and registers a cleanup to
// hard-delete it (including child rows) at the end of the test.
func insertTestService(t *testing.T, name string) string {
	t.Helper()
	id := uuid.New().String()
	if err := connect().Exec(
		`INSERT INTO services (service_id, name, url) VALUES (?, ?, ?)`,
		id, name, "http://svc:9000").Error; err != nil {
		t.Fatalf("insertTestService: %v", err)
	}
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM service_roles     WHERE service_id = ?`, id) //nolint:errcheck
		connect().Exec(`DELETE FROM service_endpoints WHERE service_id = ?`, id) //nolint:errcheck
		connect().Exec(`DELETE FROM services          WHERE service_id = ?`, id) //nolint:errcheck
	})
	return id
}

// ── handleListServices ────────────────────────────────────────────────────────

func TestHandleListServices_Success(t *testing.T) {
	requireDB(t)

	r := httptest.NewRequest(http.MethodGet, "/services", nil)
	r.Header.Set("X-Service-Key", testReadKey)
	w := httptest.NewRecorder()
	handleListServices(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var result []any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// ── handleCreateService ───────────────────────────────────────────────────────

func TestHandleCreateService_Success(t *testing.T) {
	requireDB(t)

	name := uuid.New().String()
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM services WHERE name = ?`, name) //nolint:errcheck
	})

	body := bytes.NewBufferString(`{"name":"` + name + `","url":"http://newsvc:9000","description":"test svc"}`)
	r := httptest.NewRequest(http.MethodPost, "/services", body)
	r.Header.Set("X-Service-Key", testAdminKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCreateService(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", w.Code, w.Body.String())
	}
	var svc Service
	if err := json.NewDecoder(w.Body).Decode(&svc); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if svc.Name != name {
		t.Errorf("name = %q, want %q", svc.Name, name)
	}
	if svc.ServiceID == "" {
		t.Error("expected non-empty service_id")
	}
}

func TestHandleCreateService_Duplicate(t *testing.T) {
	requireDB(t)

	name := uuid.New().String()
	insertTestService(t, name)

	body := bytes.NewBufferString(`{"name":"` + name + `","url":"http://dup:9000"}`)
	r := httptest.NewRequest(http.MethodPost, "/services", body)
	r.Header.Set("X-Service-Key", testAdminKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCreateService(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", w.Code)
	}
}

func TestHandleCreateService_MissingURL(t *testing.T) {
	requireDB(t)

	body := bytes.NewBufferString(`{"name":"somesvc"}`)
	r := httptest.NewRequest(http.MethodPost, "/services", body)
	r.Header.Set("X-Service-Key", testAdminKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCreateService(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateService_SSRFLoopback(t *testing.T) {
	requireDB(t)

	body := bytes.NewBufferString(`{"name":"loopback-svc","url":"http://127.0.0.1:9999"}`)
	r := httptest.NewRequest(http.MethodPost, "/services", body)
	r.Header.Set("X-Service-Key", testAdminKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCreateService(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for loopback URL", w.Code)
	}
}

// ── handleDeleteService ───────────────────────────────────────────────────────

func TestHandleDeleteService_Success(t *testing.T) {
	requireDB(t)

	name := uuid.New().String()
	id := insertTestService(t, name)

	r := httptest.NewRequest(http.MethodDelete, "/services/"+id, nil)
	r.SetPathValue("id", id)
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	handleDeleteService(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", w.Code, w.Body.String())
	}
}

func TestHandleDeleteService_NotFound(t *testing.T) {
	requireDB(t)

	r := httptest.NewRequest(http.MethodDelete, "/services/no-such-id", nil)
	r.SetPathValue("id", "no-such-id")
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	handleDeleteService(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}
