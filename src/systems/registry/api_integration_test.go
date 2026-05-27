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
)

// testDBReady is set to true in TestMain when the registry DB is accessible.
var testDBReady bool

func TestMain(m *testing.M) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgresql://postgres:postgres@localhost:5432/registry"
	}
	if p, err := pgxpool.New(context.Background(), dbURL); err == nil {
		ctx := context.Background()
		if _, err := p.Exec(ctx, createTables); err == nil {
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
func insertTestService(t *testing.T, name string) string {
	t.Helper()
	id := uuid.New().String()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO services (service_id, name, url) VALUES ($1, $2, $3)`,
		id, name, "http://svc:9000")
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
	insertTestService(t, name)

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

func TestHandleCreateService_SSRFLoopback(t *testing.T) {
	requireDB(t)
	t.Setenv("ADMIN_KEY", "ak")

	body := bytes.NewBufferString(`{"name":"loopback-svc","url":"http://127.0.0.1:9999"}`)
	r := httptest.NewRequest(http.MethodPost, "/services", body)
	r.Header.Set("Authorization", "Bearer ak")
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
	t.Setenv("ADMIN_KEY", "ak")

	name := uuid.New().String()
	id := insertTestService(t, name)

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
