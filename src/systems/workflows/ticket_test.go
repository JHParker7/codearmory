package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ticketStub is a stand-in tickets service that records what mirroring sent it.
type ticketStub struct {
	mu       sync.Mutex
	created  []map[string]any
	comments []string
	updates  []map[string]any
	failWith int // when non-zero, every call answers this status
}

func (s *ticketStub) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.failWith != 0 {
			http.Error(w, "nope", s.failWith)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/tickets":
			s.created = append(s.created, body)
			w.Write([]byte(`{"ticket_id":"tkt-1"}`)) //nolint:errcheck
		case strings.HasSuffix(r.URL.Path, "/comments"):
			s.comments = append(s.comments, body["body"].(string))
			w.Write([]byte(`{}`)) //nolint:errcheck
		case r.Method == http.MethodPut:
			s.updates = append(s.updates, body)
			w.Write([]byte(`{}`)) //nolint:errcheck
		default:
			w.Write([]byte(`{}`)) //nolint:errcheck
		}
	}))
	t.Cleanup(srv.Close)

	serviceURLsMu.Lock()
	old := serviceURLs[ticketService]
	serviceURLs[ticketService] = srv.URL
	serviceURLsMu.Unlock()
	t.Cleanup(func() {
		serviceURLsMu.Lock()
		if old == "" {
			delete(serviceURLs, ticketService)
		} else {
			serviceURLs[ticketService] = old
		}
		serviceURLsMu.Unlock()
	})
}

func testReporter(cfg TicketConfig) *ticketReporter {
	return &ticketReporter{cfg: cfg, store: newTokenStore("tok", "sess"), runID: "run-1", workflowID: "wf-1"}
}

// A nil reporter is the "mirroring is off" case and every entry point must tolerate it
// — the scheduler calls straight through st.ticket without checking.
func TestTicketReporter_NilIsANoOp(t *testing.T) {
	var r *ticketReporter
	r.open(context.Background(), "wf", nil, "")
	r.stepDone(context.Background(), "build", nodeCompleted, 3)
	r.pause(context.Background())
	r.closeWith(context.Background(), StatusCompleted, time.Second)
}

func TestNewTicketReporter_OnlyWhenOptedIn(t *testing.T) {
	store := newTokenStore("t", "s")
	if r := newTicketReporter(&Workflow{}, store, "run"); r != nil {
		t.Error("a workflow with no ticket config must not mirror")
	}
	if r := newTicketReporter(&Workflow{Ticket: &TicketConfig{Enabled: false}}, store, "run"); r != nil {
		t.Error("ticket.enabled=false must not mirror")
	}
	if r := newTicketReporter(&Workflow{Ticket: &TicketConfig{Enabled: true}}, store, "run"); r == nil {
		t.Error("ticket.enabled=true must mirror")
	}
}

func TestTicketReporter_OpenLinksTheRun(t *testing.T) {
	stub := &ticketStub{}
	stub.start(t)
	r := testReporter(TicketConfig{Enabled: true})
	r.open(context.Background(), "codearmory-ci", nil, "")

	if len(stub.created) != 1 {
		t.Fatalf("created %d tickets, want 1", len(stub.created))
	}
	c := stub.created[0]
	// The whole point: the ticket is linked to the run. These columns existed and
	// nothing populated them.
	if c["run_id"] != "run-1" || c["workflow_id"] != "wf-1" {
		t.Errorf("ticket not linked to the run: %+v", c)
	}
	// The run is already running when the ticket opens, so it must not sit at the
	// create-default "open".
	if c["status"] != defaultTicketRunning {
		t.Errorf("status = %v, want %q", c["status"], defaultTicketRunning)
	}
	if r.ticketID != "tkt-1" {
		t.Errorf("ticketID = %q, want the id the service returned", r.ticketID)
	}
}

// A run that pauses on an approval gate is re-executed from the top on resume. Without
// adoption that would open a SECOND ticket for the same run.
func TestTicketReporter_ResumeAdoptsTheExistingTicket(t *testing.T) {
	stub := &ticketStub{}
	stub.start(t)
	r := testReporter(TicketConfig{Enabled: true})
	r.open(context.Background(), "wf", nil, "tkt-existing")

	if len(stub.created) != 0 {
		t.Fatalf("a resumed run must not open a second ticket; created %d", len(stub.created))
	}
	if r.ticketID != "tkt-existing" {
		t.Errorf("ticketID = %q, want the adopted one", r.ticketID)
	}
	// The gate left a comment saying it was waiting; resuming puts it back to running.
	if len(stub.updates) != 1 || stub.updates[0]["status"] != defaultTicketRunning {
		t.Errorf("resume must set the ticket running again, got %+v", stub.updates)
	}
}

func TestTicketReporter_StepComments(t *testing.T) {
	stub := &ticketStub{}
	stub.start(t)
	r := testReporter(TicketConfig{Enabled: true})
	r.open(context.Background(), "wf", nil, "")
	r.stepDone(context.Background(), "checkout", nodeCompleted, 1)
	r.stepDone(context.Background(), "build", nodeCompleted, 15)
	r.stepDone(context.Background(), "test", nodeFailed, 15)
	r.stepDone(context.Background(), "deploy", nodeSkipped, 0)

	if len(stub.comments) != 4 {
		t.Fatalf("comments = %v, want 4", stub.comments)
	}
	// A single-leg step does not claim a leg count; a fan-out says how wide it was.
	if strings.Contains(stub.comments[0], "legs") {
		t.Errorf("a 1-leg step should not mention legs: %q", stub.comments[0])
	}
	for _, want := range []string{"build", "15 legs", "completed"} {
		if !strings.Contains(stub.comments[1], want) {
			t.Errorf("comment %q missing %q", stub.comments[1], want)
		}
	}
	if !strings.Contains(stub.comments[2], "failed") {
		t.Errorf("a failed step must say so: %q", stub.comments[2])
	}
	// A skipped branch is worth recording — it is why a downstream step never ran.
	if !strings.Contains(stub.comments[3], "skipped") {
		t.Errorf("a skipped step must say so: %q", stub.comments[3])
	}
}

// The tickets service validates a status against its field defs, so the mapping must
// land on its real vocabulary — inventing "failed" would be rejected at run time.
func TestTicketStatusFor_UsesTheRealVocabulary(t *testing.T) {
	cfg := TicketConfig{Enabled: true}
	cases := map[string]string{
		StatusCompleted: defaultTicketSuccess,
		StatusCancelled: defaultTicketCancelled,
		StatusFailed:    defaultTicketFailure,
	}
	for runStatus, want := range cases {
		if got := cfg.ticketStatusFor(runStatus); got != want {
			t.Errorf("ticketStatusFor(%q) = %q, want %q", runStatus, got, want)
		}
	}
	// A failed run maps to an OPEN ticket on purpose: it is the one still needing a
	// human. Closing it would bury the failure.
	if cfg.ticketStatusFor(StatusFailed) != "open" {
		t.Error("a failed run's ticket must stay open")
	}
}

// A board with custom columns rejects the default vocabulary, so the mapping is
// configurable.
func TestTicketConfig_StatusOverrides(t *testing.T) {
	cfg := TicketConfig{
		Enabled: true, StatusRunning: "building", StatusSuccess: "shipped",
		StatusFailure: "triage", StatusCancelled: "dropped",
	}
	if cfg.runningStatus() != "building" || cfg.ticketStatusFor(StatusCompleted) != "shipped" ||
		cfg.ticketStatusFor(StatusFailed) != "triage" || cfg.ticketStatusFor(StatusCancelled) != "dropped" {
		t.Error("configured statuses must win over the defaults")
	}
	// An empty override falls back rather than sending "".
	if (TicketConfig{}).runningStatus() != defaultTicketRunning {
		t.Error("an unset status must fall back to the default")
	}
}

// THE rule: mirroring must never fail a run. A tickets outage is a log line, not a
// red build — and it must not retry on every subsequent step either.
func TestTicketReporter_ServiceOutageIsSurvivable(t *testing.T) {
	stub := &ticketStub{failWith: http.StatusInternalServerError}
	stub.start(t)
	r := testReporter(TicketConfig{Enabled: true})

	r.open(context.Background(), "wf", nil, "")
	if !r.off {
		t.Fatal("a failed open must disable mirroring for the run")
	}
	// These must all be silent no-ops rather than panicking or calling again.
	r.stepDone(context.Background(), "build", nodeCompleted, 1)
	r.pause(context.Background())
	r.closeWith(context.Background(), StatusCompleted, time.Second)
}

// A comment before the ticket exists has nowhere to go; it must not be attempted.
func TestTicketReporter_NoTicketMeansNoComment(t *testing.T) {
	stub := &ticketStub{}
	stub.start(t)
	r := testReporter(TicketConfig{Enabled: true}) // never opened
	r.stepDone(context.Background(), "build", nodeCompleted, 1)
	r.closeWith(context.Background(), StatusCompleted, time.Second)
	if len(stub.comments) != 0 || len(stub.updates) != 0 {
		t.Errorf("nothing should be sent before the ticket exists: %v %v", stub.comments, stub.updates)
	}
}

// A run token can open, comment on, and close its ticket — and nothing else. It must
// never be able to delete one.
func TestTicketPermissions_AreMinimal(t *testing.T) {
	perms := ticketPermissions()
	got := map[string]string{}
	for _, p := range perms {
		if p.Service != "tickets" {
			t.Errorf("permission on the wrong service: %+v", p)
		}
		got[p.Action] = p.Resource
	}
	for _, want := range []string{"createTicket", "updateTicket", "createComment"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q; mirroring would 403", want)
		}
	}
	if _, bad := got["deleteTicket"]; bad {
		t.Error("a run must never be able to delete a ticket")
	}
	// Unprefixed, matching what the tickets service actually checks
	// (CheckPermissions(..., "createTicket", "tickets/tickets")).
	if got["createTicket"] != "tickets/tickets" {
		t.Errorf("createTicket resource = %q, want the unprefixed form", got["createTicket"])
	}
}

// Enabling ticket mirroring is what adds the ticket grants — and nothing else does, so
// an existing workflow's run token cannot suddenly touch tickets.
func TestCollectWorkflowPermissions_TicketGrantIsOptIn(t *testing.T) {
	steps := []WorkflowStep{{Step: Step{Action: ActionHTTP}}}

	has := func(perms []PermissionSpec) bool {
		for _, p := range perms {
			if p.Service == "tickets" {
				return true
			}
		}
		return false
	}
	if has(collectWorkflowPermissions(steps, nil, nil)) {
		t.Error("no ticket config must grant no ticket permissions")
	}
	if has(collectWorkflowPermissions(steps, nil, &TicketConfig{Enabled: false})) {
		t.Error("ticket.enabled=false must grant no ticket permissions")
	}
	if !has(collectWorkflowPermissions(steps, nil, &TicketConfig{Enabled: true})) {
		t.Error("ticket.enabled=true must grant the ticket permissions, or mirroring 403s")
	}
}

func TestValidateTicket(t *testing.T) {
	if msg := validateTicket(nil); msg != "" {
		t.Errorf("nil config = %q, want valid", msg)
	}
	if msg := validateTicket(&TicketConfig{Enabled: true, Title: "ok"}); msg != "" {
		t.Errorf("valid config rejected: %q", msg)
	}
	if msg := validateTicket(&TicketConfig{Enabled: true, Title: strings.Repeat("x", 501)}); msg == "" {
		t.Error("an over-long title should be rejected at authoring time")
	}
	// A disabled config is never used, so its shape does not matter.
	if msg := validateTicket(&TicketConfig{Title: strings.Repeat("x", 501)}); msg != "" {
		t.Errorf("a disabled config should not be validated: %q", msg)
	}
}

// A ticket opens before any step has run, so a title naming a step's output — the
// commit a checkout resolved — cannot be rendered yet. It must be filled in the moment
// that step completes, rather than left showing the literal template forever.
func TestTicketReporter_RetitlesOnceTheStepOutputExists(t *testing.T) {
	stub := &ticketStub{}
	stub.start(t)
	r := testReporter(TicketConfig{Enabled: true, Title: "CI — ${steps.commit.output.SHA}"})
	r.open(context.Background(), "wf", nil, "")

	// Opened with the unresolved template: the commit is not knowable yet.
	if !r.pendingTitle {
		t.Fatal("a title naming a step output must be pending after open")
	}
	if got := stub.created[0]["title"].(string); !strings.Contains(got, "${steps.commit") {
		t.Errorf("provisional title = %q, want the unresolved template", got)
	}

	// A step completes, but not the one the title names: still pending, no PUT.
	r.retitle(context.Background(), nil, map[string]string{"checkout": "{}"})
	if !r.pendingTitle || len(stub.updates) != 0 {
		t.Errorf("an unrelated step must not resolve the title; updates=%v", stub.updates)
	}

	// The named step lands.
	r.retitle(context.Background(), nil, map[string]string{"commit": `{"SHA":"abc1234"}`})
	if r.pendingTitle {
		t.Error("title should be resolved once its step produced the output")
	}
	if len(stub.updates) != 1 || stub.updates[0]["title"] != "CI — abc1234" {
		t.Fatalf("updates = %+v, want the title set to the commit", stub.updates)
	}
	// Once resolved it must not keep re-PUTting on every subsequent step.
	r.retitle(context.Background(), nil, map[string]string{"commit": `{"SHA":"abc1234"}`})
	if len(stub.updates) != 1 {
		t.Errorf("retitle must be a no-op once resolved, got %d updates", len(stub.updates))
	}
}

// A title with no step reference is final at open: it must never cost an extra call.
func TestTicketReporter_StaticTitleIsNotPending(t *testing.T) {
	stub := &ticketStub{}
	stub.start(t)
	r := testReporter(TicketConfig{Enabled: true, Title: "CI — ${run_id}"})
	r.open(context.Background(), "wf", nil, "")
	if r.pendingTitle {
		t.Error("a title with no step reference must not be pending")
	}
	if got := stub.created[0]["title"].(string); got != "CI — run-1" {
		t.Errorf("title = %q, want ${run_id} substituted at open", got)
	}
	r.retitle(context.Background(), nil, map[string]string{"any": "x"})
	if len(stub.updates) != 0 {
		t.Error("a static title must never trigger a retitle PUT")
	}
}
