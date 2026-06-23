package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/crypto/bcrypt"
)

var (
	gatekeeperURL = envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func secret(name string) string {
	if path := os.Getenv(name + "_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("cannot read secret file", "var", name+"_FILE", "path", path, "error", err)
			os.Exit(1)
		}
		return strings.TrimRight(string(data), "\n")
	}
	return os.Getenv(name)
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *statusResponseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

type logger struct{ handler http.Handler }

func (l *logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

// seedServiceAccounts registers service accounts from a "name=key" comma-separated
// string with the given role. On first sight an account row is created with the
// bootstrap key's hash; on later startups the stored hashed_key (which may be a
// rotated key) is left untouched so a restart does not lock out clients. The
// bootstrap key from the env is always remembered in seedServiceKeys as a
// permanent recovery credential — see authenticateServiceKey.
func seedServiceAccounts(ctx context.Context, raw, role string) {
	if raw == "" {
		return
	}
	for i, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		idx := strings.Index(entry, "=")
		if idx < 1 || idx == len(entry)-1 {
			slog.WarnContext(ctx, "seedServiceAccounts: invalid entry, expected name=key", "index", i, "role", role)
			continue
		}
		name, key := entry[:idx], entry[idx+1:]
		hash, err := bcrypt.GenerateFromPassword([]byte(key), 12)
		if err != nil {
			slog.ErrorContext(ctx, "seedServiceAccounts: bcrypt failed", "service", name, "error", err)
			continue
		}

		// Record the bootstrap key as a permanent fallback credential so a client (or
		// Registry) that restarts mid-rotation can always re-authenticate. See
		// authenticateServiceKey.
		seedServiceKeys[name] = seedAccount{key: key, role: role}

		acct := ServiceAccountModel{AccountID: uuid.New().String(), Name: name, HashedKey: string(hash), Role: role}
		if err := upsertServiceAccount(ctx, acct); err != nil {
			slog.ErrorContext(ctx, "seedServiceAccounts: upsert failed", "service", name, "error", err)
		} else {
			slog.InfoContext(ctx, "seedServiceAccounts: upserted", "service", name, "role", role)
		}
	}
}

// rotationMu serialises concurrent rotate-key calls for the same account name.
var rotationMu sync.Map

// handleRotateServiceKey generates a new random key for the authenticated service
// account, stores its bcrypt hash, and returns the plaintext new key.
// The client must present its current key to authenticate; on success it must
// immediately start using the returned key for all subsequent requests.
func handleRotateServiceKey(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleRotateServiceKey")
	defer span.End()

	header := r.Header.Get("X-Service-Key")
	if header == "" {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "missing X-Service-Key header", http.StatusUnauthorized)
		return
	}
	idx := strings.Index(header, ":")
	if idx < 1 {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "invalid X-Service-Key format, expected name:key", http.StatusUnauthorized)
		return
	}
	name, key := header[:idx], header[idx+1:]
	span.SetAttributes(attribute.String("service.name", name))
	slog.DebugContext(ctx, "rotate service key request", "service", name)

	// Accept the current rotated key or the bootstrap key. The bootstrap fallback
	// lets a client that restarted back to its initial key re-authenticate and roll
	// the key forward, instead of deadlocking on a rotated key it no longer holds.
	if _, ok := authenticateServiceKey(ctx, name, key); !ok {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	span.AddEvent("auth.verified")

	val, _ := rotationMu.LoadOrStore(name, &sync.Mutex{})
	mu := val.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "rand failed")
		slog.ErrorContext(ctx, "rotate service key: rand failed", "service", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	newKey := hex.EncodeToString(raw)
	newHash, err := bcrypt.GenerateFromPassword([]byte(newKey), 12)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "bcrypt failed")
		slog.ErrorContext(ctx, "rotate service key: bcrypt failed", "service", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := rotateServiceKeyDB(ctx, name, string(newHash)); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "rotate service key: db update failed", "service", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.write", trace.WithAttributes(attribute.String("service.name", name)))

	span.SetStatus(codes.Ok, "")
	slog.DebugContext(ctx, "registry service key rotated", "service", name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"key": newKey}) //nolint:errcheck
}

type manifestActionEntry struct {
	Name           string          `json:"name"`
	Method         string          `json:"method"`
	Path           string          `json:"path"`
	BodyTransforms json.RawMessage `json:"body_transforms,omitempty"`
	Async          json.RawMessage `json:"async,omitempty"`
}

type manifestDefaultGrant struct {
	GrantOn   string   `json:"grant_on"`
	Actions   []string `json:"actions"`
	Resources []string `json:"resources"`
}

type manifestEntry struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description"`
	ForwardAuth bool   `json:"forward_auth"`
	ServiceKey  string `json:"service_key"`
	Endpoints   []struct {
		Method   string `json:"method"`
		Path     string `json:"path"`
		Action   string `json:"action"`
		Resource string `json:"resource"`
		Public   bool   `json:"public"`
	} `json:"endpoints"`
	Actions       []manifestActionEntry  `json:"actions"`
	DefaultGrants []manifestDefaultGrant `json:"default_grants"`
}

func loadManifest(ctx context.Context, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.ErrorContext(ctx, "manifest: failed to read file", "path", path, "error", err)
		return
	}
	var entries []manifestEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.ErrorContext(ctx, "manifest: failed to parse JSON", "path", path, "error", err)
		return
	}
	for _, e := range entries {
		loadManifestEntry(ctx, e)
	}
}

func notifyService(ctx context.Context, name, target, key string) {
	if target == "" || key == "" {
		return
	}
	nctx, span := otel.Tracer("registry").Start(ctx, "notify."+name)
	defer span.End()
	nctx, cancel := context.WithTimeout(nctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(nctx, http.MethodPost, target, nil)
	if err != nil {
		slog.WarnContext(nctx, "notify: failed to create request", "service", name, "error", err)
		return
	}
	req.Header.Set("X-Service-Key", "registry:"+key)
	resp, err := registryHTTPClient.Do(req)
	if err != nil {
		slog.WarnContext(nctx, "notify: request failed", "service", name, "error", err)
		return
	}
	resp.Body.Close()
	slog.DebugContext(nctx, "service notified", "service", name, "status", resp.StatusCode)
}

func notifyConductor(ctx context.Context) {
	notifyService(ctx, "conductor",
		os.Getenv("CONDUCTOR_URL")+"/internal/refresh",
		os.Getenv("CONDUCTOR_NOTIFY_KEY"))
}

func notifyWorkflows(ctx context.Context) {
	notifyService(ctx, "workflows",
		os.Getenv("WORKFLOWS_URL")+"/internal/catalog/refresh",
		os.Getenv("WORKFLOWS_NOTIFY_KEY"))
}

func startNotificationTicker(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				notifyConductor(ctx)
				notifyWorkflows(ctx)
			}
		}
	}()
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

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "registry")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(context.Background())
	}

	// Rename legacy PostgreSQL auto-named constraints to match GORM's naming
	// convention. These are one-shot: silently ignored if already renamed or
	// the table doesn't exist yet (fresh install).
	for _, sql := range []string{
		`ALTER TABLE services RENAME CONSTRAINT services_name_key TO uni_services_name`,
		`ALTER TABLE registry_service_accounts RENAME CONSTRAINT registry_service_accounts_name_key TO uni_registry_service_accounts_name`,
	} {
		if r := connect().Exec(sql); r.Error != nil {
			slog.Debug("constraint rename skipped", "sql", sql, "error", r.Error)
		}
	}

	if err := connect().AutoMigrate(
		&ServiceModel{},
		&ServiceRoleModel{},
		&ServiceEndpointModel{},
		&ServiceActionModel{},
		&ServiceAccountModel{},
		&ServiceDefaultGrantModel{},
	); err != nil {
		slog.Error("failed to migrate database", "error", err)
		os.Exit(1)
	}
	slog.Info("database initialized")

	// Seed service accounts. Read accounts (e.g. Conductor) from REGISTRY_SERVICE_ACCOUNTS=name=key.
	// Admin accounts (e.g. CI pipelines) from REGISTRY_ADMIN_ACCOUNTS=name=key.
	seedServiceAccounts(ctx, os.Getenv("REGISTRY_SERVICE_ACCOUNTS"), "read")
	seedServiceAccounts(ctx, os.Getenv("REGISTRY_ADMIN_ACCOUNTS"), "admin")

	// Seed service definitions from SERVICES=name=url,... env var.
	if raw := os.Getenv("SERVICES"); raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			parts := strings.SplitN(strings.TrimSpace(entry), "=", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				continue
			}
			name, svcURL := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			svcModel := ServiceModel{ServiceID: uuid.New().String(), Name: name, URL: svcURL}
			if err := upsertServiceModelByName(ctx, svcModel); err != nil {
				slog.Error("service seed failed", "service", name, "error", err)
			} else {
				slog.Info("service seeded", "service", name)
			}
		}
	}

	if manifestPath := os.Getenv("MANIFEST_FILE"); manifestPath != "" {
		loadManifest(ctx, manifestPath)
		notifyConductor(ctx)
		notifyWorkflows(ctx)
	}

	startNotificationTicker(ctx)

	// Register Registry itself as a Gatekeeper service account so its identity rotates.
	registry.StartKeyRotation(ctx, gatekeeperURL, "registry",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	startHealthCollector(ctx)

	mux := telemetry.NewMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /openapi.yaml", handleOpenAPIYAML)
	mux.HandleFunc("GET /system_health", handleSystemHealth)
	mux.HandleFunc("GET /services", handleListServices)
	mux.HandleFunc("POST /services", handleCreateService)
	mux.HandleFunc("DELETE /services/{id}", handleDeleteService)
	mux.HandleFunc("PUT /services/{id}/endpoints", handleUpdateServiceEndpoints)
	mux.HandleFunc("GET /default-grants", handleListDefaultGrants)
	mux.HandleFunc("GET /actions", handleListActions)
	mux.HandleFunc("POST /service-accounts", handleUpsertServiceAccount)
	mux.HandleFunc("POST /service-accounts/rotate-key", handleRotateServiceKey)

	port := envOrDefault("PORT", "8084")
	wrapped := otelhttp.NewHandler(&logger{mux}, "registry",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      wrapped,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if certFile != "" && keyFile != "" {
		tlsCfg := &tls.Config{}
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
