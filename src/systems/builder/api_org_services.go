package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// scopeFromPath maps the {id} path value to a storage org id. The literal
// "default" addresses the baseline scope; anything else is a real org id.
func scopeFromPath(id string) string {
	if id == "default" || id == defaultOrgID {
		return defaultOrgID
	}
	return id
}

func handleListOrgServices(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("builder").Start(r.Context(), "handleListOrgServices")
	defer span.End()

	id := r.PathValue("id")
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listOrgServices", "builder/orgs/"+id); !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	orgID := scopeFromPath(id)

	views, err := buildEffectiveView(ctx, orgID)
	if err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "list org services: db error", "org_id", orgID, "error", err)
		http.Error(w, "failed to list services", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(views) //nolint:errcheck
}

func handleGetOrgService(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("builder").Start(r.Context(), "handleGetOrgService")
	defer span.End()

	id := r.PathValue("id")
	service := r.PathValue("service")
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getOrgService", "builder/orgs/"+id); !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	orgID := scopeFromPath(id)

	view, err := effectiveView(ctx, orgID, service)
	if err != nil {
		span.RecordError(err)
		http.Error(w, "failed to get service", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(view) //nolint:errcheck
}

// missingRequiredConfig returns the def's requiredConfig keys not satisfied by this
// request or already stored. A key is satisfied by: plain config in the request,
// DATABASE_URL via a supplied or previously-stored db_url, or a sensitive key supplied
// now (req.Secrets) or previously stored (existing.SecretsCiphertext). Config is
// replaced on each PUT, so only the request's config counts; db_url and secrets are
// kept when not re-supplied, so the stored ones count.
func missingRequiredConfig(def serviceDef, req setServiceRequest, existing OrgService, service string) ([]string, error) {
	satisfied := map[string]bool{}
	for k := range req.Config {
		satisfied[k] = true
	}
	if strings.TrimSpace(req.DBUrl) != "" || existing.DBURLCiphertext != nil {
		satisfied["DATABASE_URL"] = true
	}
	if len(req.Secrets) > 0 {
		for k := range req.Secrets {
			satisfied[k] = true
		}
	} else if existing.SecretsCiphertext != nil && secretsEncryptionEnabled() {
		// A decrypt failure is an internal error, NOT "the keys are missing" — surface
		// it so the caller returns 500 rather than wrongly blocking a re-enable whose
		// secrets are still stored (and live in the Secret) with a misleading 400.
		m, err := decryptSecretsMap(existing.SecretsCiphertext, service)
		if err != nil {
			return nil, fmt.Errorf("decrypt stored secrets for %s: %w", service, err)
		}
		for k := range m {
			satisfied[k] = true
		}
	}
	var missing []string
	for _, k := range def.RequiredConfig {
		if !satisfied[k] {
			missing = append(missing, k)
		}
	}
	return missing, nil
}

func handleSetOrgService(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("builder").Start(r.Context(), "handleSetOrgService")
	defer span.End()

	id := r.PathValue("id")
	service := strings.TrimSpace(r.PathValue("service"))
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "configureOrgService", "builder/orgs/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	orgID := scopeFromPath(id)

	if service == "" {
		http.Error(w, "service name is required", http.StatusBadRequest)
		return
	}
	if coreServices[service] {
		http.Error(w, "core services cannot be configured", http.StatusBadRequest)
		return
	}

	var req setServiceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	kind := req.Kind
	if kind == "" {
		kind = kindPlatform
	}
	if kind != kindPlatform && kind != kindCustom {
		http.Error(w, "kind must be 'platform' or 'custom'", http.StatusBadRequest)
		return
	}
	if kind == kindCustom {
		if req.Image == "" {
			http.Error(w, "custom services require an image", http.StatusBadRequest)
			return
		}
		if req.Port <= 0 || req.Port > 65535 {
			http.Error(w, "custom services require a valid port (1-65535)", http.StatusBadRequest)
			return
		}
	}

	enabled := true // enabling is the default action when the flag is omitted
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	row := OrgService{
		OrgID:       orgID,
		ServiceName: service,
		Enabled:     enabled,
		Kind:        kind,
		Config:      req.Config,
		Image:       req.Image,
		Port:        req.Port,
		Description: req.Description,
	}

	// A supplied db_url is write-only: validate, encrypt (bound to the service
	// name), and keep only a redacted host for display. Never logged or returned.
	if strings.TrimSpace(req.DBUrl) != "" {
		if !secretsEncryptionEnabled() {
			http.Error(w, "DB URL storage is disabled (BUILDER_SECRETS_KEY not set)", http.StatusServiceUnavailable)
			return
		}
		host, err := redactedDBHost(req.DBUrl)
		if err != nil {
			http.Error(w, "invalid db_url: "+err.Error(), http.StatusBadRequest)
			return
		}
		ct, err := encryptSecret(req.DBUrl, service)
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		row.DBURLCiphertext = ct
		row.DBHost = host
	}

	// Admin-supplied sensitive config (REDIS_URL, GITEA_ADMIN_TOKEN, …) is encrypted as
	// a map, bound to the service. Builder writes each entry into the service Secret.
	if len(req.Secrets) > 0 {
		if !secretsEncryptionEnabled() {
			http.Error(w, "secret storage is disabled (BUILDER_SECRETS_KEY not set)", http.StatusServiceUnavailable)
			return
		}
		ct, err := encryptSecretsMap(req.Secrets, service)
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		row.SecretsCiphertext = ct
	}

	// Validation gate: enabling a platform service in the default scope makes builder
	// deploy it, so its required connection config must be present (now or already
	// stored). Org-scope toggles only flip the access gate (they inherit the default
	// deployment), so they are not gated.
	if enabled && kind == kindPlatform && orgID == defaultOrgID {
		if def, ok := embeddedServiceDef(service); ok {
			// Read from the primary so a db_url/secret stored in a prior request is
			// seen (a lagging replica would falsely report it missing).
			existing, _ := getOrgServicePrimary(ctx, orgID, service)
			missing, err := missingRequiredConfig(def, req, existing, service)
			if err != nil {
				span.RecordError(err)
				slog.ErrorContext(ctx, "validate required config", "org_id", orgID, "service", service, "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			if len(missing) > 0 {
				http.Error(w, "missing required config before enabling "+service+": "+strings.Join(missing, ", "), http.StatusBadRequest)
				return
			}
		}
	}

	if _, err := upsertOrgService(ctx, row); err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "set org service: db error", "org_id", orgID, "service", service, "error", err)
		http.Error(w, "failed to save service config", http.StatusInternalServerError)
		return
	}

	meterServiceConfigured.Add(ctx, 1, metric.WithAttributes(attribute.String("service", service), attribute.Bool("enabled", enabled)))
	span.SetAttributes(attribute.String("org.id", orgID), attribute.String("service", service), attribute.Bool("enabled", enabled))
	slog.InfoContext(ctx, "org service configured", "org_id", orgID, "service", service, "enabled", enabled, "kind", kind, "caller_id", userID)

	// A default-scope change is the admin's deploy/destroy/reconfigure intent —
	// reconcile immediately instead of waiting for the next tick.
	if orgID == defaultOrgID {
		reconcilerNudge()
	}

	view, err := effectiveView(ctx, orgID, service)
	if err != nil {
		http.Error(w, "saved but failed to read back", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(view) //nolint:errcheck
}

func handleDeleteOrgService(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("builder").Start(r.Context(), "handleDeleteOrgService")
	defer span.End()

	id := r.PathValue("id")
	service := r.PathValue("service")
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteOrgService", "builder/orgs/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	orgID := scopeFromPath(id)

	removed, err := deleteOrgService(ctx, orgID, service)
	if err != nil {
		span.RecordError(err)
		http.Error(w, "failed to delete service config", http.StatusInternalServerError)
		return
	}
	if !removed {
		http.Error(w, "no override configured for that service", http.StatusNotFound)
		return
	}
	slog.InfoContext(ctx, "org service override removed", "org_id", orgID, "service", service, "caller_id", userID)
	if orgID == defaultOrgID {
		reconcilerNudge()
	}
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

// buildEffectiveView overlays the desired-state rows on the live service catalog
// to produce the full effective list for a scope.
func buildEffectiveView(ctx context.Context, orgID string) ([]serviceView, error) {
	views := map[string]*serviceView{}

	// 0. Core control-plane services: always present, always on, never configurable.
	// The catalog (step 1) excludes them and their rows can't be written, so seeding
	// them here is what puts them in the list — flagged Core so every consumer (the
	// admin UI, the sidebar, the CLI hub) can trust that flag instead of re-hardcoding
	// the set.
	for name := range coreServices {
		views[name] = &serviceView{Service: name, Enabled: true, Kind: kindPlatform, Source: "core", Core: true}
	}

	// 1. Seed from the registry catalog: every platform service, default-on.
	for _, c := range serviceCatalog(ctx) {
		views[c.Name] = &serviceView{
			Service:     c.Name,
			Enabled:     true,
			Kind:        kindPlatform,
			Source:      "catalog",
			Description: c.Description,
		}
	}

	// 2. Apply the default-scope baseline.
	defaults, err := listOrgServices(ctx, defaultOrgID)
	if err != nil {
		return nil, err
	}
	for _, d := range defaults {
		applyRow(views, d, "default")
	}

	// 3. Apply this org's overrides (only when not viewing the default scope).
	if orgID != defaultOrgID {
		overrides, err := listOrgServices(ctx, orgID)
		if err != nil {
			return nil, err
		}
		for _, o := range overrides {
			src := "override"
			if o.Kind == kindCustom {
				src = "custom"
			}
			applyRow(views, o, src)
		}
	}

	out := make([]serviceView, 0, len(views))
	for _, v := range views {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

func applyRow(views map[string]*serviceView, row OrgService, source string) {
	v, exists := views[row.ServiceName]
	if !exists {
		v = &serviceView{Service: row.ServiceName}
		views[row.ServiceName] = v
	}
	v.Enabled = row.Enabled
	v.Kind = row.Kind
	v.Source = source
	v.Config = row.Config
	if row.Image != "" {
		v.Image = row.Image
	}
	if row.Port != 0 {
		v.Port = row.Port
	}
	if row.Description != "" {
		v.Description = row.Description
	}
	if len(row.DBURLCiphertext) > 0 {
		v.DBConfigured = true
		v.DBHost = row.DBHost
	}
}

// redactedDBHost validates a Postgres URL and returns a credential-free
// "host:port/db" summary for display. The full URL is never stored or returned.
func redactedDBHost(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("scheme must be postgres:// or postgresql://")
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host")
	}
	return u.Host + u.Path, nil
}

// effectiveView resolves the effective state of a single service for a scope.
func effectiveView(ctx context.Context, orgID, service string) (serviceView, error) {
	if coreServices[service] {
		return serviceView{Service: service, Enabled: true, Kind: kindPlatform, Source: "core", Core: true}, nil
	}
	v := serviceView{Service: service, Enabled: true, Kind: kindPlatform, Source: "catalog"}

	if d, err := getOrgService(ctx, defaultOrgID, service); err == nil {
		applyRow(map[string]*serviceView{service: &v}, d, "default")
	} else if !isNotFound(err) {
		return serviceView{}, err
	}
	if orgID != defaultOrgID {
		if o, err := getOrgService(ctx, orgID, service); err == nil {
			src := "override"
			if o.Kind == kindCustom {
				src = "custom"
			}
			applyRow(map[string]*serviceView{service: &v}, o, src)
		} else if !isNotFound(err) {
			return serviceView{}, err
		}
	}
	return v, nil
}
