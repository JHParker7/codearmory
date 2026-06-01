package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
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
	gatekeeperKey    func() string // current workflows service key, updated by key rotation
	hooksTriggerKey  = os.Getenv("HOOKS_TRIGGER_KEY")

	// serviceURLs maps registered service names to their base URLs.
	// Seeded at startup from SERVICES env var and updated every 5 min from the registry.
	serviceURLsMu sync.RWMutex
	serviceURLs   = map[string]string{}

	// actionCatalog maps action names to their definitions, polled from the registry.
	actionCatalogMu sync.RWMutex
	actionCatalog   = map[string]ActionDef{}

	httpClient *http.Client
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

// initServices seeds serviceURLs from the SERVICES env var and always pre-populates
// gatekeeper. The registry catalog poller adds/updates entries as services register.
func initServices() {
	serviceURLsMu.Lock()
	defer serviceURLsMu.Unlock()
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

// startCatalogPoller fetches the action catalog from the registry immediately
// and then refreshes it every 5 minutes so newly registered services are picked
// up without restarting workflows.
func startCatalogPoller(ctx context.Context) {
	refreshCatalog(ctx)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshCatalog(ctx)
			}
		}
	}()
}

func refreshCatalog(ctx context.Context) {
	registryURL := envOrDefault("REGISTRY_URL", "")
	registryKey := secret("REGISTRY_SERVICE_KEY")
	if registryURL == "" || registryKey == "" {
		slog.Debug("catalog refresh skipped: REGISTRY_URL or REGISTRY_SERVICE_KEY not set")
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL+"/actions", nil)
	if err != nil {
		slog.Warn("catalog refresh: build request", "error", err)
		return
	}
	req.Header.Set("X-Service-Key", "workflows:"+registryKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Warn("catalog refresh: request failed", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("catalog refresh: unexpected status", "status", resp.StatusCode)
		return
	}

	// The registry embeds the gatekeeper permission triple as flat fields
	// (gk_service, gk_action, gk_resource). Map them into RequiredPermission.
	type registryAction struct {
		ActionDef
		GkService  string `json:"gk_service"`
		GkAction   string `json:"gk_action"`
		GkResource string `json:"gk_resource"`
	}
	var raw []registryAction
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		slog.Warn("catalog refresh: decode failed", "error", err)
		return
	}

	newCatalog := make(map[string]ActionDef, len(raw))
	for _, ra := range raw {
		def := ra.ActionDef
		if ra.GkService != "" && ra.GkAction != "" && ra.GkResource != "" {
			def.RequiredPermission = &PermissionSpec{
				Service:  ra.GkService,
				Action:   ra.GkAction,
				Resource: ra.GkResource,
			}
		}
		newCatalog[def.Name] = def
	}

	actionCatalogMu.Lock()
	actionCatalog = newCatalog
	actionCatalogMu.Unlock()

	// Also update serviceURLs with any new service URLs from the catalog.
	serviceURLsMu.Lock()
	for _, ra := range raw {
		if ra.ServiceName != "" && ra.ServiceURL != "" {
			serviceURLs[ra.ServiceName] = ra.ServiceURL
		}
	}
	serviceURLsMu.Unlock()

	slog.Info("catalog refreshed", "actions", len(raw))
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

// recoverStuckRuns marks any runs left in 'running' state (from a previous pod
// crash) as 'failed' so they do not block the worker queue indefinitely.
// Sessions are nulled out; their JWTs expire naturally within the 1-hour TTL.
func recoverStuckRuns() {
	result := connect().Exec(
		"UPDATE workflow_runs SET status='failed', ended_at=now(), token=NULL, run_session_id=NULL WHERE status='running'",
	)
	if result.Error != nil {
		slog.Error("startup: failed to recover stuck runs", "error", result.Error)
		return
	}
	if result.RowsAffected > 0 {
		slog.Warn("startup: recovered stuck runs from previous pod", "count", result.RowsAffected)
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

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "workflows")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(context.Background())
	}
	initMetrics()
	httpClient = initHTTPClient()

	if err := connect().AutoMigrate(&Step{}, &Workflow{}, &WorkflowRun{}, &WorkflowStepRun{}); err != nil {
		slog.Error("failed to run AutoMigrate", "error", err)
		os.Exit(1)
	}
	if err := connect().Exec(`CREATE INDEX IF NOT EXISTS idx_workflow_runs_queue ON workflow_runs (status, created_at) WHERE status IN ('pending', 'running')`).Error; err != nil {
		slog.Warn("failed to create workflow_runs index", "error", err)
	}
	slog.Info("database initialized")

	recoverStuckRuns()

	initServices()
	gatekeeperClient = newGatekeeperClient()
	gatekeeperKey = registry.StartKeyRotation(ctx, gatekeeperURL, "workflows",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	startCatalogPoller(ctx)

	actionCatalogMu.RLock()
	catalogSize := len(actionCatalog)
	actionCatalogMu.RUnlock()
	if catalogSize == 0 {
		slog.Warn("action catalog empty after startup — registry may be unavailable; catalog actions will fail until it responds")
	}

	workers := newWorkerPool()
	workers.Start(ctx, 5)
	slog.Info("worker pool started", "workers", 5)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /actions", handleListActions)

	mux.HandleFunc("POST /steps", handleCreateStep)
	mux.HandleFunc("GET /steps", handleListSteps)
	mux.HandleFunc("GET /steps/{id}", handleGetStep)
	mux.HandleFunc("PUT /steps/{id}", handleUpdateStep)
	mux.HandleFunc("DELETE /steps/{id}", handleDeleteStep)

	mux.HandleFunc("POST /workflows", handleCreateWorkflow)
	mux.HandleFunc("GET /workflows", handleListWorkflows)
	mux.HandleFunc("GET /workflows/{id}", handleGetWorkflow)
	mux.HandleFunc("PUT /workflows/{id}", handleUpdateWorkflow)
	mux.HandleFunc("DELETE /workflows/{id}", handleDeleteWorkflow)

	mux.HandleFunc("POST /workflows/{id}/runs", handleTriggerRun)
	mux.HandleFunc("POST /internal/workflows/{id}/runs", handleInternalTriggerRun)
	mux.HandleFunc("GET /internal/workflows/{id}", handleInternalGetWorkflow)
	mux.HandleFunc("GET /internal/runs/{id}", handleInternalGetRun)
	mux.HandleFunc("GET /runs", handleListRuns)
	mux.HandleFunc("GET /runs/{id}", handleGetRun)
	mux.HandleFunc("DELETE /runs/{id}", handleCancelRun(workers))

	port := envOrDefault("PORT", "8085")
	wrapped := otelhttp.NewHandler(limitBody(&requestLogger{mux}), "workflows",
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
