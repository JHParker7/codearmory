package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

var (
	db            *pgxpool.Pool
	gatekeeperURL = envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
	// serviceURLs maps registered service names to their base URLs.
	// Populated at startup from SERVICES (format: "name=url,name=url,...").
	serviceURLs = map[string]string{}
	httpClient  = &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   10 * time.Second,
	}
)

const createTables = `
CREATE TABLE IF NOT EXISTS workflows (
    workflow_id  TEXT        PRIMARY KEY,
    name         TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    created_by   TEXT        NOT NULL,
    org_id       TEXT        NOT NULL DEFAULT '',
    steps        JSONB       NOT NULL DEFAULT '[]',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    active       BOOL        NOT NULL DEFAULT true
);
CREATE INDEX IF NOT EXISTS workflows_created_by ON workflows (created_by);
CREATE INDEX IF NOT EXISTS workflows_org_id ON workflows (org_id) WHERE org_id != '';

CREATE TABLE IF NOT EXISTS workflow_runs (
    run_id       TEXT        PRIMARY KEY,
    workflow_id  TEXT        NOT NULL REFERENCES workflows(workflow_id),
    triggered_by TEXT        NOT NULL,
    org_id       TEXT        NOT NULL DEFAULT '',
    status       TEXT        NOT NULL DEFAULT 'pending',
    current_step INT         NOT NULL DEFAULT 0,
    inputs       JSONB       NOT NULL DEFAULT '{}',
    token        TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at   TIMESTAMPTZ,
    ended_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS workflow_runs_workflow_id ON workflow_runs (workflow_id, created_at DESC);
CREATE INDEX IF NOT EXISTS workflow_runs_triggered_by ON workflow_runs (triggered_by, created_at DESC);
CREATE INDEX IF NOT EXISTS workflow_runs_org_id ON workflow_runs (org_id, created_at DESC) WHERE org_id != '';
CREATE INDEX IF NOT EXISTS workflow_runs_pending ON workflow_runs (status) WHERE status IN ('pending', 'running');

-- Idempotent migrations for existing deployments.
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS workflow_step_runs (
    step_run_id     TEXT        PRIMARY KEY,
    run_id          TEXT        NOT NULL REFERENCES workflow_runs(run_id),
    step_index      INT         NOT NULL,
    step_name       TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending',
    response_status INT,
    response_body   TEXT,
    started_at      TIMESTAMPTZ,
    ended_at        TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS workflow_step_runs_run_id ON workflow_step_runs (run_id, step_index);
`

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
		return string(data)
	}
	return os.Getenv(name)
}

func secretOrDefault(name, def string) string {
	if v := secret(name); v != "" {
		return v
	}
	return def
}

// initServices parses SERVICES (format: "name=url,name=url,...") into serviceURLs.
// Gatekeeper is always pre-populated so steps can call it without explicit registration.
func initServices() {
	serviceURLs["gatekeeper"] = gatekeeperURL
	raw := os.Getenv("SERVICES")
	if raw == "" {
		return
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		name, url, ok := strings.Cut(entry, "=")
		if !ok || name == "" || url == "" {
			slog.Warn("initServices: invalid entry, expected name=url", "entry", entry)
			continue
		}
		serviceURLs[strings.TrimSpace(name)] = strings.TrimSpace(url)
	}
	slog.Info("services registered", "count", len(serviceURLs))
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *statusResponseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

type requestLogger struct{ handler http.Handler }

func (l *requestLogger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "workflows")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(ctx)
	}
	initMetrics()

	db, err = pgxpool.New(ctx, secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/workflows"))
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if _, err := db.Exec(ctx, createTables); err != nil {
		slog.Error("failed to create tables", "error", err)
		os.Exit(1)
	}
	slog.Info("database pool initialized")

	initServices()
	registry.StartKeyRotation(ctx, gatekeeperURL, "workflows",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	workers := newWorkerPool(db)
	workers.Start(ctx, 5)
	slog.Info("worker pool started", "workers", 5)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	mux.HandleFunc("POST /workflows", handleCreateWorkflow)
	mux.HandleFunc("GET /workflows", handleListWorkflows)
	mux.HandleFunc("GET /workflows/{id}", handleGetWorkflow)
	mux.HandleFunc("PUT /workflows/{id}", handleUpdateWorkflow)
	mux.HandleFunc("DELETE /workflows/{id}", handleDeleteWorkflow)

	mux.HandleFunc("POST /workflows/{id}/runs", handleTriggerRun)
	mux.HandleFunc("GET /runs", handleListRuns)
	mux.HandleFunc("GET /runs/{id}", handleGetRun)
	mux.HandleFunc("DELETE /runs/{id}", handleCancelRun(workers))

	port := envOrDefault("PORT", "8085")
	wrapped := otelhttp.NewHandler(limitBody(&requestLogger{mux}), "workflows",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      wrapped,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
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
