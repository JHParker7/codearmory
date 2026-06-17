package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

var forgeTestDBReady bool

// setupForgeTestDB is called from TestMain. It tries to connect to the forge
// database and runs AutoMigrate; it is a no-op when the database is not
// reachable so the existing unit tests continue to pass without a database.
// We bypass connect()'s os.Exit by opening GORM directly and only setting the
// singleton on success.
func setupForgeTestDB() {
	dsn := secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/forge")
	conn, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		return
	}
	gormDBMu.Lock()
	gormDB = conn
	gormDBMu.Unlock()

	if err := conn.AutoMigrate(&Execution{}, &RunnerClass{}, &RuntimeBackend{}); err != nil {
		return
	}
	forgeTestDBReady = true
}

func requireForgeDB(t *testing.T) {
	t.Helper()
	if !forgeTestDBReady {
		t.Skip("forge database not available")
	}
}

// authAs points gatekeeper at a fake that authorizes requests as userID, and
// returns the bearer header the handler requires. Forge handlers derive the
// caller identity from gatekeeper's response (not X-User-ID), so every
// DB-backed handler test must go through this rather than spoofing a header.
func authAs(t *testing.T, userID string) string {
	t.Helper()
	fakeGatekeeper(t, http.StatusOK, fmt.Sprintf(`{"authorized":true,"user_id":%q}`, userID))
	return "Bearer test-token"
}

// insertExecution inserts a bare execution row and registers cleanup.
func insertExecution(t *testing.T, execID, userID, status string) {
	t.Helper()
	if err := connect().Exec(
		`INSERT INTO executions (execution_id, user_id, image, command, env, timeout_secs, status)
		 VALUES (?, ?, 'alpine:3.19', '["echo","test"]'::jsonb, '{}'::jsonb, 30, ?)`,
		execID, userID, status).Error; err != nil {
		t.Fatalf("insertExecution: %v", err)
	}
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM executions WHERE execution_id = ?`, execID) //nolint:errcheck
	})
}

// ── handleSubmit ──────────────────────────────────────────────────────────────

func TestHandleSubmit_Success_DB(t *testing.T) {
	requireForgeDB(t)
	initAllowedImages("alpine:3.19")
	t.Cleanup(func() { initAllowedImages("") })

	userID := "user-" + uuid.New().String()
	bearer := authAs(t, userID)
	body := bytes.NewBufferString(`{"image":"alpine:3.19","command":["echo","hi"]}`)
	r := httptest.NewRequest(http.MethodPost, "/executions", body)
	r.Header.Set("Authorization", bearer)
	w := httptest.NewRecorder()
	handleSubmit(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	execID := resp["execution_id"]
	if execID == "" {
		t.Fatal("expected execution_id in response")
	}
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM executions WHERE execution_id = ?`, execID) //nolint:errcheck
	})

	// Verify the row exists in the DB with status=pending.
	var result struct {
		Status string `gorm:"column:status"`
	}
	connect().WithContext(context.Background()).Raw(
		`SELECT status FROM executions WHERE execution_id = ?`, execID).Scan(&result)
	if result.Status != "pending" {
		t.Errorf("status = %q, want %q", result.Status, "pending")
	}
}

// ── handleGet ─────────────────────────────────────────────────────────────────

func TestHandleGet_NotFound_DB(t *testing.T) {
	requireForgeDB(t)

	userID := "user-" + uuid.New().String()
	bearer := authAs(t, userID)
	r := httptest.NewRequest(http.MethodGet, "/executions/no-such-id", nil)
	r.SetPathValue("id", "no-such-id")
	r.Header.Set("Authorization", bearer)
	w := httptest.NewRecorder()
	handleGet(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestHandleGet_Found_DB(t *testing.T) {
	requireForgeDB(t)

	userID := "user-" + uuid.New().String()
	bearer := authAs(t, userID)
	execID := uuid.New().String()
	insertExecution(t, execID, userID, "pending")

	r := httptest.NewRequest(http.MethodGet, "/executions/"+execID, nil)
	r.SetPathValue("id", execID)
	r.Header.Set("Authorization", bearer)
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
	bearer := authAs(t, userID)
	r := httptest.NewRequest(http.MethodGet, "/executions", nil)
	r.Header.Set("Authorization", bearer)
	w := httptest.NewRecorder()
	handleList(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var executions []Execution
	json.NewDecoder(w.Body).Decode(&executions) //nolint:errcheck
	if len(executions) != 0 {
		t.Errorf("got %d executions, want 0", len(executions))
	}
}

func TestHandleList_Success_DB(t *testing.T) {
	requireForgeDB(t)

	userID := "user-" + uuid.New().String()
	bearer := authAs(t, userID)
	execID := uuid.New().String()
	insertExecution(t, execID, userID, "pending")

	r := httptest.NewRequest(http.MethodGet, "/executions", nil)
	r.Header.Set("Authorization", bearer)
	w := httptest.NewRecorder()
	handleList(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var executions []Execution
	json.NewDecoder(w.Body).Decode(&executions) //nolint:errcheck
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
	bearer := authAs(t, userID)
	wp := &WorkerPool{}
	r := httptest.NewRequest(http.MethodDelete, "/executions/no-such-id", nil)
	r.SetPathValue("id", "no-such-id")
	r.Header.Set("Authorization", bearer)
	w := httptest.NewRecorder()
	handleCancel(wp)(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestHandleCancel_Pending_DB(t *testing.T) {
	requireForgeDB(t)

	userID := "user-" + uuid.New().String()
	bearer := authAs(t, userID)
	execID := uuid.New().String()
	insertExecution(t, execID, userID, "pending")

	wp := &WorkerPool{}
	r := httptest.NewRequest(http.MethodDelete, "/executions/"+execID, nil)
	r.SetPathValue("id", execID)
	r.Header.Set("Authorization", bearer)
	w := httptest.NewRecorder()
	handleCancel(wp)(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", w.Code, w.Body.String())
	}
	var result struct {
		Status string `gorm:"column:status"`
	}
	connect().WithContext(context.Background()).Raw(
		`SELECT status FROM executions WHERE execution_id = ?`, execID).Scan(&result)
	if result.Status != "cancelled" {
		t.Errorf("status = %q, want %q", result.Status, "cancelled")
	}
}

func TestHandleCancel_AlreadyCompleted_DB(t *testing.T) {
	requireForgeDB(t)

	userID := "user-" + uuid.New().String()
	bearer := authAs(t, userID)
	execID := uuid.New().String()
	insertExecution(t, execID, userID, "completed")

	wp := &WorkerPool{}
	r := httptest.NewRequest(http.MethodDelete, "/executions/"+execID, nil)
	r.SetPathValue("id", execID)
	r.Header.Set("Authorization", bearer)
	w := httptest.NewRecorder()
	handleCancel(wp)(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", w.Code)
	}
}
