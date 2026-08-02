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

	// Every failure must report an ERROR, never ("", nil). An empty role id with no
	// error means "this workflow needs no permissions", and the callers persist that —
	// so a transient gatekeeper failure reported that way silently downgrades every
	// future run to the owner's full session permissions, permanently.

	// Non-201 status → error.
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv1.Close()
	gatekeeperURL = srv1.URL
	if rid, err := provisionWorkflowRole(context.Background(), "wf", "u", "o", steps, nil, nil); err == nil || rid != "" {
		t.Errorf("non-201 should be an error, got (%q, %v)", rid, err)
	}

	// 201 with undecodable body → error.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("{not json")) //nolint:errcheck
	}))
	defer srv2.Close()
	gatekeeperURL = srv2.URL
	if rid, err := provisionWorkflowRole(context.Background(), "wf", "u", "o", steps, nil, nil); err == nil || rid != "" {
		t.Errorf("bad JSON should be an error, got (%q, %v)", rid, err)
	}

	// 201 naming no role → error too: the permissions were requested, so an empty id
	// here is as unusable as a failure and must not read as "no role needed".
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"role_id":""}`)) //nolint:errcheck
	}))
	defer srv3.Close()
	gatekeeperURL = srv3.URL
	if rid, err := provisionWorkflowRole(context.Background(), "wf", "u", "o", steps, nil, nil); err == nil || rid != "" {
		t.Errorf("empty role_id should be an error, got (%q, %v)", rid, err)
	}
}

func TestProvisionWorkflowRole_NoKey(t *testing.T) {
	withCatalog(t, map[string]ActionDef{
		"forge/run": {Name: "forge/run", RequiredPermission: &PermissionSpec{Service: "forge", Action: "x", Resource: "y"}},
	})
	origKey := gatekeeperKey
	gatekeeperKey = func() string { return "" }
	t.Cleanup(func() { gatekeeperKey = origKey })
	// An unconfigured service key is a misconfiguration, not "no role needed": no run
	// token could be minted with it either, so it is surfaced rather than absorbed.
	if rid, err := provisionWorkflowRole(context.Background(), "wf", "u", "o", []WorkflowStep{{Step: Step{Action: "forge/run"}}}, nil, nil); err == nil || rid != "" {
		t.Errorf("no key should be an error, got (%q, %v)", rid, err)
	}
}
