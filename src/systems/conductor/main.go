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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var (
	gatekeeperURL       = envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
	registryURL         = envOrDefault("REGISTRY_URL", "http://localhost:8084")
	conductorForwardKey = secret("CONDUCTOR_FORWARD_KEY") // shared secret for signing X-User-ID on all non-forwardAuth services
	conductorNotifyKey  = secret("CONDUCTOR_NOTIFY_KEY")  // shared secret allowing registry to push refresh notifications
	httpClient          *http.Client // set in main() after telemetry.Setup so the transport uses the real OTel provider
	// gatekeeperClient and registryClient carry static peer.service attributes so
	// Tempo's service-graph processor can label edges correctly even when SERVER
	// spans arrive after the store expiry window.
	gatekeeperClient *http.Client
	registryClient   *http.Client
	// getRegistryKey returns the current rotating service key used to authenticate
	// conductor's requests to the registry. Set in main() via StartKeyRotation.
	getRegistryKey func() string
)

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

// handleServicesHealth checks /healthz on every registered backend service
// concurrently and returns a JSON summary of their status.
func handleServicesHealth(w http.ResponseWriter, r *http.Request) {
	routingMu.RLock()
	snapshot := make(map[string]serviceState, len(servicesMap))
	for k, v := range servicesMap {
		snapshot[k] = v
	}
	routingMu.RUnlock()

	type serviceHealth struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Latency string `json:"latency_ms,omitempty"`
		Error   string `json:"error,omitempty"`
	}

	results := make([]serviceHealth, 0, len(snapshot))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for name, svc := range snapshot {
		wg.Add(1)
		go func(name string, svc serviceState) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()

			target := strings.TrimRight(svc.url, "/") + "/healthz"
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				mu.Lock()
				results = append(results, serviceHealth{Name: name, Status: "unhealthy", Error: err.Error()})
				mu.Unlock()
				return
			}

			start := time.Now()
			resp, err := httpClient.Do(req)
			elapsed := time.Since(start)
			if err != nil {
				mu.Lock()
				results = append(results, serviceHealth{Name: name, Status: "unhealthy", Error: err.Error()})
				mu.Unlock()
				return
			}
			resp.Body.Close()

			status := "healthy"
			if resp.StatusCode >= 500 {
				status = "unhealthy"
			} else if resp.StatusCode >= 400 {
				status = "degraded"
			}

			mu.Lock()
			results = append(results, serviceHealth{
				Name:    name,
				Status:  status,
				Latency: strconv.FormatInt(elapsed.Milliseconds(), 10),
			})
			mu.Unlock()
		}(name, svc)
	}
	wg.Wait()

	overall := "healthy"
	for _, r := range results {
		if r.Status == "unhealthy" {
			overall = "unhealthy"
			break
		} else if r.Status == "degraded" && overall == "healthy" {
			overall = "degraded"
		}
	}

	type response struct {
		Status   string          `json:"status"`
		Services []serviceHealth `json:"services"`
	}
	w.Header().Set("Content-Type", "application/json")
	if overall == "unhealthy" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(response{Status: overall, Services: results})
}

func main() {
	initialRegistryKey := secret("REGISTRY_SERVICE_KEY")
	if initialRegistryKey == "" {
		slog.Error("REGISTRY_SERVICE_KEY is not set; refusing to start")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "conductor")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(context.Background())
	}
	initMetrics()

	httpClient = &http.Client{
		Timeout:   10 * time.Second,
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}
	// Per-service clients stamp peer.service on CLIENT spans so Tempo's service-graph
	// processor can label edges even when SERVER spans arrive after the store expiry.
	gatekeeperClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: otelhttp.NewTransport(http.DefaultTransport,
			otelhttp.WithSpanOptions(trace.WithAttributes(attribute.String("peer.service", "gatekeeper"))),
		),
	}
	registryClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: otelhttp.NewTransport(http.DefaultTransport,
			otelhttp.WithSpanOptions(trace.WithAttributes(attribute.String("peer.service", "registry"))),
		),
	}

	// Rotate conductor's registry service key every 25 minutes so the credential
	// is always short-lived. The key is used in X-Service-Key on every GET /services call.
	getRegistryKey = registry.StartKeyRotation(ctx, registryURL, "conductor", initialRegistryKey, 25*time.Minute)

	// Warm the service cache, retrying until the registry returns services with endpoints.
	for {
		refreshServiceCache(ctx)
		routingMu.RLock()
		populated := len(endpointsList) > 0
		routingMu.RUnlock()
		if populated {
			break
		}
		slog.Warn("registry not reachable or empty, retrying in 5s")
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}

	// Refresh the service registry every 5 minutes as a fallback; registry also
	// pushes an immediate refresh via POST /internal/refresh after manifest loads.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshServiceCache(ctx)
			}
		}
	}()

	mux := telemetry.NewMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /health", handleServicesHealth)
	mux.HandleFunc("POST /internal/refresh", handleInternalRefresh)
	mux.HandleFunc("GET /openapi.json", handleOpenAPISpec)
	mux.HandleFunc("GET /docs", handleDocs)
	mux.HandleFunc("GET /docs/{service}", handleServiceDocs)
	mux.HandleFunc("GET /docs/{service}/openapi.yaml", handleServiceSpec)
	mux.Handle("/{path...}", http.HandlerFunc(handleServiceProxy))

	port := envOrDefault("PORT", "8080")

	wrappedMux := otelhttp.NewHandler(newLogger(mux), "conductor",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      wrappedMux,
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

	if httpClient == nil || gatekeeperClient == nil || registryClient == nil {
		slog.Error("BUG: HTTP clients not initialized before server start")
		os.Exit(1)
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
