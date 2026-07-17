package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Run → ticket mirroring.
//
// A run is the thing people actually want to track, and the tickets service already
// modelled exactly that: Ticket carries workflow_id and run_id columns that nothing
// ever populated. This fills them in. Opt a workflow into `ticket` and every run of it
// opens a ticket, comments as each step reaches a terminal state, and closes with the
// run's outcome — so a run has a durable, linkable record without anyone wiring
// tickets/* steps into the pipeline and remembering to update them on every path.
//
// THE RULE: mirroring must never fail a run. A pipeline that went green does not turn
// red because the tickets service was down, and a comment that cannot be posted is
// logged and dropped. This is observability, not pipeline logic — the moment it can
// fail a build it costs more than it is worth. Every method here swallows its errors
// and disables the reporter for the rest of the run, so one outage is one log line
// rather than one per step.

const (
	ticketService = "tickets"
	// ticketCallTimeout bounds a single mirroring call. It is deliberately short: the
	// run is waiting on it, and a slow tickets service must not stall a pipeline.
	ticketCallTimeout = 10 * time.Second
	// ticketCommentMax caps a comment body. A step's output can be a megabyte
	// (maxResponseBody); a ticket comment is a summary, not a log store.
	ticketCommentMax = 2000
)

// The statuses a run's outcome maps onto by default.
//
// These are the tickets service's OWN vocabulary (open/in_progress/resolved/closed) —
// it validates every status against the org's field defs, so inventing "failed" or
// "blocked" here would simply be rejected. A failed run maps to `open` deliberately:
// the ticket for a broken build is the one that still needs a human.
const (
	defaultTicketRunning   = "in_progress"
	defaultTicketSuccess   = "resolved"
	defaultTicketFailure   = "open"
	defaultTicketCancelled = "closed"
)

// TicketConfig opts a workflow into mirroring each of its runs into a ticket.
//
// It lives on the workflow rather than on a step because it describes the RUN, not a
// position in the graph: a run that fails at step 0 still deserves its ticket, and a
// step-authored ticket could not close one on a path it never reached.
type TicketConfig struct {
	// Enabled turns mirroring on. Absent/false means no ticket is ever created, which
	// is why every existing workflow is unaffected.
	Enabled bool `json:"enabled"`
	// Title is the ticket's title, with ${...} substitution over the run's inputs
	// (${inputs.X}) and ${run_id}. Defaults to the workflow's name + the run id.
	Title string `json:"title,omitempty"`
	// BoardID files the ticket on a board. Empty leaves it unfiled.
	BoardID string `json:"board_id,omitempty"`
	// Priority is the ticket's priority; empty takes the tickets service's default.
	Priority string `json:"priority,omitempty"`
	// Project tags the ticket, matching the project a pipeline belongs to.
	Project string `json:"project,omitempty"`

	// The status a run's state maps onto. Each defaults to the tickets service's own
	// vocabulary (in_progress / resolved / open / closed).
	//
	// These are configurable because a status is NOT free text: tickets validates it
	// against the org's field defs — and when the ticket is filed on a board, against
	// THAT BOARD's columns. A board with custom columns ("triage", "shipped") would
	// reject every default here, so the mapping has to be the board owner's to set.
	StatusRunning   string `json:"status_running,omitempty"`
	StatusSuccess   string `json:"status_success,omitempty"`
	StatusFailure   string `json:"status_failure,omitempty"`
	StatusCancelled string `json:"status_cancelled,omitempty"`
}

func (t TicketConfig) runningStatus() string {
	return orTicketDefault(t.StatusRunning, defaultTicketRunning)
}
func (t TicketConfig) successStatus() string {
	return orTicketDefault(t.StatusSuccess, defaultTicketSuccess)
}
func (t TicketConfig) failureStatus() string {
	return orTicketDefault(t.StatusFailure, defaultTicketFailure)
}
func (t TicketConfig) cancelledStatus() string {
	return orTicketDefault(t.StatusCancelled, defaultTicketCancelled)
}

func orTicketDefault(v, def string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

// validateTicket checks a ticket config's shape. Returns "" when valid.
func validateTicket(t *TicketConfig) string {
	if t == nil || !t.Enabled {
		return ""
	}
	if len(t.Title) > 500 {
		return "ticket.title exceeds 500 characters"
	}
	return ""
}

// ticketPermissions is what a run token needs to mirror its run: open a ticket,
// comment on it, and close it. Nothing else — a run must not be able to delete a
// ticket, least of all one it did not open.
//
// Resources are UNPREFIXED ("tickets/tickets", not "{username}/tickets/tickets"):
// the {username} form is a default_grant template gatekeeper expands at grant time,
// and it is the unprefixed form the tickets service checks
// (CheckPermissions(..., "createTicket", "tickets/tickets")).
func ticketPermissions() []PermissionSpec {
	return []PermissionSpec{
		{Service: ticketService, Action: "createTicket", Resource: "tickets/tickets"},
		{Service: ticketService, Action: "updateTicket", Resource: "tickets/tickets/*"},
		{Service: ticketService, Action: "createComment", Resource: "tickets/tickets/*"},
	}
}

// ticketReporter mirrors one run into one ticket. It is used only from the scheduler
// goroutine, which owns all run state, so it needs no locking.
type ticketReporter struct {
	cfg        TicketConfig
	store      *tokenStore
	runID      string
	workflowID string
	ticketID   string
	// title is the rendered title, and pendingTitle is true while it still contains
	// an unresolved ${steps...} reference — see retitle.
	title        string
	pendingTitle bool
	// off disables the reporter after a failure, so a tickets outage costs one log
	// line per run rather than one per step.
	off bool
}

// newTicketReporter returns a reporter for a run, or nil when the workflow has not
// opted in — a nil reporter is the "mirroring is off" case and every call site treats
// it as a no-op.
func newTicketReporter(wf *Workflow, store *tokenStore, runID string) *ticketReporter {
	if wf.Ticket == nil || !wf.Ticket.Enabled {
		return nil
	}
	return &ticketReporter{cfg: *wf.Ticket, store: store, runID: runID, workflowID: wf.WorkflowID}
}

// ticketURL resolves the tickets service's base URL from the registry-populated map.
func ticketURL() (string, bool) {
	serviceURLsMu.RLock()
	defer serviceURLsMu.RUnlock()
	u, ok := serviceURLs[ticketService]
	return u, ok
}

// call makes one mirroring request. Errors are returned for the caller to log and
// disable on; they never propagate into the run.
func (t *ticketReporter) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	base, ok := ticketURL()
	if !ok {
		return nil, fmt.Errorf("tickets service is not registered")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, ticketCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, method, strings.TrimRight(base, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// The run token, the same credential the run's steps use: mirroring gets exactly
	// the authority the run already has, and the workflow role carries the ticket
	// permissions only because the workflow opted in (collectWorkflowPermissions).
	req.Header.Set("Authorization", "Bearer "+t.store.getToken())
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tickets returned %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// disable turns the reporter off for the rest of the run after a failure.
func (t *ticketReporter) disable(ctx context.Context, op string, err error) {
	t.off = true
	slog.WarnContext(ctx, "worker: ticket mirroring disabled for this run",
		"run_id", t.runID, "op", op, "error", err)
}

// open creates the run's ticket, or ADOPTS the one a previous attempt created.
//
// existing is the ticket id already recorded on the run. A run that pauses on an
// approval gate is re-executed from the top when it resumes, so without this every
// gate would open a second ticket for the same run.
//
// A failure disables mirroring rather than the run.
func (t *ticketReporter) open(ctx context.Context, wfName string, inputs map[string]string, existing string) {
	if t == nil || t.off {
		return
	}
	if existing != "" {
		t.ticketID = existing
		// Back to in_progress: a resumed run is running again, and the gate left the
		// ticket saying otherwise.
		if _, err := t.call(ctx, http.MethodPut, "/tickets/"+t.ticketID,
			map[string]any{"status": t.cfg.runningStatus()}); err != nil {
			t.disable(ctx, "resume", err)
		}
		return
	}
	title := t.cfg.Title
	if strings.TrimSpace(title) == "" {
		title = fmt.Sprintf("%s — run %s", wfName, shortID(t.runID))
	}
	// The ticket opens BEFORE any step has run, so a title referencing a step's output
	// — the commit a checkout resolved, say — cannot be rendered yet. substitute leaves
	// an unresolved ${...} literal, so the ticket opens with a provisional title and
	// retitle() fills it in the moment the step it names completes.
	title = substitute(title, substContext{inputs: inputs, runID: t.runID})
	t.title = title
	t.pendingTitle = referencesSteps(title)

	body := map[string]any{
		"title": title,
		// The run is already running by the time this is called, so the ticket says so
		// rather than sitting at the create-default "open".
		"status": t.cfg.runningStatus(),
		// The link that makes this worth doing: the columns were always there.
		"workflow_id": t.workflowID,
		"run_id":      t.runID,
		"description": fmt.Sprintf("Automatically opened for workflow run `%s`.", t.runID),
	}
	if t.cfg.BoardID != "" {
		body["board_id"] = t.cfg.BoardID
	}
	if t.cfg.Priority != "" {
		body["priority"] = t.cfg.Priority
	}
	if t.cfg.Project != "" {
		body["project"] = t.cfg.Project
	}
	out, err := t.call(ctx, http.MethodPost, "/tickets", body)
	if err != nil {
		t.disable(ctx, "open", err)
		return
	}
	var created struct {
		TicketID string `json:"ticket_id"`
	}
	if err := json.Unmarshal(out, &created); err != nil || created.TicketID == "" {
		t.disable(ctx, "open", fmt.Errorf("tickets did not return a ticket_id"))
		return
	}
	t.ticketID = created.TicketID
	// Record it on the run BEFORE anything else can fail: this is what a resume after
	// an approval gate adopts, and what links a run to its ticket.
	(WorkflowRun{RunID: t.runID}).SetTicket(ctx, t.ticketID)
	slog.InfoContext(ctx, "worker: opened run ticket", "run_id", t.runID, "ticket_id", t.ticketID)
}

// referencesSteps reports whether a rendered string still names a step output — i.e.
// substitute could not resolve it, because that step had not run yet.
func referencesSteps(s string) bool { return strings.Contains(s, "${steps.") }

// retitle re-renders a title that referenced a step output, once that output exists.
//
// Without this a title like "CI — ${steps.commit.output.SHA}" would be stuck on the
// literal text forever: the ticket is opened before any step runs, which is exactly
// when the interesting facts about a run (its commit) are not yet known.
func (t *ticketReporter) retitle(ctx context.Context, inputs, outputs map[string]string) {
	if t == nil || t.off || t.ticketID == "" || !t.pendingTitle {
		return
	}
	rendered := substitute(t.cfg.Title, substContext{inputs: inputs, outputs: outputs, runID: t.runID})
	if referencesSteps(rendered) {
		return // the step it names still has not produced its output
	}
	if rendered == t.title {
		t.pendingTitle = false
		return
	}
	if _, err := t.call(ctx, http.MethodPut, "/tickets/"+t.ticketID,
		map[string]any{"title": rendered}); err != nil {
		t.disable(ctx, "retitle", err)
		return
	}
	t.title = rendered
	t.pendingTitle = false
}

// stepDone comments the outcome of one node. Called only from the top-level graph, so
// a map region contributes one comment per body step rather than one per iteration.
func (t *ticketReporter) stepDone(ctx context.Context, name string, state nodeState, legs int) {
	if t == nil || t.off || t.ticketID == "" {
		return
	}
	body := fmt.Sprintf("**%s** — %s", name, nodeStateLabel(state))
	if legs > 1 {
		body = fmt.Sprintf("**%s** (%d legs) — %s", name, legs, nodeStateLabel(state))
	}
	if _, err := t.call(ctx, http.MethodPost, "/tickets/"+t.ticketID+"/comments",
		map[string]any{"body": truncate(body, ticketCommentMax)}); err != nil {
		t.disable(ctx, "comment", err)
	}
}

// closeWith sets the ticket's final status from the run's outcome.
func (t *ticketReporter) closeWith(ctx context.Context, runStatus string, took time.Duration) {
	if t == nil || t.off || t.ticketID == "" {
		return
	}
	status := t.cfg.ticketStatusFor(runStatus)
	if _, err := t.call(ctx, http.MethodPut, "/tickets/"+t.ticketID,
		map[string]any{"status": status}); err != nil {
		t.disable(ctx, "close", err)
		return
	}
	// Best-effort summary; a failure here has already disabled nothing important.
	_, _ = t.call(ctx, http.MethodPost, "/tickets/"+t.ticketID+"/comments",
		map[string]any{"body": fmt.Sprintf("Run %s in %s.", runStatus, took.Round(time.Second))})
}

// pause records that the run is parked on an approval gate.
//
// The ticket is NOT closed and its status is NOT changed: the run has not finished, so
// closing would say the opposite of what is true, and the default vocabulary has no
// "blocked" to move it to. A comment is the honest record — and it is what tells the
// approver the run is waiting on them.
func (t *ticketReporter) pause(ctx context.Context) {
	if t == nil || t.off || t.ticketID == "" {
		return
	}
	if _, err := t.call(ctx, http.MethodPost, "/tickets/"+t.ticketID+"/comments",
		map[string]any{"body": "Run is paused awaiting approval."}); err != nil {
		t.disable(ctx, "pause", err)
	}
}

// ticketStatusFor maps a run's terminal status onto the ticket's.
func (t TicketConfig) ticketStatusFor(runStatus string) string {
	switch runStatus {
	case StatusCompleted:
		return t.successStatus()
	case StatusCancelled:
		return t.cancelledStatus()
	default:
		return t.failureStatus()
	}
}

// nodeStateLabel renders a node state for a human reading the ticket.
func nodeStateLabel(s nodeState) string {
	switch s {
	case nodeCompleted:
		return "completed"
	case nodeFailed:
		return "failed"
	case nodeSkipped:
		return "skipped"
	case nodeCancelled:
		return "cancelled"
	case nodeAwaiting:
		return "awaiting approval"
	default:
		return "finished"
	}
}

func shortID(id string) string { return truncate(id, 8) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
