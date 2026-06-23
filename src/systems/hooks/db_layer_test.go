package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ── PipelineRule Add/Get/Update/Remove/List ───────────────────────────────────

func TestPipelineRule_AddGet(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	rule := PipelineRule{
		RuleID:     uuid.New().String(),
		Name:       "ci",
		Source:     "owner/repo-" + uuid.NewString(),
		Events:     []string{"push"},
		WorkflowID: "wf-1",
		Secret:     secretPtr("topsecret"),
		CreatedBy:  "user-1",
		OrgID:      "org-1",
		Active:     true,
		CreatedAt:  time.Now().UTC(),
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM pipeline_rules WHERE rule_id = ?`, rule.RuleID) }) //nolint:errcheck

	if err := rule.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := getRule(ctx, rule.RuleID)
	if err != nil {
		t.Fatalf("getRule: %v", err)
	}
	if got.Name != "ci" || got.WorkflowID != "wf-1" || len(got.Events) != 1 || got.Events[0] != "push" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Secret == nil || *got.Secret != "topsecret" {
		t.Fatalf("secret not persisted, got %v", got.Secret)
	}
}

func TestPipelineRule_Update(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	rule := insertRule(t, PipelineRule{Name: "old", Source: "s/" + uuid.NewString(), Events: []string{"push"}, WorkflowID: "wf-1", Secret: secretPtr("k"), CreatedBy: "u1", OrgID: "o1", Active: true})

	rule.Name = "new-name"
	rule.WorkflowID = "wf-99"
	if err := rule.Update(ctx); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := getRule(ctx, rule.RuleID)
	if err != nil {
		t.Fatalf("getRule: %v", err)
	}
	if got.Name != "new-name" || got.WorkflowID != "wf-99" {
		t.Fatalf("update not persisted: %+v", got)
	}
	if !got.UpdatedAt.After(got.CreatedAt) && !got.UpdatedAt.Equal(got.CreatedAt) {
		// UpdatedAt is set to now in Update(); just confirm it's non-zero.
		if got.UpdatedAt.IsZero() {
			t.Fatal("UpdatedAt should be set after Update")
		}
	}
}

func TestPipelineRule_RemoveSoftDeactivates(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	rule := insertRule(t, PipelineRule{Name: "r", Source: "s/" + uuid.NewString(), Events: []string{"push"}, WorkflowID: "wf-1", Secret: secretPtr("k"), CreatedBy: "u1", Active: true})

	if err := rule.Remove(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Get only returns active rows, so it should now be a not-found.
	if _, err := getRule(ctx, rule.RuleID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected ErrRecordNotFound after soft delete, got %v", err)
	}
	// But the row still exists with active=false.
	var active bool
	if err := connect().Raw(`SELECT active FROM pipeline_rules WHERE rule_id = ?`, rule.RuleID).Scan(&active).Error; err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if active {
		t.Fatal("row should be deactivated, not hard-deleted")
	}
}

func TestPipelineRule_GetNotFound(t *testing.T) {
	requireDB(t)
	if _, err := getRule(context.Background(), uuid.New().String()); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected ErrRecordNotFound, got %v", err)
	}
}

func TestListRules_ScopesByOwnerAndOrg(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	owner := "owner-" + uuid.NewString()
	org := "org-" + uuid.NewString()
	src := "repo/" + uuid.NewString()

	mine := insertRule(t, PipelineRule{Name: "mine", Source: src, Events: []string{"push"}, WorkflowID: "w", Secret: secretPtr("k"), CreatedBy: owner, Active: true})
	orgShared := insertRule(t, PipelineRule{Name: "org", Source: src, Events: []string{"push"}, WorkflowID: "w", Secret: secretPtr("k"), CreatedBy: "someone-else", OrgID: org, Active: true})
	// Another user's personal rule — must NOT appear.
	insertRule(t, PipelineRule{Name: "other", Source: src, Events: []string{"push"}, WorkflowID: "w", Secret: secretPtr("k"), CreatedBy: "stranger", Active: true})
	// Inactive rule owned by me — must NOT appear.
	insertRule(t, PipelineRule{Name: "inactive", Source: src, Events: []string{"push"}, WorkflowID: "w", Secret: secretPtr("k"), CreatedBy: owner, Active: false})

	rules, err := listRules(ctx, owner, org)
	if err != nil {
		t.Fatalf("listRules: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range rules {
		ids[r.RuleID] = true
	}
	if !ids[mine.RuleID] {
		t.Error("own rule missing from list")
	}
	if !ids[orgShared.RuleID] {
		t.Error("org-shared rule missing from list")
	}
	for _, r := range rules {
		if r.CreatedBy == "stranger" {
			t.Error("stranger's personal rule leaked into list")
		}
		if !r.Active {
			t.Error("inactive rule leaked into list")
		}
	}
}

func TestListRules_EmptyReturnsNonNil(t *testing.T) {
	requireDB(t)
	rules, err := listRules(context.Background(), "nobody-"+uuid.NewString(), "")
	if err != nil {
		t.Fatalf("listRules: %v", err)
	}
	if rules == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(rules) != 0 {
		t.Fatalf("expected 0 rules, got %d", len(rules))
	}
}

func TestGetMatchedRules_RequiresSecretAndEvent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	src := "repo/" + uuid.NewString()

	withSecret := insertRule(t, PipelineRule{Name: "good", Source: src, Events: []string{"push", "tag"}, WorkflowID: "w", Secret: secretPtr("k"), CreatedBy: "u", Active: true})
	// Empty-string secret — must be excluded so it can never fire unauthenticated.
	insertRule(t, PipelineRule{Name: "nosecret", Source: src, Events: []string{"push"}, WorkflowID: "w", Secret: secretPtr(""), CreatedBy: "u", Active: true})
	// Different event — must not match a "push" query.
	insertRule(t, PipelineRule{Name: "wrongevent", Source: src, Events: []string{"deploy"}, WorkflowID: "w", Secret: secretPtr("k"), CreatedBy: "u", Active: true})
	// Inactive — must not match.
	insertRule(t, PipelineRule{Name: "inactive", Source: src, Events: []string{"push"}, WorkflowID: "w", Secret: secretPtr("k"), CreatedBy: "u", Active: false})

	matched, err := getMatchedRules(ctx, src, "push")
	if err != nil {
		t.Fatalf("getMatchedRules: %v", err)
	}
	if len(matched) != 1 {
		t.Fatalf("expected exactly 1 matched rule, got %d (%+v)", len(matched), matched)
	}
	if matched[0].RuleID != withSecret.RuleID {
		t.Fatalf("wrong rule matched: %s", matched[0].RuleID)
	}
}

// ── HookEvent Add/Get/List + listEvents/getTriggersForEvent ───────────────────

func TestHookEvent_AddGet(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	id := uuid.New().String()
	cleanupEvent(t, id)
	ev := HookEvent{EventID: id, Source: "src", EventType: "push", Payload: map[string]string{"a": "b"}, Status: "received", CreatedAt: time.Now().UTC()}
	if err := ev.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := getEvent(ctx, id)
	if err != nil {
		t.Fatalf("getEvent: %v", err)
	}
	if got.EventType != "push" || got.Payload["a"] != "b" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestHookEvent_GetNotFound(t *testing.T) {
	requireDB(t)
	if _, err := getEvent(context.Background(), uuid.New().String()); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected ErrRecordNotFound, got %v", err)
	}
}

func TestHookEvent_RemoveIsNoop(t *testing.T) {
	requireDB(t)
	if err := (HookEvent{EventID: "x"}).Remove(context.Background()); err != nil {
		t.Fatalf("HookEvent.Remove should be a no-op, got %v", err)
	}
}

func TestUpdateEventStatus(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	id := uuid.New().String()
	cleanupEvent(t, id)
	ev := HookEvent{EventID: id, Source: "src", EventType: "push", Status: "received", CreatedAt: time.Now().UTC()}
	if err := ev.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}
	updateEventStatus(id, 3, "triggered")
	got, err := getEvent(ctx, id)
	if err != nil {
		t.Fatalf("getEvent: %v", err)
	}
	if got.RulesMatched != 3 || got.Status != "triggered" {
		t.Fatalf("status not updated: matched=%d status=%q", got.RulesMatched, got.Status)
	}
}

func TestListEvents_VisibleViaOwnedRules(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	owner := "owner-" + uuid.NewString()
	src := "repo/" + uuid.NewString()

	rule := insertRule(t, PipelineRule{Name: "r", Source: src, Events: []string{"push"}, WorkflowID: "w", Secret: secretPtr("k"), CreatedBy: owner, Active: true})

	eventID := uuid.New().String()
	cleanupEvent(t, eventID)
	if err := (HookEvent{EventID: eventID, Source: src, EventType: "push", Status: "triggered", CreatedAt: time.Now().UTC()}).Add(ctx); err != nil {
		t.Fatalf("add event: %v", err)
	}
	trig := HookTrigger{TriggerID: uuid.New().String(), EventID: eventID, RuleID: rule.RuleID, WorkflowID: "w", Status: "triggered", CreatedAt: time.Now().UTC()}
	if err := trig.Add(ctx); err != nil {
		t.Fatalf("add trigger: %v", err)
	}

	// Owner sees it.
	events, err := listEvents(ctx, owner, "", "")
	if err != nil {
		t.Fatalf("listEvents owner: %v", err)
	}
	if !containsEvent(events, eventID) {
		t.Fatal("owner should see their event")
	}

	// Source filter that matches.
	events, err = listEvents(ctx, owner, "", src)
	if err != nil {
		t.Fatalf("listEvents source: %v", err)
	}
	if !containsEvent(events, eventID) {
		t.Fatal("owner should see event via source filter")
	}

	// A stranger sees nothing.
	events, err = listEvents(ctx, "stranger-"+uuid.NewString(), "", "")
	if err != nil {
		t.Fatalf("listEvents stranger: %v", err)
	}
	if containsEvent(events, eventID) {
		t.Fatal("stranger should not see another user's event")
	}
}

func containsEvent(events []HookEvent, id string) bool {
	for _, e := range events {
		if e.EventID == id {
			return true
		}
	}
	return false
}

func TestGetTriggersForEvent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	eventID := uuid.New().String()
	cleanupEvent(t, eventID)
	for i := 0; i < 2; i++ {
		trig := HookTrigger{TriggerID: uuid.New().String(), EventID: eventID, RuleID: "r", WorkflowID: "w", Status: "triggered", CreatedAt: time.Now().UTC()}
		if err := trig.Add(ctx); err != nil {
			t.Fatalf("add trigger: %v", err)
		}
	}
	triggers, err := getTriggersForEvent(ctx, eventID)
	if err != nil {
		t.Fatalf("getTriggersForEvent: %v", err)
	}
	if len(triggers) != 2 {
		t.Fatalf("expected 2 triggers, got %d", len(triggers))
	}
	// Empty case returns non-nil.
	empty, err := getTriggersForEvent(ctx, uuid.New().String())
	if err != nil {
		t.Fatalf("getTriggersForEvent empty: %v", err)
	}
	if empty == nil {
		t.Fatal("expected non-nil empty slice")
	}
}

func TestCountEventAccessAndRulesForRepo(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	owner := "owner-" + uuid.NewString()
	src := "repo/" + uuid.NewString()
	rule := insertRule(t, PipelineRule{Name: "r", Source: src, Events: []string{"push"}, WorkflowID: "w", Secret: secretPtr("k"), CreatedBy: owner, Active: true})

	eventID := uuid.New().String()
	cleanupEvent(t, eventID)
	if err := (HookEvent{EventID: eventID, Source: src, EventType: "push", Status: "triggered", CreatedAt: time.Now().UTC()}).Add(ctx); err != nil {
		t.Fatalf("add event: %v", err)
	}
	if err := (HookTrigger{TriggerID: uuid.New().String(), EventID: eventID, RuleID: rule.RuleID, WorkflowID: "w", Status: "triggered", CreatedAt: time.Now().UTC()}).Add(ctx); err != nil {
		t.Fatalf("add trigger: %v", err)
	}

	n, err := countEventAccess(ctx, eventID, owner, "")
	if err != nil {
		t.Fatalf("countEventAccess: %v", err)
	}
	if n != 1 {
		t.Fatalf("owner countEventAccess = %d, want 1", n)
	}
	n, err = countEventAccess(ctx, eventID, "stranger", "")
	if err != nil {
		t.Fatalf("countEventAccess stranger: %v", err)
	}
	if n != 0 {
		t.Fatalf("stranger countEventAccess = %d, want 0", n)
	}

	c, err := countRulesForRepo(ctx, src, owner, "")
	if err != nil {
		t.Fatalf("countRulesForRepo: %v", err)
	}
	if c != 1 {
		t.Fatalf("countRulesForRepo = %d, want 1", c)
	}
	c, err = countRulesForRepo(ctx, src, "stranger", "")
	if err != nil {
		t.Fatalf("countRulesForRepo stranger: %v", err)
	}
	if c != 0 {
		t.Fatalf("countRulesForRepo stranger = %d, want 0", c)
	}
}

// ── HookTrigger Get/Update + markTrigger* ─────────────────────────────────────

func TestHookTrigger_GetUpdateMark(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	eventID := uuid.New().String()
	cleanupEvent(t, eventID)
	trig := HookTrigger{TriggerID: uuid.New().String(), EventID: eventID, RuleID: "r", WorkflowID: "w", Status: "pending", CreatedAt: time.Now().UTC()}
	if err := trig.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}

	row, err := trig.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.(HookTrigger).Status != "pending" {
		t.Fatalf("got status %q", row.(HookTrigger).Status)
	}

	runID := "run-123"
	if err := markTriggerTriggered(ctx, trig.TriggerID, &runID); err != nil {
		t.Fatalf("markTriggerTriggered: %v", err)
	}
	row, _ = trig.Get(ctx)
	got := row.(HookTrigger)
	if got.Status != "triggered" || got.RunID == nil || *got.RunID != "run-123" {
		t.Fatalf("markTriggerTriggered not applied: %+v", got)
	}

	if err := markTriggerFailed(ctx, trig.TriggerID, "boom"); err != nil {
		t.Fatalf("markTriggerFailed: %v", err)
	}
	row, _ = trig.Get(ctx)
	got = row.(HookTrigger)
	if got.Status != "failed" || got.Error == nil || *got.Error != "boom" {
		t.Fatalf("markTriggerFailed not applied: %+v", got)
	}

	// Update path (Save).
	got.Status = "manual"
	if err := got.Update(ctx); err != nil {
		t.Fatalf("Update: %v", err)
	}
	row, _ = trig.Get(ctx)
	if row.(HookTrigger).Status != "manual" {
		t.Fatalf("Update not applied: %+v", row)
	}
}

func TestHookTrigger_GetNotFound(t *testing.T) {
	requireDB(t)
	if _, err := (HookTrigger{TriggerID: uuid.New().String()}).Get(context.Background()); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected ErrRecordNotFound, got %v", err)
	}
}

func TestHookTrigger_List(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	eventID := uuid.New().String()
	cleanupEvent(t, eventID)
	for i := 0; i < 3; i++ {
		if err := (HookTrigger{TriggerID: uuid.New().String(), EventID: eventID, RuleID: "r", WorkflowID: "w", Status: "triggered", CreatedAt: time.Now().UTC()}).Add(ctx); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	list, err := (HookTrigger{EventID: eventID}).List(ctx, 2, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("limit not honoured: got %d, want 2", len(list))
	}
}

func TestHookTrigger_RemoveIsNoop(t *testing.T) {
	requireDB(t)
	if err := (HookTrigger{TriggerID: "x"}).Remove(context.Background()); err != nil {
		t.Fatalf("HookTrigger.Remove should be a no-op, got %v", err)
	}
}

// ── retry records: addRetry / claimDueRetries / advanceRetry / deleteRetry ─────

func TestRetryLifecycle(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	triggerID := uuid.New().String()
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM hook_trigger_retries WHERE trigger_id = ?`, triggerID) //nolint:errcheck
	})

	if err := addRetry(ctx, triggerID, "wf-1", "user-1", "org-1", map[string]string{"K": "V"}, "transient"); err != nil {
		t.Fatalf("addRetry: %v", err)
	}

	// next_retry_at was set ~60s in the future, so it should NOT be due yet.
	due, err := claimDueRetries(ctx, 10)
	if err != nil {
		t.Fatalf("claimDueRetries: %v", err)
	}
	for _, r := range due {
		if r.TriggerID == triggerID {
			t.Fatal("retry should not be due yet (next_retry_at in future)")
		}
	}

	// Find the retry id and pull it into the past, then it must be claimable.
	var retryID string
	if err := connect().Raw(`SELECT retry_id FROM hook_trigger_retries WHERE trigger_id = ?`, triggerID).Scan(&retryID).Error; err != nil {
		t.Fatalf("lookup retry_id: %v", err)
	}
	if retryID == "" {
		t.Fatal("retry record not inserted")
	}
	if err := advanceRetry(ctx, retryID, 2, "still failing", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("advanceRetry: %v", err)
	}
	due, err = claimDueRetries(ctx, 10)
	if err != nil {
		t.Fatalf("claimDueRetries after advance: %v", err)
	}
	found := false
	for _, r := range due {
		if r.RetryID == retryID {
			found = true
			if r.Attempt != 2 || r.LastError != "still failing" {
				t.Fatalf("advance not applied: %+v", r)
			}
			if r.Inputs["K"] != "V" {
				t.Fatalf("inputs not round-tripped: %+v", r.Inputs)
			}
		}
	}
	if !found {
		t.Fatal("advanced retry should now be due")
	}

	if err := deleteRetry(ctx, retryID); err != nil {
		t.Fatalf("deleteRetry: %v", err)
	}
	var count int
	if err := connect().Raw(`SELECT COUNT(*) FROM hook_trigger_retries WHERE retry_id = ?`, retryID).Scan(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatal("retry not deleted")
	}
}

func TestRetryBackoff(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 60 * time.Second},
		{2, 120 * time.Second},
		{3, 240 * time.Second},
		{4, 480 * time.Second},
		{5, 900 * time.Second}, // 960 capped at 900
		{6, 900 * time.Second}, // capped
	}
	for _, c := range cases {
		if got := retryBackoff(c.attempt); got != c.want {
			t.Errorf("retryBackoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}
