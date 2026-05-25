package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

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
	db := connect()
	db.AutoMigrate(&Org{}, &Role{}, &Team{}, &User{}, &Session{}, &Permissions{}, &Invite{}, &PermissionsCheck{}, &Service{}, &ServiceEndpoint{})
	applyForeignKeys(db)

	// Auto-register services declared in SERVICES=name=url=key,name=url=key.
	// The key is optional; when present it is hashed and stored so the service
	// can authenticate its own POST /services/register calls.
	// Upserts on name so restarts don't create duplicates.
	if raw := os.Getenv("SERVICES"); raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			parts := strings.SplitN(strings.TrimSpace(entry), "=", 3)
			if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
				continue
			}
			name, svcURL := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			var keyHash string
			if len(parts) == 3 && parts[2] != "" {
				if h, err := bcrypt.GenerateFromPassword([]byte(strings.TrimSpace(parts[2])), bcrypt.DefaultCost); err == nil {
					keyHash = string(h)
				}
			}
			var svc Service
			if db.Where("name = ?", name).First(&svc).Error != nil {
				db.Create(&Service{ServiceID: uuid.New().String(), Name: name, URL: svcURL, ServiceKeyHash: keyHash, Active: true})
				slog.Info("service auto-registered", "name", name)
			} else {
				updates := map[string]any{"url": svcURL, "active": true}
				if keyHash != "" {
					updates["service_key_hash"] = keyHash
				}
				db.Model(&svc).Updates(updates)
				slog.Info("service auto-updated", "name", name)
			}
		}
	}

	// Create the Conductor service account if CONDUCTOR_SERVICE_PASSWORD is set.
	if pw := os.Getenv("CONDUCTOR_SERVICE_PASSWORD"); pw != "" {
		email := os.Getenv("CONDUCTOR_SERVICE_EMAIL")
		if email == "" {
			email = "conductor@internal"
		}
		ensureConductorUser(db, email, pw)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("POST /signup", handleSignup)
	mux.HandleFunc("POST /login", handleLogin)
	mux.Handle("GET /check_permissions", authMiddleware(http.HandlerFunc(handleCheckPermissions)))

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

	mux.Handle("GET /services", mw(handleListServices))
	mux.Handle("POST /services", mw(handleRegisterService))
	mux.Handle("DELETE /services/{id}", mw(handleDeleteService))
	mux.HandleFunc("POST /services/register", handleServiceSelfRegister)

	mux.Handle("POST /orgs/{id}/invites", mw(handleCreateOrgInvite))
	mux.Handle("POST /teams/{id}/invites", mw(handleCreateTeamInvite))
	mux.Handle("GET /invites", mw(handleListInvites))
	mux.Handle("GET /invites/{id}", mw(handleGetInvite))
	mux.Handle("POST /invites/{id}/accept", mw(handleAcceptInvite))
	mux.Handle("POST /invites/{id}/decline", mw(handleDeclineInvite))
	mux.Handle("DELETE /invites/{id}", mw(handleDeleteInvite))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	otelHandler, shutdown, err := setupOTel(context.Background())
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(&fanoutHandler{handlers: []slog.Handler{jsonHandler, otelHandler}}))
		defer shutdown(context.Background())
	}
	initMetrics()
	initCache()

	wrappedMux := otelhttp.NewHandler(NewLogger(mux), "gatekeeper",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")
	if certFile != "" && keyFile != "" {
		slog.Info("listening with TLS", "port", port)
		if err := http.ListenAndServeTLS(":"+port, certFile, keyFile, wrappedMux); err != nil {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	} else {
		slog.Info("listening", "port", port)
		if err := http.ListenAndServe(":"+port, wrappedMux); err != nil {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
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
	}
	for _, c := range constraints {
		db.Exec(c)
	}
}
