package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

var (
	gatekeeperURL   = envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
	forgeHTTPClient *http.Client
)

// initHTTPClient builds an instrumented HTTP client. When TLS_CLIENT_CERT_FILE and
// TLS_CLIENT_KEY_FILE are set, the client presents a certificate on outbound TLS
// connections — required when calling services with TLS_CLIENT_AUTH=require.
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
		Timeout:   10 * time.Second,
	}
}


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

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "forge")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(ctx)
	}
	initMetrics()
	forgeHTTPClient = initHTTPClient()

	if err := connect().AutoMigrate(&Execution{}); err != nil {
		slog.Error("failed to migrate database", "error", err)
		os.Exit(1)
	}
	if err := connect().Exec(`CREATE INDEX IF NOT EXISTS executions_user_created ON executions (user_id, created_at DESC)`).Error; err != nil {
		slog.Warn("failed to create executions index", "error", err)
	}
	if err := connect().Exec(`CREATE INDEX IF NOT EXISTS executions_pending ON executions (status) WHERE status IN ('pending', 'running')`).Error; err != nil {
		slog.Warn("failed to create executions pending index", "error", err)
	}
	if err := migrateAndSeedRunnerClasses(); err != nil {
		slog.Error("failed to migrate runner classes", "error", err)
		os.Exit(1)
	}
	slog.Info("database initialized")

	rt, err := newRuntime()
	if err != nil {
		slog.Error("failed to initialize runtime", "error", err)
		os.Exit(1)
	}
	slog.Info("runtime initialized", "type", envOrDefault("RUNTIME", "kubernetes"))

	initAllowedImages(os.Getenv("ALLOWED_IMAGES"))

	// Rotate the gatekeeper service key every 25 minutes. GATEKEEPER_SERVICE_KEY
	// must match the key in GATEKEEPER_SERVICES on gatekeeper. No-op if unset.
	registry.StartKeyRotation(ctx, gatekeeperURL, "forge",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	workers := newWorkerPool(rt)
	workers.Start(ctx, 10)
	slog.Info("worker pool started", "workers", 10)

	mux := telemetry.NewMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /executions", handleSubmit)
	mux.HandleFunc("GET /executions", handleList)
	mux.HandleFunc("GET /executions/{id}", handleGet)
	mux.HandleFunc("DELETE /executions/{id}", handleCancel(workers))
	mux.HandleFunc("GET /runner-classes", handleListRunnerClasses)
	mux.HandleFunc("POST /runner-classes", handleCreateRunnerClass)
	mux.HandleFunc("GET /runner-classes/{name}", handleGetRunnerClass)
	mux.HandleFunc("PUT /runner-classes/{name}", handleUpdateRunnerClass)
	mux.HandleFunc("DELETE /runner-classes/{name}", handleDeleteRunnerClass)

	port := envOrDefault("PORT", "8083")
	wrapped := otelhttp.NewHandler(&logger{mux}, "forge",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      wrapped,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
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
