package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// testDBReady is set in TestMain when the in-memory test DB is migrated. It stays
// true in normal runs; the requireDB guard remains so a future driver swap that
// fails degrades to skips rather than panics.
var testDBReady bool

func TestMain(m *testing.M) {
	initMetrics()
	httpClient = &http.Client{Timeout: 10 * time.Second}
	gatekeeperClient = newGatekeeperClient()

	// Hermetic in-memory sqlite (mirrors gatekeeper) — no external Postgres. The
	// only Postgres-specific query (getMatchedRules) was rewritten to be portable.
	conn, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err == nil {
		if migErr := conn.AutoMigrate(&PipelineRule{}, &HookEvent{}, &HookTrigger{}, &HookTriggerRetry{}); migErr == nil {
			dbInitMu.Lock()
			gormDB = conn
			gormDBRead = conn
			dbInitMu.Unlock()
			testDBReady = true
		}
	}

	os.Exit(m.Run())
}

// requireDB skips a test when the test database failed to initialise.
func requireDB(t *testing.T) {
	t.Helper()
	if !testDBReady {
		t.Skip("hooks test database not available")
	}
}

// withTriggerKey sets hooksTriggerKey for the duration of the test.
func withTriggerKey(t *testing.T, key string) {
	t.Helper()
	orig := hooksTriggerKey
	hooksTriggerKey = key
	t.Cleanup(func() { hooksTriggerKey = orig })
}

// insertRule writes a pipeline rule directly and registers a hard-delete cleanup.
func insertRule(t *testing.T, rule PipelineRule) PipelineRule {
	t.Helper()
	if rule.RuleID == "" {
		rule.RuleID = uuid.New().String()
	}
	if rule.CreatedAt.IsZero() {
		rule.CreatedAt = time.Now().UTC()
	}
	rule.UpdatedAt = rule.CreatedAt
	if rule.Events == nil {
		rule.Events = []string{}
	}
	if rule.InputMapping == nil {
		rule.InputMapping = map[string]string{}
	}
	// Capture the intended Active BEFORE Create: GORM omits the zero value for a
	// column with a `default:` tag AND writes the DB default back into the struct
	// via RETURNING, so rule.Active would read true again after Create.
	wantActive := rule.Active
	if err := connect().Create(&rule).Error; err != nil {
		t.Fatalf("insertRule: %v", err)
	}
	// An Active:false rule is stored as active=true (the column default); force it.
	if !wantActive {
		if err := connect().Exec(`UPDATE pipeline_rules SET active = false WHERE rule_id = ?`, rule.RuleID).Error; err != nil {
			t.Fatalf("insertRule deactivate: %v", err)
		}
	}
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM pipeline_rules WHERE rule_id = ?`, rule.RuleID) //nolint:errcheck
	})
	rule.Active = wantActive // reflect the intended state, not the RETURNING default
	return rule
}

// secretPtr is a small helper for the *string secret field.
func secretPtr(s string) *string { return &s }

// cleanupEvent registers a hard delete for an event and its triggers/retries.
func cleanupEvent(t *testing.T, eventID string) {
	t.Helper()
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM hook_trigger_retries WHERE trigger_id IN (SELECT trigger_id FROM hook_triggers WHERE event_id = ?)`, eventID) //nolint:errcheck
		connect().Exec(`DELETE FROM hook_triggers WHERE event_id = ?`, eventID)                                                                    //nolint:errcheck
		connect().Exec(`DELETE FROM hook_events WHERE event_id = ?`, eventID)                                                                      //nolint:errcheck
	})
}

// fakeWorkflows stands up an httptest server emulating the workflows internal
// API and points workflowsURL at it. The handler decides per-route responses.
//
//   - GET  /internal/pipelines/{id}        → org-check (used by fetchWorkflowOrgID)
//   - POST /internal/pipelines/{id}/runs   → dispatch (returns a run_id)
//   - GET  /internal/runs/{id}             → run status poll
//
// orgByWorkflow maps a workflow ID to the org it belongs to (for the org check).
// Unknown workflows return 404. runID is returned by the dispatch endpoint.
func fakeWorkflows(t *testing.T, orgByWorkflow map[string]string, runID string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && len(path) > len("/runs") && path[len(path)-5:] == "/runs":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"run_id":"` + runID + `"}`)) //nolint:errcheck
		case r.Method == http.MethodGet:
			// /internal/pipelines/{id} org check.
			id := path[len("/internal/pipelines/"):]
			org, ok := orgByWorkflow[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"org_id":"` + org + `"}`)) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	orig := workflowsURL
	workflowsURL = srv.URL
	t.Cleanup(func() {
		workflowsURL = orig
		srv.Close()
	})
}
