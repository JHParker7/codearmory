package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	bpDBOnce  sync.Once
	bpDBReady bool
)

// requireBlueprintsDB connects to the blueprints database on first call and
// skips the test if the database is not reachable.
func requireBlueprintsDB(t *testing.T) {
	t.Helper()
	bpDBOnce.Do(func() {
		dbURL := os.Getenv("DATABASE_URL")
		if dbURL == "" {
			dbURL = "postgresql://postgres:postgres@localhost:5432/blueprints"
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
		db.Exec(context.Background(), `DELETE FROM states WHERE workspace = $1`, ws)
		db.Exec(context.Background(), `DELETE FROM locks  WHERE workspace = $1`, ws)
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
	db.Exec(context.Background(),
		`INSERT INTO states (workspace, data) VALUES ($1, $2)`, ws, stateData)

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

	var stored []byte
	db.QueryRow(context.Background(), `SELECT data FROM states WHERE workspace = $1`, ws).Scan(&stored)
	if !bytes.Equal(stored, []byte(stateData)) {
		t.Errorf("stored = %q, want %q", stored, stateData)
	}
}

func TestHandleUpdateState_LockedNoID_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	lockBody := `{"ID":"lock-abc"}`
	db.Exec(context.Background(),
		`INSERT INTO locks (workspace, lock_data) VALUES ($1, $2)`, ws, lockBody)

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
	db.Exec(context.Background(),
		`INSERT INTO locks (workspace, lock_data) VALUES ($1, $2)`, ws, lockBody)

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

	db.Exec(context.Background(),
		`INSERT INTO states (workspace, data) VALUES ($1, $2)`, ws, []byte(`{}`))

	w := httptest.NewRecorder()
	r := authorized(t, "DELETE", "")
	handleDeleteState(w, r, ws)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	var count int
	db.QueryRow(context.Background(), `SELECT count(*) FROM states WHERE workspace = $1`, ws).Scan(&count)
	if count != 0 {
		t.Errorf("state row count = %d, want 0 after delete", count)
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

	var lockData string
	db.QueryRow(context.Background(), `SELECT lock_data FROM locks WHERE workspace = $1`, ws).Scan(&lockData)
	if lockData == "" {
		t.Error("expected lock row in DB")
	}
}

func TestHandleLockState_Conflict_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	// Pre-insert an existing lock.
	db.Exec(context.Background(),
		`INSERT INTO locks (workspace, lock_data) VALUES ($1, $2)`, ws, `{"ID":"already-locked"}`)

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

	db.Exec(context.Background(),
		`INSERT INTO locks (workspace, lock_data) VALUES ($1, $2)`, ws, `{"ID":"lock-to-release"}`)

	w := httptest.NewRecorder()
	r := authorized(t, "UNLOCK", `{"ID":"lock-to-release"}`)
	handleUnlockState(w, r, ws)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	var count int
	db.QueryRow(context.Background(), `SELECT count(*) FROM locks WHERE workspace = $1`, ws).Scan(&count)
	if count != 0 {
		t.Errorf("lock row count = %d, want 0 after unlock", count)
	}
}

func TestHandleUnlockState_WrongID_DB(t *testing.T) {
	requireBlueprintsDB(t)
	ws, _ := wsKey(t)

	db.Exec(context.Background(),
		`INSERT INTO locks (workspace, lock_data) VALUES ($1, $2)`, ws, `{"ID":"correct-id"}`)

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
