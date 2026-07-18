package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// withBrokenDB swaps the gorm singletons for a closed connection so every query
// errors, exercising the handlers' DB-failure (500) branches. Auth for these
// handlers is stubbed (gatekeeper httptest / HMAC), so it succeeds and control
// reaches the DB call. Restores the real singletons on cleanup.
func withBrokenDB(t *testing.T) {
	t.Helper()
	broken, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open broken db: %v", err)
	}
	if sqldb, err := broken.DB(); err == nil {
		sqldb.Close() // every subsequent query → "sql: database is closed"
	}
	dbInitMu.Lock()
	origW, origR := gormDB, gormDBRead
	gormDB, gormDBRead = broken, broken
	dbInitMu.Unlock()
	t.Cleanup(func() {
		dbInitMu.Lock()
		gormDB, gormDBRead = origW, origR
		dbInitMu.Unlock()
	})
}

func TestHandleCreateOutpost_DBError(t *testing.T) {
	stubGatekeeper(t, "u1", "o1", true)
	withBrokenDB(t)
	body := jsonReader(t, map[string]any{"name": "n", "modules": []string{"chaos"}})
	r := httptest.NewRequest(http.MethodPost, "/outposts", body)
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleCreateOutpost(w, r)
	if w.Code < 500 {
		t.Fatalf("got %d, want 5xx on DB failure", w.Code)
	}
}

func TestHandleListOutposts_DBError(t *testing.T) {
	stubGatekeeper(t, "u1", "o1", true)
	withBrokenDB(t)
	w := httptest.NewRecorder()
	handleListOutposts(w, bearerReq(http.MethodGet, "/outposts", nil))
	if w.Code < 500 {
		t.Fatalf("got %d, want 5xx", w.Code)
	}
}

func TestHandleGetOutpost_DBError(t *testing.T) {
	stubGatekeeper(t, "u1", "o1", true)
	withBrokenDB(t)
	id := uuid.New().String()
	r := bearerReq(http.MethodGet, "/outposts/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleGetOutpost(w, r)
	if w.Code < 500 {
		t.Fatalf("got %d, want 5xx", w.Code)
	}
}

func TestHandleDeleteOutpost_DBError(t *testing.T) {
	stubGatekeeper(t, "u1", "o1", true)
	withBrokenDB(t)
	id := uuid.New().String()
	r := bearerReq(http.MethodDelete, "/outposts/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeleteOutpost(w, r)
	if w.Code < 500 {
		t.Fatalf("got %d, want 5xx", w.Code)
	}
}

func TestHandleEnqueueCommand_DBError(t *testing.T) {
	prev := outpostInternalKey
	outpostInternalKey = "enq-fault-key"
	t.Cleanup(func() { outpostInternalKey = prev })
	withBrokenDB(t)
	req := enqueueCommandRequest{
		OutpostID: uuid.New().String(), Integration: "chaos", Type: "x",
		OrgID: "o", UserID: "u", Payload: map[string]any{},
	}
	w := httptest.NewRecorder()
	handleEnqueueCommand(w, internalCmdReq(t, req))
	if w.Code < 500 {
		t.Fatalf("got %d, want 5xx on outpost lookup failure", w.Code)
	}
}

// A couple of edge-case branches that aren't DB-failure paths.
func TestHandleCreateOutpost_InvalidJSON(t *testing.T) {
	stubGatekeeper(t, "u1", "o1", true)
	r := httptest.NewRequest(http.MethodPost, "/outposts", bytesReader([]byte("{bad")))
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleCreateOutpost(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleEnqueueCommand_BadJSON(t *testing.T) {
	prev := outpostInternalKey
	outpostInternalKey = "enq-badjson-key"
	t.Cleanup(func() { outpostInternalKey = prev })
	// Sign a non-JSON body so HMAC passes but decode fails.
	raw := []byte("{not json")
	tok, ts := signInternal("command", raw)
	r := httptest.NewRequest(http.MethodPost, "/internal/commands", bytesReader(raw))
	r.Header.Set("X-Internal-Token", tok)
	r.Header.Set("X-Internal-Timestamp", ts)
	w := httptest.NewRecorder()
	handleEnqueueCommand(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

// Per-outpost-key handlers reject unauthenticated requests (covers the !ok branch).
func TestPerOutpostHandlers_Unauthenticated(t *testing.T) {
	for _, h := range []http.HandlerFunc{handleHeartbeat, handleAckCommand, handleCommands} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/outpost/x", nil)
		r.SetPathValue("id", uuid.New().String())
		h(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("unauth handler got %d, want 401", w.Code)
		}
	}
}

func TestSmallHelpers(t *testing.T) {
	a, b := randToken(16), randToken(16)
	if a == "" || a == b {
		t.Error("randToken should be non-empty and random")
	}
	if envOrDefault("OG_NOPE_X", "d") != "d" {
		t.Error("envOrDefault fallback")
	}
	t.Setenv("OG_SET_X", "v")
	if envOrDefault("OG_SET_X", "d") != "v" || secret("OG_SET_X") != "v" {
		t.Error("env/secret read")
	}
	if initHTTPClient() == nil {
		t.Error("initHTTPClient nil")
	}
}

func TestClaimCommands_ReturnsAndAck(t *testing.T) {
	requireDB(t)
	o, _ := seedOutpost(t, "u-claim", "", "chaos")
	cmdID := uuid.New().String()
	if err := enqueueCommandDB(context.Background(), OutpostCommand{
		ID: cmdID, OutpostID: o.OutpostID, Integration: "chaos", Type: "t", Status: CmdPending, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	cmds, err := claimCommands(context.Background(), o.OutpostID, 16)
	if err != nil || len(cmds) != 1 || cmds[0].ID != cmdID {
		t.Fatalf("claim = %+v err=%v", cmds, err)
	}
	if err := ackCommand(context.Background(), o.OutpostID, cmdID, "done", ""); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// The ack records the terminal status a poller (a CI step) gates on.
	got, err := getCommandByID(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("getCommandByID: %v", err)
	}
	if got.Status != CmdDone {
		t.Fatalf("status = %q, want %q", got.Status, CmdDone)
	}
}

var _ = json.Marshal
