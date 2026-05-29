package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

var (
	db              *pgxpool.Pool
	gatekeeperURL   = envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
	workflowsURL    = envOrDefault("WORKFLOWS_URL", "http://localhost:8085")
	hooksTriggerKey = os.Getenv("HOOKS_TRIGGER_KEY")
	httpClient      = &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   10 * time.Second,
	}
)

const createTables = `
CREATE TABLE IF NOT EXISTS pipeline_rules (
    rule_id       TEXT        PRIMARY KEY,
    name          TEXT        NOT NULL,
    repo          TEXT        NOT NULL,
    events        TEXT[]      NOT NULL,
    ref_filter    TEXT,
    workflow_id   TEXT        NOT NULL,
    secret        TEXT,
    input_mapping JSONB       NOT NULL DEFAULT '{}',
    created_by    TEXT        NOT NULL,
    org_id        TEXT        NOT NULL DEFAULT '',
    active        BOOL        NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS pipeline_rules_repo ON pipeline_rules (repo, active);
CREATE INDEX IF NOT EXISTS pipeline_rules_org ON pipeline_rules (org_id) WHERE org_id != '';

CREATE TABLE IF NOT EXISTS hook_events (
    event_id      TEXT        PRIMARY KEY,
    repo          TEXT        NOT NULL,
    event_type    TEXT        NOT NULL,
    ref           TEXT        NOT NULL DEFAULT '',
    payload       JSONB       NOT NULL DEFAULT '{}',
    rules_matched INT         NOT NULL DEFAULT 0,
    status        TEXT        NOT NULL DEFAULT 'received',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS hook_events_created_at ON hook_events (created_at DESC);

CREATE TABLE IF NOT EXISTS hook_triggers (
    trigger_id    TEXT        PRIMARY KEY,
    event_id      TEXT        NOT NULL REFERENCES hook_events(event_id),
    rule_id       TEXT        NOT NULL,
    workflow_id   TEXT        NOT NULL,
    run_id        TEXT,
    status        TEXT        NOT NULL DEFAULT 'pending',
    error         TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS hook_triggers_event ON hook_triggers (event_id);

ALTER TABLE pipeline_rules ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
ALTER TABLE pipeline_rules ADD COLUMN IF NOT EXISTS secret TEXT;
ALTER TABLE pipeline_rules ADD COLUMN IF NOT EXISTS input_mapping JSONB NOT NULL DEFAULT '{}';
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

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "hooks")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(ctx)
	}
	initMetrics()

	db, err = pgxpool.New(ctx, secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/hooks"))
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

	registry.StartKeyRotation(ctx, gatekeeperURL, "hooks",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	mux.HandleFunc("POST /rules", handleCreateRule)
	mux.HandleFunc("GET /rules", handleListRules)
	mux.HandleFunc("GET /rules/{id}", handleGetRule)
	mux.HandleFunc("PUT /rules/{id}", handleUpdateRule)
	mux.HandleFunc("DELETE /rules/{id}", handleDeleteRule)

	mux.HandleFunc("POST /hooks", handleWebhook)

	mux.HandleFunc("GET /events", handleListEvents)
	mux.HandleFunc("GET /events/{id}", handleGetEvent)

	port := envOrDefault("PORT", "8087")
	wrapped := otelhttp.NewHandler(limitBody(&requestLogger{mux}), "hooks",
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
