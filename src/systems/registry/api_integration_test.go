package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
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

// TestHandleCreateService_ReactivatesSoftDeleted proves the disable→re-enable path:
// an active name is still a 409 duplicate, but a soft-deleted (disabled) name is
// reactivated in place — preserving its service_id — instead of 409ing forever. It
// also asserts conductor is notified on the writes.
func TestHandleCreateService_ReactivatesSoftDeleted(t *testing.T) {
	requireDB(t)

	name := uuid.New().String()
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM services WHERE name = ?`, name) //nolint:errcheck
	})

	var notified int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&notified, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("CONDUCTOR_URL", srv.URL)
	t.Setenv("CONDUCTOR_NOTIFY_KEY", "k")

	create := func() *httptest.ResponseRecorder {
		body := bytes.NewBufferString(`{"name":"` + name + `","url":"http://svc:9000"}`)
		r := httptest.NewRequest(http.MethodPost, "/services", body)
		r.Header.Set("X-Service-Key", testAdminKey)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handleCreateService(w, r)
		return w
	}

	// 1. first create → 201
	w1 := create()
	if w1.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201: %s", w1.Code, w1.Body.String())
	}
	var svc1 Service
	json.NewDecoder(w1.Body).Decode(&svc1) //nolint:errcheck

	// 2. active duplicate → still 409
	if w2 := create(); w2.Code != http.StatusConflict {
		t.Fatalf("active duplicate: got %d, want 409", w2.Code)
	}

	// 3. soft-delete (disable)
	rdel := httptest.NewRequest(http.MethodDelete, "/services/"+svc1.ServiceID, nil)
	rdel.Header.Set("X-Service-Key", testAdminKey)
	rdel.SetPathValue("id", svc1.ServiceID)
	wdel := httptest.NewRecorder()
	handleDeleteService(wdel, rdel)
	if wdel.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204", wdel.Code)
	}

	// 4. re-create same name → reactivated (200), same id, active
	w3 := create()
	if w3.Code != http.StatusOK {
		t.Fatalf("reactivate: got %d, want 200: %s", w3.Code, w3.Body.String())
	}
	var svc3 Service
	json.NewDecoder(w3.Body).Decode(&svc3) //nolint:errcheck
	if svc3.ServiceID != svc1.ServiceID {
		t.Errorf("reactivated id = %q, want same as original %q", svc3.ServiceID, svc1.ServiceID)
	}
	if !svc3.Active {
		t.Error("reactivated service should be active")
	}
	if atomic.LoadInt32(&notified) == 0 {
		t.Error("expected conductor to be notified on writes")
	}
}

// TestHandleUpsertServiceAccount_CreatesUsableReadAccount proves builder can grant a
// runtime-deployed service (e.g. workflows) a registry read-account that then
// authenticates — the path that lets a builder-deployed service pull GET /actions.
func TestHandleUpsertServiceAccount_CreatesUsableReadAccount(t *testing.T) {
	requireDB(t)

	name := "wf-" + uuid.New().String()
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM registry_service_accounts WHERE name = ?`, name) //nolint:errcheck
	})

	body := bytes.NewBufferString(`{"name":"` + name + `","key":"derived-key","role":"read"}`)
	r := httptest.NewRequest(http.MethodPost, "/service-accounts", body)
	r.Header.Set("X-Service-Key", testAdminKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleUpsertServiceAccount(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("upsert: got %d, want 204: %s", w.Code, w.Body.String())
	}

	// The new account authenticates as a read account: GET /services succeeds.
	r2 := httptest.NewRequest(http.MethodGet, "/services", nil)
	r2.Header.Set("X-Service-Key", name+":derived-key")
	w2 := httptest.NewRecorder()
	handleListServices(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("list with upserted account: got %d, want 200", w2.Code)
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
