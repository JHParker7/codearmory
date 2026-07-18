package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// An async step polls a remote job that enforces its own timeout; the poll must
// outlast that job to observe its terminal state. Bounding it by the (short,
// earlier-started) step timeout could fail a step whose job actually succeeded —
// the reported "context deadline exceeded" on a forge run forge marked
// completed. Here the job stays "running" past the 1s step timeout, then reports
// success; executeStep must still return that success.
func TestExecuteStep_AsyncOutlastsStepTimeout(t *testing.T) {
	start := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"execution_id":"j1"}`)) //nolint:errcheck
			return
		}
		// Finish a little after the step's 1s deadline, like a forge container that
		// completes just past the step timeout.
		if time.Since(start) < 1500*time.Millisecond {
			w.Write([]byte(`{"status":"running"}`)) //nolint:errcheck
			return
		}
		w.Write([]byte(`{"status":"completed","stdout":"done"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	def := ActionDef{
		Name: "forge/run", ServiceURL: srv.URL, Method: http.MethodPost, Path: "/executions",
		Async: &AsyncConfig{
			IDField: "execution_id", PollPath: "/executions/{id}", PollIntervalSecs: 1,
			StatusField: "status", SuccessStates: []string{"completed"}, FailureStates: []string{"failed"},
			OutputField: "stdout",
		},
	}
	actionCatalogMu.Lock()
	prev := actionCatalog
	actionCatalog = map[string]ActionDef{"forge/run": def}
	actionCatalogMu.Unlock()
	t.Cleanup(func() { actionCatalogMu.Lock(); actionCatalog = prev; actionCatalogMu.Unlock() })

	// Timeout: 1 second is far shorter than the ~1.5s the job runs. Before the
	// fix the poll context expired at 1s and the step failed on a job that
	// succeeded; now async steps get the full budget instead.
	res, err := (&WorkerPool{}).executeStep(
		context.Background(), newTokenStore("", ""),
		Step{Action: "forge/run", Timeout: 1}, substContext{},
	)
	if err != nil {
		t.Fatalf("async step should outlast the step timeout, got error: %v", err)
	}
	if res.Output != "done" {
		t.Errorf("output = %q, want %q", res.Output, "done")
	}
}
