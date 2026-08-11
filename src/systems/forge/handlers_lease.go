package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// leaseRuntime resolves a backend to a runtime that can hold a sandbox open. A
// backend that cannot say so explicitly, rather than silently falling back to
// one-shot behaviour: a caller who asked for a lease and got per-command sandboxes
// would see their second command run against none of the first's work.
func leaseRuntime(ctx context.Context, reg *runtimeRegistry, backend string) (LeaseRuntime, error) {
	rt, err := reg.Get(ctx, backend)
	if err != nil {
		return nil, err
	}
	lr, ok := rt.(LeaseRuntime)
	if !ok {
		return nil, fmt.Errorf("backend %q does not support leases", backend)
	}
	return lr, nil
}

// ── Create ───────────────────────────────────────────────────────────────────

// handleCreateLease boots a sandbox and holds it open for the caller.
//
// The boot happens in the background and the handler returns immediately with a
// starting lease, matching how create-volume already works. Blocking the request
// for the boot would be simpler, but it ties up a connection for the seconds a
// microVM takes and — worse — a client that disconnects mid-boot leaves a sandbox
// nobody is waiting for. Returning a row the caller polls means the lease is
// tracked from the instant it exists, so the reaper can collect it whatever the
// caller does next.
func handleCreateLease(reg *runtimeRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("forge").Start(r.Context(), "handleCreateLease")
		defer span.End()

		userID, orgID, ok := checkGatekeeperOrg(ctx, w, r, "createLease", "forge/leases")
		if !ok {
			span.SetStatus(codes.Ok, "")
			return
		}
		span.SetAttributes(attribute.String("user.id", userID))
		span.AddEvent("permission.granted")

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil || !json.Valid(body) {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		var req createLeaseRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		if err := validateLeaseRequest(&req); err != nil {
			span.SetStatus(codes.Error, "validation failed")
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// A lease runs whatever image it was given for as long as it is held, so the
		// image allowlist matters at least as much here as it does for one command.
		if allowedImages == nil || !allowedImages[req.Image] {
			http.Error(w, "image not allowed", http.StatusBadRequest)
			return
		}
		if err := validateEnvKeys(req.Env); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateSecretRefs(req.SecretRefs, req.Env, orgID, userID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateVolumeMounts(req.Volumes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, m := range req.Volumes {
			vol, verr := getActiveVolume(ctx, m.WorkflowID, m.Name)
			if errors.Is(verr, gorm.ErrRecordNotFound) {
				http.Error(w, fmt.Sprintf("volume %q not found for workflow %q (create it first)", m.Name, m.WorkflowID), http.StatusBadRequest)
				return
			}
			if verr != nil {
				slog.ErrorContext(ctx, "create lease: volume lookup", "error", verr)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			if vol.UserID != userID {
				http.Error(w, fmt.Sprintf("volume %q is not owned by the caller", m.Name), http.StatusForbidden)
				return
			}
		}

		if req.RunnerClass == "" {
			req.RunnerClass = "standard"
		}
		rc, err := runnerClassSpec(ctx, req.RunnerClass)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		backend := rc.Backend
		if backend == "" {
			backend = "default"
		}
		// Resolve the runtime before writing the row, so "this backend cannot hold a
		// sandbox" is a clean 400 rather than a lease that exists only to fail.
		lr, err := leaseRuntime(ctx, reg, backend)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// A held sandbox is reserved memory for as long as it lives, so the quota is
		// checked here and not merely at dispatch: without it a caller could hold the
		// cluster by creating leases and never using them, which the per-execution
		// concurrency limits would never notice because nothing is running.
		held, err := activeLeaseCount(ctx, userID)
		if err != nil {
			slog.ErrorContext(ctx, "create lease: count active", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if maxLeasesPerUser > 0 && held >= int64(maxLeasesPerUser) {
			http.Error(w, fmt.Sprintf("you already hold %d leases (the limit); release one first", held), http.StatusTooManyRequests)
			return
		}

		lease := Lease{
			LeaseID:         uuid.New().String(),
			UserID:          userID,
			OrgID:           orgID,
			Image:           req.Image,
			RunnerClass:     req.RunnerClass,
			Backend:         backend,
			Env:             req.Env,
			SecretRefs:      req.SecretRefs,
			Volumes:         req.Volumes,
			Checkout:        req.Checkout,
			Project:         req.Project,
			Status:          leaseStarting,
			IdleTimeoutSecs: req.IdleTimeout,
			MaxLifetimeSecs: req.MaxLifetime,
			CreatedAt:       time.Now().UTC(),
		}
		if req.Project != "" {
			bearer := r.Header.Get("Authorization")
			if p := resolveProjectSlug(ctx, bearer, req.Project); p != nil {
				if !checkProjectPermission(ctx, bearer, "createExecution", "executions", p.Slug, "") {
					span.SetStatus(codes.Ok, "")
					http.Error(w, "you cannot create executions in project "+p.Slug, http.StatusForbidden)
					return
				}
				lease.ProjectID = p.ProjectID
				lease.ProjectNamespace = p.Namespace
			}
		}

		if err := lease.Add(ctx); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.ErrorContext(ctx, "create lease: insert", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		go startLeaseAsync(lease, lr)

		span.SetAttributes(attribute.String("lease.id", lease.LeaseID), attribute.String("backend", backend))
		span.SetStatus(codes.Ok, "")
		slog.InfoContext(ctx, "lease created", "lease_id", lease.LeaseID, "user_id", userID, "image", lease.Image, "runner_class", lease.RunnerClass)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"lease_id": lease.LeaseID, "status": leaseStarting}) //nolint:errcheck
	}
}

// startLeaseAsync boots the sandbox and records the outcome on the row.
//
// It runs detached from the request, so it deliberately does not inherit the
// request context — the caller having hung up is not a reason to abandon a sandbox
// mid-creation, which is exactly how one gets orphaned. Its own timeout bounds it
// instead.
func startLeaseAsync(lease Lease, lr LeaseRuntime) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(leaseStartTimeoutSecs)*time.Second)
	defer cancel()

	// Credentials are resolved once, here, and injected into the sandbox for its whole
	// life. Only the references were persisted; the values are not written back to the
	// row and never logged, matching the one-shot path.
	creds, err := resolveLeaseCredentials(ctx, lease)
	if err != nil {
		failLease(ctx, lease, fmt.Sprintf("resolve credentials: %v", err))
		return
	}
	started := lease
	if len(creds) > 0 {
		merged := make(map[string]string, len(lease.Env)+len(creds))
		for k, v := range lease.Env {
			merged[k] = v
		}
		for k, v := range creds {
			merged[k] = v
		}
		started.Env = merged
	}

	if err := lr.StartLease(ctx, started); err != nil {
		failLease(ctx, lease, err.Error())
		return
	}
	moved, err := markLeaseReady(ctx, lease.LeaseID)
	if err != nil {
		slog.ErrorContext(ctx, "lease: mark ready", "lease_id", lease.LeaseID, "error", err)
		return
	}
	if !moved {
		// Released or reaped while it was booting. The sandbox is real and nothing
		// else is going to tear it down — the releaser saw a starting lease and had no
		// sandbox to stop yet — so it is this path's job to clean up.
		slog.InfoContext(ctx, "lease was released while starting; tearing its sandbox down", "lease_id", lease.LeaseID)
		if err := lr.StopLease(ctx, lease); err != nil {
			slog.ErrorContext(ctx, "lease: stop after lost race", "lease_id", lease.LeaseID, "error", err)
		}
		return
	}
	slog.InfoContext(ctx, "lease ready", "lease_id", lease.LeaseID)
}

// failLease records why a lease never came up. StartLease has already torn down
// whatever it created, so there is no sandbox left to stop here.
func failLease(ctx context.Context, lease Lease, detail string) {
	slog.ErrorContext(ctx, "lease failed to start", "lease_id", lease.LeaseID, "detail", detail)
	if _, err := finishLease(ctx, lease.LeaseID, leaseFailed, detail); err != nil {
		slog.ErrorContext(ctx, "lease: record failure", "lease_id", lease.LeaseID, "error", err)
	}
}

// ── Read ─────────────────────────────────────────────────────────────────────

// handleGetLease reports a lease's state. This is what a caller polls after create
// to learn the sandbox is ready.
func handleGetLease(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleGetLease")
	defer span.End()

	id := r.PathValue("id")
	userID, ok := checkGatekeeper(ctx, w, r, "getLease", "forge/leases")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	lease, err := getLease(ctx, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		http.Error(w, "lease not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "get lease", "lease_id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if lease.UserID != userID {
		// 404 rather than 403: a lease id the caller does not own should not be
		// confirmable as existing.
		http.Error(w, "lease not found", http.StatusNotFound)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(lease) //nolint:errcheck
}

// handleListLeases lists the caller's own leases.
func handleListLeases(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleListLeases")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "listLease", "forge/leases")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	rows, err := (Lease{UserID: userID}).List(ctx, 100, 0)
	if err != nil {
		slog.ErrorContext(ctx, "list leases", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	leases := make([]Lease, len(rows))
	for i, row := range rows {
		leases[i] = row.(Lease)
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"leases": leases}) //nolint:errcheck
}

// ── Release ──────────────────────────────────────────────────────────────────

// handleReleaseLease tears a lease's sandbox down.
//
// Releasing is the normal end of a lease and callers should always do it — the
// timeouts exist for callers that cannot, not as the expected path. Every second
// between the last command and the release is memory nobody is using.
func handleReleaseLease(reg *runtimeRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("forge").Start(r.Context(), "handleReleaseLease")
		defer span.End()

		id := r.PathValue("id")
		userID, ok := checkGatekeeper(ctx, w, r, "deleteLease", "forge/leases")
		if !ok {
			span.SetStatus(codes.Ok, "")
			return
		}
		lease, err := getLease(ctx, id)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "lease not found", http.StatusNotFound)
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "release lease: load", "lease_id", id, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if lease.UserID != userID {
			http.Error(w, "lease not found", http.StatusNotFound)
			return
		}

		if err := releaseLease(ctx, reg, lease, "released by "+userID); err != nil {
			slog.ErrorContext(ctx, "release lease", "lease_id", id, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		span.SetStatus(codes.Ok, "")
		w.WriteHeader(http.StatusNoContent)
	}
}

// releaseLease moves a lease to stopped and tears its sandbox down. Shared by the
// explicit release and the reaper.
//
// The row is moved FIRST and the sandbox stopped only if that move won. Ordering it
// this way means two releasers cannot both call StopLease, and — more importantly —
// a crash between the two leaves a stopped row with a live sandbox, which the
// orphan sweep will collect. The opposite order would leave an active row pointing
// at a sandbox that no longer exists, and every exec against it would fail
// confusingly until the idle timeout eventually fired.
func releaseLease(ctx context.Context, reg *runtimeRegistry, lease Lease, detail string) error {
	moved, err := finishLease(ctx, lease.LeaseID, leaseStopped, detail)
	if err != nil {
		return err
	}
	if !moved {
		return nil // already terminal; whoever moved it owns the teardown
	}
	lr, err := leaseRuntime(ctx, reg, lease.Backend)
	if err != nil {
		// The row is already stopped, so the quota is freed either way. Log rather
		// than fail: a backend that vanished cannot be asked to clean up, and the
		// caller can do nothing with the error.
		slog.WarnContext(ctx, "lease: no runtime to stop sandbox on", "lease_id", lease.LeaseID, "backend", lease.Backend, "error", err)
		return nil
	}
	if err := lr.StopLease(ctx, lease); err != nil {
		slog.ErrorContext(ctx, "lease: stop sandbox", "lease_id", lease.LeaseID, "error", err)
	}
	return nil
}
