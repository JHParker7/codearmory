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
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// testDBReady is set to true in TestMain when the registry DB is accessible.
var testDBReady bool

func TestMain(m *testing.M) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgresql://postgres:postgres@localhost:5432/registry"
	}
	if p, err := pgxpool.New(context.Background(), dbURL); err == nil {
		if _, err := p.Exec(context.Background(), createTables); err == nil {
			pool = p
			testDBReady = true
		} else {
			p.Close()
		}
	}
	code := m.Run()
	if pool != nil {
		pool.Close()
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
func insertTestService(t *testing.T, name, serviceKey string) string {
	t.Helper()
	id := uuid.New().String()
	var keyHash string
	if serviceKey != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(serviceKey), bcrypt.MinCost)
		if err != nil {
			t.Fatalf("insertTestService: bcrypt: %v", err)
		}
		keyHash = string(h)
	}
	_, err := pool.Exec(context.Background(),
		`INSERT INTO services (service_id, name, url, service_key_hash) VALUES ($1, $2, $3, $4)`,
		id, name, "http://svc:9000", keyHash)
	if err != nil {
		t.Fatalf("insertTestService: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM service_roles     WHERE service_id = $1`, id)
		pool.Exec(context.Background(), `DELETE FROM service_endpoints WHERE service_id = $1`, id)
		pool.Exec(context.Background(), `DELETE FROM services          WHERE service_id = $1`, id)
	})
	return id
}

// ── handleListServices ────────────────────────────────────────────────────────

func TestHandleListServices_Success(t *testing.T) {
	requireDB(t)
	t.Setenv("READ_KEY", "rk")
	t.Setenv("ADMIN_KEY", "ak")

	r := httptest.NewRequest(http.MethodGet, "/services", nil)
	r.Header.Set("Authorization", "Bearer rk")
	w := httptest.NewRecorder()
	handleListServices(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	// Response must be a valid JSON array.
	var result []any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// ── handleCreateService ───────────────────────────────────────────────────────

func TestHandleCreateService_Success(t *testing.T) {
	requireDB(t)
	t.Setenv("ADMIN_KEY", "ak")

	name := uuid.New().String()
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM services WHERE name = $1`, name)
	})

	body := bytes.NewBufferString(`{"name":"` + name + `","url":"http://newsvc:9000","description":"test svc"}`)
	r := httptest.NewRequest(http.MethodPost, "/services", body)
	r.Header.Set("Authorization", "Bearer ak")
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
	t.Setenv("ADMIN_KEY", "ak")

	name := uuid.New().String()
	insertTestService(t, name, "")

	body := bytes.NewBufferString(`{"name":"` + name + `","url":"http://dup:9000"}`)
	r := httptest.NewRequest(http.MethodPost, "/services", body)
	r.Header.Set("Authorization", "Bearer ak")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCreateService(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", w.Code)
	}
}

func TestHandleCreateService_MissingURL(t *testing.T) {
	requireDB(t)
	t.Setenv("ADMIN_KEY", "ak")

	body := bytes.NewBufferString(`{"name":"somesvc"}`)
	r := httptest.NewRequest(http.MethodPost, "/services", body)
	r.Header.Set("Authorization", "Bearer ak")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCreateService(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

// ── handleDeleteService ───────────────────────────────────────────────────────

func TestHandleDeleteService_Success(t *testing.T) {
	requireDB(t)
	t.Setenv("ADMIN_KEY", "ak")

	name := uuid.New().String()
	id := insertTestService(t, name, "")

	r := httptest.NewRequest(http.MethodDelete, "/services/"+id, nil)
	r.SetPathValue("id", id)
	r.Header.Set("Authorization", "Bearer ak")
	w := httptest.NewRecorder()
	handleDeleteService(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", w.Code, w.Body.String())
	}
}

func TestHandleDeleteService_NotFound(t *testing.T) {
	requireDB(t)
	t.Setenv("ADMIN_KEY", "ak")

	r := httptest.NewRequest(http.MethodDelete, "/services/no-such-id", nil)
	r.SetPathValue("id", "no-such-id")
	r.Header.Set("Authorization", "Bearer ak")
	w := httptest.NewRecorder()
	handleDeleteService(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

// ── handleServiceSelfRegister ─────────────────────────────────────────────────

func TestHandleServiceSelfRegister_Success(t *testing.T) {
	requireDB(t)

	name := uuid.New().String()
	serviceKey := "key-" + uuid.New().String()
	insertTestService(t, name, serviceKey)

	payload, _ := json.Marshal(map[string]any{
		"name":        name,
		"service_key": serviceKey,
		"url":         "http://updated:9001",
		"description": "registered",
		"roles": []map[string]string{
			{"name": "admin", "description": "Admin"},
		},
		"endpoints": []map[string]any{
			{"method": "GET", "path": "/foo", "action": "read", "resource": "foo"},
		},
	})
	r := httptest.NewRequest(http.MethodPost, "/services/register", bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleServiceSelfRegister(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", w.Code, w.Body.String())
	}

	// Verify the URL and endpoint were persisted.
	var gotURL string
	pool.QueryRow(context.Background(), `SELECT url FROM services WHERE name = $1`, name).Scan(&gotURL)
	if gotURL != "http://updated:9001" {
		t.Errorf("url = %q, want %q", gotURL, "http://updated:9001")
	}
	var epCount int
	pool.QueryRow(context.Background(), `SELECT count(*) FROM service_endpoints WHERE service_id = (SELECT service_id FROM services WHERE name = $1)`, name).Scan(&epCount)
	if epCount != 1 {
		t.Errorf("endpoint count = %d, want 1", epCount)
	}
}

func TestHandleServiceSelfRegister_WrongKey(t *testing.T) {
	requireDB(t)

	name := uuid.New().String()
	insertTestService(t, name, "correct-key")

	payload, _ := json.Marshal(map[string]string{
		"name":        name,
		"service_key": "wrong-key",
	})
	r := httptest.NewRequest(http.MethodPost, "/services/register", bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleServiceSelfRegister(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleServiceSelfRegister_UnknownService(t *testing.T) {
	requireDB(t)

	payload, _ := json.Marshal(map[string]string{
		"name":        "no-such-service-" + uuid.New().String(),
		"service_key": "anykey",
	})
	r := httptest.NewRequest(http.MethodPost, "/services/register", bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleServiceSelfRegister(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}
