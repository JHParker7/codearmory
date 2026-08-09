package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// Replication (ARCHITECTURE §5 Step 4). A write lands on the PRIMARY; afterwards the
// primary bumps the repo's Version and pushes the new packs to each replica by asking it
// to fetch. A replica records the Version it reached (ReplicaState.Applied), and a read
// is routed to it only once Applied >= Version (pickReadNode) — so a replica is never
// served a clone that predates the push that produced it. Replication is best-effort: a
// replica that is down just stays behind and is skipped for reads until it catches up.

// bumpRepoVersion atomically increments a repo's Version and returns the new value.
func bumpRepoVersion(ctx context.Context, id string) (int64, error) {
	if err := connect().WithContext(ctx).Model(&Repo{}).Where("id = ?", id).
		Update("version", gorm.Expr("version + 1")).Error; err != nil {
		return 0, err
	}
	var re Repo
	if err := connectRead().WithContext(ctx).Select("version").Where("id = ?", id).First(&re).Error; err != nil {
		return 0, err
	}
	return re.Version, nil
}

// replicateRepo pushes a repo's current state to every replica in its shard by asking
// each to fetch from this (primary) node. Best-effort and synchronous-but-cheap: a fetch
// with nothing new to pull is a fast negotiation. selfNodeAddress must be set for a
// replica to reach back — without it (single-node/dev) there are no replicas anyway.
func replicateRepo(ctx context.Context, re Repo, version int64) {
	self := selfNodeAddress()
	if self == "" {
		return
	}
	nodes, err := resolvePlacement(ctx, shardOf(re.ID))
	if err != nil {
		slog.WarnContext(ctx, "replicate: placement lookup failed", "repo_id", re.ID, "error", err)
		return
	}
	primaryURL := self + "/" + re.Namespace + "/" + re.Name + ".git"
	for _, n := range nodes {
		if n.Role != roleReplica || n.Address == "" {
			continue
		}
		body := map[string]any{"repo_id": re.ID, "primary_url": primaryURL, "version": version}
		raw, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Address+"/internal/replicate", bytes.NewReader(raw))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(forwardHeader, nodeForwardKey())
		resp, err := httpClient.Do(req)
		if err != nil || resp.StatusCode/100 != 2 {
			status := 0
			if resp != nil {
				status = resp.StatusCode
				resp.Body.Close()
			}
			slog.WarnContext(ctx, "replicate: replica did not accept", "repo_id", re.ID, "replica", n.Address, "status", status, "error", err)
			continue
		}
		resp.Body.Close()
	}
}

type replicateRequest struct {
	RepoID     string `json:"repo_id"`
	PrimaryURL string `json:"primary_url"`
	Version    int64  `json:"version"`
}

// handleReplicate runs on a REPLICA: it fetches the repo from the primary into its local
// bare copy and records the version it reached. Authenticated by the node forward key,
// the same trust boundary as a proxied wire request.
func handleReplicate(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleReplicate")
	defer span.End()

	if !isTrustedForward(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req replicateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RepoID == "" || req.PrimaryURL == "" {
		http.Error(w, "repo_id and primary_url are required", http.StatusBadRequest)
		return
	}
	// primary_url becomes a `git fetch` argv element — see validateFetchURL (mirror.go).
	if err := validateFetchURL(req.PrimaryURL); err != nil {
		http.Error(w, "primary_url is not a valid remote", http.StatusBadRequest)
		return
	}
	if err := fetchFromPrimary(ctx, req.RepoID, req.PrimaryURL); err != nil {
		slog.ErrorContext(ctx, "replicate: fetch from primary failed", "repo_id", req.RepoID, "error", err)
		http.Error(w, "fetch from primary failed", http.StatusBadGateway)
		return
	}
	if err := recordApplied(ctx, req.RepoID, selfNodeAddress(), req.Version); err != nil {
		slog.ErrorContext(ctx, "replicate: record applied failed", "repo_id", req.RepoID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "replicate: caught up", "repo_id", req.RepoID, "version", req.Version)
	w.WriteHeader(http.StatusNoContent)
}

// fetchFromPrimary mirrors the repo from the primary's wire endpoint into this node's
// bare repo, carrying the node forward key so the primary trusts the read.
func fetchFromPrimary(ctx context.Context, repoID, primaryURL string) error {
	dir, err := repoDiskPath(repoID)
	if err != nil {
		return err
	}
	if !dirExists(dir) {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		if err := exec.CommandContext(ctx, gitBinary, "-C", dir, "init", "--bare").Run(); err != nil {
			return err
		}
	}
	// primaryURL arrives in a request body and lands in git's argv, where a value
	// starting with a dash is parsed as an OPTION rather than a remote —
	// "--upload-pack=<cmd>" being one git executes. Screen it, and pass "--" so the
	// exec is safe even if this check is ever relaxed. See validateFetchURL (mirror.go).
	if err := validateFetchURL(primaryURL); err != nil {
		return fmt.Errorf("replicate fetch: %w", err)
	}
	args := []string{"-c", "http.extraHeader=" + forwardHeader + ": " + nodeForwardKey(),
		"-C", dir, "fetch", "--prune", "--force", "--", primaryURL}
	args = append(args, mirrorRefspecs...)
	cmd := exec.CommandContext(ctx, gitBinary, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.ErrorContext(ctx, "replicate: git fetch", "repo_id", repoID, "output", string(out), "error", err)
		return err
	}
	return nil
}

// recordApplied upserts a replica's caught-up version for a repo.
func recordApplied(ctx context.Context, repoID, address string, version int64) error {
	return connect().WithContext(ctx).Save(&ReplicaState{
		RepoID: repoID, Address: address, Applied: version, UpdatedAt: time.Now().UTC(),
	}).Error
}
