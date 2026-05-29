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
	httpClient    = &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   10 * time.Second,
	}
)

const createTables = `
CREATE TABLE IF NOT EXISTS tickets (
    ticket_id           TEXT        PRIMARY KEY,
    title               TEXT        NOT NULL,
    description         TEXT        NOT NULL DEFAULT '',
    status              TEXT        NOT NULL DEFAULT 'open',
    priority            TEXT        NOT NULL DEFAULT 'medium',
    created_by          TEXT        NOT NULL,
    org_id              TEXT        NOT NULL DEFAULT '',
    assignee_id         TEXT,
    workflow_id         TEXT,
    run_id              TEXT,
    forge_execution_id  TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    active              BOOL        NOT NULL DEFAULT true
);
CREATE INDEX IF NOT EXISTS tickets_created_by ON tickets (created_by, created_at DESC);
CREATE INDEX IF NOT EXISTS tickets_org_id ON tickets (org_id, created_at DESC) WHERE org_id != '';
CREATE INDEX IF NOT EXISTS tickets_status ON tickets (status) WHERE active = true;

CREATE TABLE IF NOT EXISTS ticket_comments (
    comment_id  TEXT        PRIMARY KEY,
    ticket_id   TEXT        NOT NULL REFERENCES tickets(ticket_id),
    author_id   TEXT        NOT NULL,
    body        TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    active      BOOL        NOT NULL DEFAULT true
);
CREATE INDEX IF NOT EXISTS ticket_comments_ticket_id ON ticket_comments (ticket_id, created_at);

-- Idempotent migrations for existing deployments.
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS assignee_id TEXT;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS workflow_id TEXT;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS run_id TEXT;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS forge_execution_id TEXT;
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
		return strings.TrimRight(string(data), "\n")
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

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "tickets")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(ctx)
	}
	initMetrics()

	db, err = pgxpool.New(ctx, secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/tickets"))
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

	registry.StartKeyRotation(ctx, gatekeeperURL, "tickets",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	mux.HandleFunc("POST /tickets", handleCreateTicket)
	mux.HandleFunc("GET /tickets", handleListTickets)
	mux.HandleFunc("GET /tickets/{id}", handleGetTicket)
	mux.HandleFunc("PUT /tickets/{id}", handleUpdateTicket)
	mux.HandleFunc("DELETE /tickets/{id}", handleDeleteTicket)

	mux.HandleFunc("POST /tickets/{id}/comments", handleAddComment)
	mux.HandleFunc("DELETE /tickets/{id}/comments/{comment_id}", handleDeleteComment)

	port := envOrDefault("PORT", "8086")
	wrapped := otelhttp.NewHandler(limitBody(&requestLogger{mux}), "tickets",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      wrapped,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
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
