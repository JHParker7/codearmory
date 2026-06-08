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
	slog.Info(r.Method+" "+r.URL.Path,
		"status", rw.status,
		"duration", time.Since(start),
		"trace_id", sc.TraceID().String(),
		"span_id", sc.SpanID().String(),
	)
}

// seedServiceAccounts upserts service accounts from a "name=key" comma-separated
// string with the given role. On every startup the hash is refreshed from the
// env var so that a restarted Registry re-synchronises with clients that are
// about to rotate using their initial key.
func seedServiceAccounts(ctx context.Context, raw, role string) {
	if raw == "" {
		return
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		idx := strings.Index(entry, "=")
		if idx < 1 || idx == len(entry)-1 {
			slog.Warn("seedServiceAccounts: invalid entry, expected name=key", "entry", entry, "role", role)
			continue
		}
		name, key := entry[:idx], entry[idx+1:]
		hash, err := bcrypt.GenerateFromPassword([]byte(key), 12)
		if err != nil {
			slog.Error("seedServiceAccounts: bcrypt failed", "name", name, "error", err)
			continue
		}

		var existing string
		scanErr := pool.QueryRow(ctx,
			`SELECT account_id FROM registry_service_accounts WHERE name = $1`, name,
		).Scan(&existing)
		if scanErr != nil {
			if _, err := pool.Exec(ctx,
				`INSERT INTO registry_service_accounts (account_id, name, hashed_key, role)
				 VALUES ($1, $2, $3, $4)`,
				uuid.New().String(), name, string(hash), role,
			); err != nil {
				slog.Error("seedServiceAccounts: insert failed", "name", name, "error", err)
			} else {
				slog.Info("seedServiceAccounts: created", "name", name, "role", role)
			}
		} else {
			if _, err := pool.Exec(ctx,
				`UPDATE registry_service_accounts SET hashed_key = $1, role = $2, updated_at = now()
				 WHERE name = $3`,
				string(hash), role, name,
			); err != nil {
				slog.Error("seedServiceAccounts: update failed", "name", name, "error", err)
			} else {
				slog.Info("seedServiceAccounts: updated key", "name", name, "role", role)
			}
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
	header := r.Header.Get("X-Service-Key")
	if header == "" {
		http.Error(w, "missing X-Service-Key header", http.StatusUnauthorized)
		return
	}
	idx := strings.Index(header, ":")
	if idx < 1 {
		http.Error(w, "invalid X-Service-Key format, expected name:key", http.StatusUnauthorized)
		return
	}
	name, key := header[:idx], header[idx+1:]

	var hashedKey string
	if err := pool.QueryRow(r.Context(),
		`SELECT hashed_key FROM registry_service_accounts WHERE name = $1`, name,
	).Scan(&hashedKey); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hashedKey), []byte(key)) != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	val, _ := rotationMu.LoadOrStore(name, &sync.Mutex{})
	mu := val.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		slog.Error("rotate service key: rand failed", "service", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	newKey := hex.EncodeToString(raw)
	newHash, err := bcrypt.GenerateFromPassword([]byte(newKey), 12)
	if err != nil {
		slog.Error("rotate service key: bcrypt failed", "service", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if _, err := pool.Exec(r.Context(),
		`UPDATE registry_service_accounts SET hashed_key = $1, updated_at = now() WHERE name = $2`,
		string(newHash), name,
	); err != nil {
		slog.Error("rotate service key: db update failed", "service", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("registry service key rotated", "service", name)
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
		slog.Error("manifest: failed to read file", "path", path, "error", err)
		return
	}
	var entries []manifestEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Error("manifest: failed to parse JSON", "path", path, "error", err)
		return
	}
	for _, e := range entries {
		hashedKey, err := hashServiceKey(e.ServiceKey)
		if err != nil {
			slog.Error("manifest: failed to hash service key", "name", e.Name, "error", err)
			continue
		}

		var serviceID string
		err = pool.QueryRow(ctx, `SELECT service_id FROM services WHERE name = $1`, e.Name).Scan(&serviceID)
		if err != nil {
			serviceID = uuid.New().String()
			if _, err := pool.Exec(ctx,
				`INSERT INTO services (service_id, name, url, description, forward_auth, service_key)
				 VALUES ($1, $2, $3, $4, $5, $6)`,
				serviceID, e.Name, e.URL, e.Description, e.ForwardAuth, hashedKey); err != nil {
				slog.Error("manifest: failed to insert service", "name", e.Name, "error", err)
				continue
			}
			slog.Info("manifest: service created", "name", e.Name)
		} else {
			if _, err := pool.Exec(ctx,
				`UPDATE services SET description = $1, forward_auth = $2, service_key = $3, active = true, updated_at = now() WHERE service_id = $4`,
				e.Description, e.ForwardAuth, hashedKey, serviceID); err != nil {
				slog.Error("manifest: failed to update service", "name", e.Name, "error", err)
				continue
			}
			slog.Info("manifest: service updated", "name", e.Name)
		}
		pool.Exec(ctx, `DELETE FROM service_endpoints WHERE service_id = $1`, serviceID)      //nolint:errcheck
		pool.Exec(ctx, `DELETE FROM service_roles WHERE service_id = $1`, serviceID)         //nolint:errcheck
		pool.Exec(ctx, `DELETE FROM service_actions WHERE service_id = $1`, serviceID)       //nolint:errcheck
		pool.Exec(ctx, `DELETE FROM service_default_grants WHERE service_id = $1`, serviceID) //nolint:errcheck
		for _, ep := range e.Endpoints {
			if _, err := pool.Exec(ctx,
				`INSERT INTO service_endpoints (endpoint_id, service_id, method, path, action, resource, public)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				uuid.New().String(), serviceID, ep.Method, ep.Path, ep.Action, ep.Resource, ep.Public); err != nil {
				slog.Error("manifest: failed to insert endpoint", "service", e.Name, "path", ep.Path, "error", err)
			}
		}
		slog.Info("manifest: endpoints registered", "name", e.Name, "count", len(e.Endpoints))
		for _, a := range e.Actions {
			if a.Name == "" || a.Method == "" || a.Path == "" {
				continue
			}
			if _, err := pool.Exec(ctx,
				`INSERT INTO service_actions (action_id, service_id, name, method, path, body_transforms, async_config)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				uuid.New().String(), serviceID, a.Name, a.Method, a.Path,
				jsonbOrNil(a.BodyTransforms), jsonbOrNil(a.Async)); err != nil {
				slog.Error("manifest: failed to insert action", "service", e.Name, "action", a.Name, "error", err)
			}
		}
		if len(e.Actions) > 0 {
			slog.Info("manifest: actions registered", "name", e.Name, "count", len(e.Actions))
		}
		for _, g := range e.DefaultGrants {
			if g.GrantOn == "" || len(g.Actions) == 0 || len(g.Resources) == 0 {
				continue
			}
			actionsJSON, _ := json.Marshal(g.Actions)
			resourcesJSON, _ := json.Marshal(g.Resources)
			if _, err := pool.Exec(ctx,
				`INSERT INTO service_default_grants (grant_id, service_id, grant_on, actions, resources)
				 VALUES ($1, $2, $3, $4, $5)`,
				uuid.New().String(), serviceID, g.GrantOn, actionsJSON, resourcesJSON); err != nil {
				slog.Error("manifest: failed to insert default grant", "service", e.Name, "grant_on", g.GrantOn, "error", err)
			}
		}
		if len(e.DefaultGrants) > 0 {
			slog.Info("manifest: default grants registered", "name", e.Name, "count", len(e.DefaultGrants))
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
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

	connectDB(ctx)
	defer pool.Close()

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
			var existing string
			err := pool.QueryRow(ctx, `SELECT service_id FROM services WHERE name = $1`, name).Scan(&existing)
			if err != nil {
				id := uuid.New().String()
				pool.Exec(ctx, //nolint:errcheck
					`INSERT INTO services (service_id, name, url) VALUES ($1, $2, $3)`,
					id, name, svcURL)
				slog.Info("service seeded", "name", name)
			} else {
				pool.Exec(ctx, //nolint:errcheck
					`UPDATE services SET url = $1, active = true, updated_at = now() WHERE name = $2`,
					svcURL, name)
				slog.Info("service updated from seed", "name", name)
			}
		}
	}

	if manifestPath := os.Getenv("MANIFEST_FILE"); manifestPath != "" {
		loadManifest(ctx, manifestPath)
	}

	// Register Registry itself as a Gatekeeper service account so its identity rotates.
	registry.StartKeyRotation(ctx, gatekeeperURL, "registry",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	startHealthCollector(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /system_health", handleSystemHealth)
	mux.HandleFunc("GET /services", handleListServices)
	mux.HandleFunc("POST /services", handleCreateService)
	mux.HandleFunc("DELETE /services/{id}", handleDeleteService)
	mux.HandleFunc("PUT /services/{id}/endpoints", handleUpdateServiceEndpoints)
	mux.HandleFunc("GET /default-grants", handleListDefaultGrants)
	mux.HandleFunc("GET /actions", handleListActions)
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
