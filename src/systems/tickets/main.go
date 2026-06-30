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
		Timeout:   10 * time.Second,
	}
}

func newGatekeeperClient() *gk.Client {
	return &gk.Client{URL: gatekeeperURL, Service: "tickets", HTTPClient: httpClient}
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
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		_ = logLevel.UnmarshalText([]byte(v))
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
		defer shutdown(context.Background())
	}
	initMetrics()
	httpClient = initHTTPClient()

	if err := connect().AutoMigrate(&Ticket{}, &TicketComment{}, &TicketFieldDef{}, &Board{}); err != nil {
		slog.Error("failed to migrate tables", "error", err)
		os.Exit(1)
	}
	// A field def's (kind, value) is its identifier within an org — e.g. an org
	// must not have two status defs with value "open". Enforced as a partial
	// unique index so a value can be reused after its def is deleted.
	if err := connect().Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_ticket_field_defs_org_kind_value ON ticket_field_defs (org_id, kind, value) WHERE active`).Error; err != nil {
		slog.Warn("failed to create ticket_field_defs unique index (existing duplicate values?)", "error", err)
	}
	// Board names are unique within their owner scope: per-org for org-backed
	// boards, per-user for personal ones. Partial (WHERE active) so a name frees
	// up on delete and the two scopes never collide.
	if err := connect().Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_ticket_boards_org_name ON ticket_boards (org_id, name) WHERE active AND org_id <> ''`).Error; err != nil {
		slog.Warn("failed to create ticket_boards org unique index (existing duplicate names?)", "error", err)
	}
	if err := connect().Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_ticket_boards_user_name ON ticket_boards (created_by, name) WHERE active AND org_id = ''`).Error; err != nil {
		slog.Warn("failed to create ticket_boards user unique index (existing duplicate names?)", "error", err)
	}
	if err := seedDefaultFieldDefs(ctx); err != nil {
		slog.Error("failed to seed field defs", "error", err)
		os.Exit(1)
	}
	slog.Info("database initialized")

	gatekeeperClient = newGatekeeperClient()
	registry.StartKeyRotation(ctx, gatekeeperURL, "tickets",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	if hooksEnabled() {
		slog.Info("hooks integration enabled — ticket lifecycle events will be emitted", "hooks_url", hooksURL)
	} else {
		slog.Info("hooks integration disabled — set HOOKS_URL and HOOKS_TRIGGER_KEY to enable")
	}

	mux := telemetry.NewMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /openapi.yaml", handleOpenAPIYAML)

	mux.HandleFunc("POST /tickets", handleCreateTicket)
	mux.HandleFunc("GET /tickets", handleListTickets)
	mux.HandleFunc("GET /tickets/{id}", handleGetTicket)
	mux.HandleFunc("PUT /tickets/{id}", handleUpdateTicket)
	mux.HandleFunc("DELETE /tickets/{id}", handleDeleteTicket)

	mux.HandleFunc("POST /tickets/{id}/comments", handleAddComment)
	mux.HandleFunc("DELETE /tickets/{id}/comments/{comment_id}", handleDeleteComment)

	mux.HandleFunc("GET /field-defs", handleListFieldDefs)
	mux.HandleFunc("POST /field-defs", handleCreateFieldDef)
	mux.HandleFunc("PUT /field-defs/{id}", handleUpdateFieldDef)
	mux.HandleFunc("DELETE /field-defs/{id}", handleDeleteFieldDef)

	mux.HandleFunc("POST /boards", handleCreateBoard)
	mux.HandleFunc("GET /boards", handleListBoards)
	mux.HandleFunc("GET /boards/{id}", handleGetBoard)
	mux.HandleFunc("PUT /boards/{id}", handleUpdateBoard)
	mux.HandleFunc("DELETE /boards/{id}", handleDeleteBoard)

	port := envOrDefault("PORT", "8086")
	wrapped := otelhttp.NewHandler(limitBody(&requestLogger{mux}), "tickets",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      wrapped,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
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
