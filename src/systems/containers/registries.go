package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// ctxRegistryKey is the context key for the upstream registry a request is
// proxied to. Set by the OCI handler before invoking the reverse proxy.
type ctxRegistryKey struct{}

// newRegistryClient builds an HTTP client bound to a single upstream registry.
func newRegistryClient(rg Registry) *registryClient {
	return &registryClient{
		baseURL:  strings.TrimRight(rg.URL, "/"),
		username: rg.Username,
		password: rg.Password,
		http:     httpClient,
	}
}

// errNoRegistry is returned when no registry is configured yet — the service
// boots without one and an admin must add at least one before push/pull works.
var errNoRegistry = errors.New("no container registry configured")

// ── default-registry cache ──────────────────────────────────────────────────
// /v2 blob traffic hits the default registry on every request (a single push
// can be hundreds of chunks), so the default row is cached briefly. Writes to
// any registry bust the cache immediately.

var (
	defRegMu      sync.RWMutex
	defRegCache   *registryClient
	defRegExpires time.Time
)

const defRegTTL = 5 * time.Second

func invalidateRegistryCache() {
	defRegMu.Lock()
	defRegCache = nil
	defRegExpires = time.Time{}
	defRegMu.Unlock()
}

// resolveDefaultRegistry returns a client for the default registry, cached for
// a few seconds. Returns errNoRegistry when none is configured.
func resolveDefaultRegistry(ctx context.Context) (*registryClient, error) {
	defRegMu.RLock()
	if defRegCache != nil && time.Now().Before(defRegExpires) {
		c := defRegCache
		defRegMu.RUnlock()
		return c, nil
	}
	defRegMu.RUnlock()

	rg, err := getDefaultRegistry(ctx)
	if err != nil {
		if errors.Is(err, errRegistryNotFound) {
			return nil, errNoRegistry
		}
		return nil, err
	}
	c := newRegistryClient(rg)

	defRegMu.Lock()
	defRegCache = c
	defRegExpires = time.Now().Add(defRegTTL)
	defRegMu.Unlock()
	return c, nil
}

// resolveRegistry returns a client for the named registry, or — when name is
// empty — the default registry. Read APIs accept a ?registry=<name> selector;
// the OCI proxy always uses the default.
func resolveRegistry(ctx context.Context, name string) (*registryClient, error) {
	if name == "" {
		return resolveDefaultRegistry(ctx)
	}
	rg, err := getRegistryByName(ctx, name)
	if err != nil {
		return nil, err // errRegistryNotFound when the named registry is absent
	}
	return newRegistryClient(rg), nil
}

// seedRegistryFromEnv creates a "default" registry from the legacy
// REGISTRY_URL / REGISTRY_USERNAME / REGISTRY_PASSWORD env vars when set and no
// registries exist yet. This keeps existing env-configured deployments working
// while making runtime configuration of additional registries possible. It is
// best-effort: failures are logged, never fatal, so the service still boots.
func seedRegistryFromEnv(ctx context.Context) {
	url := secret("REGISTRY_URL")
	if url == "" {
		return
	}
	n, err := countRegistries(ctx)
	if err != nil {
		slog.Warn("registry seed: count failed, skipping", "error", err)
		return
	}
	if n > 0 {
		return // already configured — never override an operator's runtime set
	}
	rg := Registry{
		ID:        newID(),
		Name:      envOrDefault("REGISTRY_NAME", "default"),
		URL:       strings.TrimRight(url, "/"),
		Username:  secret("REGISTRY_USERNAME"),
		Password:  secret("REGISTRY_PASSWORD"),
		IsDefault: true,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := addRegistryTx(ctx, rg); err != nil {
		slog.Warn("registry seed: insert failed", "name", rg.Name, "error", err)
		return
	}
	slog.Info("seeded default registry from REGISTRY_URL", "name", rg.Name, "url", rg.URL)
}
