package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postAuditEntry sends an ingest request, optionally authenticated as svc.
func postAuditEntry(t *testing.T, body map[string]any, header string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/internal/audit-logs", bytes.NewReader(raw))
	if header != "" {
		r.Header.Set("X-Service-Key", header)
	}
	w := httptest.NewRecorder()
	handleIngestAuditLog(w, r)
	return w
}

// latestAuditFor returns the most recent entry for a resource.
func latestAuditFor(t *testing.T, resourceID string) AuditLog {
	t.Helper()
	rows, err := AuditLog{ResourceID: resourceID}.List(context.Background(), 1, 0)
	if err != nil {
		t.Fatalf("list audit logs: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("no audit entry for resource %q", resourceID)
	}
	return rows[0].(AuditLog)
}

// The trust property: the stored action carries the AUTHENTICATED service's name, so a
// service can add entries about itself and cannot forge one attributed elsewhere.
func TestIngestAuditLog_StampsTheCallingService(t *testing.T) {
	svc, key := createTestServiceAccount(t)

	w := postAuditEntry(t, map[string]any{
		"action":      "push",
		"resource_id": "repo-123",
		"actor_id":    "user-7",
		"detail":      "refs/heads/main",
	}, serviceKeyHeader(svc, key))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}

	got := latestAuditFor(t, "repo-123")
	if want := svc.ServiceName + ".push"; got.Action != want {
		t.Errorf("action = %q, want %q", got.Action, want)
	}
	if got.ActorID != "user-7" || got.ActorType != "user" {
		t.Errorf("actor = %q/%q, want user-7/user", got.ActorID, got.ActorType)
	}
	if got.Detail != "refs/heads/main" {
		t.Errorf("detail = %q", got.Detail)
	}
}

// No actor means the service acted on its own — an anonymous fetch of a public repo,
// or a scheduled sweep. The entry is attributed to the service rather than dropped.
func TestIngestAuditLog_AttributesToTheServiceWithoutAnActor(t *testing.T) {
	svc, key := createTestServiceAccount(t)

	w := postAuditEntry(t, map[string]any{"action": "fetch", "resource_id": "repo-anon"}, serviceKeyHeader(svc, key))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}

	got := latestAuditFor(t, "repo-anon")
	if got.ActorID != svc.ServiceName || got.ActorType != "service" {
		t.Errorf("actor = %q/%q, want %q/service", got.ActorID, got.ActorType, svc.ServiceName)
	}
}

func TestIngestAuditLog_RequiresServiceAuth(t *testing.T) {
	w := postAuditEntry(t, map[string]any{"action": "push", "resource_id": "repo-123"}, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — an unauthenticated caller must not be able to write history", w.Code)
	}
}

func TestIngestAuditLog_RejectsBadInput(t *testing.T) {
	svc, key := createTestServiceAccount(t)
	header := serviceKeyHeader(svc, key)

	cases := []struct {
		name string
		body map[string]any
		why  string
	}{
		{"no action", map[string]any{"resource_id": "r1"}, "an entry with no action says nothing"},
		{"no resource", map[string]any{"action": "push"}, "an entry about nothing is not worth storing"},
		{"action with a space", map[string]any{"action": "did a push", "resource_id": "r1"}, "the action is a filterable token, not prose"},
		{"action with a slash", map[string]any{"action": "role/update", "resource_id": "r1"}, "no separator that could imitate another namespace"},
		{"uppercase action", map[string]any{"action": "Push", "resource_id": "r1"}, "one spelling per action, or filtering misses entries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := postAuditEntry(t, tc.body, header); w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 — %s", w.Code, tc.why)
			}
		})
	}
}

// Detail is attacker-influenced (ref names, repo paths) and this table is append-only,
// so an oversized field is truncated rather than rejected — the entry still lands.
func TestIngestAuditLog_TruncatesDetail(t *testing.T) {
	svc, key := createTestServiceAccount(t)

	w := postAuditEntry(t, map[string]any{
		"action":      "push",
		"resource_id": "repo-long",
		"detail":      strings.Repeat("x", maxAuditDetailLen*2),
	}, serviceKeyHeader(svc, key))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if got := latestAuditFor(t, "repo-long"); len(got.Detail) != maxAuditDetailLen {
		t.Errorf("detail length = %d, want %d", len(got.Detail), maxAuditDetailLen)
	}
}
