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
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	ctx := context.Background()

	otelHandler, shutdown, err := setupOTel(ctx)
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(&fanoutHandler{handlers: []slog.Handler{jsonHandler, otelHandler}}))
		defer shutdown(ctx)
	}

	connectDB(ctx)
	defer pool.Close()

	if _, err := pool.Exec(ctx, createTables); err != nil {
		slog.Error("failed to create tables", "error", err)
		os.Exit(1)
	}

	// Bootstrap services from SERVICES=name=url=key,... env var.
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
			var existing string
			err := pool.QueryRow(ctx, `SELECT service_id FROM services WHERE name = $1`, name).Scan(&existing)
			if err != nil {
				id := uuid.New().String()
				pool.Exec(ctx,
					`INSERT INTO services (service_id, name, url, service_key_hash) VALUES ($1, $2, $3, $4)`,
					id, name, svcURL, keyHash)
				slog.Info("service auto-registered", "name", name)
			} else {
				if keyHash != "" {
					pool.Exec(ctx,
						`UPDATE services SET url = $1, service_key_hash = $2, active = true, updated_at = now() WHERE name = $3`,
						svcURL, keyHash, name)
				} else {
					pool.Exec(ctx,
						`UPDATE services SET url = $1, active = true, updated_at = now() WHERE name = $2`,
						svcURL, name)
				}
				slog.Info("service auto-updated", "name", name)
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /services", handleListServices)
	mux.HandleFunc("POST /services", handleCreateService)
	mux.HandleFunc("DELETE /services/{id}", handleDeleteService)
	mux.HandleFunc("POST /services/register", handleServiceSelfRegister)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8084"
	}

	wrapped := otelhttp.NewHandler(&logger{mux}, "registry",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	slog.Info("listening", "port", port)
	if err := http.ListenAndServe(":"+port, wrapped); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
