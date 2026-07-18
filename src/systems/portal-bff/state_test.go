package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// withConductor points the package at a stub conductor for the duration of a
// test and restores the previous value afterward.
func withConductor(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	prev := conductorURL
	conductorURL = ts.URL
	t.Cleanup(func() {
		conductorURL = prev
		ts.Close()
	})
	return ts
}

func doState(t *testing.T, method, target string, cache *stateCache) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rr := httptest.NewRecorder()
	handleState(cache)(rr, req)
	return rr
}

func TestStateGet_Empty204(t *testing.T) {
	withConductor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blueprints/state/alice/prod" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	rr := doState(t, http.MethodGet, "/api/state/alice/prod", newStateCache(time.Minute))
	var v workspaceView
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if rr.Code != 200 || !v.IsEmpty {
		t.Fatalf("expected empty view 200, got %d %+v", rr.Code, v)
	}
}

func TestStateGet_Locked423(t *testing.T) {
	withConductor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusLocked)
		_, _ = w.Write([]byte(`{"ID":"lock-1","Operation":"OperationTypeApply","Who":"bob","Version":"1.7.0","Created":"2026-07-07"}`))
	})
	rr := doState(t, http.MethodGet, "/api/state/alice/prod", newStateCache(time.Minute))
	var v workspaceView
	_ = json.Unmarshal(rr.Body.Bytes(), &v)
	if rr.Code != 200 || !v.Locked || v.Lock == nil || v.Lock.ID != "lock-1" {
		t.Fatalf("expected locked view, got %d %+v", rr.Code, v)
	}
}

func TestStateGet_State200(t *testing.T) {
	withConductor(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"terraform_version":"1.7.0","serial":9,"lineage":"lin","resources":[{"type":"aws_instance"},{"type":"aws_instance"}]}`))
	})
	rr := doState(t, http.MethodGet, "/api/state/alice/prod", newStateCache(time.Minute))
	var v workspaceView
	_ = json.Unmarshal(rr.Body.Bytes(), &v)
	if v.State == nil || v.State.ResourceCount != 2 || v.State.ResourceTypes[0].Count != 2 {
		t.Fatalf("expected populated state, got %+v", v.State)
	}
}

func TestStateGet_UpstreamErrorMirrored(t *testing.T) {
	withConductor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"denied"}`))
	})
	rr := doState(t, http.MethodGet, "/api/state/alice/prod", newStateCache(time.Minute))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected upstream 403 mirrored, got %d", rr.Code)
	}
}

func TestStateGet_CacheHitSkipsUpstream(t *testing.T) {
	calls := 0
	withConductor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"terraform_version":"1.7.0","serial":1,"lineage":"l","resources":[]}`))
	})
	cache := newStateCache(time.Minute)
	doState(t, http.MethodGet, "/api/state/alice/prod", cache)
	doState(t, http.MethodGet, "/api/state/alice/prod", cache)
	if calls != 1 {
		t.Fatalf("expected 1 upstream call (second served from cache), got %d", calls)
	}
}

func TestStateGet_TraversalRejected(t *testing.T) {
	rr := doState(t, http.MethodGet, "/api/state/alice/../../etc", newStateCache(time.Minute))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for traversal, got %d", rr.Code)
	}
}

func TestStateDelete_InvalidatesCache(t *testing.T) {
	getCalls := 0
	withConductor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		getCalls++
		_, _ = w.Write([]byte(`{"terraform_version":"1.7.0","serial":1,"lineage":"l","resources":[]}`))
	})
	cache := newStateCache(time.Minute)
	doState(t, http.MethodGet, "/api/state/alice/prod", cache)    // populates cache (getCalls=1)
	doState(t, http.MethodDelete, "/api/state/alice/prod", cache) // invalidates
	doState(t, http.MethodGet, "/api/state/alice/prod", cache)    // must refetch (getCalls=2)
	if getCalls != 2 {
		t.Fatalf("expected refetch after delete, got %d GET calls", getCalls)
	}
}
