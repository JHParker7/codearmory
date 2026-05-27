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

var forgeTestDBReady bool

// setupForgeTestDB is called from TestMain. It tries to connect to the forge
// database and creates the schema; it is a no-op when the database is not
// reachable so the existing unit tests continue to pass without a database.
func setupForgeTestDB() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgresql://postgres:postgres@localhost:5432/forge"
	}
	p, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		return
	}
	if _, err := p.Exec(context.Background(), createTables); err != nil {
		p.Close()
		return
	}
	db = p
	forgeTestDBReady = true
}

func requireForgeDB(t *testing.T) {
	t.Helper()
	if !forgeTestDBReady {
		t.Skip("forge database not available")
	}
}

// insertExecution inserts a bare execution row and registers cleanup.
func insertExecution(t *testing.T, execID, userID, status string) {
	t.Helper()
	_, err := db.Exec(context.Background(),
		`INSERT INTO executions (execution_id, user_id, image, command, env, timeout_secs, status)
		 VALUES ($1, $2, 'alpine:3.19', '["echo","test"]'::jsonb, '{}'::jsonb, 30, $3)`,
		execID, userID, status)
	if err != nil {
		t.Fatalf("insertExecution: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(context.Background(), `DELETE FROM executions WHERE execution_id = $1`, execID)
	})
}

// ── handleSubmit ──────────────────────────────────────────────────────────────

func TestHandleSubmit_Success_DB(t *testing.T) {
	requireForgeDB(t)
	initAllowedImages("alpine:3.19")
	t.Cleanup(func() { initAllowedImages("") })

	userID := "user-" + uuid.New().String()
	pool := &WorkerPool{}
	body := bytes.NewBufferString(`{"image":"alpine:3.19","command":["echo","hi"]}`)
	r := httptest.NewRequest(http.MethodPost, "/executions", body)
	r.Header.Set("X-User-ID", userID)
	w := httptest.NewRecorder()
	handleSubmit(pool)(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	execID := resp["execution_id"]
	if execID == "" {
		t.Fatal("expected execution_id in response")
	}
	t.Cleanup(func() {
		db.Exec(context.Background(), `DELETE FROM executions WHERE execution_id = $1`, execID)
	})

	// Verify the row exists in the DB with status=pending.
	var status string
	db.QueryRow(context.Background(), `SELECT status FROM executions WHERE execution_id = $1`, execID).Scan(&status)
	if status != "pending" {
		t.Errorf("status = %q, want %q", status, "pending")
	}
}

// ── handleGet ─────────────────────────────────────────────────────────────────

func TestHandleGet_NotFound_DB(t *testing.T) {
	requireForgeDB(t)

	userID := "user-" + uuid.New().String()
	r := httptest.NewRequest(http.MethodGet, "/executions/no-such-id", nil)
	r.SetPathValue("id", "no-such-id")
	r.Header.Set("X-User-ID", userID)
	w := httptest.NewRecorder()
	handleGet(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestHandleGet_Found_DB(t *testing.T) {
	requireForgeDB(t)

	userID := "user-" + uuid.New().String()
	execID := uuid.New().String()
	insertExecution(t, execID, userID, "pending")

	r := httptest.NewRequest(http.MethodGet, "/executions/"+execID, nil)
	r.SetPathValue("id", execID)
	r.Header.Set("X-User-ID", userID)
	w := httptest.NewRecorder()
	handleGet(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	var exec Execution
	if err := json.NewDecoder(w.Body).Decode(&exec); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if exec.ExecutionID != execID {
		t.Errorf("execution_id = %q, want %q", exec.ExecutionID, execID)
	}
}

// ── handleList ────────────────────────────────────────────────────────────────

func TestHandleList_Empty_DB(t *testing.T) {
	requireForgeDB(t)

	// Use a user ID that is guaranteed to have no executions.
	userID := "user-" + uuid.New().String()
	r := httptest.NewRequest(http.MethodGet, "/executions", nil)
	r.Header.Set("X-User-ID", userID)
	w := httptest.NewRecorder()
	handleList(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var executions []Execution
	json.NewDecoder(w.Body).Decode(&executions)
	if len(executions) != 0 {
		t.Errorf("got %d executions, want 0", len(executions))
	}
}

func TestHandleList_Success_DB(t *testing.T) {
	requireForgeDB(t)

	userID := "user-" + uuid.New().String()
	execID := uuid.New().String()
	insertExecution(t, execID, userID, "pending")

	r := httptest.NewRequest(http.MethodGet, "/executions", nil)
	r.Header.Set("X-User-ID", userID)
	w := httptest.NewRecorder()
	handleList(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var executions []Execution
	json.NewDecoder(w.Body).Decode(&executions)
	if len(executions) != 1 {
		t.Errorf("got %d executions, want 1", len(executions))
	}
	if len(executions) > 0 && executions[0].ExecutionID != execID {
		t.Errorf("execution_id = %q, want %q", executions[0].ExecutionID, execID)
	}
}

// ── handleCancel ──────────────────────────────────────────────────────────────

func TestHandleCancel_NotFound_DB(t *testing.T) {
	requireForgeDB(t)

	userID := "user-" + uuid.New().String()
	wp := &WorkerPool{}
	r := httptest.NewRequest(http.MethodDelete, "/executions/no-such-id", nil)
	r.SetPathValue("id", "no-such-id")
	r.Header.Set("X-User-ID", userID)
	w := httptest.NewRecorder()
	handleCancel(wp)(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestHandleCancel_Pending_DB(t *testing.T) {
	requireForgeDB(t)

	userID := "user-" + uuid.New().String()
	execID := uuid.New().String()
	insertExecution(t, execID, userID, "pending")

	wp := &WorkerPool{}
	r := httptest.NewRequest(http.MethodDelete, "/executions/"+execID, nil)
	r.SetPathValue("id", execID)
	r.Header.Set("X-User-ID", userID)
	w := httptest.NewRecorder()
	handleCancel(wp)(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", w.Code, w.Body.String())
	}
	var status string
	db.QueryRow(context.Background(), `SELECT status FROM executions WHERE execution_id = $1`, execID).Scan(&status)
	if status != "cancelled" {
		t.Errorf("status = %q, want %q", status, "cancelled")
	}
}

func TestHandleCancel_AlreadyCompleted_DB(t *testing.T) {
	requireForgeDB(t)

	userID := "user-" + uuid.New().String()
	execID := uuid.New().String()
	insertExecution(t, execID, userID, "completed")

	wp := &WorkerPool{}
	r := httptest.NewRequest(http.MethodDelete, "/executions/"+execID, nil)
	r.SetPathValue("id", execID)
	r.Header.Set("X-User-ID", userID)
	w := httptest.NewRecorder()
	handleCancel(wp)(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", w.Code)
	}
}
