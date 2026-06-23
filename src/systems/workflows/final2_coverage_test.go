package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPollAction_ContextCancelled(t *testing.T) {
	pool := &WorkerPool{}
	def := ActionDef{Name: "x", ServiceURL: "http://unused", Async: &AsyncConfig{
		PollPath: "/p/{id}", PollIntervalSecs: 1, StatusField: "status", SuccessStates: []string{"done"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled → pollAction returns ctx.Err()
	if _, err := pool.pollAction(ctx, newTokenStore("", ""), def, "job-1"); err == nil {
		t.Error("expected error for cancelled context")
	}
}

func TestProvisionWorkflowRole_ErrorResponses(t *testing.T) {
	withCatalog(t, map[string]ActionDef{
		"forge/run": {Name: "forge/run", RequiredPermission: &PermissionSpec{Service: "forge", Action: "createExecution", Resource: "forge/executions"}},
	})
	origURL, origKey := gatekeeperURL, gatekeeperKey
	gatekeeperKey = func() string { return "k" }
	t.Cleanup(func() { gatekeeperURL = origURL; gatekeeperKey = origKey })
	steps := []WorkflowStep{{Step: Step{Action: "forge/run"}}}

	// Non-201 status → "".
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv1.Close()
	gatekeeperURL = srv1.URL
	if rid := provisionWorkflowRole(context.Background(), "wf", "u", "o", steps); rid != "" {
		t.Errorf("non-201 should yield empty role id, got %q", rid)
	}

	// 201 with undecodable body → "".
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("{not json")) //nolint:errcheck
	}))
	defer srv2.Close()
	gatekeeperURL = srv2.URL
	if rid := provisionWorkflowRole(context.Background(), "wf", "u", "o", steps); rid != "" {
		t.Errorf("bad JSON should yield empty role id, got %q", rid)
	}
}

func TestProvisionWorkflowRole_NoKey(t *testing.T) {
	withCatalog(t, map[string]ActionDef{
		"forge/run": {Name: "forge/run", RequiredPermission: &PermissionSpec{Service: "forge", Action: "x", Resource: "y"}},
	})
	origKey := gatekeeperKey
	gatekeeperKey = func() string { return "" }
	t.Cleanup(func() { gatekeeperKey = origKey })
	if rid := provisionWorkflowRole(context.Background(), "wf", "u", "o", []WorkflowStep{{Step: Step{Action: "forge/run"}}}); rid != "" {
		t.Errorf("no key should yield empty role id, got %q", rid)
	}
}
