package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// A step that submits an async job owns a REMOTE resource for as long as that job
// lives, and abandoning the step does not release it. Forge admits new work against
// the RESERVATION of everything it still considers running, so a single abandoned
// execution is enough to freeze a whole cluster's pipelines: run 479d25fe left an
// execution holding cpu=4/memory=8Gi against a node budget of 4000 millicores /
// 7995 MB — 100% of the CPU and more than all of the memory — while its pod could not
// even schedule (run teardown had already deleted the clone volume it mounts). The
// next run's checkout sat in waiting_for_resources for 34 minutes until the execution
// was cancelled by hand.
//
// Forge's own reaper is not a substitute. It cancels an execution running_grace_secs
// past its DEADLINE, and that deadline derives from the step's timeout — so a step
// with a (correct) 2400s timeout that was cancelled seconds in still holds its slot
// for 45 minutes. The grace is measured from the time the step was ALLOWED, not from
// when it stopped doing work, which makes generous timeouts on slow builds actively
// harmful.
//
// So workflows cancels what it submitted, the moment it stops waiting for it: when
// the step is cancelled, when the run times out, when a map region trips its failure
// tolerance, when the run tears down, and — via the recorded job id, which outlives
// this process — when a sweep reaps a run whose worker died.

// asyncCancelTimeout bounds one cancel call. Short on purpose: every caller is either
// tearing a run down or answering a user's cancel, and neither may be made to wait on
// a service that is not answering. Whatever is left is forge's reaper's problem again.
const asyncCancelTimeout = 5 * time.Second

// asyncCancelURL is the target service's cancel endpoint for a submitted job, or ""
// when the action declares no poll path to derive it from.
//
// It is the poll path under DELETE. The poll path IS the job resource (forge:
// /executions/{id}; workflows: /runs/{id}), and DELETE on a job resource is cancel
// across this platform — DELETE /executions/{id} is exactly what an operator uses by
// hand to free a stuck admission slot. Deriving it means no new field has to be added
// to every async action in the registry manifest, and a service that has no DELETE
// answers 404/405, which this path treats like any other best-effort failure.
func asyncCancelURL(def ActionDef, jobID string) string {
	if def.Async == nil || def.Async.PollPath == "" || jobID == "" {
		return ""
	}
	return strings.TrimRight(def.ServiceURL, "/") +
		strings.ReplaceAll(def.Async.PollPath, "{id}", neturl.PathEscape(jobID))
}

// cancelAsyncJob asks the target service to cancel a job this run submitted.
//
// Best-effort by construction, like teardownRunVolumes: every caller is already on a
// teardown path, so a forge that is down, slow, or answers 404 for a job that has
// since finished must never turn teardown into a hang or a green run into a failed
// one. Every outcome is logged and swallowed.
//
// ctx is used only for cancellation of THIS call — callers whose run context is
// already dead pass context.WithoutCancel so the cancel still goes out.
func cancelAsyncJob(ctx context.Context, def ActionDef, jobID, token string) {
	url := asyncCancelURL(def, jobID)
	if url == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, asyncCancelTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		slog.WarnContext(ctx, "worker: build job cancel request", "action", def.Name, "job_id", jobID, "error", err)
		return
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "worker: cancel abandoned job failed (service reaper will retry)",
			"action", def.Name, "job_id", jobID, "error", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	// 404/409 mean the job is already gone or already terminal — the outcome we wanted,
	// reached by someone else. Anything else is a real failure worth a warning, but not
	// worth failing a teardown over.
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusConflict {
		slog.WarnContext(ctx, "worker: cancel abandoned job non-2xx (service reaper will retry)",
			"action", def.Name, "job_id", jobID, "status", resp.StatusCode)
		return
	}
	slog.InfoContext(ctx, "worker: cancelled abandoned job", "action", def.Name, "job_id", jobID, "status", resp.StatusCode)
}

// cancelStepJob cancels a job recorded against a step run, resolving the action in the
// current catalog to find where it lives. An action that has since left the catalog
// (a service was unregistered) leaves nothing to call, which is logged rather than
// treated as an error — there is no other place the job id could point.
func cancelStepJob(ctx context.Context, action, jobID, token string) {
	actionCatalogMu.RLock()
	def, ok := actionCatalog[action]
	actionCatalogMu.RUnlock()
	if !ok {
		slog.WarnContext(ctx, "worker: cannot cancel abandoned job, action not in catalog", "action", action, "job_id", jobID)
		return
	}
	cancelAsyncJob(ctx, def, jobID, token)
}

// cancelRunJobs cancels every async job this run still has outstanding — the jobs
// recorded against step runs that never reached a terminal state.
//
// It is the backstop for the per-step cancellation in pollAction: it catches a job
// whose step was abandoned by a path that never ran the poll loop's exit, and it is
// the ONLY option for a caller that is not the worker executing the run (the reaper
// sweeps, and a cancel request that lands on a different replica) — they have no
// in-memory job ids at all, only what the step runs recorded.
//
// token is the run's plaintext credential; it carries the cancel grant via the async
// companion permission (see collectWorkflowPermissions).
func cancelRunJobs(ctx context.Context, runID, token string) {
	jobs, err := liveStepJobs(ctx, runID)
	if err != nil {
		slog.WarnContext(ctx, "worker: list outstanding jobs for cancellation", "run_id", runID, "error", err)
		return
	}
	for _, j := range jobs {
		cancelStepJob(ctx, j.Action, j.JobID, token)
	}
}

// cancelAbandonedRunJobs is cancelRunJobs for runs a SWEEP reaped: the run's worker is
// gone (or was never in this process), so the only credential available is the one
// stored on the run row. Its session is not revoked when a run is reaped — the JWT is
// simply left to expire — so it is still good for the cancel.
//
// Runs are handled independently and every failure is logged and skipped: the sweep
// must reclaim the rest of its batch regardless.
func cancelAbandonedRunJobs(ctx context.Context, runs []abandonedRun) {
	for _, r := range runs {
		if r.RunID == "" {
			continue
		}
		jobs, err := liveStepJobs(ctx, r.RunID)
		if err != nil {
			slog.WarnContext(ctx, "sweep: list outstanding jobs for cancellation", "run_id", r.RunID, "error", err)
			continue
		}
		if len(jobs) == 0 {
			continue
		}
		// Decrypted only once there is something to cancel, so a reaped run that held no
		// jobs never touches the key.
		token, err := decryptToken(r.Token)
		if err != nil {
			slog.WarnContext(ctx, "sweep: decrypt run token for job cancellation", "run_id", r.RunID, "error", err)
			continue
		}
		slog.InfoContext(ctx, "sweep: cancelling jobs abandoned by a dead worker", "run_id", r.RunID, "jobs", len(jobs))
		for _, j := range jobs {
			cancelStepJob(ctx, j.Action, j.JobID, token)
		}
	}
}
