package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"
)

// envKeyRe matches POSIX-compliant environment variable names.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// blockedEnvKeys is an explicit denylist of names that could redirect interpreter
// execution or dynamic linker behaviour in user containers.
var blockedEnvKeys = map[string]bool{
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "LD_AUDIT": true,
	"PYTHONSTARTUP": true, "PYTHONPATH": true,
	"NODE_OPTIONS": true, "NODE_PATH": true,
	"RUBYOPT": true, "RUBYLIB": true,
	"PERL5LIB": true, "PERLLIB": true,
	"JAVA_TOOL_OPTIONS": true, "JAVA_OPTIONS": true, "_JAVA_OPTIONS": true,
	"DYLD_INSERT_LIBRARIES": true, "DYLD_LIBRARY_PATH": true,
}

func validateEnvKeys(env map[string]string) error {
	for k := range env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("invalid env key %q: must match [A-Za-z_][A-Za-z0-9_]*", k)
		}
		if blockedEnvKeys[strings.ToUpper(k)] {
			return fmt.Errorf("env key %q is not permitted", k)
		}
	}
	return nil
}

// maxOutputEnv caps how many env vars an execution may capture as output.
const maxOutputEnv = 32

// validateOutputEnv checks the output_env names are valid POSIX identifiers (they
// are injected into the capture shell loop) and bounds the count.
func validateOutputEnv(names []string) error {
	if len(names) > maxOutputEnv {
		return fmt.Errorf("output_env: at most %d variables may be captured", maxOutputEnv)
	}
	for _, k := range names {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("invalid output_env name %q: must match [A-Za-z_][A-Za-z0-9_]*", k)
		}
	}
	return nil
}

// allowedImages is nil when ALLOWED_IMAGES is not configured → deny all submissions.
// allowedImageList is the same set, deduplicated and sorted, served by GET /images
// so clients (e.g. the CLI's forge TUI) can offer the permitted images for selection.
var (
	allowedImages    map[string]bool
	allowedImageList []string
)

func initAllowedImages(raw string) {
	if raw == "" {
		allowedImages = nil // deny-all when not configured
		allowedImageList = nil
		slog.Warn("ALLOWED_IMAGES is not set — all image submissions will be rejected; set ALLOWED_IMAGES to a comma-separated list of permitted images")
		return
	}
	allowedImages = make(map[string]bool)
	allowedImageList = nil
	for _, img := range splitTrim(raw) {
		if !allowedImages[img] {
			allowedImageList = append(allowedImageList, img)
		}
		allowedImages[img] = true
	}
	sort.Strings(allowedImageList)
}

// handleListImages returns the configured image allowlist (deduplicated, sorted).
// An unconfigured allowlist (deny-all) returns an empty array rather than null.
func handleListImages(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleListImages")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "listImage", "forge/images")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("permission.granted")

	images := allowedImageList
	if images == nil {
		images = []string{}
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "list images: success", "user_id", userID, "count", len(images))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(images) //nolint:errcheck
}

func splitTrim(s string) []string {
	parts := make([]string, 0)
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			parts = append(parts, t)
		}
	}
	return parts
}

// checkGatekeeper calls gatekeeper's /check_permissions endpoint with the Bearer
// token from the incoming request. It returns the user_id and true when the
// caller is authorised; it writes an HTTP error and returns false otherwise.
// Forge calls gatekeeper directly so that auth is enforced even if a compromised
// conductor strips or forges the X-User-ID header.
func checkGatekeeper(ctx context.Context, w http.ResponseWriter, r *http.Request, action, resource string) (string, bool) {
	userID, _, ok := checkGatekeeperOrg(ctx, w, r, action, resource)
	return userID, ok
}

// checkGatekeeperOrg is checkGatekeeper that also returns the caller's org ID
// (empty when the user has no org). The org is needed to resolve org-scoped
// secrets, so submit snapshots it onto the execution.
func checkGatekeeperOrg(ctx context.Context, w http.ResponseWriter, r *http.Request, action, resource string) (userID, orgID string, ok bool) {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", false
	}

	body, _ := json.Marshal(map[string]string{
		"service":  "forge",
		"resource": resource,
		"action":   action,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		slog.ErrorContext(ctx, "forge: failed to build gatekeeper request", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", "", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := forgeHTTPClient.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "forge: gatekeeper check_permissions failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", false
	}
	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		slog.ErrorContext(ctx, "forge: gatekeeper unavailable", "status", resp.StatusCode)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return "", "", false
	}

	var result struct {
		Authorized bool    `json:"authorized"`
		UserID     string  `json:"user_id"`
		OrgID      *string `json:"org_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.Authorized {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", "", false
	}

	if result.OrgID != nil {
		orgID = *result.OrgID
	}
	return result.UserID, orgID, true
}

// ── Submit ────────────────────────────────────────────────────────────────────

func handleSubmit(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleSubmit")
	defer span.End()

	userID, orgID, ok := checkGatekeeperOrg(ctx, w, r, "createExecution", "forge/executions")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("permission.granted")
	slog.InfoContext(ctx, "submit execution request", "user_id", userID)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || !json.Valid(body) {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	var req submitRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Env == nil {
		req.Env = map[string]string{}
	}
	if req.SecretRefs == nil {
		req.SecretRefs = map[string]string{}
	}

	// An image-build execution derives its image (forge's Kaniko builder)
	// and command (the assembled build invocation) from the build spec, so the usual
	// image/command/allowlist checks don't apply to it. The engine choice and the
	// privileged-runner requirement are enforced once the runner class resolves its
	// backend, below.
	isBuild := req.Build != nil
	// A volume-copy execution (the scatter-clone / gather primitive) derives its image
	// (forge's minimal runner image) and command (a synthesised `cp` script) from the
	// copy spec, so — like build — it bypasses the user image/command/allowlist checks.
	isCopy := req.Copy != nil
	// A resolve-paths execution (the scatter fan-out generator) likewise derives its
	// image and command (a synthesised find | grep) from the resolve spec.
	isResolve := req.Resolve != nil
	// An artifact transfer derives its image (forge's minimal runner) and command (a
	// synthesised tar|curl) from the spec, so like copy/resolve it bypasses the user
	// image/command/allowlist checks — the caller never picks an image just to move a
	// cache in or out.
	isArtifact := req.Artifact != nil
	// A checkout step that supplies no image of its own (the forge/git-clone action)
	// runs on forge's controlled minimal git image: like the Kaniko builder it is
	// forge-supplied and bypasses ALLOWED_IMAGES, so users never pick or maintain a
	// git-capable image just to clone a repo into a shared volume.
	isDefaultGitCheckout := !isBuild && !isCopy && !isResolve && !isArtifact && req.Checkout != nil && req.Image == ""
	switch {
	case isBuild:
		if err := validateBuild(req.Build, req.SecretRefs, req.Env); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Timeout <= 0 {
			req.Timeout = defaultBuildTimeoutSecs
		}
	case isArtifact:
		// Wire the store bearer automatically unless the caller supplied their own.
		// Forge mints one scoped to this user's artifacts alone, so save/restore needs
		// no configuration and no standing credential — and the sandbox gets authority
		// over nothing else.
		if tok := req.Artifact.tokenEnv(); req.SecretRefs[tok] == "" && req.Env[tok] == "" {
			if req.SecretRefs == nil {
				req.SecretRefs = map[string]string{}
			}
			req.SecretRefs[tok] = refSchemeToken + ":" + tokenArgArtifacts
		}
		if err := validateArtifact(req.Artifact, req.SecretRefs, req.Env, req.Volumes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Command is materialised below, once the attached volumes are validated (the
		// script cds into the workdir mount).
		req.Image = gitImage
		if req.Timeout <= 0 {
			req.Timeout = defaultTimeout
		}
	case isCopy, isResolve:
		// Command is materialised below, once the attached volumes have been shape- and
		// ownership-validated (the scripts reference their mount paths).
		req.Image = gitImage
		if req.Timeout <= 0 {
			req.Timeout = defaultTimeout
		}
	case isDefaultGitCheckout:
		if len(req.Command) == 0 {
			http.Error(w, "command is required", http.StatusBadRequest)
			return
		}
		req.Image = gitImage
		if req.Timeout <= 0 {
			req.Timeout = defaultTimeout
		}
	default:
		if req.Image == "" || len(req.Command) == 0 {
			http.Error(w, "image and command are required", http.StatusBadRequest)
			return
		}
		// allowedImages == nil means ALLOWED_IMAGES was not configured: deny all.
		if allowedImages == nil || !allowedImages[req.Image] {
			http.Error(w, "image not allowed", http.StatusBadRequest)
			return
		}
		if req.Timeout <= 0 {
			req.Timeout = defaultTimeout
		}
	}
	if req.Timeout > maxTimeout {
		req.Timeout = maxTimeout
	}
	if err := validateEnvKeys(req.Env); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateOutputEnv(req.OutputEnv); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateSecretRefs(req.SecretRefs, req.Env, orgID, userID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateCheckout(req.Checkout, req.Command, req.SecretRefs, req.Env); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateVolumeMounts(req.Volumes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Each attached volume must already exist and belong to the caller, so a job
	// cannot mount another user's workspace. The run-scoped identity that created the
	// volume is the same one that submits the steps attaching it.
	for _, m := range req.Volumes {
		vol, err := getActiveVolume(ctx, m.WorkflowID, m.Name)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, fmt.Sprintf("volume %q not found for workflow %q (create it first)", m.Name, m.WorkflowID), http.StatusBadRequest)
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "submit: volume lookup", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if vol.UserID != userID {
			http.Error(w, fmt.Sprintf("volume %q is not owned by the caller", m.Name), http.StatusForbidden)
			return
		}
	}
	// Materialise the artifact transfer now that the volume mounts are validated: the
	// script cds into the workdir mount and streams tar to/from the store.
	if isArtifact {
		req.Command = artifactCommand(req.Artifact, req.Volumes)
	}
	// Materialise the copy command now that the volume mounts are validated: it copies
	// declared paths between them (whole-tree clone, or a disjoint-checked gather union).
	if isCopy {
		if err := validateCopy(req.Copy, req.Volumes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Command = copyCommand(req.Copy, req.Volumes)
	}
	// Materialise the resolve scan and ensure its captured variable is in output_env so
	// the matched-path list is returned as the step's structured output.
	if isResolve {
		if err := validateResolve(req.Resolve, req.Volumes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Command = resolveCommand(req.Resolve, req.Volumes)
		if v := resolveOutputVar(req.Resolve); !containsString(req.OutputEnv, v) {
			req.OutputEnv = append(req.OutputEnv, v)
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
	// Snapshot the class's backend onto the execution so an admin re-pointing the
	// class after submit cannot move this already-queued job to another runtime.
	backend := rc.Backend
	if backend == "" {
		backend = "default"
	}

	// Materialise an image build: it requires a privileged runner class (root +
	// writable rootfs), which forge only honours on a kernel-isolated backend (kata or
	// gvisor) — so that one check is the whole guard. Forge builds it with Kaniko,
	// setting the forge-controlled builder image and the assembled build command, which
	// bypass the user image allowlist checked above.
	if isBuild {
		if !rc.Privileged {
			http.Error(w, "image builds require a privileged runner class (root + writable rootfs on a kata or gvisor backend)", http.StatusBadRequest)
			return
		}
		req.Image = builderImage
		req.Command = kanikoCommand(req.Build)
	}

	executionID := uuid.New().String()
	span.SetAttributes(
		attribute.String("execution.id", executionID),
		attribute.String("image", req.Image),
		attribute.String("runner_class", req.RunnerClass),
		attribute.String("backend", backend),
	)

	exec := Execution{
		ExecutionID: executionID,
		UserID:      userID,
		Image:       req.Image,
		Command:     req.Command,
		Env:         req.Env,
		TimeoutSecs: req.Timeout,
		RunnerClass: req.RunnerClass,
		Backend:     backend,
		OrgID:       orgID,
		Project:     req.Project,
		SecretRefs:  req.SecretRefs,
		OutputEnv:   req.OutputEnv,
		Checkout:    req.Checkout,
		Volumes:     req.Volumes,
		Build:       req.Build,
		Copy:        req.Copy,
		Resolve:     req.Resolve,
		Status:      StatusPending,
	}

	// If Project names a real gatekeeper project the caller can reach, file the
	// execution into it — but only if the caller may create within it (developer/admin/
	// owner). A slug that resolves to nothing stays a free-text label (unchanged
	// behaviour); a slug the caller may only view is refused rather than silently
	// downgraded to a label.
	if req.Project != "" {
		bearer := r.Header.Get("Authorization")
		if p := resolveProjectSlug(ctx, bearer, req.Project); p != nil {
			if !checkProjectPermission(ctx, bearer, "createExecution", p.Namespace, "executions", p.Slug, "") {
				span.SetStatus(codes.Ok, "")
				http.Error(w, "you cannot create executions in project "+p.Slug, http.StatusForbidden)
				return
			}
			exec.ProjectID = p.ProjectID
			exec.ProjectNamespace = p.Namespace
		}
	}

	if err := exec.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "submit: insert execution", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	meterSubmit.Add(ctx, 1, metric.WithAttributes(attribute.String("image", req.Image)))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "execution submitted", "execution_id", executionID, "user_id", userID, "image", req.Image, "runner_class", req.RunnerClass)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"execution_id": executionID})
}

// ── Get ───────────────────────────────────────────────────────────────────────

func handleGet(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleGet")
	defer span.End()

	executionID := r.PathValue("id")
	userID, orgID, ok := checkGatekeeperOrg(ctx, w, r, "getExecution", "forge/executions/"+executionID)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("execution.id", executionID),
	)
	span.AddEvent("permission.granted")
	slog.InfoContext(ctx, "get execution request", "user_id", userID, "execution_id", executionID)

	exec, err := getExecution(ctx, executionID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		span.SetStatus(codes.Error, "execution not found")
		slog.WarnContext(ctx, "get execution: not found", "user_id", userID, "execution_id", executionID)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Owner OR a member of the project the execution is filed into. A non-owner without
	// a project grant is indistinguishable from a missing row (404, not 403).
	if !authorizeExecution(ctx, r.Header.Get("Authorization"), "getExecution", exec, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		slog.WarnContext(ctx, "get execution: forbidden", "user_id", userID, "execution_id", executionID)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "get execution: success", "user_id", userID, "execution_id", executionID, "status", exec.Status)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(exec)
}

// ── List ──────────────────────────────────────────────────────────────────────

func handleList(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleList")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "listExecution", "forge/executions")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("permission.granted")
	slog.InfoContext(ctx, "list executions request", "user_id", userID)

	executions, err := listExecutions(ctx, userID, r.URL.Query().Get("project"), 100, 0, accessibleProjectIDs(ctx, r.Header.Get("Authorization")))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "list executions: success", "user_id", userID, "count", len(executions))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(executions)
}

// ── Cancel ────────────────────────────────────────────────────────────────────

func handleCancel(pool *WorkerPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("forge").Start(r.Context(), "handleCancel")
		defer span.End()

		executionID := r.PathValue("id")
		userID, orgID, ok := checkGatekeeperOrg(ctx, w, r, "deleteExecution", "forge/executions/"+executionID)
		if !ok {
			span.SetStatus(codes.Ok, "")
			return
		}
		span.SetAttributes(
			attribute.String("user.id", userID),
			attribute.String("execution.id", executionID),
		)
		span.AddEvent("permission.granted")
		slog.InfoContext(ctx, "cancel execution request", "user_id", userID, "execution_id", executionID)

		exec, err := getExecution(ctx, executionID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Error, "execution not found")
			slog.WarnContext(ctx, "cancel execution: not found", "user_id", userID, "execution_id", executionID)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.ErrorContext(ctx, "cancel execution: db error", "user_id", userID, "execution_id", executionID, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		// Owner OR a member of the execution's project holding deleteExecution on it.
		if !authorizeExecution(ctx, r.Header.Get("Authorization"), "deleteExecution", exec, userID, orgID) {
			span.SetStatus(codes.Ok, "")
			slog.WarnContext(ctx, "cancel execution: forbidden", "user_id", userID, "execution_id", executionID)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		switch exec.Status {
		case StatusCompleted, StatusFailed, StatusTimedOut, StatusCancelled:
			span.SetStatus(codes.Ok, "")
			slog.WarnContext(ctx, "cancel execution: already finished", "user_id", userID, "execution_id", executionID, "status", exec.Status)
			http.Error(w, "execution already finished", http.StatusConflict)
			return
		case StatusPending:
			if _, err := exec.Cancel(ctx); err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "db update failed")
				slog.ErrorContext(ctx, "cancel execution: db update failed", "user_id", userID, "execution_id", executionID, "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
		case StatusRunning:
			pool.Cancel(executionID)
		}

		meterCancel.Add(ctx, 1)
		span.SetStatus(codes.Ok, "")
		slog.InfoContext(ctx, "execution cancelled", "execution_id", executionID, "user_id", userID)
		w.WriteHeader(http.StatusNoContent)
	}
}
