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
	db             *pgxpool.Pool
	gatekeeperURL  = envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
	forgeHTTPClient = &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   10 * time.Second,
	}
)

const createTables = `
CREATE TABLE IF NOT EXISTS executions (
    execution_id  TEXT        PRIMARY KEY,
    user_id       TEXT        NOT NULL,
    image         TEXT        NOT NULL,
    command       JSONB       NOT NULL,
    env           JSONB       NOT NULL DEFAULT '{}',
    timeout_secs  BIGINT      NOT NULL DEFAULT 30,
    status        TEXT        NOT NULL DEFAULT 'pending',
    exit_code     INT,
    stdout        TEXT,
    stderr        TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at    TIMESTAMPTZ,
    ended_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS executions_user_created ON executions (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS executions_pending ON executions (status) WHERE status IN ('pending', 'running');
`

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// secret reads a secret from an env var. If NAME_FILE is set, the value is read
// from that file path instead — the standard convention for Docker secrets and
// Kubernetes secret volume mounts.
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

// ── Logger middleware ─────────────────────────────────────────────────────────

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

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), serviceConfig.Name)
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(ctx)
	}
	initMetrics()

	db, err = pgxpool.New(ctx, secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/forge"))
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

	rt, err := newRuntime()
	if err != nil {
		slog.Error("failed to initialize runtime", "error", err)
		os.Exit(1)
	}
	slog.Info("runtime initialized", "type", envOrDefault("RUNTIME", "kubernetes"))

	initAllowedImages(os.Getenv("ALLOWED_IMAGES"))

	// Rotate the gatekeeper service key every 25 minutes. GATEKEEPER_SERVICE_KEY
	// must match the key in GATEKEEPER_SERVICES on gatekeeper. No-op if unset.
	registry.StartKeyRotation(ctx, gatekeeperURL, serviceConfig.Name,
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	workers := newWorkerPool(db, rt)
	workers.Start(ctx, 10)
	slog.Info("worker pool started", "workers", 10)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /executions", handleSubmit(workers))
	mux.HandleFunc("GET /executions", handleList)
	mux.HandleFunc("GET /executions/{id}", handleGet)
	mux.HandleFunc("DELETE /executions/{id}", handleCancel(workers))

	port := envOrDefault("PORT", "8083")
	wrapped := otelhttp.NewHandler(&logger{mux}, "forge",
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
