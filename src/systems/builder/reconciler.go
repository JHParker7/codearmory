package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// The reconciler turns the instance admin's desired state — the services enabled
// in the default/admin scope — into running workloads. It is the single-namespace,
// instance-level "deploy what you enable" controller: only a platform admin can
// write the default scope, so only they drive deploys. Core services are never
// reconciled (they are the control plane the reconciler runs on).
type reconciler struct {
	backend  clusterBackend
	interval time.Duration

	mu     sync.Mutex
	wakeCh chan struct{}
}

func newReconciler(backend clusterBackend, interval time.Duration) *reconciler {
	return &reconciler{backend: backend, interval: interval, wakeCh: make(chan struct{}, 1)}
}

// globalReconciler is set by startReconciler when reconciliation is enabled, so a
// default-scope write can trigger an immediate reconcile via reconcilerNudge.
var globalReconciler *reconciler

// reconcilerNudge asks the running reconciler to reconcile now. No-op when
// reconciliation is disabled.
func reconcilerNudge() {
	if globalReconciler != nil {
		globalReconciler.nudge()
	}
}

// startReconciler starts the reconcile loop when BUILDER_RECONCILE is set truthy.
// Otherwise it is a no-op: the service runs as a pure control-plane API. It reads
// the target namespace/prefix/image settings from the environment and connects to
// the cluster (in-cluster config, or KUBECONFIG for out-of-cluster use).
func startReconciler(ctx context.Context) {
	if !reconcileEnabled() {
		slog.InfoContext(ctx, "reconciler disabled (set BUILDER_RECONCILE=true to deploy enabled services)")
		return
	}
	namespace := envOrDefault("BUILDER_TARGET_NAMESPACE", os.Getenv("POD_NAMESPACE"))
	prefix := envOrDefault("BUILDER_RELEASE_PREFIX", "codearmory")
	imgRegistry := envOrDefault("BUILDER_IMAGE_REGISTRY", "ghcr.io/code-armory-app")
	imgTag := envOrDefault("BUILDER_IMAGE_TAG", "latest")

	backend, err := newK8sBackend(namespace, prefix, imgRegistry, imgTag)
	if err != nil {
		slog.ErrorContext(ctx, "reconciler: failed to connect to cluster — running without it", "error", err)
		return
	}
	// Dynamic provisioning: register the gatekeeper identity + synthesize the Secret
	// so a service deploys with no Helm change. Active only when the internal key is
	// present (provisioningOn also requires it). On by default; BUILDER_PROVISION=false
	// disables it (e.g. when services are pre-provisioned by the chart).
	backend.prov = provisioningConfig{
		enabled:             provisionEnabled(),
		gatekeeperURL:       strings.TrimRight(gatekeeperURL, "/"),
		internalKey:         builderInternalKey,
		conductorSecretName: envOrDefault("CONDUCTOR_SECRET_NAME", prefix+"-conductor"),
	}
	if backend.provisioningOn() {
		slog.InfoContext(ctx, "dynamic provisioning enabled (gatekeeper identity + secret)")
	}
	backend.dbcfg = globalDBConfig
	// Availability defaults for managed workloads: a replica floor (≥2 to survive a
	// pod loss) and an optional periodic rolling restart that restores pods to the
	// image. Per-service config knobs override these.
	backend.defaultReplicas = defaultReplicaFloor()
	backend.rotateInterval = defaultRotateInterval()
	if v := strings.TrimSpace(os.Getenv("BUILDER_IMAGE_PULL_SECRETS")); v != "" {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				backend.imagePullSecrets = append(backend.imagePullSecrets, s)
			}
		}
	}
	if backend.defaultReplicas > 0 || backend.rotateInterval > 0 {
		slog.InfoContext(ctx, "workload availability defaults", "replicas", backend.defaultReplicas, "rotate_interval", backend.rotateInterval)
	}
	interval := reconcileInterval()
	globalReconciler = newReconciler(backend, interval)
	go globalReconciler.run(ctx)
}

func reconcileEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BUILDER_RECONCILE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// provisionEnabled defaults to true (dynamic provisioning is the point of the
// reconciler); set BUILDER_PROVISION=false to disable, e.g. when the chart
// pre-provisions every service's identity and Secret.
func provisionEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BUILDER_PROVISION"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

func reconcileInterval() time.Duration {
	if v := os.Getenv("BUILDER_RECONCILE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

// defaultReplicaFloor is the replica count applied to every managed workload without
// its own override. BUILDER_DEFAULT_REPLICAS; 0 (unset/invalid) leaves it at 1.
func defaultReplicaFloor() int32 {
	if v := strings.TrimSpace(os.Getenv("BUILDER_DEFAULT_REPLICAS")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			return int32(n)
		}
	}
	return 0
}

// defaultRotateInterval is the periodic rolling-restart cadence applied to every
// managed workload without its own override. BUILDER_ROTATE_INTERVAL is a Go duration
// ("30m", "1h") or a bare number of minutes ("45"); empty/invalid disables rotation.
// It shares parseRotateInterval with the per-service config knob so both paths agree
// on units and the same sanity cap.
func defaultRotateInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("BUILDER_ROTATE_INTERVAL")); v != "" {
		return parseRotateInterval(v)
	}
	return 0
}

// nudge requests an out-of-band reconcile (e.g. right after an admin write) without
// blocking the caller. Coalesced: a pending nudge is not duplicated.
func (r *reconciler) nudge() {
	select {
	case r.wakeCh <- struct{}{}:
	default:
	}
}

// run drives the reconcile loop until ctx is cancelled: on the interval ticker and
// whenever nudged.
func (r *reconciler) run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	slog.InfoContext(ctx, "reconciler started", "interval", r.interval)
	r.reconcileLogged(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reconcileLogged(ctx)
		case <-r.wakeCh:
			r.reconcileLogged(ctx)
		}
	}
}

func (r *reconciler) reconcileLogged(ctx context.Context) {
	if err := r.reconcileOnce(ctx); err != nil {
		slog.ErrorContext(ctx, "reconcile failed", "error", err)
	}
}

// reconcileOnce drives one pass: ensure every desired workload exists and tear down
// any builder-managed workload that is no longer desired.
func (r *reconciler) reconcileOnce(ctx context.Context) error {
	ctx, span := otel.Tracer("builder").Start(ctx, "reconcile")
	defer span.End()

	desired, err := desiredWorkloads(ctx)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("load desired state: %w", err)
	}
	err = r.applyDesired(ctx, desired)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	return err
}

// applyDesired ensures every desired workload exists and tears down any
// builder-managed workload no longer desired. Split from reconcileOnce so the diff
// logic is unit-tested with a fake backend, without a database.
func (r *reconciler) applyDesired(ctx context.Context, desired map[string]workloadSpec) error {
	var firstErr error
	for _, spec := range desired {
		if err := r.backend.EnsureService(ctx, spec); err != nil {
			slog.ErrorContext(ctx, "ensure service failed", "service", spec.Service, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	managed, err := r.backend.ListManaged(ctx)
	if err != nil {
		return fmt.Errorf("list managed: %w", err)
	}
	for _, service := range managed {
		if _, ok := desired[service]; ok {
			continue
		}
		if err := r.backend.RemoveService(ctx, service); err != nil {
			slog.ErrorContext(ctx, "remove service failed", "service", service, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			slog.InfoContext(ctx, "torn down service no longer enabled", "service", service)
		}
	}
	return firstErr
}

// desiredWorkloads reads the default-scope rows and returns the workload spec for
// every enabled non-core service, keyed by service name.
func desiredWorkloads(ctx context.Context) (map[string]workloadSpec, error) {
	rows, err := listOrgServices(ctx, defaultOrgID)
	if err != nil {
		return nil, err
	}
	out := map[string]workloadSpec{}
	for _, row := range rows {
		// Never deploy core (chart-shipped) or coming-soon (not-in-repo, images not
		// built yet) services, even if a stale enabled row survives.
		if coreServices[row.ServiceName] || comingSoonServices[row.ServiceName] || !row.Enabled {
			continue
		}
		out[row.ServiceName] = specFromRow(row)
	}
	return out, nil
}

func specFromRow(row OrgService) workloadSpec {
	replicas, rotate, rest := extractDeployKnobs(row.Config)
	spec := workloadSpec{
		Service:        row.ServiceName,
		Image:          row.Image,
		Tag:            row.Tag,
		PullPolicy:     row.PullPolicy,
		Port:           int32(row.Port),
		Env:            configToEnv(rest),
		Replicas:       replicas,
		RotateInterval: rotate,
		DBBackend:      globalDBConfig.backendFor(row.Config),
	}
	if len(row.DBURLCiphertext) > 0 && secretsEncryptionEnabled() {
		if dbURL, err := decryptSecret(row.DBURLCiphertext, row.ServiceName); err == nil {
			spec.DBUrl = dbURL
		} else {
			slog.Error("failed to decrypt service db url", "service", row.ServiceName, "error", err)
		}
	}
	if len(row.SecretsCiphertext) > 0 && secretsEncryptionEnabled() {
		if m, err := decryptSecretsMap(row.SecretsCiphertext, row.ServiceName); err == nil {
			spec.Secrets = m
		} else {
			slog.Error("failed to decrypt service secrets", "service", row.ServiceName, "error", err)
		}
	}
	return spec
}

// Reserved config keys that shape the Deployment itself rather than the container
// env. They are pulled out before configToEnv so they never leak in as env vars.
const (
	cfgReplicas       = "replicas"
	cfgRotateInterval = "rotateInterval"
	cfgDBBackend      = "dbBackend"
)

// extractDeployKnobs splits the reserved deployment knobs (replicas, rotateInterval)
// out of the free-form config and returns the remaining keys for configToEnv. A
// per-service knob overrides the backend default; an absent/zero knob falls back to
// it. The original map is never mutated.
func extractDeployKnobs(config map[string]any) (int32, time.Duration, map[string]any) {
	if len(config) == 0 {
		return 0, 0, config
	}
	var replicas int32
	var rotate time.Duration
	rest := make(map[string]any, len(config))
	for k, v := range config {
		switch k {
		case cfgReplicas:
			replicas = parseReplicas(v)
		case cfgRotateInterval:
			rotate = parseRotateInterval(v)
		case cfgDBBackend:
			// selects the db backend (see dbConfig.backendFor); not a container env var
		default:
			rest[k] = v
		}
	}
	return replicas, rotate, rest
}

// parseReplicas reads a replica count from a JSON number or a numeric string;
// anything invalid or negative yields 0 (use the default). Capped at a sane ceiling.
func parseReplicas(v any) int32 {
	var n int64 = -1
	switch val := v.(type) {
	case float64:
		n = int64(val)
	case int:
		n = int64(val)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
			n = int64(parsed)
		}
	}
	if n < 0 {
		return 0
	}
	if n > 100 {
		n = 100
	}
	return int32(n)
}

// maxRotateInterval caps any configured cadence so a wild value can't overflow the
// int64-nanosecond Duration — which would wrap negative (silently disabling rotation)
// or wrap small-positive (rolling the pods on nearly every reconcile).
const maxRotateInterval = 720 * time.Hour // 30 days

// parseRotateInterval reads a cadence from a Go duration string ("30m", "1h") or a
// number interpreted as minutes (a bare numeric string counts as minutes too, so the
// env and per-service config paths agree). Invalid or non-positive values yield 0
// (disabled); anything above maxRotateInterval is clamped to it.
func parseRotateInterval(v any) time.Duration {
	var d time.Duration
	switch val := v.(type) {
	case string:
		s := strings.TrimSpace(val)
		if parsed, err := time.ParseDuration(s); err == nil {
			d = parsed
		} else if mins, err := strconv.Atoi(s); err == nil {
			d = minutesToDuration(float64(mins))
		}
	case float64:
		d = minutesToDuration(val)
	case int:
		d = minutesToDuration(float64(val))
	}
	if d <= 0 {
		return 0
	}
	if d > maxRotateInterval {
		return maxRotateInterval
	}
	return d
}

// minutesToDuration converts a minute count to a Duration without overflowing int64
// nanoseconds: a count beyond the cap is returned as the cap, non-positive as 0.
func minutesToDuration(mins float64) time.Duration {
	if mins <= 0 {
		return 0
	}
	if mins > float64(maxRotateInterval/time.Minute) {
		return maxRotateInterval
	}
	return time.Duration(mins) * time.Minute
}

// configToEnv flattens the free-form config object to env overrides: scalar values
// pass through as strings; arrays/objects are JSON-encoded. Config keys are env var
// names, so an admin tunes a service by setting the env vars it already reads.
func configToEnv(config map[string]any) map[string]string {
	if len(config) == 0 {
		return nil
	}
	env := make(map[string]string, len(config))
	for k, v := range config {
		switch val := v.(type) {
		case string:
			env[k] = val
		case bool:
			env[k] = fmt.Sprintf("%t", val)
		case float64:
			env[k] = trimFloat(val)
		case nil:
			// skip null values
		default:
			if b, err := json.Marshal(val); err == nil {
				env[k] = string(b)
			}
		}
	}
	return env
}

// trimFloat renders a JSON number without a trailing ".0" so integers stay integers
// in env values (e.g. 600, not 600.000000).
func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}
