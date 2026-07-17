package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
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
)

var (
	gatekeeperClient *gk.Client
	gatekeeperURL    = envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
	httpClient       *http.Client
)

func initHTTPClient() *http.Client {
	tlsCfg := &tls.Config{}
	if certFile, keyFile := os.Getenv("TLS_CLIENT_CERT_FILE"), os.Getenv("TLS_CLIENT_KEY_FILE"); certFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			slog.Error("failed to load client TLS certificate", "error", err)
			os.Exit(1)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	if caFile := os.Getenv("TLS_CA_FILE"); caFile != "" {
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			slog.Error("failed to read TLS_CA_FILE", "error", err)
			os.Exit(1)
		}
		caPool := x509.NewCertPool()
		caPool.AppendCertsFromPEM(caCert)
		tlsCfg.RootCAs = caPool
	}
	return &http.Client{
		Transport: otelhttp.NewTransport(&http.Transport{TLSClientConfig: tlsCfg}),
		Timeout:   15 * time.Second,
	}
}

func newGatekeeperClient() *gk.Client {
	return &gk.Client{URL: gatekeeperURL, Service: "outpost-gateway", HTTPClient: httpClient}
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

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// buildClientTLSConfig builds the optional mTLS client-auth config from the
// TLS_CLIENT_AUTH / TLS_CLIENT_CA_FILE env vars. Returns an error instead of
// exiting so the branches are unit-testable (main turns the error into a fatal).
func buildClientTLSConfig() (*tls.Config, error) {
	tlsCfg := &tls.Config{}
	switch os.Getenv("TLS_CLIENT_AUTH") {
	case "require":
		caFile := os.Getenv("TLS_CLIENT_CA_FILE")
		if caFile == "" {
			return nil, errors.New("TLS_CLIENT_AUTH=require but TLS_CLIENT_CA_FILE is not set")
		}
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS_CLIENT_CA_FILE %q: %w", caFile, err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("TLS_CLIENT_CA_FILE %q contains no valid PEM certificates", caFile)
		}
		tlsCfg.ClientCAs = caPool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	case "request":
		tlsCfg.ClientAuth = tls.RequestClientCert
	}
	return tlsCfg, nil
}

// buildMux registers every route and returns the handler. Extracted from main so
// the routing table is unit-testable without standing up a server or TLS.
func buildMux() http.Handler {
	mux := telemetry.NewMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /openapi.yaml", handleOpenAPIYAML)

	// User-facing (via conductor, gatekeeper bearer).
	mux.HandleFunc("POST /outposts", handleCreateOutpost)
	mux.HandleFunc("GET /outposts", handleListOutposts)
	mux.HandleFunc("GET /outposts/{id}", handleGetOutpost)
	mux.HandleFunc("DELETE /outposts/{id}", handleDeleteOutpost)
	mux.HandleFunc("POST /outposts/{id}/commands", handleEnqueueOutpostCommand)

	// Outpost-facing (direct, outpost-key auth). Not routed through conductor.
	mux.HandleFunc("POST /outpost/register", handleRegister)
	mux.HandleFunc("GET /outpost/commands", handleCommands)
	mux.HandleFunc("POST /outpost/commands/{id}/ack", handleAckCommand)
	mux.HandleFunc("POST /outpost/events", handleEvents)
	mux.HandleFunc("POST /outpost/heartbeat", handleHeartbeat)

	// Internal (service HMAC). Control-plane services enqueue commands here.
	mux.HandleFunc("POST /internal/commands", handleEnqueueCommand)
	return mux
}

func main() {
	logLevel := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		_ = logLevel.UnmarshalText([]byte(v))
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "outpost-gateway")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(context.Background())
	}
	if err := run(ctx); err != nil {
		slog.Error("fatal", "error", err)
		stop()
		os.Exit(1)
	}
}

// run owns the full server lifecycle: migrate, wire dependencies, serve, and
// gracefully shut down when ctx is cancelled. Extracted from main and returning
// an error instead of os.Exit-ing so a test can start it on an ephemeral port,
// hit it, and cancel — covering the bootstrap and ListenAndServe/Shutdown paths.
func run(ctx context.Context) error {
	initMetrics()
	httpClient = initHTTPClient()

	if err := connect().AutoMigrate(&Outpost{}, &OutpostCommand{}, &OutpostEvent{}); err != nil {
		return fmt.Errorf("migrate tables: %w", err)
	}
	// Outpost names are unique within their scope (the org when set, else the
	// owning user) so an outpost can be referenced by name rather than its UUID.
	// Partial indexes (WHERE active) let a name be reused after an outpost is removed.
	if err := connect().Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_outposts_org_name ON outposts (org_id, name) WHERE active AND org_id <> ''`).Error; err != nil {
		slog.Warn("failed to create outposts org-name unique index (existing duplicate names?)", "error", err)
	}
	if err := connect().Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_outposts_user_name ON outposts (user_id, name) WHERE active AND org_id = ''`).Error; err != nil {
		slog.Warn("failed to create outposts user-name unique index (existing duplicate names?)", "error", err)
	}
	slog.Info("database initialized")

	if outpostInternalKey == "" {
		slog.Error("OUTPOST_INTERNAL_KEY not set — internal command/event auth is disabled and will reject all internal traffic")
	}
	if len(eventConsumers) == 0 {
		slog.Warn("EVENT_CONSUMERS not set — no integration consumers configured; events cannot be delivered")
	} else {
		slog.Info("event consumers configured", "integrations", len(eventConsumers))
	}

	gatekeeperClient = newGatekeeperClient()
	registry.StartKeyRotation(ctx, gatekeeperURL, "outpost-gateway",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	startDispatcher(ctx)

	port := envOrDefault("PORT", "8092")
	wrapped := otelhttp.NewHandler(limitBody(&requestLogger{buildMux()}), "outpost-gateway",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")
	srv := &http.Server{
		Addr:        ":" + port,
		Handler:     wrapped,
		ReadTimeout: 15 * time.Second,
		// Write timeout must exceed the command long-poll hold (30s).
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	if certFile != "" && keyFile != "" {
		tlsCfg, err := buildClientTLSConfig()
		if err != nil {
			return fmt.Errorf("TLS client-auth config: %w", err)
		}
		srv.TLSConfig = tlsCfg
	}
	serveErr := make(chan error, 1)
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
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
