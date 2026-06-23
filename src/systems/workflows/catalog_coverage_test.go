package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefreshCatalog(t *testing.T) {
	// Snapshot + restore the catalog globals so other tests see a clean state.
	actionCatalogMu.Lock()
	origCatalog := actionCatalog
	actionCatalogMu.Unlock()
	serviceURLsMu.Lock()
	origURLs, origNames, origHosts := serviceURLs, catalogServiceNames, hostToService
	serviceURLsMu.Unlock()
	t.Cleanup(func() {
		actionCatalogMu.Lock()
		actionCatalog = origCatalog
		actionCatalogMu.Unlock()
		serviceURLsMu.Lock()
		serviceURLs, catalogServiceNames, hostToService = origURLs, origNames, origHosts
		serviceURLsMu.Unlock()
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/actions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name":"forge/run","service_name":"forge","service_url":"http://forge:8083","method":"POST","path":"/executions","gk_service":"forge","gk_action":"createExecution","gk_resource":"forge/executions"}]`)) //nolint:errcheck
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("REGISTRY_SERVICE_KEY", "k")

	refreshCatalog(context.Background())

	actionCatalogMu.RLock()
	def, ok := actionCatalog["forge/run"]
	actionCatalogMu.RUnlock()
	if !ok {
		t.Fatal("forge/run not in catalog after refresh")
	}
	if def.RequiredPermission == nil || def.RequiredPermission.Action != "createExecution" {
		t.Errorf("permission triple not mapped: %+v", def.RequiredPermission)
	}
}

func TestRefreshCatalog_SkipsWithoutConfig(t *testing.T) {
	t.Setenv("REGISTRY_URL", "")
	refreshCatalog(context.Background()) // must be a no-op, not panic
}

func TestExecuteAction_Sync(t *testing.T) {
	pool := &WorkerPool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result":"ok"}`)) //nolint:errcheck
	}))
	defer srv.Close()
	def := ActionDef{Name: "x/run", ServiceURL: srv.URL, Method: http.MethodPost, Path: "/run"}
	res, err := pool.executeAction(context.Background(), newTokenStore("tok", "sess"), def, map[string]any{"a": "b"})
	if err != nil {
		t.Fatalf("executeAction: %v", err)
	}
	if !strings.Contains(res.Output, "ok") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestExecuteAction_Sync_Non2xx(t *testing.T) {
	pool := &WorkerPool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	def := ActionDef{Name: "x/run", ServiceURL: srv.URL, Method: http.MethodPost, Path: "/run"}
	if _, err := pool.executeAction(context.Background(), newTokenStore("", ""), def, nil); err == nil {
		t.Error("expected error on non-2xx")
	}
}

func TestExecuteAction_AsyncPoll(t *testing.T) {
	pool := &WorkerPool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"execution_id":"job-1"}`)) //nolint:errcheck
			return
		}
		// poll → completed
		w.Write([]byte(`{"status":"completed","stdout":"async-output"}`)) //nolint:errcheck
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
	res, err := pool.executeAction(context.Background(), newTokenStore("", ""), def, map[string]any{"run": "echo hi"})
	if err != nil {
		t.Fatalf("async executeAction: %v", err)
	}
	if res.Output != "async-output" {
		t.Errorf("output = %q, want async-output", res.Output)
	}
}
