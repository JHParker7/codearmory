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

// builderResource is the fixed RBAC resource every builder endpoint checks. It is
// org-independent — builder manages one global baseline, and only the system admin
// (whose wildcard grant matches anything) holds access; no per-org grant exists.
//
// Owned by the INSTANCE, hence the codearmory/ prefix: the default-org baseline every
// org inherits belongs to the deployment, not to any user. That prefix is not
// decoration — gatekeeper prepends the CALLER's username to any resource that does not
// already name an owner, so the bare "builder/orgs/default" evaluated as
// "<caller>/builder/orgs/default", a different string per caller for what is one
// global object. It worked only because the admin's wildcard matches anything; the
// moment a non-wildcard grant was written against it, it would have been per-user.
//
// Must stay byte-identical to the resource the registry manifests declare for these
// endpoints, and to what any pipeline declares when it grants itself setOrgServiceImage
// — conductor checks the manifest's string, builder re-checks with this one.
const builderResource = "codearmory/builder/orgs/default"

func handleListOrgServices(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("builder").Start(r.Context(), "handleListOrgServices")
	defer span.End()

	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listOrgServices", builderResource); !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	views, err := buildEffectiveView(ctx)
	if err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "list services: db error", "error", err)
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

	service := r.PathValue("service")
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getOrgService", builderResource); !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	view, err := effectiveView(ctx, service)
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
// dbSatisfied marks DATABASE_URL satisfied regardless of a stored/supplied db_url: true
// when a foreign-Secret backend (cnpg/external) provides it structurally, or the sql
// backend just provisioned and stored a derived URL on the row being saved.
func missingRequiredConfig(def serviceDef, req setServiceRequest, existing OrgService, service string, dbSatisfied bool) ([]string, error) {
	satisfied := map[string]bool{}
	for k := range req.Config {
		satisfied[k] = true
	}
	if dbSatisfied || strings.TrimSpace(req.DBUrl) != "" || existing.DBURLCiphertext != nil {
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

	service := strings.TrimSpace(r.PathValue("service"))
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "configureOrgService", builderResource)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	orgID := defaultOrgID

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

	// Coming-soon services (source not in this repo) can't be deployed yet; refuse to
	// enable them. A disable request still passes through so a previously-enabled row
	// can be turned off.
	if enabled && comingSoonServices[service] {
		http.Error(w, "service is coming soon and cannot be enabled yet", http.StatusBadRequest)
		return
	}

	row := OrgService{
		OrgID:       orgID,
		ServiceName: service,
		Enabled:     enabled,
		Kind:        kind,
		Config:      req.Config,
		Image:       req.Image,
		Registry:    req.Registry,
		Tag:         req.Tag,
		PullPolicy:  req.PullPolicy,
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

	// Validation + provisioning gate: enabling a platform service in the default scope
	// makes builder deploy it, so its database + required config must be in place. Org-
	// scope toggles only flip the access gate (they inherit the default deployment), so
	// they are not gated.
	if enabled && kind == kindPlatform && orgID == defaultOrgID {
		if def, ok := embeddedServiceDef(service); ok {
			// Read from the primary so a db_url/secret stored in a prior request is
			// seen (a lagging replica would falsely report it missing).
			existing, _ := getOrgServicePrimary(ctx, orgID, service)
			backend := globalDBConfig.backendFor(req.Config)

			// sql backend: provision the per-service database now (one-shot) and store
			// the derived URL like a manual one, so the reconciler treats it as manual
			// thereafter. Skipped when the admin supplied an explicit db_url this request,
			// or a URL is already stored and no new maintenance URL is given.
			if backend == dbBackendSQL && len(row.DBURLCiphertext) == 0 {
				maint := strings.TrimSpace(req.MaintenanceDBUrl)
				if maint == "" {
					maint = globalDBConfig.sqlMaintenanceURL
				}
				if maint != "" || len(existing.DBURLCiphertext) == 0 {
					if maint == "" {
						http.Error(w, "sql db backend requires a maintenance_db_url (or BUILDER_DB_SQL_MAINTENANCE_URL)", http.StatusBadRequest)
						return
					}
					if !secretsEncryptionEnabled() {
						http.Error(w, "DB URL storage is disabled (BUILDER_SECRETS_KEY not set)", http.StatusServiceUnavailable)
						return
					}
					derived, err := provisionSQLDatabase(ctx, maint, service, globalDBConfig)
					if err != nil {
						span.RecordError(err)
						slog.ErrorContext(ctx, "sql db provisioning failed", "service", service, "error", err)
						http.Error(w, "database provisioning failed: "+err.Error(), http.StatusBadGateway)
						return
					}
					host, _ := redactedDBHost(derived)
					ct, encErr := encryptSecret(derived, service)
					if encErr != nil {
						http.Error(w, "internal server error", http.StatusInternalServerError)
						return
					}
					row.DBURLCiphertext = ct
					row.DBHost = host
					slog.InfoContext(ctx, "provisioned database for service", "service", service, "db_host", host, "caller_id", userID)
				}
			}

			// cnpg/external deliver DATABASE_URL via an operator/external-secrets Secret,
			// so it is satisfied structurally; sql satisfies it via the row just stored.
			dbSatisfied := backendUsesForeignSecret(backend) || len(row.DBURLCiphertext) > 0
			missing, err := missingRequiredConfig(def, req, existing, service, dbSatisfied)
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

	view, err := effectiveView(ctx, service)
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

	service := r.PathValue("service")
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteOrgService", builderResource)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	orgID := defaultOrgID

	removed, err := deleteOrgService(ctx, orgID, service)
	if err != nil {
		span.RecordError(err)
		http.Error(w, "failed to delete service config", http.StatusInternalServerError)
		return
	}
	if !removed {
		http.Error(w, "no baseline row configured for that service", http.StatusNotFound)
		return
	}
	slog.InfoContext(ctx, "service baseline removed", "service", service, "caller_id", userID)
	reconcilerNudge()
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

// buildEffectiveView overlays the desired-state rows on the live service catalog
// to produce the full effective list for the global baseline.
func buildEffectiveView(ctx context.Context) ([]serviceView, error) {
	defaults, err := listOrgServices(ctx, defaultOrgID)
	if err != nil {
		return nil, err
	}
	return mergeViews(serviceCatalog(ctx), liveServiceNames(ctx), defaults), nil
}

// mergeViews resolves the effective service list from its inputs. It is pure (no DB
// or registry I/O) so the precedence — core → registry-live → catalog → baseline row
// — is unit-tested directly. Precedence, low to high:
//
//  0. Core control-plane services: always present, always on, never configurable.
//     The catalog excludes them and their rows can't be written, so seeding them here
//     is what puts them in the list — flagged Core so every consumer (the admin UI,
//     the sidebar, the CLI hub) can trust that flag instead of re-hardcoding the set.
//  1. Catalog services. Seeded ENABLED only when the registry — the source of truth
//     for what conductor routes — advertises them (live), otherwise OFF. This keeps
//     the view honest for services registered out-of-band with no builder baseline
//     row (e.g. forge/workflows, which the chart ships and registers directly)
//     instead of claiming they're disabled while they're live and routable.
//  2. Baseline rows: the admin's explicit desired state, which overrides everything.
func mergeViews(catalog []catalogEntry, live map[string]bool, defaults []OrgService) []serviceView {
	views := map[string]*serviceView{}

	for name := range coreServices {
		views[name] = &serviceView{Service: name, Enabled: true, Kind: kindPlatform, Source: "core", Core: true}
	}

	for _, c := range catalog {
		if coreServices[c.Name] {
			continue
		}
		v := newCatalogView(c)
		if live[c.Name] {
			v.Enabled = true
			v.Source = "registry"
		}
		views[c.Name] = v
	}

	for _, d := range defaults {
		applyRow(views, d, "default")
	}

	// Coming-soon services (source not in this repo) can't be deployed yet, so force
	// them disabled and flag them regardless of any catalog/registry/baseline state.
	for name, v := range views {
		if comingSoonServices[name] {
			v.Enabled = false
			v.ComingSoon = true
		}
	}

	out := make([]serviceView, 0, len(views))
	for _, v := range views {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// newCatalogView builds the default view for a catalog (platform) service that has
// no baseline row. It is default-OFF on purpose: a catalog service builder could
// deploy is only enabled once an admin enables it (a baseline row with Enabled=true,
// applied by applyRow — that is also what the reconciler keys off in desiredWorkloads)
// OR once it actually appears in the registry's live list (mergeViews flips it on).
// Seeding default-on unconditionally would make the admin view claim every service is
// enabled while nothing is actually deployed or routable, so the portal hides all
// their tabs — the exact contradiction we avoid by defaulting to disabled.
func newCatalogView(c catalogEntry) *serviceView {
	return &serviceView{
		Service:     c.Name,
		Enabled:     false,
		Kind:        kindPlatform,
		Source:      "catalog",
		Description: c.Description,
	}
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
	if row.Registry != "" {
		v.Registry = row.Registry
	}
	if row.Tag != "" {
		v.Tag = row.Tag
	}
	if row.PullPolicy != "" {
		v.PullPolicy = row.PullPolicy
	}
	if row.Port != 0 {
		v.Port = row.Port
	}
	if row.Description != "" {
		v.Description = row.Description
	}
	if row.RolloutStatus != "" {
		v.RolloutStatus = row.RolloutStatus
		v.RolloutMessage = row.RolloutMessage
		if !row.RolloutAt.IsZero() {
			at := row.RolloutAt
			v.RolloutAt = &at
		}
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

// effectiveView resolves the effective state of a single service on the global
// baseline.
func effectiveView(ctx context.Context, service string) (serviceView, error) {
	if coreServices[service] {
		return serviceView{Service: service, Enabled: true, Kind: kindPlatform, Source: "core", Core: true}, nil
	}
	// Default-OFF, but the registry is the source of truth: if it advertises the
	// service it is live/routable, so seed it enabled even without a baseline row
	// (e.g. forge/workflows, registered directly by the chart). A baseline row, if
	// present, overrides below. See mergeViews for the full precedence.
	v := serviceView{Service: service, Enabled: false, Kind: kindPlatform, Source: "catalog"}
	if liveServiceNames(ctx)[service] {
		v.Enabled = true
		v.Source = "registry"
	}

	if d, err := getOrgService(ctx, defaultOrgID, service); err == nil {
		applyRow(map[string]*serviceView{service: &v}, d, "default")
	} else if !isNotFound(err) {
		return serviceView{}, err
	}
	// Coming-soon services (source not in this repo) can't be deployed yet — force
	// disabled and flag, mirroring mergeViews.
	if comingSoonServices[service] {
		v.Enabled = false
		v.ComingSoon = true
	}
	return v, nil
}
