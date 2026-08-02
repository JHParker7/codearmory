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

// A transient empty response from the registry (mid-restart / still ingesting)
// must not wipe a populated catalog — otherwise forge/run vanishes from running
// workflows until the next 5-minute poll.
func TestRefreshCatalog_EmptyResponseKeepsExistingCatalog(t *testing.T) {
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

	body := `[{"name":"forge/run","service_name":"forge","service_url":"http://forge:8083","method":"POST","path":"/executions","gk_service":"forge","gk_action":"createExecution","gk_resource":"forge/executions"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body)) //nolint:errcheck
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("REGISTRY_SERVICE_KEY", "k")

	// First refresh populates the catalog.
	refreshCatalog(context.Background())
	if catalogSize() == 0 {
		t.Fatal("catalog should be populated after the first refresh")
	}

	// A subsequent empty response must be ignored, not applied.
	body = `[]`
	refreshCatalog(context.Background())

	actionCatalogMu.RLock()
	_, ok := actionCatalog["forge/run"]
	n := len(actionCatalog)
	actionCatalogMu.RUnlock()
	if !ok {
		t.Fatalf("empty registry response wiped the catalog (size now %d); forge/run must be retained", n)
	}
}

func TestExecuteAction_Sync(t *testing.T) {
	pool := &WorkerPool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result":"ok"}`)) //nolint:errcheck
	}))
	defer srv.Close()
	def := ActionDef{Name: "x/run", ServiceURL: srv.URL, Method: http.MethodPost, Path: "/run"}
	res, err := pool.executeAction(context.Background(), newTokenStore("tok", "sess"), def, map[string]any{"a": "b"}, "", 0)
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
	if _, err := pool.executeAction(context.Background(), newTokenStore("", ""), def, nil, "", 0); err == nil {
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
	res, err := pool.executeAction(context.Background(), newTokenStore("", ""), def, map[string]any{"run": "echo hi"}, "", 0)
	if err != nil {
		t.Fatalf("async executeAction: %v", err)
	}
	if res.Output != "async-output" {
		t.Errorf("output = %q, want async-output", res.Output)
	}
}

// A {param} spliced into an action's path comes from an already-substituted With value,
// so it can be a previous step's OUTPUT — untrusted with respect to the URL. Spliced
// raw, a value carrying '/', '?' or '#' rewrote the request path or appended a query,
// on a request that still carries the run's bearer token. executeHTTP has always
// rejected ".."; executeAction must not disagree with it about what a safe path is.
func TestExecuteAction_PathParamIsEscapedAndTraversalRejected(t *testing.T) {
	pool := &WorkerPool{}
	var gotEscapedPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscapedPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`)) //nolint:errcheck
	}))
	defer srv.Close()
	def := ActionDef{Name: "tickets/update", ServiceURL: srv.URL, Method: http.MethodPut, Path: "/tickets/{id}"}

	// Traversal is refused outright, and the request is never sent.
	if _, err := pool.executeAction(context.Background(), newTokenStore("tok", "s"), def,
		map[string]any{"id": "../../internal/admin"}, "", 0); err == nil {
		t.Fatal("expected traversal in a path parameter to be rejected")
	}
	if gotEscapedPath != "" {
		t.Fatalf("a traversing request was still sent, to %q", gotEscapedPath)
	}

	// A value carrying URL syntax stays ONE path segment: no extra path, no query.
	if _, err := pool.executeAction(context.Background(), newTokenStore("tok", "s"), def,
		map[string]any{"id": "abc/close?force=true"}, "", 0); err != nil {
		t.Fatalf("executeAction: %v", err)
	}
	if gotEscapedPath != "/tickets/abc%2Fclose%3Fforce=true" {
		t.Errorf("path parameter was not escaped: got %q", gotEscapedPath)
	}
	if gotQuery != "" {
		t.Errorf("path parameter injected a query string: %q", gotQuery)
	}
}
