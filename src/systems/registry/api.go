package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"golang.org/x/crypto/bcrypt"
)

// Service is a registered backend service known to the registry.
// ForwardAuth, when true, signals that conductor should forward the user's
// Authorization header when proxying requests to this service.
type Service struct {
	ServiceID   string    `json:"service_id"`
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	Description string    `json:"description"`
	ForwardAuth bool      `json:"forward_auth"`
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ServiceRole is a named role defined by a service and published in the registry.
// Gatekeeper resolves these names when evaluating permission requests.
type ServiceRole struct {
	RoleID      string    `json:"role_id"`
	ServiceID   string    `json:"service_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// ServiceEndpoint maps an HTTP method + path on a service to a gatekeeper
// action/resource pair. Public endpoints bypass permission checks entirely.
type ServiceEndpoint struct {
	EndpointID string    `json:"endpoint_id"`
	ServiceID  string    `json:"service_id"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Action     string    `json:"action"`
	Resource   string    `json:"resource"`
	Public     bool      `json:"public"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type serviceWithEndpoints struct {
	Service
	Roles         []ServiceRole         `json:"roles"`
	Endpoints     []ServiceEndpoint     `json:"endpoints"`
	DefaultGrants []ServiceDefaultGrant `json:"default_grants,omitempty"`
}

// ServiceDefaultGrant is a permission template a service declares for new
// users, orgs, or teams. Gatekeeper reads these at startup and applies them
// when creating new principals.
type ServiceDefaultGrant struct {
	GrantID     string    `json:"grant_id"`
	ServiceID   string    `json:"service_id"`
	ServiceName string    `json:"service_name"`
	GrantOn     string    `json:"grant_on"` // "user", "org", or "team"
	Actions     []string  `json:"actions"`
	Resources   []string  `json:"resources"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ServiceAction is a callable workflow action registered by a service.
// BodyTransforms and AsyncConfig are stored as raw JSONB and passed through.
// GkService/GkAction/GkResource are the gatekeeper permission triple required
// to call this action, derived from the matching service_endpoint record. Empty
// when the endpoint has no registered service_endpoint entry.
type ServiceAction struct {
	ActionID       string          `json:"action_id"`
	ServiceID      string          `json:"service_id"`
	ServiceName    string          `json:"service_name"`
	ServiceURL     string          `json:"service_url"`
	Name           string          `json:"name"`
	Method         string          `json:"method"`
	Path           string          `json:"path"`
	BodyTransforms json.RawMessage `json:"body_transforms,omitempty"`
	AsyncConfig    json.RawMessage `json:"async,omitempty"`
	Active         bool            `json:"active"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	GkService      string          `json:"gk_service,omitempty"`
	GkAction       string          `json:"gk_action,omitempty"`
	GkResource     string          `json:"gk_resource,omitempty"`
}

// resolveHost is the DNS lookup used by validateServiceURL. Tests can replace it.
var resolveHost = net.LookupHost

// jsonbOrNil returns nil when raw is empty or the JSON null literal,
// so that pgx inserts a SQL NULL into a JSONB column instead of the string "null".
func jsonbOrNil(raw json.RawMessage) interface{} {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return []byte(raw)
}

// jsonbBytes returns nil when raw is empty or the JSON null literal, else the
// raw bytes. Used where a []byte field is required instead of interface{}.
func jsonbBytes(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return []byte(raw)
}

// hashServiceKey bcrypt-hashes a plaintext service key for storage.
// Returns an empty string and no error when key is empty (no key configured).
func hashServiceKey(key string) (string, error) {
	if key == "" {
		return "", nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(key), 12)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// seedAccount is the bootstrap credential for one service account, captured from
// the environment at startup.
type seedAccount struct {
	key  string
	role string
}

// seedServiceKeys holds each service account's bootstrap (env-seeded) key, keyed
// by account name. The bootstrap key is accepted as a permanent fallback in
// authenticateServiceKey so a client that restarted back to its initial key — or a
// Registry that restarted before a client rotated — can always re-authenticate and
// roll the key forward again, instead of deadlocking on a rotated key neither side
// can reproduce. Populated by seedServiceAccounts during startup, before the HTTP
// server begins serving; read-only thereafter.
var seedServiceKeys = map[string]seedAccount{}

// matchesSeedKey reports whether key equals the bootstrap key seeded for name,
// returning the seeded role on a match. Pure and DB-free so it stays testable.
func matchesSeedKey(name, key string) (string, bool) {
	seed, ok := seedServiceKeys[name]
	if !ok || seed.key == "" {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(seed.key), []byte(key)) == 1 {
		return seed.role, true
	}
	return "", false
}

// authenticateServiceKey verifies a presented key for the named account. It
// accepts either the current rotated key (the bcrypt hash stored in Postgres) or
// the bootstrap key seeded from the environment, and returns the account's role.
// The bootstrap fallback is the recovery path that keeps key rotation from
// deadlocking across restarts.
func authenticateServiceKey(ctx context.Context, name, key string) (string, bool) {
	if acct, err := lookupServiceAccount(ctx, name); err == nil {
		if bcrypt.CompareHashAndPassword([]byte(acct.HashedKey), []byte(key)) == nil {
			return acct.Role, true
		}
	}
	if role, ok := matchesSeedKey(name, key); ok {
		// Audit the recovery path: the rotated key did not match and the caller fell
		// back to the long-lived bootstrap key. This is expected immediately after a
		// restart, but unexpected at any other time — alert on it so a leaked
		// bootstrap key being used in steady state is visible. Mirrors gatekeeper.
		slog.WarnContext(ctx, "service auth: bootstrap key fallback used", "service", name)
		return role, true
	}
	return "", false
}

// requireReadAuth verifies X-Service-Key against registry_service_accounts.
// Any valid service account (any role) is accepted.
// Returns the caller's service name and true on success.
func requireReadAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	return requireAuthWithRole(w, r, "")
}

// requireAdminAuth verifies X-Service-Key and requires role=admin.
// Returns the caller's service name and true on success.
func requireAdminAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	return requireAuthWithRole(w, r, "admin")
}

func requireAuthWithRole(w http.ResponseWriter, r *http.Request, requiredRole string) (string, bool) {
	header := r.Header.Get("X-Service-Key")
	if header == "" {
		http.Error(w, "missing X-Service-Key header", http.StatusUnauthorized)
		return "", false
	}
	idx := strings.Index(header, ":")
	if idx < 1 {
		http.Error(w, "invalid X-Service-Key format, expected name:key", http.StatusUnauthorized)
		return "", false
	}
	name, key := header[:idx], header[idx+1:]

	role, ok := authenticateServiceKey(r.Context(), name, key)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	if requiredRole != "" && role != requiredRole {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}
	return name, true
}

// validateServiceURL rejects URLs that target loopback, link-local, or any
// private/reserved address (IPv4 RFC-1918, IPv6 ULA fc00::/7, etc.) to prevent
// SSRF via the service registry.
func validateServiceURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	host := u.Hostname()

	// checkIP rejects any address that is loopback, link-local, or private.
	// net.IP.IsPrivate covers IPv4 RFC-1918 and IPv6 ULA (fc00::/7).
	checkIP := func(ip net.IP) error {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
			return fmt.Errorf("URL must not target a private or reserved address")
		}
		return nil
	}

	if ip := net.ParseIP(host); ip != nil {
		return checkIP(ip)
	}

	// Hostname — resolve and validate every returned address to prevent DNS-based
	// SSRF (e.g. a public domain resolving to 169.254.169.254). Fail closed: if
	// the hostname can't be resolved we reject it rather than allow it, since an
	// unresolvable name today could resolve to a private address tomorrow.
	// Internal service URLs (e.g. Docker service names) should be pre-seeded via
	// SERVICES env var or MANIFEST_FILE, which bypass this validation.
	addrs, err := resolveHost(host)
	if err != nil {
		return fmt.Errorf("hostname %q could not be resolved: %w", host, err)
	}
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil {
			continue
		}
		if err := checkIP(ip); err != nil {
			return fmt.Errorf("hostname %q resolves to blocked address %s: %w", host, addr, err)
		}
	}
	return nil
}

// handleListActions returns all active workflow actions joined with their service URLs.
func handleListActions(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleListActions")
	defer span.End()

	caller, ok := requireReadAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	actions, err := listAllActions(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "list actions: query", "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if actions == nil {
		actions = []ServiceAction{}
	}

	span.SetAttributes(attribute.Int("actions.count", len(actions)))
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(actions) //nolint:errcheck
}

// handleListServices returns all active services with their endpoint manifests.
func handleListServices(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleListServices")
	defer span.End()

	caller, ok := requireReadAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	result, err := listServicesWithEndpoints(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "list services: query", "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetAttributes(attribute.Int("services.count", len(result)))
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleCreateService registers a new service entry (admin key required).
func handleCreateService(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleCreateService")
	defer span.End()

	caller, ok := requireAdminAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	var req struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		Description string `json:"description"`
		ForwardAuth bool   `json:"forward_auth"`
		ServiceKey  string `json:"service_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.URL == "" {
		span.SetStatus(codes.Error, "bad request")
		http.Error(w, "name and url are required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("service.name", req.Name))

	if err := validateServiceURL(req.URL); err != nil {
		span.SetStatus(codes.Error, "invalid url")
		http.Error(w, "invalid url: "+err.Error(), http.StatusBadRequest)
		return
	}

	hashedKey, err := hashServiceKey(req.ServiceKey)
	if err != nil {
		slog.ErrorContext(ctx, "create service: hash key", "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "hash key failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	id := uuid.New().String()
	newSvcModel := ServiceModel{
		ServiceID:   id,
		Name:        req.Name,
		URL:         req.URL,
		Description: req.Description,
		ForwardAuth: req.ForwardAuth,
		ServiceKey:  hashedKey,
	}
	if err := newSvcModel.Add(ctx); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			span.SetStatus(codes.Error, "service already registered")
			http.Error(w, "service already registered", http.StatusConflict)
			return
		}
		slog.ErrorContext(ctx, "create service: db", "error", err, "service", req.Name)
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(attribute.String("service.id", id))

	fetchedRow, err := (ServiceModel{ServiceID: id}).Get(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "create service: fetch after insert", "error", err, "service_id", id)
		span.RecordError(err)
		span.SetStatus(codes.Error, "fetch after insert failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	fetchedModel := fetchedRow.(ServiceModel)
	svc := Service{
		ServiceID:   fetchedModel.ServiceID,
		Name:        fetchedModel.Name,
		URL:         fetchedModel.URL,
		Description: fetchedModel.Description,
		ForwardAuth: fetchedModel.ForwardAuth,
		Active:      fetchedModel.Active,
		CreatedAt:   fetchedModel.CreatedAt,
		UpdatedAt:   fetchedModel.UpdatedAt,
	}

	slog.InfoContext(ctx, "service registered", "service", svc.Name)
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(svc)
}

// handleDeleteService soft-deletes a service by ID (admin key required).
func handleDeleteService(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleDeleteService")
	defer span.End()

	caller, ok := requireAdminAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	id := r.PathValue("id")
	span.SetAttributes(attribute.String("service.id", id))

	if err := (ServiceModel{ServiceID: id}).Remove(ctx); err != nil {
		if isDbNotFound(err) {
			span.SetStatus(codes.Error, "not found")
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(ctx, "delete service: db", "error", err, "service_id", id)
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateServiceEndpoints replaces the full endpoint manifest for a service
// (admin key required). Used to seed endpoint definitions without self-registration.
func handleUpdateServiceEndpoints(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleUpdateServiceEndpoints")
	defer span.End()

	caller, ok := requireAdminAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	id := r.PathValue("id")
	span.SetAttributes(attribute.String("service.id", id))

	var req struct {
		URL           string               `json:"url"`
		Description   string               `json:"description"`
		Roles         []manifestRoleSpec   `json:"roles"`
		Endpoints     []manifestEndpointSpec `json:"endpoints"`
		Actions       []manifestActionEntry `json:"actions"`
		DefaultGrants []manifestDefaultGrant `json:"default_grants"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.SetStatus(codes.Error, "bad request")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.URL != "" {
		if err := validateServiceURL(req.URL); err != nil {
			span.SetStatus(codes.Error, "invalid url")
			http.Error(w, "invalid url: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	if err := replaceServiceManifest(ctx, id, req.URL, req.Description,
		req.Roles, req.Endpoints, req.Actions, req.DefaultGrants); err != nil {
		if isDbNotFound(err) {
			span.SetStatus(codes.Error, "not found")
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(ctx, "update endpoints: db", "error", err, "service_id", id)
		span.RecordError(err)
		span.SetStatus(codes.Error, "db failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleListDefaultGrants returns all active default grants across all services,
// enriched with the service name so callers don't need a second lookup.
// Requires a valid read service key.
func handleListDefaultGrants(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleListDefaultGrants")
	defer span.End()

	caller, ok := requireReadAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	grants, err := listAllDefaultGrants(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "list default grants: db", "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if grants == nil {
		grants = []ServiceDefaultGrant{}
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(grants) //nolint:errcheck
}

// ── System health ─────────────────────────────────────────────────────────────

var registryHTTPClient = &http.Client{
	Transport: otelhttp.NewTransport(http.DefaultTransport),
	Timeout:   5 * time.Second,
}

var (
	healthMu    sync.RWMutex
	healthCache map[string]serviceHealth
)

type serviceHealth struct {
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// startHealthCollector queries active services from the DB every 30 seconds,
// calls /healthz on each concurrently, and caches the results for handleSystemHealth.
func startHealthCollector(ctx context.Context) {
	collect := func() {
		cctx, cspan := otel.Tracer("registry").Start(ctx, "health.collect")
		defer cspan.End()

		rawSvcs, err := queryActiveServiceURLs(cctx)
		if err != nil {
			slog.WarnContext(cctx, "health collector: db query failed", "error", err)
			return
		}
		type svc struct{ name, url string }
		svcs := make([]svc, len(rawSvcs))
		for i, s := range rawSvcs {
			svcs[i] = svc{name: s.Name, url: s.URL}
		}

		results := make(map[string]serviceHealth, len(svcs))
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, s := range svcs {
			wg.Add(1)
			go func(name, svcURL string) {
				defer wg.Done()
				hctx, cancel := context.WithTimeout(cctx, 5*time.Second)
				defer cancel()
				now := time.Now().UTC()
				req, err := http.NewRequestWithContext(hctx, http.MethodGet, svcURL+"/healthz", nil)
				if err != nil {
					mu.Lock()
					results[name] = serviceHealth{Status: "unhealthy", Error: err.Error(), CheckedAt: now}
					mu.Unlock()
					return
				}
				resp, err := registryHTTPClient.Do(req)
				if err != nil {
					mu.Lock()
					results[name] = serviceHealth{Status: "unhealthy", Error: err.Error(), CheckedAt: now}
					mu.Unlock()
					return
				}
				resp.Body.Close()
				mu.Lock()
				if resp.StatusCode == http.StatusOK {
					results[name] = serviceHealth{Status: "healthy", CheckedAt: now}
				} else {
					results[name] = serviceHealth{Status: "unhealthy", Error: fmt.Sprintf("status %d", resp.StatusCode), CheckedAt: now}
				}
				mu.Unlock()
			}(s.name, s.url)
		}
		wg.Wait()

		healthMu.Lock()
		healthCache = results
		healthMu.Unlock()
	}

	go func() {
		collect()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				collect()
			}
		}
	}()
}

func handleSystemHealth(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer("registry").Start(r.Context(), "handleSystemHealth")
	defer span.End()

	caller, ok := requireReadAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	healthMu.RLock()
	cache := healthCache
	healthMu.RUnlock()

	overall := "healthy"
	for _, h := range cache {
		if h.Status != "healthy" {
			overall = "degraded"
			break
		}
	}

	span.SetAttributes(attribute.String("health.status", overall))
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct { //nolint:errcheck
		Status   string                   `json:"status"`
		Services map[string]serviceHealth `json:"services"`
	}{Status: overall, Services: cache})
}
