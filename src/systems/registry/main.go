package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

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
}

// loadManifest reads a JSON manifest file and upserts service+endpoint definitions.
// Existing endpoints for each service are replaced; the service URL and service_key
// are updated if the service already exists.
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
			// Service doesn't exist: insert it.
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
			// Service exists: update URL, description, and service_key.
			if _, err := pool.Exec(ctx,
				`UPDATE services SET url = $1, description = $2, forward_auth = $3, service_key = $4, active = true, updated_at = now() WHERE service_id = $5`,
				e.URL, e.Description, e.ForwardAuth, hashedKey, serviceID); err != nil {
				slog.Error("manifest: failed to update service", "name", e.Name, "error", err)
				continue
			}
			slog.Info("manifest: service updated", "name", e.Name)
		}
		// Replace endpoints.
		pool.Exec(ctx, `DELETE FROM service_endpoints WHERE service_id = $1`, serviceID) //nolint:errcheck
		pool.Exec(ctx, `DELETE FROM service_roles WHERE service_id = $1`, serviceID)     //nolint:errcheck
		for _, ep := range e.Endpoints {
			if _, err := pool.Exec(ctx,
				`INSERT INTO service_endpoints (endpoint_id, service_id, method, path, action, resource, public)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				uuid.New().String(), serviceID, ep.Method, ep.Path, ep.Action, ep.Resource, ep.Public); err != nil {
				slog.Error("manifest: failed to insert endpoint", "service", e.Name, "path", ep.Path, "error", err)
			}
		}
		slog.Info("manifest: endpoints registered", "name", e.Name, "count", len(e.Endpoints))
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

	// Seed services from SERVICES=name=url,name=url env var.
	// Format: comma-separated name=url pairs. This is a convenience for initial
	// setup; services can also be registered via POST /services with the admin key.
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

	// Load endpoint manifest from MANIFEST_FILE if configured.
	if manifestPath := os.Getenv("MANIFEST_FILE"); manifestPath != "" {
		loadManifest(ctx, manifestPath)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /services", handleListServices)
	mux.HandleFunc("POST /services", handleCreateService)
	mux.HandleFunc("DELETE /services/{id}", handleDeleteService)
	mux.HandleFunc("PUT /services/{id}/endpoints", handleUpdateServiceEndpoints)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}

	wrapped := otelhttp.NewHandler(&logger{mux}, "registry",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      wrapped,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		slog.Info("listening", "port", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
