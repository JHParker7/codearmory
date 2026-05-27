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

	"codearmory.local/svckit/telemetry"
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /services", handleListServices)
	mux.HandleFunc("POST /services", handleCreateService)
	mux.HandleFunc("DELETE /services/{id}", handleDeleteService)
	mux.HandleFunc("PUT /services/{id}/endpoints", handleUpdateServiceEndpoints)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8084"
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
