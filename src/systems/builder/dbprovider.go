package main

import (
	"context"
	"os"
	"strings"
)

// A db backend decides how a builder-deployed service obtains its DATABASE_URL.
// The default (manual) is unchanged: the admin supplies a per-service db_url that
// builder encrypts and writes into the service's own Secret. The other backends
// either provision the database for the admin (sql) or remove builder from the
// credential-custody chain entirely (cnpg, external). See docs/builder/db-provisioning.md.
//
// Two delivery models, expressed by dbResolution:
//   - builder-owned Secret (manual, sql): builder writes the URL into the
//     codearmory-<svc> Secret it already manages (dbResolution.inline).
//   - foreign Secret reference (cnpg, external): DATABASE_URL points at a Secret an
//     operator / external-secrets controller owns and fills (dbResolution.ref).
const (
	dbBackendManual   = "manual"
	dbBackendSQL      = "sql"
	dbBackendCNPG     = "cnpg"
	dbBackendExternal = "external"
)

// validDBBackends is the set accepted from config; anything else falls back to manual.
var validDBBackends = map[string]bool{
	dbBackendManual:   true,
	dbBackendSQL:      true,
	dbBackendCNPG:     true,
	dbBackendExternal: true,
}

// dbSecretRef names a Secret + key the pod's DATABASE_URL env var should read from.
// Used by the foreign-reference backends, where builder does not own the Secret.
type dbSecretRef struct {
	secretName string
	key        string
}

// dbResolution is how a service obtains DATABASE_URL for one reconcile. Exactly one
// of inline/ref is meaningful: inline is written into builder's own Secret; ref makes
// the pod read a foreign Secret directly (and builder writes no database-url).
type dbResolution struct {
	inline string
	ref    *dbSecretRef
}

// dbProvider resolves DATABASE_URL delivery during reconcile. It must be idempotent —
// resolve runs on every reconcile pass. teardown is best-effort cleanup on service
// removal and must NEVER drop a database or its data.
type dbProvider interface {
	resolve(ctx context.Context, b *k8sBackend, service, inline string) (dbResolution, error)
	teardown(ctx context.Context, b *k8sBackend, service string)
}

// manualProvider is the passthrough: it delivers the (decrypted, stored) URL via
// builder's own Secret unchanged. It also serves the sql backend at reconcile time —
// sql provisions the database once at enable and stores the derived URL like a manual
// one, so from the reconciler's view the two are identical.
type manualProvider struct{}

func (manualProvider) resolve(_ context.Context, _ *k8sBackend, _, inline string) (dbResolution, error) {
	return dbResolution{inline: inline}, nil
}
func (manualProvider) teardown(context.Context, *k8sBackend, string) {}

// dbConfig is builder's global database-backend configuration, loaded from the
// environment at startup. Backend-specific fields are only consulted when that backend
// is selected. The backend is global with an optional per-service override (dbBackend
// config key); backend-specific settings are global.
type dbConfig struct {
	backend string

	// sql backend
	sqlMaintenanceURL string // stored maintenance URL; empty => ephemeral (supplied at enable)
	sqlCreateRole     bool   // false => CREATEDB-only profile, reuse sqlOwnerRole
	sqlOwnerRole      string // owner role when sqlCreateRole is false
	sqlOwnerPassword  string // owner role's password (CREATEDB-only profile, for the derived URL)
	sqlSSLMode        string // sslmode forced onto derived URLs when set

	// cnpg backend
	cnpgCluster        string
	cnpgNamespace      string // defaults to the workload namespace when empty
	cnpgCredSecretTmpl string // e.g. "{cluster}-{service}"; "{service}"/"{cluster}" expanded
	cnpgCredKey        string // key in that Secret holding the connection string (default "uri")

	// external (External Secrets Operator) backend
	extStoreRef     string
	extStoreKind    string // "SecretStore" | "ClusterSecretStore"
	extPathTemplate string // e.g. "codearmory/{service}/db"; "{service}" expanded
	extProperty     string // property within the remote secret holding the URL
	extRefresh      string // refreshInterval (e.g. "1h"); empty => operator default
	extVersion      string // ExternalSecret API version (default "v1beta1")
}

// globalDBConfig is loaded once in main and read by the API handler (sql enable-time
// provisioning) and the reconciler (provider construction).
var globalDBConfig dbConfig

// loadDBConfig reads the db-backend configuration from the environment, normalizing the
// backend name to manual when unset or invalid so a typo can never silently change
// custody behavior.
func loadDBConfig() dbConfig {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("BUILDER_DB_BACKEND")))
	if !validDBBackends[backend] {
		backend = dbBackendManual
	}
	c := dbConfig{
		backend:            backend,
		sqlMaintenanceURL:  strings.TrimSpace(secret("BUILDER_DB_SQL_MAINTENANCE_URL")),
		sqlCreateRole:      envBoolDefault("BUILDER_DB_SQL_CREATE_ROLE", true),
		sqlOwnerRole:       strings.TrimSpace(os.Getenv("BUILDER_DB_SQL_OWNER_ROLE")),
		sqlOwnerPassword:   strings.TrimSpace(secret("BUILDER_DB_SQL_OWNER_PASSWORD")),
		sqlSSLMode:         strings.TrimSpace(os.Getenv("BUILDER_DB_SQL_SSLMODE")),
		cnpgCluster:        strings.TrimSpace(os.Getenv("BUILDER_DB_CNPG_CLUSTER")),
		cnpgNamespace:      strings.TrimSpace(os.Getenv("BUILDER_DB_CNPG_NAMESPACE")),
		cnpgCredSecretTmpl: strings.TrimSpace(os.Getenv("BUILDER_DB_CNPG_CRED_SECRET_TEMPLATE")),
		cnpgCredKey:        envOrDefault("BUILDER_DB_CNPG_CRED_KEY", "uri"),
		extStoreRef:        strings.TrimSpace(os.Getenv("BUILDER_DB_EXTERNAL_STORE_REF")),
		extStoreKind:       envOrDefault("BUILDER_DB_EXTERNAL_STORE_KIND", "SecretStore"),
		extPathTemplate:    strings.TrimSpace(os.Getenv("BUILDER_DB_EXTERNAL_PATH_TEMPLATE")),
		extProperty:        strings.TrimSpace(os.Getenv("BUILDER_DB_EXTERNAL_PROPERTY")),
		extRefresh:         strings.TrimSpace(os.Getenv("BUILDER_DB_EXTERNAL_REFRESH")),
		extVersion:         envOrDefault("BUILDER_DB_EXTERNAL_API_VERSION", "v1beta1"),
	}
	return c
}

// backendFor resolves the effective backend for one service: a valid per-service
// dbBackend config key overrides the global default; anything else uses the default.
func (c dbConfig) backendFor(svcConfig map[string]any) string {
	if svcConfig != nil {
		if v, ok := svcConfig["dbBackend"].(string); ok {
			if b := strings.ToLower(strings.TrimSpace(v)); validDBBackends[b] {
				return b
			}
		}
	}
	if c.backend == "" {
		return dbBackendManual
	}
	return c.backend
}

// managesOwnSecret reports whether the backend delivers DATABASE_URL through builder's
// own Secret (manual, sql) rather than a foreign reference (cnpg, external). The API
// handler uses it to decide whether DATABASE_URL is satisfied structurally at enable.
func backendUsesForeignSecret(backend string) bool {
	return backend == dbBackendCNPG || backend == dbBackendExternal
}

// dbProviderFor returns the reconcile-time provider for a backend. sql shares the
// manual provider at reconcile (its provisioning is a one-shot at enable). cnpg/external
// degrade to manual passthrough when their dynamic client is unavailable so a
// misconfigured backend never wedges the whole reconcile.
func (b *k8sBackend) dbProviderFor(backend string) dbProvider {
	switch backend {
	case dbBackendCNPG:
		if b.dynamic == nil {
			return manualProvider{}
		}
		return cnpgProvider{}
	case dbBackendExternal:
		if b.dynamic == nil {
			return manualProvider{}
		}
		return externalProvider{}
	default:
		return manualProvider{}
	}
}

// envBoolDefault parses a truthy/falsey env var, returning def when unset or unrecognized.
func envBoolDefault(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}
