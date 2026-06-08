package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

var (
	bpDBOnce  sync.Once
	bpDBReady bool
)

// requireBlueprintsDB connects to the blueprints database on first call and
// skips the test if the database is not reachable.
// Bypasses connect()'s os.Exit by opening GORM directly.
func requireBlueprintsDB(t *testing.T) {
	t.Helper()
	bpDBOnce.Do(func() {
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			dsn = "postgresql://postgres:postgres@localhost:5432/blueprints"
		}
		conn, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
			Logger: gormlogger.Default.LogMode(gormlogger.Silent),
		})
		if err != nil {
			return
		}
		gormDBMu.Lock()
		gormDB = conn
		gormDBMu.Unlock()

		if err := conn.AutoMigrate(&State{}, &StateLock{}, &BackendCredential{}); err != nil {
			return
		}
		bpDBReady = true
	})
	if !bpDBReady {
		t.Skip("blueprints database not available")
	}
}

// wsKey returns a test-scoped workspace key and its resource path, using the
// test name to ensure uniqueness. A cleanup is registered to delete both
// the state and lock rows.
func wsKey(t *testing.T) (string, string) {
	t.Helper()
	ws := "integtest/" + t.Name()
	res := "blueprints/states/" + ws
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM states WHERE workspace = ?`, ws) //nolint:errcheck
		connect().Exec(`DELETE FROM locks  WHERE workspace = ?`, ws) //nolint:errcheck
	})
	return ws, res
}

// authorized sets up a fake gatekeeper that returns {"authorized":true} and
// returns an http.Request with a Bearer token header.
func authorized(t *testing.T, method, body string) *http.Request {
	t.Helper()
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true}`)
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, "/", bytes.NewBufferString(body))
	} else {
		r = httptest.NewRequest(method, "/", nil)
	}
	r.Header.Set("Authorization", "Bearer testtoken")
	return r
}

// ── handleGetState ────────────────────────────────────────────────────────────

func TestHandleGetState_NotFound_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	w := httptest.NewRecorder()
	r := authorized(t, "GET", "")
	handleGetState(w, r, ws)

	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204", w.Code)
	}
}

func TestHandleGetState_Found_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	stateData := []byte(`{"version":4,"resources":[]}`)
	connect().Exec(`INSERT INTO states (workspace, data) VALUES (?, ?)`, ws, stateData) //nolint:errcheck

	w := httptest.NewRecorder()
	r := authorized(t, "GET", "")
	handleGetState(w, r, ws)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if got := w.Body.Bytes(); !bytes.Equal(got, stateData) {
		t.Errorf("body = %q, want %q", got, stateData)
	}
}

// ── handleUpdateState ─────────────────────────────────────────────────────────

func TestHandleUpdateState_Creates_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	stateData := `{"version":4,"resources":[]}`
	w := httptest.NewRecorder()
	r := authorized(t, "POST", stateData)
	handleUpdateState(w, r, ws)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	var result struct {
		Data []byte `gorm:"column:data"`
	}
	connect().Raw(`SELECT data FROM states WHERE workspace = ?`, ws).Scan(&result)
	if !bytes.Equal(result.Data, []byte(stateData)) {
		t.Errorf("stored = %q, want %q", result.Data, stateData)
	}
}

func TestHandleUpdateState_LockedNoID_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	lockBody := `{"ID":"lock-abc"}`
	connect().Exec(`INSERT INTO locks (workspace, lock_data) VALUES (?, ?)`, ws, lockBody) //nolint:errcheck

	// POST without ?ID= query parameter → 409 conflict with lock body.
	w := httptest.NewRecorder()
	r := authorized(t, "POST", `{"version":4}`)
	handleUpdateState(w, r, ws)

	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", w.Code)
	}
}

func TestHandleUpdateState_LockedMatchingID_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	lockBody := `{"ID":"lock-xyz"}`
	connect().Exec(`INSERT INTO locks (workspace, lock_data) VALUES (?, ?)`, ws, lockBody) //nolint:errcheck

	stateData := `{"version":4}`
	w := httptest.NewRecorder()
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true}`)
	r := httptest.NewRequest("POST", "/?ID=lock-xyz", bytes.NewBufferString(stateData))
	r.Header.Set("Authorization", "Bearer testtoken")
	handleUpdateState(w, r, ws)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}

// ── handleDeleteState ─────────────────────────────────────────────────────────

func TestHandleDeleteState_Success_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	connect().Exec(`INSERT INTO states (workspace, data) VALUES (?, ?)`, ws, []byte(`{}`)) //nolint:errcheck

	w := httptest.NewRecorder()
	r := authorized(t, "DELETE", "")
	handleDeleteState(w, r, ws)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	var result struct {
		Count int `gorm:"column:count"`
	}
	connect().Raw(`SELECT count(*) FROM states WHERE workspace = ?`, ws).Scan(&result)
	if result.Count != 0 {
		t.Errorf("state row count = %d, want 0 after delete", result.Count)
	}
}

// ── handleLockState ───────────────────────────────────────────────────────────

func TestHandleLockState_Success_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	w := httptest.NewRecorder()
	r := authorized(t, "LOCK", `{"ID":"lock-1","Operation":"OperationPlanWithDestroy"}`)
	handleLockState(w, r, ws)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	var result struct {
		LockData string `gorm:"column:lock_data"`
	}
	connect().Raw(`SELECT lock_data FROM locks WHERE workspace = ?`, ws).Scan(&result)
	if result.LockData == "" {
		t.Error("expected lock row in DB")
	}
}

func TestHandleLockState_Conflict_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	// Pre-insert an existing lock.
	connect().Exec(`INSERT INTO locks (workspace, lock_data) VALUES (?, ?)`, ws, `{"ID":"already-locked"}`) //nolint:errcheck

	w := httptest.NewRecorder()
	r := authorized(t, "LOCK", `{"ID":"new-lock"}`)
	handleLockState(w, r, ws)

	if w.Code != http.StatusLocked {
		t.Fatalf("got %d, want 423", w.Code)
	}
}

// ── handleUnlockState ─────────────────────────────────────────────────────────

func TestHandleUnlockState_Success_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	connect().Exec(`INSERT INTO locks (workspace, lock_data) VALUES (?, ?)`, ws, `{"ID":"lock-to-release"}`) //nolint:errcheck

	w := httptest.NewRecorder()
	r := authorized(t, "UNLOCK", `{"ID":"lock-to-release"}`)
	handleUnlockState(w, r, ws)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	var result struct {
		Count int `gorm:"column:count"`
	}
	connect().Raw(`SELECT count(*) FROM locks WHERE workspace = ?`, ws).Scan(&result)
	if result.Count != 0 {
		t.Errorf("lock row count = %d, want 0 after unlock", result.Count)
	}
}

func TestHandleUnlockState_WrongID_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	connect().Exec(`INSERT INTO locks (workspace, lock_data) VALUES (?, ?)`, ws, `{"ID":"correct-id"}`) //nolint:errcheck

	w := httptest.NewRecorder()
	r := authorized(t, "UNLOCK", `{"ID":"wrong-id"}`)
	handleUnlockState(w, r, ws)

	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", w.Code)
	}
}

func TestHandleUnlockState_NoLock_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	// Unlock when no lock exists is idempotent → 200.
	w := httptest.NewRecorder()
	r := authorized(t, "UNLOCK", `{"ID":"any"}`)
	handleUnlockState(w, r, ws)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (unlock is idempotent)", w.Code)
	}
}
