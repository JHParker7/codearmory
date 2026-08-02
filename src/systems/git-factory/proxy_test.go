package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// seedShardPlacement maps a shard's PRIMARY onto a node address in the routing table.
func seedShardPlacement(t *testing.T, shard, addr string) {
	t.Helper()
	if err := connect().Create(&ShardNode{Shard: shard, Address: addr, Role: rolePrimary, Healthy: true}).Error; err != nil {
		t.Fatalf("seed shard node: %v", err)
	}
}

// seedReplica adds a replica node to a shard's placement.
func seedReplica(t *testing.T, shard, addr string) {
	t.Helper()
	if err := connect().Create(&ShardNode{Shard: shard, Address: addr, Role: roleReplica, Healthy: true}).Error; err != nil {
		t.Fatalf("seed replica: %v", err)
	}
}

func TestIsTrustedForward(t *testing.T) {
	t.Setenv("GIT_NODE_FORWARD_KEY", "node-key")
	mk := func(v string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/x/y/info/refs", nil)
		if v != "" {
			r.Header.Set(forwardHeader, v)
		}
		return r
	}
	if isTrustedForward(mk("")) {
		t.Error("no header trusted")
	}
	if isTrustedForward(mk("wrong")) {
		t.Error("wrong key trusted")
	}
	if !isTrustedForward(mk("node-key")) {
		t.Error("correct key not trusted")
	}
	t.Setenv("GIT_NODE_FORWARD_KEY", "")
	if isTrustedForward(mk("anything")) {
		t.Error("unset key must trust nothing")
	}
}

// A repo whose shard is placed on another node has its wire request reverse-proxied
// there, carrying the trust header, with the path preserved.
func TestWireProxy_ForwardsRemoteRepoToNode(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")
	t.Setenv("GIT_NODE_FORWARD_KEY", "node-key")

	var gotPath, gotFwd string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotFwd = r.URL.Path, r.Header.Get(forwardHeader)
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, "PROXIED-ADVERTISEMENT")
	}))
	defer node.Close()

	id := uuid.New().String()
	seedRepo(t, id, "user-1", "admin", "remote-repo", "")
	seedShardPlacement(t, shardOf(id), node.URL)

	rec := infoRefs(t, "/admin/remote-repo.git", string(svcUploadPack), "Bearer tok")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "PROXIED-ADVERTISEMENT") {
		t.Fatalf("status=%d body=%q, want the proxied node response", rec.Code, rec.Body.String())
	}
	if gotFwd != "node-key" {
		t.Errorf("node saw forward header %q, want node-key", gotFwd)
	}
	if !strings.HasSuffix(gotPath, "/admin/remote-repo.git/info/refs") {
		t.Errorf("node saw path %q, want the wire path preserved", gotPath)
	}
}

// A local repo (no placement row) is served locally, never proxied.
func TestWireProxy_LocalRepoNotProxied(t *testing.T) {
	setupTestDB(t)
	id := uuid.New().String()
	re := seedRepo(t, id, "user-1", "admin", "local-repo", "")
	req := httptest.NewRequest(http.MethodGet, "/admin/local-repo.git/info/refs?service="+string(svcUploadPack), nil)
	if maybeProxyToNode(httptest.NewRecorder(), req, re, svcUploadPack) {
		t.Fatal("a local repo must not be proxied")
	}
}

// A trusted forward is served from local disk WITHOUT a gatekeeper round-trip — the
// entry node already authorized it. No gatekeeper stub is set here on purpose.
func TestTrustedForward_ServesWithoutGatekeeper(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	t.Setenv("GIT_NODE_FORWARD_KEY", "node-key")

	id := "aa000000-0000-0000-0000-000000000001"
	re := seedRepo(t, id, "user-1", "admin", "fwd-repo", "")
	if err := CreateGitRepo(context.Background(), re); err != nil {
		t.Fatalf("create bare repo: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/fwd-repo.git/info/refs?service="+string(svcUploadPack), nil)
	req.Header.Set(forwardHeader, "node-key")
	rec := httptest.NewRecorder()
	gitMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trusted forward: status = %d, want 200 (served without gatekeeper)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "service=git-upload-pack") {
		t.Errorf("body %q missing the upload-pack advertisement", rec.Body.String())
	}
}

// A trusted forward for a mirror is still refused a push — defence in depth on the node.
func TestTrustedForward_MirrorStillReadOnly(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	t.Setenv("GIT_NODE_FORWARD_KEY", "node-key")
	t.Setenv("GIT_FACTORY_INTERNAL_KEY", testInternalKey)

	re := ensureMirror(t, "admin", "fwd-mirror")
	_ = re
	req := httptest.NewRequest(http.MethodPost, "/admin/fwd-mirror.git/"+string(svcReceivePack), strings.NewReader(""))
	req.Header.Set(forwardHeader, "node-key")
	rec := httptest.NewRecorder()
	gitMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("push to mirror via forward: status = %d, want 403", rec.Code)
	}
}
