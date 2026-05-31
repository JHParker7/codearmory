package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
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
	"github.com/google/uuid"
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
			slog.Warn("rate limit exceeded", "ip", ip, "path", r.URL.Path)
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

var loginLimiter sync.Map  // per-IP login attempt buckets
var signupLimiter sync.Map // per-IP signup attempt buckets

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// seedServiceAccounts reads GATEKEEPER_SERVICES (format "name=key,name=key") and
// upserts a ServiceAccount row for each entry with a fresh bcrypt hash. This sets
// the initial key; services rotate their keys at runtime via POST /service-accounts/rotate-key.
func seedServiceAccounts(db *gorm.DB) {
	raw := secret("GATEKEEPER_SERVICES")
	if raw == "" {
		return
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		idx := strings.Index(entry, "=")
		if idx < 1 || idx == len(entry)-1 {
			slog.Warn("seedServiceAccounts: invalid entry, expected name=key", "entry", entry)
			continue
		}
		name, key := entry[:idx], entry[idx+1:]
		hash, err := bcrypt.GenerateFromPassword([]byte(key), 12)
		if err != nil {
			slog.Error("seedServiceAccounts: bcrypt failed", "name", name, "error", err)
			continue
		}
		var existing ServiceAccount
		err = db.Where("service_name = ?", name).First(&existing).Error
		if err != nil {
			svc := ServiceAccount{
				ServiceAccountID: uuid.New().String(),
				ServiceName:      name,
				HashedKey:        string(hash),
				Active:           true,
			}
			if err := db.Create(&svc).Error; err != nil {
				slog.Error("seedServiceAccounts: create failed", "name", name, "error", err)
			} else {
				slog.Info("seedServiceAccounts: created", "name", name)
			}
		} else {
			existing.HashedKey = string(hash)
			existing.UpdatedAt = time.Now()
			if err := db.Save(&existing).Error; err != nil {
				slog.Error("seedServiceAccounts: update failed", "name", name, "error", err)
			} else {
				slog.Info("seedServiceAccounts: updated key", "name", name)
			}
		}
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
	slog.Info(r.Method+" "+r.URL.Path,
		"status", rw.status,
		"duration", time.Since(start),
		"trace_id", sc.TraceID().String(),
		"span_id", sc.SpanID().String(),
	)
}

// NewLogger constructs a new Logger middleware handler
func NewLogger(handlerToWrap http.Handler) *Logger {
	return &Logger{handlerToWrap}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	initSecretsEncryption()

	db := connect()
	db.AutoMigrate(&Org{}, &Role{}, &Team{}, &User{}, &Session{}, &Permissions{}, &Invite{}, &PermissionsCheck{}, &ServiceAccount{}, &ServicePermissionRequest{}, &AuditLog{}, &Secret{}, &OrgSecretProvider{})
	applyForeignKeys(db)
	seedServiceAccounts(db)

	if registryURL := os.Getenv("REGISTRY_URL"); registryURL != "" {
		serviceKey := "gatekeeper:" + secret("REGISTRY_SERVICE_KEY")
		startDefaultGrantPoller(ctx, registryURL, serviceKey)
	} else {
		slog.Warn("REGISTRY_URL not set — default grants will not be loaded from registry; signup permissions will be minimal")
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /signup", rateLimitMiddleware("signup", &signupLimiter, envInt("SIGNUP_RATE_LIMIT", 10), envDuration("SIGNUP_RATE_WINDOW", 10*time.Minute), handleSignup))
	mux.HandleFunc("POST /login", rateLimitMiddleware("login", &loginLimiter, envInt("LOGIN_RATE_LIMIT", 5), envDuration("LOGIN_RATE_WINDOW", time.Minute), handleLogin))
	mux.Handle("POST /check_permissions", authMiddleware(http.HandlerFunc(handleCheckPermissions)))

	mw := func(h http.HandlerFunc) http.Handler { return authMiddleware(http.HandlerFunc(h)) }

	mux.Handle("GET /users/{id}", mw(handleGetUser))
	mux.Handle("GET /users", mw(handleListUsers))
	mux.Handle("PUT /users/{id}", mw(handleUpdateUser))
	mux.Handle("DELETE /users/{id}", mw(handleDeleteUser))

	mux.Handle("POST /orgs", mw(handleCreateOrg))
	mux.Handle("GET /orgs/{id}", mw(handleGetOrg))
	mux.Handle("GET /orgs", mw(handleListOrgs))
	mux.Handle("PUT /orgs/{id}", mw(handleUpdateOrg))
	mux.Handle("DELETE /orgs/{id}", mw(handleDeleteOrg))

	mux.Handle("POST /teams", mw(handleCreateTeam))
	mux.Handle("GET /teams", mw(handleListTeams))
	mux.Handle("GET /teams/{id}", mw(handleGetTeam))
	mux.Handle("PUT /teams/{id}", mw(handleUpdateTeam))
	mux.Handle("DELETE /teams/{id}", mw(handleDeleteTeam))

	mux.Handle("POST /roles", mw(handleCreateRole))
	mux.Handle("GET /roles/{id}", mw(handleGetRole))
	mux.Handle("PUT /roles/{id}", mw(handleUpdateRole))
	mux.Handle("DELETE /roles/{id}", mw(handleDeleteRole))

	mux.Handle("POST /permissions", mw(handleCreatePermissions))
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

	// Secrets: user-authenticated CRUD (values write-only) + internal resolve for the workflow worker.
	mux.Handle("POST /secrets", mw(handleCreateSecret))
	mux.Handle("GET /secrets", mw(handleListSecrets))
	mux.Handle("PUT /secrets/{id}", mw(handleUpdateSecret))
	mux.Handle("DELETE /secrets/{id}", mw(handleDeleteSecret))
	mux.Handle("GET /orgs/{id}/secret-provider", mw(handleGetSecretProvider))
	mux.Handle("PUT /orgs/{id}/secret-provider", mw(handleSetSecretProvider))
	mux.Handle("DELETE /orgs/{id}/secret-provider", mw(handleDeleteSecretProvider))
	mux.HandleFunc("POST /internal/secrets/resolve", handleResolveSecrets)

	// Key rotation: service-key authenticated; generates a new key server-side and returns it.
	mux.HandleFunc("POST /service-accounts/rotate-key", handleRotateServiceKey)

	// Service permission requests: POST is service-key authenticated; the rest require user JWT.
	mux.HandleFunc("POST /service-permission-requests", handleCreateServicePermissionRequest)
	mux.Handle("GET /service-permission-requests", mw(handleListServicePermissionRequests))
	mux.Handle("GET /service-permission-requests/{id}", mw(handleGetServicePermissionRequest))
	mux.Handle("POST /service-permission-requests/{id}/approve", mw(handleApproveServicePermissionRequest))
	mux.Handle("POST /service-permission-requests/{id}/decline", mw(handleDeclineServicePermissionRequest))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

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
		tlsCfg := &tls.Config{}
		// TLS_CLIENT_AUTH controls whether client certificates are requested.
		// Set to "require" to enforce mTLS (needed for ClientCertFingerprints binding).
		//   Requires TLS_CLIENT_CA_FILE to be set; clients must present a cert signed by that CA.
		// Set to "request" to request but not require a client cert.
		// Default (unset): no client certificate requested.
		switch os.Getenv("TLS_CLIENT_AUTH") {
		case "require":
			caFile := os.Getenv("TLS_CLIENT_CA_FILE")
			if caFile == "" {
				slog.Error("TLS_CLIENT_AUTH=require but TLS_CLIENT_CA_FILE is not set")
				os.Exit(1)
			}
			caCert, err := os.ReadFile(caFile)
			if err != nil {
				slog.Error("failed to read TLS_CLIENT_CA_FILE", "path", caFile, "error", err)
				os.Exit(1)
			}
			caPool := x509.NewCertPool()
			if !caPool.AppendCertsFromPEM(caCert) {
				slog.Error("TLS_CLIENT_CA_FILE contains no valid PEM certificates", "path", caFile)
				os.Exit(1)
			}
			tlsCfg.ClientCAs = caPool
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		case "request":
			tlsCfg.ClientAuth = tls.RequestClientCert
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

// applyForeignKeys adds FK constraints after all tables exist. Each statement
// is wrapped in a DO block so re-running on an already-migrated database is safe.
func applyForeignKeys(db *gorm.DB) {
	constraints := []string{
		`DO $$ BEGIN ALTER TABLE roles ADD CONSTRAINT fk_roles_org FOREIGN KEY (org_id) REFERENCES orgs(org_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE teams ADD CONSTRAINT fk_teams_role FOREIGN KEY (role_id) REFERENCES roles(role_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE users ADD CONSTRAINT fk_users_org FOREIGN KEY (org_id) REFERENCES orgs(org_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE users ADD CONSTRAINT fk_users_role FOREIGN KEY (role_id) REFERENCES roles(role_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE users ADD CONSTRAINT fk_users_team FOREIGN KEY (team_id) REFERENCES teams(team_id); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
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
