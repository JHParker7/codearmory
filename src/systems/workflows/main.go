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

	gk "github.com/code-armory-app/codearmory_sdk/gatekeeper"
	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	db               *gorm.DB
	gatekeeperClient *gk.Client
	gatekeeperURL    = envOrDefault("GATEKEEPER_URL", "http://localhost:8081")
	hooksTriggerKey = os.Getenv("HOOKS_TRIGGER_KEY")
	// serviceURLs maps registered service names to their base URLs.
	// Populated at startup from SERVICES (format: "name=url,name=url,...").
	serviceURLs = map[string]string{}
	httpClient  = &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   10 * time.Second,
	}
)

func newGatekeeperClient() *gk.Client {
	return &gk.Client{URL: gatekeeperURL, Service: "workflows", HTTPClient: httpClient}
}

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
		defer shutdown(context.Background())
	}
	initMetrics()

	dsn := secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/workflows")
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}

	if err := db.AutoMigrate(&Workflow{}, &WorkflowRun{}, &WorkflowStepRun{}); err != nil {
		slog.Error("failed to run AutoMigrate", "error", err)
		os.Exit(1)
	}
	slog.Info("database initialized")

	initServices()
	gatekeeperClient = newGatekeeperClient()
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
	mux.HandleFunc("POST /internal/workflows/{id}/runs", handleInternalTriggerRun)
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
