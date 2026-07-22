package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// ipBucket is a fixed-window counter used for per-IP rate limiting.
type ipBucket struct {
	mu       sync.Mutex
	count    int
	windowAt time.Time
}

// rateLimitMiddleware rejects requests from a single IP that exceed maxAttempts
// within window. Uses Redis when available (shared across pods); falls back to
// the in-memory limiterMap when Redis is not configured.
func rateLimitMiddleware(endpoint string, limiterMap *sync.Map, maxAttempts int, window time.Duration, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := realClientIP(r)
		var allowed bool
		if redisClient != nil {
			allowed = redisRateLimit(r.Context(), endpoint, ip, maxAttempts, window)
		} else {
			val, _ := limiterMap.LoadOrStore(ip, &ipBucket{})
			b := val.(*ipBucket)
			b.mu.Lock()
			now := time.Now()
			if now.Sub(b.windowAt) >= window {
				b.count = 0
				b.windowAt = now
			}
			b.count++
			allowed = b.count <= maxAttempts
			b.mu.Unlock()
		}
		if !allowed {
			slog.WarnContext(r.Context(), "rate limit exceeded", "ip", ip, "path", r.URL.Path)
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

var loginLimiter sync.Map       // per-IP login attempt buckets
var signupLimiter sync.Map      // per-IP signup attempt buckets
var setupStatusLimiter sync.Map // per-IP setup-status attempt buckets

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// envBool reports whether key is set to a truthy value ("1", "true", "yes", "on",
// case-insensitive). Any other value — including unset — is false.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// coreServiceNames is the set of service identities seeded from the configured
// GATEKEEPER_SERVICES env var. Runtime service-account registration (builder's
// /internal/service-accounts) must never overwrite one of these — otherwise a
// holder of BUILDER_INTERNAL_KEY could re-key a core service and impersonate it.
var coreServiceNames = map[string]bool{}

// isCoreServiceName reports whether name is a statically-configured core service.
func isCoreServiceName(name string) bool { return coreServiceNames[name] }

// seedServiceAccounts reads GATEKEEPER_SERVICES (format "name=key,name=key") and
// upserts ServiceAccount rows. For new accounts, both HashedKey and
// HashedBootstrapKey are set to the provided key's hash. For existing accounts,
// only HashedBootstrapKey is refreshed — HashedKey is left intact so that keys
// rotated at runtime survive Gatekeeper restarts. The bootstrap key fallback in
// requireServiceAuth lets a service pod re-authenticate after a restart with its
// original GATEKEEPER_SERVICE_KEY even when HashedKey holds a rotated value.
func seedServiceAccounts(ctx context.Context) {
	raw := secret("GATEKEEPER_SERVICES")
	if raw == "" {
		return
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		idx := strings.Index(entry, "=")
		if idx < 1 || idx == len(entry)-1 {
			slog.WarnContext(ctx, "seedServiceAccounts: invalid entry, expected name=key", "entry", entry)
			continue
		}
		name, key := entry[:idx], entry[idx+1:]
		coreServiceNames[name] = true
		hash, err := bcrypt.GenerateFromPassword([]byte(key), 12)
		if err != nil {
			slog.ErrorContext(ctx, "seedServiceAccounts: bcrypt failed", "name", name, "error", err)
			continue
		}
		upsertServiceAccountDB(ctx, name, string(hash))
	}
}

const maxBodyBytes = 64 * 1024 // 64 KB — sufficient for any gatekeeper payload

// limitBody caps inbound request bodies to prevent memory-exhaustion via huge payloads.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *statusResponseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

// Logger is a middleware handler that does request logging
type Logger struct {
	handler http.Handler
}

func (l *Logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	l.handler.ServeHTTP(rw, r)
	sc := trace.SpanFromContext(r.Context()).SpanContext()
	attrs := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"status", rw.status,
		"duration", time.Since(start),
		"trace_id", sc.TraceID().String(),
		"span_id", sc.SpanID().String(),
	}
	switch r.URL.Path {
	case "/healthz", "/health", "/system_health", "/readyz", "/livez":
		// Background liveness/readiness probes are noise at info; log them at
		// debug, escalating to warn only when the probe itself fails.
		if rw.status >= 500 {
			slog.WarnContext(r.Context(), "http request", attrs...)
		} else {
			slog.DebugContext(r.Context(), "http request", attrs...)
		}
	default:
		slog.InfoContext(r.Context(), "http request", attrs...)
	}
}

// NewLogger constructs a new Logger middleware handler
func NewLogger(handlerToWrap http.Handler) *Logger {
	return &Logger{handlerToWrap}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	logLevel := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		_ = logLevel.UnmarshalText([]byte(v))
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	initSecretsEncryption()

	conn := connect()
	conn.AutoMigrate(&Org{}, &Role{}, &RoleMembership{}, &Team{}, &User{}, &UserOrgMembership{}, &Session{}, &Permissions{}, &Invite{}, &PermissionsCheck{}, &ServiceAccount{}, &ServicePermissionRequest{}, &AuditLog{}, &Secret{}, &OrgSecretProvider{}, &OAuthClient{}, &OAuthCode{}, &TOTPCredential{}, &MFAPending{}, &SignupAllowlistEntry{}, &SignupPolicy{})
	applyForeignKeys(conn)
	applyUniqueIndexes(conn)
	// Give existing single-org accounts a membership row so they participate in the
	// user↔org join table. Idempotent; non-fatal so a transient DB hiccup here never
	// blocks startup.
	if err := backfillMemberships(ctx); err != nil {
		slog.Warn("membership backfill failed; existing users may not appear in their org memberships until re-run", "error", err)
	}
	seedServiceAccounts(ctx)
	seedAdminUser(ctx)
	seedSignupPolicy(ctx)
	seedSignupAllowlist(ctx)
	initOIDC()

	if registryURL := os.Getenv("REGISTRY_URL"); registryURL != "" {
		serviceKey := "gatekeeper:" + secret("REGISTRY_SERVICE_KEY")
		startDefaultGrantPoller(ctx, registryURL, serviceKey)
	} else {
		slog.Warn("REGISTRY_URL not set — default grants will not be loaded from registry; signup permissions will be minimal")
	}

	mux := buildMux()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "gatekeeper")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(context.Background())
	}
	initMetrics()
	initCache()
	initPermittedServices()
	initTrustedProxies()
	initAuditPermissionChecks()

	wrappedMux := otelhttp.NewHandler(NewLogger(limitBody(mux)), "gatekeeper",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      wrappedMux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if certFile != "" && keyFile != "" {
		tlsCfg, err := buildClientTLSConfig()
		if err != nil {
			slog.Error("TLS client-auth config", "error", err)
			os.Exit(1)
		}
		srv.TLSConfig = tlsCfg
	}

	go func() {
		var err error
		if certFile != "" && keyFile != "" {
			slog.Info("listening with TLS", "port", port, "client_auth", os.Getenv("TLS_CLIENT_AUTH"))
			err = srv.ListenAndServeTLS(certFile, keyFile)
		} else {
			slog.Info("listening", "port", port)
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	stop()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
}

// buildClientTLSConfig builds the optional mTLS client-auth config from env,
// returning an error instead of exiting so its branches are unit-testable.
func buildClientTLSConfig() (*tls.Config, error) {
	tlsCfg := &tls.Config{}
	switch os.Getenv("TLS_CLIENT_AUTH") {
	case "require":
		caFile := os.Getenv("TLS_CLIENT_CA_FILE")
		if caFile == "" {
			return nil, errors.New("TLS_CLIENT_AUTH=require but TLS_CLIENT_CA_FILE is not set")
		}
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS_CLIENT_CA_FILE %q: %w", caFile, err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("TLS_CLIENT_CA_FILE %q contains no valid PEM certificates", caFile)
		}
		tlsCfg.ClientCAs = caPool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	case "request":
		tlsCfg.ClientAuth = tls.RequestClientCert
	}
	return tlsCfg, nil
}

// buildMux builds the full routing table. Extracted from main so the route set
// is unit-testable without standing up the server.
func buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /openapi.yaml", handleOpenAPIYAML)
	mux.HandleFunc("GET /setup/status", rateLimitMiddleware("setup-status", &setupStatusLimiter, envInt("SETUP_STATUS_RATE_LIMIT", 60), envDuration("SETUP_STATUS_RATE_WINDOW", time.Minute), handleSetupStatus))
	mux.HandleFunc("POST /signup", rateLimitMiddleware("signup", &signupLimiter, envInt("SIGNUP_RATE_LIMIT", 10), envDuration("SIGNUP_RATE_WINDOW", 10*time.Minute), handleSignup))
	mux.HandleFunc("POST /login", rateLimitMiddleware("login", &loginLimiter, envInt("LOGIN_RATE_LIMIT", 5), envDuration("LOGIN_RATE_WINDOW", time.Minute), handleLogin))
	mux.HandleFunc("POST /logout", handleLogout)
	mux.HandleFunc("POST /mfa/verify", rateLimitMiddleware("mfa-verify", &loginLimiter, envInt("LOGIN_RATE_LIMIT", 5), envDuration("LOGIN_RATE_WINDOW", time.Minute), handleMFAVerify))

	// OIDC provider — used by Forgejo/Gitea and any other OAuth2 client.
	mux.HandleFunc("GET /.well-known/openid-configuration", handleOIDCDiscovery)
	mux.HandleFunc("GET /oauth/jwks", handleJWKS)
	mux.HandleFunc("GET /oauth/authorize", handleAuthorize)
	mux.HandleFunc("POST /oauth/authorize", handleAuthorizeSubmit)
	mux.HandleFunc("GET /oauth/mfa", handleOAuthMFAGet)
	mux.HandleFunc("POST /oauth/mfa", rateLimitMiddleware("oauth-mfa", &loginLimiter, envInt("LOGIN_RATE_LIMIT", 5), envDuration("LOGIN_RATE_WINDOW", time.Minute), handleOAuthMFAPost))
	mux.HandleFunc("POST /oauth/token", handleToken)
	mux.Handle("GET /oauth/userinfo", authMiddleware(http.HandlerFunc(handleUserinfo)))
	mux.HandleFunc("POST /internal/oauth/clients", handleCreateOAuthClient)
	mux.HandleFunc("GET /internal/oauth/clients", handleListOAuthClients)
	mux.HandleFunc("DELETE /internal/oauth/clients/{id}", handleDeleteOAuthClient)
	// Runtime service registration for builder (auth: BUILDER_INTERNAL_KEY) — lets
	// builder bring a non-core service online with no Helm change.
	mux.HandleFunc("POST /internal/service-accounts", handleRegisterServiceAccount)
	mux.HandleFunc("DELETE /internal/service-accounts/{name}", handleDeregisterServiceAccount)
	mux.Handle("POST /check_permissions", authMiddleware(http.HandlerFunc(handleCheckPermissions)))
	mux.Handle("GET /auth/validate", authMiddleware(http.HandlerFunc(handleAuthValidate)))

	mw := func(h http.HandlerFunc) http.Handler { return authMiddleware(http.HandlerFunc(h)) }

	mux.Handle("POST /mfa/totp/enroll", mw(handleTOTPEnroll))
	mux.Handle("POST /mfa/totp/confirm", mw(handleTOTPConfirm))
	mux.Handle("GET /mfa/totp/status", mw(handleTOTPStatus))
	mux.Handle("DELETE /mfa/totp", mw(handleTOTPDisable))

	mux.Handle("GET /users/{id}", mw(handleGetUser))
	mux.Handle("GET /users", mw(handleListUsers))
	mux.Handle("PUT /users/{id}", mw(handleUpdateUser))
	mux.Handle("DELETE /users/{id}", mw(handleDeleteUser))

	mux.Handle("POST /orgs", mw(handleCreateOrg))
	mux.Handle("GET /orgs/{id}", mw(handleGetOrg))
	mux.Handle("GET /orgs", mw(handleListOrgs))
	mux.Handle("PUT /orgs/{id}", mw(handleUpdateOrg))
	mux.Handle("DELETE /orgs/{id}", mw(handleDeleteOrg))
	// Multi-org membership: switch the caller's active org, or leave an org. Both
	// are gated (in the registry manifest) by getOrg on the target org, which every
	// member already holds; the handlers additionally enforce membership.
	mux.Handle("POST /orgs/{id}/switch", mw(handleSwitchOrg))
	mux.Handle("POST /orgs/{id}/leave", mw(handleLeaveOrg))

	mux.Handle("POST /teams", mw(handleCreateTeam))
	mux.Handle("GET /teams", mw(handleListTeams))
	mux.Handle("GET /teams/{id}", mw(handleGetTeam))
	mux.Handle("PUT /teams/{id}", mw(handleUpdateTeam))
	mux.Handle("DELETE /teams/{id}", mw(handleDeleteTeam))

	mux.Handle("POST /roles", mw(handleCreateRole))
	mux.Handle("GET /roles", mw(handleListRoles))
	mux.Handle("GET /roles/{id}", mw(handleGetRole))
	mux.Handle("PUT /roles/{id}", mw(handleUpdateRole))
	mux.Handle("DELETE /roles/{id}", mw(handleDeleteRole))
	// Namespace roles: how an ordinary user shares what they own, without an admin.
	// Confined to the caller's namespace and attenuated to permissions they hold.
	mux.Handle("POST /roles/namespace", mw(handleCreateNamespaceRole))
	mux.Handle("PUT /roles/{id}/members/{user_id}", mw(handleAssignRole))
	mux.Handle("DELETE /roles/{id}/members/{user_id}", mw(handleRevokeRole))

	mux.Handle("POST /permissions", mw(handleCreatePermissions))
	mux.Handle("GET /permissions", mw(handleListPermissions))
	mux.Handle("GET /permissions/{id}", mw(handleGetPermissions))
	mux.Handle("PUT /permissions/{id}", mw(handleUpdatePermissions))
	mux.Handle("DELETE /permissions/{id}", mw(handleDeletePermissions))

	mux.Handle("GET /sessions/{id}", mw(handleGetSession))
	mux.Handle("DELETE /sessions/{id}", mw(handleDeleteSession))

	mux.Handle("POST /orgs/{id}/invites", mw(handleCreateOrgInvite))
	mux.Handle("POST /teams/{id}/invites", mw(handleCreateTeamInvite))
	mux.Handle("GET /invites", mw(handleListInvites))
	mux.Handle("GET /invites/{id}", mw(handleGetInvite))
	mux.Handle("POST /invites/{id}/accept", mw(handleAcceptInvite))
	mux.Handle("POST /invites/{id}/decline", mw(handleDeclineInvite))
	mux.Handle("DELETE /invites/{id}", mw(handleDeleteInvite))

	mux.Handle("GET /audit-logs", mw(handleListAuditLogs))
	mux.Handle("GET /permission-checks", mw(handleListPermissionChecks))

	// Invite-only registration: admin-managed signup allowlist + policy toggle.
	mux.Handle("POST /signup-allowlist", mw(handleCreateSignupAllowlist))
	mux.Handle("GET /signup-allowlist", mw(handleListSignupAllowlist))
	mux.Handle("DELETE /signup-allowlist/{id}", mw(handleDeleteSignupAllowlist))
	mux.Handle("GET /signup-policy", mw(handleGetSignupPolicy))
	mux.Handle("PUT /signup-policy", mw(handleUpdateSignupPolicy))

	// Secrets: user-authenticated CRUD (values write-only) + internal resolve for the workflow worker.
	mux.Handle("POST /secrets", mw(handleCreateSecret))
	mux.Handle("GET /secrets", mw(handleListSecrets))
	mux.Handle("PUT /secrets/{id}", mw(handleUpdateSecret))
	mux.Handle("DELETE /secrets/{id}", mw(handleDeleteSecret))
	mux.Handle("GET /orgs/{id}/secret-provider", mw(handleGetSecretProvider))
	mux.Handle("PUT /orgs/{id}/secret-provider", mw(handleSetSecretProvider))
	mux.Handle("DELETE /orgs/{id}/secret-provider", mw(handleDeleteSecretProvider))
	mux.HandleFunc("POST /internal/secrets/resolve", handleResolveSecrets)
	mux.HandleFunc("POST /internal/secrets/lookup", handleLookupSecret)

	// Key rotation: service-key authenticated; generates a new key server-side and returns it.
	mux.HandleFunc("POST /service-accounts/rotate-key", handleRotateServiceKey)

	// Run tokens: short-lived session JWTs issued to the workflows service so that
	// workflow runs never store the triggering user's own session token in the DB.
	mux.HandleFunc("POST /internal/run-tokens", handleCreateRunToken)
	mux.HandleFunc("DELETE /internal/run-tokens/{session_id}", handleRevokeRunToken)

	// Workflow service roles: minimal-permission roles provisioned at workflow
	// creation time; each permission is verified against the owner's access first.
	mux.HandleFunc("POST /internal/workflow-roles", handleCreateWorkflowRole)
	mux.HandleFunc("DELETE /internal/workflow-roles/{role_id}", handleDeleteWorkflowRole)

	// Service permission requests: POST is service-key authenticated; the rest require user JWT.
	mux.HandleFunc("POST /service-permission-requests", handleCreateServicePermissionRequest)
	mux.Handle("GET /service-permission-requests", mw(handleListServicePermissionRequests))
	mux.Handle("GET /service-permission-requests/{id}", mw(handleGetServicePermissionRequest))
	mux.Handle("POST /service-permission-requests/{id}/approve", mw(handleApproveServicePermissionRequest))
	mux.Handle("POST /service-permission-requests/{id}/decline", mw(handleDeclineServicePermissionRequest))
	return mux
}

// applyForeignKeys adds FK constraints after all tables exist. Each statement
// is wrapped in a DO block so re-running on an already-migrated database is safe.
func applyForeignKeys(db *gorm.DB) {
	constraints := []string{
		`DO $$ BEGIN ALTER TABLE roles ADD CONSTRAINT fk_roles_org FOREIGN KEY (org_id) REFERENCES orgs(org_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE teams ADD CONSTRAINT fk_teams_role FOREIGN KEY (role_id) REFERENCES roles(role_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE users ADD CONSTRAINT fk_users_org FOREIGN KEY (org_id) REFERENCES orgs(org_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE users ADD CONSTRAINT fk_users_role FOREIGN KEY (role_id) REFERENCES roles(role_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE users ADD CONSTRAINT fk_users_team FOREIGN KEY (team_id) REFERENCES teams(team_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE user_org_memberships ADD CONSTRAINT fk_memberships_user FOREIGN KEY (user_id) REFERENCES users(user_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE user_org_memberships ADD CONSTRAINT fk_memberships_org FOREIGN KEY (org_id) REFERENCES orgs(org_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE sessions ADD CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users(user_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE teams ADD CONSTRAINT fk_teams_user FOREIGN KEY (owner_id) REFERENCES users(user_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE invites ADD CONSTRAINT fk_invites_inviter FOREIGN KEY (inviter_id) REFERENCES users(user_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE teams ADD CONSTRAINT fk_teams_org FOREIGN KEY (org_id) REFERENCES orgs(org_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE permissions_checks ADD CONSTRAINT fk_permissions_checks_user FOREIGN KEY (user_id) REFERENCES users(user_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE permissions_checks ADD CONSTRAINT fk_permissions_checks_org FOREIGN KEY (org_id) REFERENCES orgs(org_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE permissions_checks ADD CONSTRAINT fk_permissions_checks_team FOREIGN KEY (team_id) REFERENCES teams(team_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE service_accounts ADD CONSTRAINT fk_service_accounts_role FOREIGN KEY (role_id) REFERENCES roles(role_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE service_permission_requests ADD CONSTRAINT fk_service_permission_requests_resolved_by FOREIGN KEY (resolved_by) REFERENCES users(user_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
	}
	for _, c := range constraints {
		db.Exec(c)
	}
}

// applyUniqueIndexes enforces that name-referenced resources have a unique
// human-readable name within their owning scope, so they can be addressed by
// name rather than by an opaque UUID. Partial indexes (WHERE active) are used so
// soft-deleted rows don't block re-creating a name, and so a name freed by a
// delete becomes available again — mirroring the workflows service pattern.
//
// Roles and Permissions are intentionally excluded: a role's name is empty for
// user-facing roles (only set to "workflow:<id>" for service roles), so it is
// not a human-facing identifier. Index creation is best-effort: a pre-existing
// row collision logs a warning rather than aborting startup.
func applyUniqueIndexes(db *gorm.DB) {
	indexes := []string{
		// Secrets are referenced by name (e.g. forge's git:/secret_ref schemes);
		// unique per org. The create path already rejects duplicates.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_secrets_org_name ON secrets (org_id, name) WHERE active`,
		// Teams are unique by name within their org; org-less teams are unique per owner.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_teams_org_name ON teams (org_id, team_name) WHERE active AND org_id IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_teams_owner_name ON teams (owner_id, team_name) WHERE active AND org_id IS NULL`,
		// OAuth clients are unique by name within their org.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_oauth_clients_org_name ON oauth_clients (org_id, name) WHERE active`,
		// A user holds at most one active membership per org; a soft-deleted (left)
		// membership can be re-added by accepting a fresh invite.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_user_org_memberships ON user_org_memberships (user_id, org_id) WHERE active`,
		// A signup allowlist value (email or @domain rule) is unique among active
		// entries; a soft-deleted value can be re-added. Stored already-lowercased,
		// so the index is on the raw column.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_signup_allowlist_email ON signup_allowlist (email) WHERE active`,
	}
	for _, idx := range indexes {
		if err := db.Exec(idx).Error; err != nil {
			slog.Warn("unique index not created (existing duplicate names?); name uniqueness not enforced for this resource", "stmt", idx, "error", err)
		}
	}
}
