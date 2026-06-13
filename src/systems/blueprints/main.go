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
	"strings"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

var (
	gatekeeperURL = envOrDefault("GATEKEEPER_URL", "http://localhost:8081")
	httpClient    = &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   10 * time.Second,
	}
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// secret reads the named environment variable. If <NAME>_FILE is set, the
// value is read from that file instead (trailing whitespace stripped), so that
// Docker Compose secrets mounts and Kubernetes Secret volumes work without any
// code changes. The file path takes precedence over the plain env var. If the
// file is specified but unreadable the process exits immediately.
func secret(name string) string {
	if path := os.Getenv(name + "_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("cannot read secret file", "var", name+"_FILE", "path", path, "error", err)
			os.Exit(1)
		}
		return strings.TrimSpace(string(data))
	}
	return os.Getenv(name)
}

func secretOrDefault(name, def string) string {
	if v := secret(name); v != "" {
		return v
	}
	return def
}

const maxBodyBytes = 64 * 1024 * 1024 // 64 MB — generous upper bound for Terraform state

// ── Logger middleware ─────────────────────────────────────────────────────────

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *statusResponseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

type Logger struct{ handler http.Handler }

func (l *Logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	l.handler.ServeHTTP(rw, r)
	sc := trace.SpanFromContext(r.Context()).SpanContext()
	msg := r.Method + " " + r.URL.Path
	attrs := []any{
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
			slog.Warn(msg, attrs...)
		} else {
			slog.Debug(msg, attrs...)
		}
	default:
		slog.InfoContext(r.Context(), "http request", append([]any{"method", r.Method, "path", r.URL.Path}, attrs...)...)
	}
}

func newLogger(h http.Handler) *Logger { return &Logger{h} }

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logLevel := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		_ = logLevel.UnmarshalText([]byte(v))
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "blueprints")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(context.Background())
	}
	initMetrics()

	if err := initEncryption(); err != nil {
		slog.Error("encryption init failed", "error", err)
		os.Exit(1)
	}
	if err := initClientCA(); err != nil {
		slog.Error("client CA init failed", "error", err)
		os.Exit(1)
	}
	initCache()

	// Rename legacy PostgreSQL auto-named constraints to GORM's convention (one-shot).
	for _, sql := range []string{
		`ALTER TABLE backend_credentials RENAME CONSTRAINT backend_credentials_cert_fp_key TO uni_backend_credentials_cert_fp`,
		`ALTER TABLE backend_credentials RENAME CONSTRAINT backend_credentials_token_hash_key TO uni_backend_credentials_token_hash`,
	} {
		if r := connect().Exec(sql); r.Error != nil {
			slog.Debug("constraint rename skipped", "sql", sql, "error", r.Error)
		}
	}

	if err := connect().AutoMigrate(&State{}, &StateLock{}, &BackendCredential{}); err != nil {
		slog.Error("failed to migrate database", "error", err)
		os.Exit(1)
	}
	slog.Info("database initialized")

	// Rotate the gatekeeper service key every 25 minutes so credentials are always
	// short-lived. GATEKEEPER_SERVICE_KEY must match the key in GATEKEEPER_SERVICES
	// on gatekeeper. The loop is a no-op if the variable is unset.
	registry.StartKeyRotation(ctx, gatekeeperURL, "blueprints",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	mux := telemetry.NewMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /openapi.yaml", handleOpenAPIYAML)
	mux.HandleFunc("POST /backend", handleCreateBackend)

	// User-scoped: /state/{username}/{workspace}
	mux.HandleFunc("GET /state/{username}/{workspace}", func(w http.ResponseWriter, r *http.Request) {
		k, _ := userKey(r)
		handleGetState(w, r, k)
	})
	mux.HandleFunc("POST /state/{username}/{workspace}", func(w http.ResponseWriter, r *http.Request) {
		k, _ := userKey(r)
		handleUpdateState(w, r, k)
	})
	mux.HandleFunc("DELETE /state/{username}/{workspace}", func(w http.ResponseWriter, r *http.Request) {
		k, _ := userKey(r)
		handleDeleteState(w, r, k)
	})
	mux.HandleFunc("/state/{username}/{workspace}", lockUnlock(userKey))

	port := envOrDefault("PORT", "8093")

	wrappedMux := otelhttp.NewHandler(newLogger(mux), "blueprints",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")
	caFile := os.Getenv("CA_CERT_FILE")

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      wrappedMux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if certFile != "" && keyFile != "" {
		tlsConfig := &tls.Config{}
		if caFile != "" {
			caCert, err := os.ReadFile(caFile)
			if err != nil {
				slog.Error("failed to read CA cert", "error", err)
				os.Exit(1)
			}
			caPool := x509.NewCertPool()
			caPool.AppendCertsFromPEM(caCert)
			tlsConfig.ClientCAs = caPool
			tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		}
		srv.TLSConfig = tlsConfig
	}

	go func() {
		var err error
		if certFile != "" && keyFile != "" {
			slog.Info("listening with TLS", "port", port)
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
