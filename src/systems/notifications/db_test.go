package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// withTestDB swaps in a fresh in-memory SQLite database for the test. connect()/
// connectRead() both fall back to gormDB when gormDBRead is nil, so DB-backed code
// runs against this isolated database. The original handles are restored on
// cleanup.
func withTestDB(t *testing.T) {
	t.Helper()
	oldDB, oldRead := gormDB, gormDBRead
	t.Cleanup(func() { gormDB, gormDBRead = oldDB, oldRead })
	conn, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := conn.AutoMigrate(&Channel{}, &Notification{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	gormDB, gormDBRead = conn, nil
}

func addNotif(t *testing.T, n Notification) Notification {
	t.Helper()
	now := time.Now().UTC()
	if n.CreatedAt.IsZero() {
		n.CreatedAt = now
	}
	n.UpdatedAt = now
	if err := n.Add(context.Background()); err != nil {
		t.Fatalf("add notification: %v", err)
	}
	return n
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 30 * time.Second}, // shift clamped to 0
		{1, 30 * time.Second}, // 30s << 0
		{2, 60 * time.Second},
		{3, 120 * time.Second},
		{4, 240 * time.Second},
		{5, 480 * time.Second},
		{6, 15 * time.Minute}, // 30s<<5 = 16m, capped at 15m
		{20, 15 * time.Minute},
	}
	for _, c := range cases {
		if got := retryDelay(c.attempts); got != c.want {
			t.Errorf("retryDelay(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

// TestClaimRetryable verifies the durable-queue claim only picks up rows that are
// genuinely due: pending/failed, attempts < maxAttempts, and past their backoff
// (next_attempt_at <= now). It also confirms the claim increments attempts.
func TestClaimRetryable(t *testing.T) {
	withTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mk := func(status string, attempts int, next time.Time) string {
		n := addNotif(t, Notification{
			NotificationID: uuid.New().String(),
			ChannelID:      "c", Body: "b",
			Status: status, Attempts: attempts, NextAttemptAt: next,
		})
		return n.NotificationID
	}
	duePending := mk(StatusPending, 0, now.Add(-time.Minute))       // claimable
	dueFailed := mk(StatusFailed, 2, now.Add(-time.Second))         // claimable
	backoffFailed := mk(StatusFailed, 1, now.Add(time.Hour))        // NOT — backoff not elapsed
	exhausted := mk(StatusFailed, maxAttempts, now.Add(-time.Hour)) // NOT — attempts == max
	sent := mk(StatusSent, 0, now.Add(-time.Hour))                  // NOT — already sent

	claimed, err := claimRetryable(ctx, 50)
	if err != nil {
		t.Fatalf("claimRetryable: %v", err)
	}
	got := map[string]int{}
	for _, n := range claimed {
		got[n.NotificationID] = n.Attempts
	}

	if _, ok := got[duePending]; !ok {
		t.Error("due pending notification should be claimed")
	}
	if _, ok := got[dueFailed]; !ok {
		t.Error("due failed notification (attempts<max) should be claimed")
	}
	if _, ok := got[backoffFailed]; ok {
		t.Error("failed notification still within backoff (future next_attempt_at) must NOT be claimed")
	}
	if _, ok := got[exhausted]; ok {
		t.Error("exhausted notification (attempts==maxAttempts) must NOT be claimed")
	}
	if _, ok := got[sent]; ok {
		t.Error("already-sent notification must NOT be claimed")
	}
	if got[duePending] != 1 {
		t.Errorf("claim should increment attempts: duePending attempts = %d, want 1", got[duePending])
	}
}

// TestClaimRetryable_PicksUpNullNextAttempt guards the migration-safety fix: a row
// whose next_attempt_at column is NULL (a row that predates the column, added by
// ALTER TABLE on Postgres) must still be claimed, not stranded by `NULL <= now`.
func TestClaimRetryable_PicksUpNullNextAttempt(t *testing.T) {
	withTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := uuid.New().String()
	if err := gormDB.Exec(
		"INSERT INTO notifications (notification_id, channel_id, status, attempts, next_attempt_at, created_at, updated_at) VALUES (?, 'c', ?, 0, NULL, ?, ?)",
		id, StatusPending, now, now,
	).Error; err != nil {
		t.Fatalf("insert null row: %v", err)
	}
	claimed, err := claimRetryable(ctx, 50)
	if err != nil {
		t.Fatalf("claimRetryable: %v", err)
	}
	found := false
	for _, n := range claimed {
		if n.NotificationID == id {
			found = true
		}
	}
	if !found {
		t.Error("a pending row with NULL next_attempt_at must be claimed (schema-upgrade safety)")
	}
}

func TestPagination(t *testing.T) {
	req := func(q string) (int, int) {
		r := httptest.NewRequest("GET", "/x?"+q, nil)
		return pagination(r)
	}
	if l, o := req(""); l != listDefaultLimit || o != 0 {
		t.Errorf("default: got limit=%d offset=%d, want %d/0", l, o, listDefaultLimit)
	}
	if l, o := req("limit=10&offset=5"); l != 10 || o != 5 {
		t.Errorf("explicit: got limit=%d offset=%d, want 10/5", l, o)
	}
	if l, _ := req("limit=99999"); l != listMaxLimit {
		t.Errorf("over-cap limit should clamp to %d, got %d", listMaxLimit, l)
	}
	if l, o := req("limit=abc&offset=-3"); l != listDefaultLimit || o != 0 {
		t.Errorf("invalid values should fall back to defaults, got limit=%d offset=%d", l, o)
	}
}
