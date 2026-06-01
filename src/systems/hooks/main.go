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
	"strconv"
	"strings"
	"syscall"
	"time"

	gk "github.com/code-armory-app/codearmory_sdk/gatekeeper"
	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	gatekeeperClient *gk.Client
	gatekeeperURL    = envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
	workflowsURL     = envOrDefault("WORKFLOWS_URL", "http://localhost:8085")
	hooksTriggerKey  = os.Getenv("HOOKS_TRIGGER_KEY")
	httpClient       *http.Client
)

// initHTTPClient builds an instrumented HTTP client with optional TLS client cert.
// Set TLS_CLIENT_CERT_FILE + TLS_CLIENT_KEY_FILE to present a cert on outbound calls.
// Set TLS_CA_FILE to trust a custom CA for server certificate verification.
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
	return &gk.Client{URL: gatekeeperURL, Service: "hooks", HTTPClient: httpClient}
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

// startRetryLoop runs a background goroutine that periodically re-dispatches
// failed hook triggers, providing at-least-once delivery when the workflows
// service is temporarily unavailable.
func startRetryLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				processRetries(ctx)
			}
		}
	}()
}

func processRetries(ctx context.Context) {
	retries, err := claimDueRetries(ctx, 10)
	if err != nil {
		slog.Error("retry loop: query retries", "error", err)
		return
	}
	for _, r := range retries {
		runID, dispatchErr := dispatchWorkflow(ctx, r.WorkflowID, r.TriggeredBy, r.OrgID, r.Inputs)
		if dispatchErr == nil {
			if updateErr := connect().WithContext(ctx).Model(&HookTrigger{}).
				Where("trigger_id = ?", r.TriggerID).
				Updates(map[string]any{"status": "triggered", "run_id": runID}).Error; updateErr != nil {
				slog.Error("retry loop: update trigger", "trigger_id", r.TriggerID, "error", updateErr)
			}
			connect().WithContext(ctx).Delete(&HookTriggerRetry{RetryID: r.RetryID}) //nolint:errcheck
			meterRunsTriggered.Add(ctx, 1, metric.WithAttributes(
				attribute.String("workflow.id", r.WorkflowID),
			))
			slog.Info("retry: dispatch succeeded", "trigger_id", r.TriggerID, "attempt", r.Attempt)
			continue
		}
		nextAttempt := r.Attempt + 1
		if nextAttempt > maxRetryAttempts || errors.Is(dispatchErr, errWorkflowNotFound) {
			if updateErr := connect().WithContext(ctx).Model(&HookTrigger{}).
				Where("trigger_id = ?", r.TriggerID).
				Updates(map[string]any{"status": "failed", "error": dispatchErr.Error()}).Error; updateErr != nil {
				slog.Error("retry loop: mark trigger failed", "trigger_id", r.TriggerID, "error", updateErr)
			}
			connect().WithContext(ctx).Delete(&HookTriggerRetry{RetryID: r.RetryID}) //nolint:errcheck
			slog.Warn("retry: permanently failed", "trigger_id", r.TriggerID, "attempts", r.Attempt, "error", dispatchErr)
		} else {
			backoff := retryBackoff(nextAttempt)
			if updateErr := connect().WithContext(ctx).Model(&HookTriggerRetry{}).
				Where("retry_id = ?", r.RetryID).
				Updates(map[string]any{
					"attempt":      nextAttempt,
					"last_error":   dispatchErr.Error(),
					"next_retry_at": time.Now().UTC().Add(backoff),
				}).Error; updateErr != nil {
				slog.Error("retry loop: update retry record", "retry_id", r.RetryID, "error", updateErr)
			}
			slog.Warn("retry: will retry", "trigger_id", r.TriggerID, "attempt", nextAttempt, "backoff", backoff, "error", dispatchErr)
		}
	}
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
		defer shutdown(context.Background())
	}
	initMetrics()
	httpClient = initHTTPClient()

	// Safely migrate existing TEXT[] events column to JSONB.
	func() {
		defer func() { recover() }() //nolint:errcheck
		connect().Exec("ALTER TABLE pipeline_rules ALTER COLUMN events TYPE jsonb USING to_jsonb(events) WHERE pg_typeof(events)::text = 'text[]'")
	}()

	if err := connect().AutoMigrate(&PipelineRule{}, &HookEvent{}, &HookTrigger{}, &HookTriggerRetry{}); err != nil {
		slog.Error("failed to migrate database", "error", err)
		os.Exit(1)
	}
	if err := connect().Exec(`CREATE INDEX IF NOT EXISTS idx_pipeline_rules_repo_active ON pipeline_rules (repo, active) WHERE active = true`).Error; err != nil {
		slog.Warn("failed to create pipeline_rules index", "error", err)
	}
	if err := connect().Exec(`CREATE INDEX IF NOT EXISTS idx_hook_trigger_retries_next ON hook_trigger_retries (next_retry_at) WHERE next_retry_at IS NOT NULL`).Error; err != nil {
		slog.Warn("failed to create hook_trigger_retries index", "error", err)
	}
	slog.Info("database initialized")

	if hooksTriggerKey == "" {
		slog.Warn("HOOKS_TRIGGER_KEY not set — internal trigger endpoint will reject all hook-to-workflow dispatch requests")
	}

	gatekeeperClient = newGatekeeperClient()
	registry.StartKeyRotation(ctx, gatekeeperURL, "hooks",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)
	startRetryLoop(ctx)

	var ghApp *githubApp
	if rawID := secret("GITHUB_APP_ID"); rawID != "" {
		appID, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil {
			slog.Error("GITHUB_APP_ID is not a valid integer", "error", err)
			os.Exit(1)
		}
		webhookSecret := secret("GITHUB_APP_WEBHOOK_SECRET")
		if webhookSecret == "" {
			slog.Error("GITHUB_APP_WEBHOOK_SECRET must be set when GITHUB_APP_ID is configured")
			os.Exit(1)
		}
		app, err := newGithubApp(appID, secret("GITHUB_APP_PRIVATE_KEY"), webhookSecret)
		if err != nil {
			slog.Error("failed to initialise GitHub App", "error", err)
			os.Exit(1)
		}
		ghApp = app
		slog.Info("GitHub App configured", "app_id", appID)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	mux.HandleFunc("POST /rules", handleCreateRule)
	mux.HandleFunc("GET /rules", handleListRules)
	mux.HandleFunc("GET /rules/{id}", handleGetRule)
	mux.HandleFunc("PUT /rules/{id}", handleUpdateRule)
	mux.HandleFunc("DELETE /rules/{id}", handleDeleteRule)

	mux.HandleFunc("POST /hooks", handleWebhook)
	if ghApp != nil {
		mux.HandleFunc("POST /hooks/github", handleGitHubWebhook(ghApp))
	}

	mux.HandleFunc("GET /events", handleListEvents)
	mux.HandleFunc("GET /events/{id}", handleGetEvent)

	port := envOrDefault("PORT", "8087")
	wrapped := otelhttp.NewHandler(limitBody(&requestLogger{mux}), "hooks",
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
