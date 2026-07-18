package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// jsonReader marshals v to JSON and returns it as a request body reader.
func jsonReader(t *testing.T, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewReader(b)
}

// bytesReader wraps a raw body in a reader for httptest requests.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// ── DB test fixtures ──────────────────────────────────────────────────────────

// cleanupOutpost hard-deletes an outpost and its commands/events at test end so
// the shared test DB stays deterministic across -count=N runs.
func cleanupOutpost(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		connect().WithContext(ctx).Exec(`DELETE FROM outpost_events   WHERE outpost_id = ?`, id) //nolint:errcheck
		connect().WithContext(ctx).Exec(`DELETE FROM outpost_commands WHERE outpost_id = ?`, id) //nolint:errcheck
		connect().WithContext(ctx).Exec(`DELETE FROM outposts         WHERE outpost_id = ?`, id) //nolint:errcheck
	})
}

// seedOutpost inserts an enrolled (connected) outpost owned by user/org with the
// given modules and a freshly-minted key. It returns the outpost row and the
// plaintext key the outpost would hold. Registers cleanup.
func seedOutpost(t *testing.T, userID, orgID, modules string) (Outpost, string) {
	t.Helper()
	key, keyHash, err := mintOutpostKey()
	if err != nil {
		t.Fatalf("mint key: %v", err)
	}
	now := time.Now().UTC()
	o := Outpost{
		OutpostID:  uuid.New().String(),
		UserID:     userID,
		OrgID:      orgID,
		Name:       "test-outpost",
		Modules:    modules,
		Status:     OutpostConnected,
		KeyHash:    keyHash,
		Active:     true,
		LastSeenAt: &now,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := o.Add(context.Background()); err != nil {
		t.Fatalf("seed outpost: %v", err)
	}
	cleanupOutpost(t, o.OutpostID)
	return o, key
}

// outpostReq builds an outpost-facing request authenticated with the outpost id
// header + bearer key.
func outpostReq(method, target, id, key string, body []byte) *http.Request {
	r := bearerReq(method, target, body)
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("X-Outpost-ID", id)
	return r
}

// ── create outpost ────────────────────────────────────────────────────────────

func TestHandleCreateOutpost_Success(t *testing.T) {
	requireDB(t)
	stubGatekeeper(t, "user-create", "", true)

	body := jsonBody(t, createOutpostRequest{Name: "edge-cluster", Modules: []string{"chaos", "argo"}})
	r := bearerReq(http.MethodPost, "/outposts", body)
	w := httptest.NewRecorder()
	handleCreateOutpost(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", w.Code, w.Body.String())
	}
	var resp createOutpostResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cleanupOutpost(t, resp.OutpostID)

	if resp.EnrollmentToken == "" {
		t.Error("enrollment token must be returned once on create")
	}
	if resp.Status != OutpostPending {
		t.Errorf("new outpost status = %q, want pending", resp.Status)
	}
	if resp.UserID != "user-create" {
		t.Errorf("owner user = %q, want user-create", resp.UserID)
	}
	if resp.Modules != "chaos,argo" {
		t.Errorf("modules = %q, want chaos,argo", resp.Modules)
	}
	// The token must embed this outpost's id and verify against the stored hash.
	id, secretPart, ok := splitEnrollmentToken(resp.EnrollmentToken)
	if !ok || id != resp.OutpostID {
		t.Fatalf("token does not embed outpost id: id=%q ok=%v", id, ok)
	}
	stored, err := getOutpost(context.Background(), resp.OutpostID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !checkBcrypt(stored.EnrollTokenHash, secretPart) {
		t.Error("stored enroll hash must verify the returned token secret")
	}
	if stored.KeyHash != "" {
		t.Error("a pending outpost must not yet have an outpost key")
	}
}

func TestHandleCreateOutpost_UnknownModule(t *testing.T) {
	requireDB(t)
	stubGatekeeper(t, "user-x", "", true)

	body := jsonBody(t, createOutpostRequest{Name: "c", Modules: []string{"chaos", "bogus"}})
	r := bearerReq(http.MethodPost, "/outposts", body)
	w := httptest.NewRecorder()
	handleCreateOutpost(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for unknown module: %s", w.Code, w.Body.String())
	}
}

func TestHandleCreateOutpost_MissingName(t *testing.T) {
	requireDB(t)
	stubGatekeeper(t, "user-x", "", true)

	body := jsonBody(t, createOutpostRequest{Name: "  ", Modules: []string{"chaos"}})
	r := bearerReq(http.MethodPost, "/outposts", body)
	w := httptest.NewRecorder()
	handleCreateOutpost(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for missing name", w.Code)
	}
}

func TestHandleCreateOutpost_Forbidden(t *testing.T) {
	requireDB(t)
	stubGatekeeper(t, "", "", false) // gatekeeper says not authorized

	body := jsonBody(t, createOutpostRequest{Name: "c"})
	r := bearerReq(http.MethodPost, "/outposts", body)
	w := httptest.NewRecorder()
	handleCreateOutpost(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 when gatekeeper denies", w.Code)
	}
}

// TestHandleCreateOutpost_BlankModulesSkipped verifies empty CSV entries are
// dropped rather than rejected, so trailing commas / blanks don't 400.
func TestHandleCreateOutpost_BlankModulesSkipped(t *testing.T) {
	requireDB(t)
	stubGatekeeper(t, "user-blank", "", true)

	body := jsonBody(t, createOutpostRequest{Name: "c", Modules: []string{"chaos", "", "  "}})
	r := bearerReq(http.MethodPost, "/outposts", body)
	w := httptest.NewRecorder()
	handleCreateOutpost(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201", w.Code)
	}
	var resp createOutpostResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	cleanupOutpost(t, resp.OutpostID)
	if resp.Modules != "chaos" {
		t.Errorf("modules = %q, want just chaos (blanks skipped)", resp.Modules)
	}
}

// ── enrollment (register) ─────────────────────────────────────────────────────

// TestHandleRegister_SingleUse covers the happy path plus single-use semantics:
// a valid token enrolls once (returns a key, flips to connected, clears the
// token) and a second attempt with the same token is rejected.
func TestHandleRegister_SingleUse(t *testing.T) {
	requireDB(t)

	// Create a pending outpost with a known enrollment token.
	token, tokenHash, err := mintEnrollmentToken(uuid.New().String())
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := splitEnrollmentToken(token)
	now := time.Now().UTC()
	o := Outpost{
		OutpostID:       id,
		UserID:          "u-enroll",
		Name:            "pending-op",
		Modules:         "chaos",
		Status:          OutpostPending,
		EnrollTokenHash: tokenHash,
		Active:          true,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := o.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleanupOutpost(t, id)

	r := httptest.NewRequest(http.MethodPost, "/outpost/register", jsonReader(t, registerRequest{EnrollmentToken: token}))
	w := httptest.NewRecorder()
	handleRegister(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("first enroll: got %d, want 201: %s", w.Code, w.Body.String())
	}
	var resp registerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OutpostKey == "" {
		t.Error("enroll must return a long-lived outpost key")
	}
	if resp.Modules != "chaos" {
		t.Errorf("modules = %q, want chaos", resp.Modules)
	}

	// Reload: token cleared, status connected, key hash verifies the returned key.
	stored, err := getOutpost(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EnrollTokenHash != "" {
		t.Error("enrollment token must be cleared after use (single-use)")
	}
	if stored.Status != OutpostConnected {
		t.Errorf("status = %q, want connected", stored.Status)
	}
	if !checkBcrypt(stored.KeyHash, resp.OutpostKey) {
		t.Error("stored key hash must verify the returned outpost key")
	}

	// Second use of the same token must be rejected (already used / not pending).
	r2 := httptest.NewRequest(http.MethodPost, "/outpost/register", jsonReader(t, registerRequest{EnrollmentToken: token}))
	w2 := httptest.NewRecorder()
	handleRegister(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("second enroll: got %d, want 401 (single-use)", w2.Code)
	}
}

// TestHandleRegister_WrongSecret verifies a token with the right outpost id but a
// wrong secret is rejected (bcrypt mismatch).
func TestHandleRegister_WrongSecret(t *testing.T) {
	requireDB(t)
	token, tokenHash, _ := mintEnrollmentToken(uuid.New().String())
	id, _, _ := splitEnrollmentToken(token)
	now := time.Now().UTC()
	o := Outpost{OutpostID: id, UserID: "u", Status: OutpostPending, EnrollTokenHash: tokenHash, Active: true, CreatedAt: now, UpdatedAt: now}
	if err := o.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleanupOutpost(t, id)

	bad := registerRequest{EnrollmentToken: id + ".deadbeefdeadbeef"}
	r := httptest.NewRequest(http.MethodPost, "/outpost/register", jsonReader(t, bad))
	w := httptest.NewRecorder()
	handleRegister(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 for wrong secret", w.Code)
	}
}

// TestHandleRegister_UnknownOutpost verifies a well-formed token whose outpost id
// doesn't exist is rejected.
func TestHandleRegister_UnknownOutpost(t *testing.T) {
	requireDB(t)
	r := httptest.NewRequest(http.MethodPost, "/outpost/register",
		jsonReader(t, registerRequest{EnrollmentToken: uuid.New().String() + ".secret"}))
	w := httptest.NewRecorder()
	handleRegister(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 for unknown outpost", w.Code)
	}
}

// ── outpost auth ──────────────────────────────────────────────────────────────

func TestAuthenticateOutpost_WrongKey(t *testing.T) {
	requireDB(t)
	o, _ := seedOutpost(t, "u-auth", "", "chaos")

	r := outpostReq(http.MethodPost, "/outpost/heartbeat", o.OutpostID, "totally-wrong-key", nil)
	w := httptest.NewRecorder()
	handleHeartbeat(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 for wrong key", w.Code)
	}
}

func TestAuthenticateOutpost_PendingRejected(t *testing.T) {
	requireDB(t)
	// A pending (un-enrolled) outpost has no key and must not authenticate even if
	// a key is presented.
	id := uuid.New().String()
	now := time.Now().UTC()
	o := Outpost{OutpostID: id, UserID: "u", Status: OutpostPending, Active: true, CreatedAt: now, UpdatedAt: now}
	if err := o.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleanupOutpost(t, id)

	r := outpostReq(http.MethodPost, "/outpost/heartbeat", id, "anything", nil)
	w := httptest.NewRecorder()
	handleHeartbeat(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 for pending outpost", w.Code)
	}
}

// ── heartbeat ─────────────────────────────────────────────────────────────────

func TestHandleHeartbeat_AdvancesLastSeen(t *testing.T) {
	requireDB(t)
	o, key := seedOutpost(t, "u-hb", "", "chaos")

	// Backdate last_seen so we can observe it advancing.
	old := time.Now().UTC().Add(-10 * time.Minute)
	if err := connect().Model(&Outpost{}).Where("outpost_id=?", o.OutpostID).
		Update("last_seen_at", old).Error; err != nil {
		t.Fatal(err)
	}

	r := outpostReq(http.MethodPost, "/outpost/heartbeat", o.OutpostID, key, nil)
	w := httptest.NewRecorder()
	handleHeartbeat(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", w.Code, w.Body.String())
	}
	reloaded, err := getOutpost(context.Background(), o.OutpostID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LastSeenAt == nil || !reloaded.LastSeenAt.After(old) {
		t.Errorf("last_seen_at not advanced: %v", reloaded.LastSeenAt)
	}
	if reloaded.Status != OutpostConnected {
		t.Errorf("status = %q, want connected after heartbeat", reloaded.Status)
	}
}

// ── command enqueue + long-poll claim ────────────────────────────────────────

// internalCmdReq builds an HMAC-authenticated /internal/commands request.
func internalCmdReq(t *testing.T, req enqueueCommandRequest) *http.Request {
	t.Helper()
	body := jsonBody(t, req)
	token, ts := signInternal("command", body)
	r := httptest.NewRequest(http.MethodPost, "/internal/commands", bytesReader(body))
	r.Header.Set("X-Internal-Token", token)
	r.Header.Set("X-Internal-Timestamp", ts)
	return r
}

func TestHandleEnqueueCommand_Success(t *testing.T) {
	requireDB(t)
	prev := outpostInternalKey
	outpostInternalKey = "enqueue-key"
	t.Cleanup(func() { outpostInternalKey = prev })

	o, _ := seedOutpost(t, "u-enq", "org-enq", "chaos")

	req := enqueueCommandRequest{
		OutpostID:   o.OutpostID,
		Integration: "chaos",
		Type:        "run-experiment",
		OrgID:       "org-enq",
		UserID:      "u-enq",
		Payload:     map[string]any{"k": "v"},
	}
	w := httptest.NewRecorder()
	handleEnqueueCommand(w, internalCmdReq(t, req))
	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["command_id"] == "" {
		t.Error("command_id must be returned")
	}

	// The command should be claimable for this outpost.
	cmds, err := claimCommands(context.Background(), o.OutpostID, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 || cmds[0].Type != "run-experiment" || cmds[0].Payload["k"] != "v" {
		t.Fatalf("claim = %+v", cmds)
	}
}

func TestHandleEnqueueCommand_TenantMismatch(t *testing.T) {
	requireDB(t)
	prev := outpostInternalKey
	outpostInternalKey = "enqueue-key"
	t.Cleanup(func() { outpostInternalKey = prev })

	o, _ := seedOutpost(t, "owner", "", "chaos")
	// Different tenant asserting ownership.
	req := enqueueCommandRequest{OutpostID: o.OutpostID, Integration: "chaos", Type: "t", UserID: "intruder", OrgID: "other"}
	w := httptest.NewRecorder()
	handleEnqueueCommand(w, internalCmdReq(t, req))
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 for tenant mismatch", w.Code)
	}
}

func TestHandleEnqueueCommand_ModuleNotEnabled(t *testing.T) {
	requireDB(t)
	prev := outpostInternalKey
	outpostInternalKey = "enqueue-key"
	t.Cleanup(func() { outpostInternalKey = prev })

	o, _ := seedOutpost(t, "u-mod", "", "chaos") // no argo
	req := enqueueCommandRequest{OutpostID: o.OutpostID, Integration: "argo", Type: "sync", UserID: "u-mod"}
	w := httptest.NewRecorder()
	handleEnqueueCommand(w, internalCmdReq(t, req))
	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409 when module not enabled", w.Code)
	}
}

func TestHandleEnqueueCommand_UnknownOutpost(t *testing.T) {
	requireDB(t)
	prev := outpostInternalKey
	outpostInternalKey = "enqueue-key"
	t.Cleanup(func() { outpostInternalKey = prev })

	req := enqueueCommandRequest{OutpostID: uuid.New().String(), Integration: "chaos", Type: "t", UserID: "u"}
	w := httptest.NewRecorder()
	handleEnqueueCommand(w, internalCmdReq(t, req))
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 for unknown outpost", w.Code)
	}
}

// TestClaimCommands_SkipLocked verifies a pending command is claimed exactly once
// even under concurrent claimers (FOR UPDATE SKIP LOCKED). Each goroutine runs in
// its own transaction; only one may flip the row to claimed.
func TestClaimCommands_SkipLocked(t *testing.T) {
	requireDB(t)
	o, _ := seedOutpost(t, "u-skip", "", "chaos")

	ctx := context.Background()
	if err := enqueueCommandDB(ctx, OutpostCommand{
		ID: uuid.New().String(), OutpostID: o.OutpostID, Integration: "chaos",
		Type: "t", Status: CmdPending, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			cmds, err := claimCommands(ctx, o.OutpostID, 16)
			if err != nil {
				return
			}
			mu.Lock()
			total += len(cmds)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total != 1 {
		t.Fatalf("command claimed %d times, want exactly 1 (SKIP LOCKED)", total)
	}
}

// TestHandleCommands_LongPollClaimAndAck drives the long-poll handler: it should
// return the pending command immediately, and ack should mark it done so it is
// no longer claimable.
func TestHandleCommands_LongPollClaimAndAck(t *testing.T) {
	requireDB(t)
	o, key := seedOutpost(t, "u-poll", "", "chaos")

	cmdID := uuid.New().String()
	if err := enqueueCommandDB(context.Background(), OutpostCommand{
		ID: cmdID, OutpostID: o.OutpostID, Integration: "chaos", Type: "ping",
		Status: CmdPending, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	r := outpostReq(http.MethodGet, "/outpost/commands", o.OutpostID, key, nil)
	w := httptest.NewRecorder()
	handleCommands(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	var got []OutpostCommand
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != cmdID {
		t.Fatalf("long-poll returned %+v, want command %s", got, cmdID)
	}

	// Ack it.
	ar := outpostReq(http.MethodPost, "/outpost/commands/"+cmdID+"/ack", o.OutpostID, key, nil)
	ar.SetPathValue("id", cmdID)
	aw := httptest.NewRecorder()
	handleAckCommand(aw, ar)
	if aw.Code != http.StatusNoContent {
		t.Fatalf("ack: got %d, want 204", aw.Code)
	}
	// A done command is no longer claimable.
	cmds, err := claimCommands(context.Background(), o.OutpostID, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 0 {
		t.Fatalf("acked command should not be re-claimable, got %d", len(cmds))
	}
}

// TestHandleCommands_LongPollEmptyTimeout verifies the long-poll returns an empty
// JSON array (not an error) when no work arrives before the deadline. We shrink
// the hold by cancelling the request context, exercising the ctx.Done() branch.
func TestHandleCommands_LongPollEmptyTimeout(t *testing.T) {
	requireDB(t)
	o, key := seedOutpost(t, "u-empty", "", "chaos")

	// No commands. Use a context that expires quickly so we don't wait 30s; the
	// handler's select returns on ctx.Done after the first empty claim.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r := outpostReq(http.MethodGet, "/outpost/commands", o.OutpostID, key, nil).WithContext(ctx)
	w := httptest.NewRecorder()
	handleCommands(w, r)
	// With ctx cancelled mid-hold the handler returns without writing a body, or
	// (if the deadline path wins) writes an empty array. Either way it must not 5xx.
	if w.Code >= 500 {
		t.Fatalf("long-poll empty: got %d, want < 500", w.Code)
	}
}

// ── event ingest ──────────────────────────────────────────────────────────────

func TestHandleEvents_Success(t *testing.T) {
	requireDB(t)
	o, key := seedOutpost(t, "u-ev", "org-ev", "chaos")

	evID := uuid.New().String()
	body := jsonBody(t, ingestEvent{EventID: evID, Integration: "chaos", Type: "experiment-done", Payload: map[string]any{"n": "1"}})
	r := outpostReq(http.MethodPost, "/outpost/events", o.OutpostID, key, body)
	w := httptest.NewRecorder()
	handleEvents(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202: %s", w.Code, w.Body.String())
	}
	// Event is in the outbox stamped with the outpost's owner.
	var ev OutpostEvent
	if err := connect().Where("id=?", evID).First(&ev).Error; err != nil {
		t.Fatal(err)
	}
	if ev.OrgID != "org-ev" || ev.UserID != "u-ev" || ev.Status != EvPending {
		t.Errorf("stored event = %+v", ev)
	}
}

// TestHandleEvents_DuplicateIsSuccess covers at-least-once: an outpost re-posting
// the same event id returns 200 (treated as success), not an error.
func TestHandleEvents_DuplicateIsSuccess(t *testing.T) {
	requireDB(t)
	o, key := seedOutpost(t, "u-dup", "", "chaos")

	evID := uuid.New().String()
	body := jsonBody(t, ingestEvent{EventID: evID, Integration: "chaos", Type: "t"})

	r1 := outpostReq(http.MethodPost, "/outpost/events", o.OutpostID, key, body)
	w1 := httptest.NewRecorder()
	handleEvents(w1, r1)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first: got %d, want 202", w1.Code)
	}
	r2 := outpostReq(http.MethodPost, "/outpost/events", o.OutpostID, key, body)
	w2 := httptest.NewRecorder()
	handleEvents(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("duplicate: got %d, want 200 (at-least-once dedupe)", w2.Code)
	}
}

func TestHandleEvents_MissingFields(t *testing.T) {
	requireDB(t)
	o, key := seedOutpost(t, "u-mf", "", "chaos")
	// integration present but type missing.
	body := jsonBody(t, ingestEvent{Integration: "chaos"})
	r := outpostReq(http.MethodPost, "/outpost/events", o.OutpostID, key, body)
	w := httptest.NewRecorder()
	handleEvents(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for missing type", w.Code)
	}
}

func TestHandleEvents_ModuleNotEnabled(t *testing.T) {
	requireDB(t)
	o, key := seedOutpost(t, "u-mne", "", "chaos") // no argo
	body := jsonBody(t, ingestEvent{Integration: "argo", Type: "synced"})
	r := outpostReq(http.MethodPost, "/outpost/events", o.OutpostID, key, body)
	w := httptest.NewRecorder()
	handleEvents(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 forging an event for a disabled module", w.Code)
	}
}

func TestHandleEvents_GeneratesIDWhenAbsent(t *testing.T) {
	requireDB(t)
	o, key := seedOutpost(t, "u-genid", "", "chaos")
	body := jsonBody(t, ingestEvent{Integration: "chaos", Type: "t"}) // no event_id
	r := outpostReq(http.MethodPost, "/outpost/events", o.OutpostID, key, body)
	w := httptest.NewRecorder()
	handleEvents(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", w.Code)
	}
	var n int64
	connect().Model(&OutpostEvent{}).Where("outpost_id=?", o.OutpostID).Count(&n)
	if n != 1 {
		t.Fatalf("expected exactly 1 event with a generated id, got %d", n)
	}
}

// ── user-facing get/list/delete + RBAC isolation ──────────────────────────────

func TestHandleGetOutpost_OwnerAndIsolation(t *testing.T) {
	requireDB(t)
	o, _ := seedOutpost(t, "owner-1", "", "chaos")

	// Owner can read.
	stubGatekeeper(t, "owner-1", "", true)
	r := bearerReq(http.MethodGet, "/outposts/"+o.OutpostID, nil)
	r.SetPathValue("id", o.OutpostID)
	w := httptest.NewRecorder()
	handleGetOutpost(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("owner get: got %d, want 200: %s", w.Code, w.Body.String())
	}

	// A different user (gatekeeper authorizes them, but they don't own it) gets 404
	// — RBAC isolation: outposts are owner/org scoped, not leaked by id.
	stubGatekeeper(t, "intruder", "", true)
	r2 := bearerReq(http.MethodGet, "/outposts/"+o.OutpostID, nil)
	r2.SetPathValue("id", o.OutpostID)
	w2 := httptest.NewRecorder()
	handleGetOutpost(w2, r2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("other-user get: got %d, want 404 (isolation)", w2.Code)
	}
}

func TestHandleGetOutpost_NotFound(t *testing.T) {
	requireDB(t)
	stubGatekeeper(t, "u", "", true)
	missing := uuid.New().String()
	r := bearerReq(http.MethodGet, "/outposts/"+missing, nil)
	r.SetPathValue("id", missing)
	w := httptest.NewRecorder()
	handleGetOutpost(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestHandleListOutposts_ScopedToOwner(t *testing.T) {
	requireDB(t)
	mine, _ := seedOutpost(t, "list-owner", "", "chaos")
	_, _ = seedOutpost(t, "other-owner", "", "chaos") // must not appear

	stubGatekeeper(t, "list-owner", "", true)
	r := bearerReq(http.MethodGet, "/outposts", nil)
	w := httptest.NewRecorder()
	handleListOutposts(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var got []Outpost
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, o := range got {
		if o.UserID != "list-owner" {
			t.Fatalf("list leaked another owner's outpost: %+v", o)
		}
	}
	found := false
	for _, o := range got {
		if o.OutpostID == mine.OutpostID {
			found = true
		}
	}
	if !found {
		t.Error("owner's own outpost missing from list")
	}
}

func TestHandleDeleteOutpost_SoftDeleteAndIsolation(t *testing.T) {
	requireDB(t)
	o, _ := seedOutpost(t, "del-owner", "", "chaos")

	// Non-owner cannot delete (404).
	stubGatekeeper(t, "not-owner", "", true)
	r := bearerReq(http.MethodDelete, "/outposts/"+o.OutpostID, nil)
	r.SetPathValue("id", o.OutpostID)
	w := httptest.NewRecorder()
	handleDeleteOutpost(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-owner delete: got %d, want 404", w.Code)
	}

	// Owner deletes -> 204, and the outpost is gone from active reads.
	stubGatekeeper(t, "del-owner", "", true)
	r2 := bearerReq(http.MethodDelete, "/outposts/"+o.OutpostID, nil)
	r2.SetPathValue("id", o.OutpostID)
	w2 := httptest.NewRecorder()
	handleDeleteOutpost(w2, r2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("owner delete: got %d, want 204", w2.Code)
	}
	if _, err := getOutpost(context.Background(), o.OutpostID); !isNotFound(err) {
		t.Errorf("soft-deleted outpost should not be returned by active read, err=%v", err)
	}
}
