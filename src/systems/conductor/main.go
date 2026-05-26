package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"codearmory.local/svckit/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	gatekeeperURL = envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
	registryURL   = envOrDefault("REGISTRY_URL", "http://localhost:8084")
	registryKey   = os.Getenv("REGISTRY_READ_KEY")
	httpClient    = &http.Client{Timeout: 10 * time.Second}
)

// ── Service registry cache ────────────────────────────────────────────────────

// endpointEntry is a compiled representation of one endpoint declared by a
// backend service. pattern matches the full request path as registered.
type endpointEntry struct {
	method      string
	pattern     *regexp.Regexp
	action      string
	resource    string
	public      bool   // skip user auth and permission check
	serviceName string // which service owns this endpoint
}

// serviceState holds the proxy and per-service routing config for one service.
type serviceState struct {
	url         string
	proxy       *httputil.ReverseProxy
	forwardAuth bool // whether to forward the caller's Authorization header
}

var (
	servicesMu    sync.RWMutex
	servicesMap   = map[string]serviceState{}
	endpointsMu   sync.RWMutex
	endpointsList []endpointEntry
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
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

type Logger struct{ handler http.Handler }

func (l *Logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func newLogger(h http.Handler) *Logger { return &Logger{h} }

// ── Proxy helpers ─────────────────────────────────────────────────────────────

func newProxy(target string) *httputil.ReverseProxy {
	u, err := url.Parse(target)
	if err != nil {
		slog.Error("invalid proxy target", "url", target, "error", err)
		os.Exit(1)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	base := proxy.Director
	proxy.Director = func(req *http.Request) {
		base(req)
		// X-Service-Key is for direct service-to-service calls only.
		// Strip it so clients cannot relay a service identity through conductor.
		req.Header.Del("X-Service-Key")
	}
	return proxy
}

// ── Auth ──────────────────────────────────────────────────────────────────────

type authOutcome int

const (
	authAllowed authOutcome = iota
	authUnauthorized
	authForbidden
)

// checkAuth forwards the caller's Bearer token to Gatekeeper's /check_permissions
// endpoint and interprets the response. It performs a single round-trip to
// Gatekeeper, which is responsible for both token validation and RBAC checks.
func checkAuth(r *http.Request, service, action, resource string) authOutcome {
	ctx, span := otel.Tracer("conductor").Start(r.Context(), "checkAuth")
	defer span.End()

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		span.SetStatus(codes.Error, "no bearer token")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "no_token")))
		return authUnauthorized
	}

	body, _ := json.Marshal(map[string]string{
		"service":  service,
		"action":   action,
		"resource": resource,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatekeeperURL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return authUnauthorized
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return authUnauthorized
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		span.SetStatus(codes.Error, "unauthorized")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "unauthorized")))
		return authUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		span.SetStatus(codes.Error, "permission denied")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "forbidden")))
		return authForbidden
	}

	var result struct {
		Authorized bool `json:"authorized"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return authForbidden
	}
	if !result.Authorized {
		span.SetStatus(codes.Error, "not authorized")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "forbidden")))
		return authForbidden
	}
	span.SetStatus(codes.Ok, "")
	meterAllowed.Add(ctx, 1)
	return authAllowed
}

// ── Service registry ──────────────────────────────────────────────────────────

// refreshServiceCache fetches GET /services from the registry and rebuilds the
// in-memory proxy map and endpoint list.
func refreshServiceCache(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL+"/services", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+registryKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Warn("service registry refresh failed", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		slog.Warn("service registry refresh: unexpected status", "status", resp.StatusCode)
		return
	}

	var svcs []struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		ForwardAuth bool   `json:"forward_auth"`
		Endpoints   []struct {
			Method   string `json:"method"`
			Path     string `json:"path"`
			Action   string `json:"action"`
			Resource string `json:"resource"`
			Public   bool   `json:"public"`
		} `json:"endpoints"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&svcs); err != nil {
		return
	}

	servicesMu.RLock()
	oldServices := make(map[string]serviceState, len(servicesMap))
	for k, v := range servicesMap {
		oldServices[k] = v
	}
	servicesMu.RUnlock()

	newServices := make(map[string]serviceState, len(svcs))
	var newEndpoints []endpointEntry

	for _, s := range svcs {
		if _, err := url.Parse(s.URL); err != nil {
			slog.Warn("invalid service URL", "name", s.Name, "url", s.URL)
			continue
		}

		var proxy *httputil.ReverseProxy
		if old, ok := oldServices[s.Name]; ok && old.url == s.URL {
			proxy = old.proxy
		} else {
			proxy = newProxy(s.URL)
			slog.Info("service cache updated", "name", s.Name)
		}

		newServices[s.Name] = serviceState{
			url:         s.URL,
			proxy:       proxy,
			forwardAuth: s.ForwardAuth,
		}

		for _, ep := range s.Endpoints {
			newEndpoints = append(newEndpoints, endpointEntry{
				method:      ep.Method,
				pattern:     compilePathPattern(ep.Path),
				action:      ep.Action,
				resource:    ep.Resource,
				public:      ep.Public,
				serviceName: s.Name,
			})
		}
	}

	for name := range oldServices {
		if _, ok := newServices[name]; !ok {
			slog.Info("service removed from cache", "name", name)
		}
	}

	servicesMu.Lock()
	servicesMap = newServices
	servicesMu.Unlock()

	endpointsMu.Lock()
	endpointsList = newEndpoints
	endpointsMu.Unlock()
}

// compilePathPattern converts a path template such as /users/{id} into a
// regexp that matches concrete paths. Each {param} segment is replaced with
// [^/]+ so only a single path segment is consumed per wildcard.
func compilePathPattern(pattern string) *regexp.Regexp {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			parts[i] = `[^/]+`
		} else {
			parts[i] = regexp.QuoteMeta(p)
		}
	}
	return regexp.MustCompile(`^` + strings.Join(parts, `/`) + `$`)
}

// lookupEndpoint finds the registered endpoint for method+path across all
// services. Returns the entry and true on match, zero value and false otherwise.
func lookupEndpoint(method, path string) (endpointEntry, bool) {
	endpointsMu.RLock()
	defer endpointsMu.RUnlock()
	for _, e := range endpointsList {
		if e.method == method && e.pattern.MatchString(path) {
			return e, true
		}
	}
	return endpointEntry{}, false
}

// handleServiceProxy is the universal handler. It rejects any request whose
// method+path is not registered in the service registry, enforces user auth on
// non-public endpoints, checks RBAC permissions, then forwards to the backend.
func handleServiceProxy(w http.ResponseWriter, r *http.Request) {
	entry, ok := lookupEndpoint(r.Method, r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	servicesMu.RLock()
	svc, ok := servicesMap[entry.serviceName]
	servicesMu.RUnlock()
	if !ok {
		http.Error(w, entry.serviceName+" service is not available", http.StatusServiceUnavailable)
		return
	}

	if !entry.public {
		switch checkAuth(r, entry.serviceName, entry.action, entry.resource) {
		case authUnauthorized:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		case authForbidden:
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	r2 := r.Clone(r.Context())
	// Strip headers that could be used to spoof identity or routing metadata.
	r2.Header.Del("X-User-ID")
	r2.Header.Del("X-Forwarded-Host")
	r2.Header.Del("X-Forwarded-Proto")
	r2.Header.Del("X-Real-IP")
	// Replace X-Forwarded-For with only the immediate client address so
	// downstream services see the real origin, not a client-supplied chain.
	r2.Header.Set("X-Forwarded-For", r.RemoteAddr)

	// Strip the bearer token before forwarding so backend services cannot replay
	// it against other services. Services that need to re-verify the caller
	// (e.g. gatekeeper itself) declare forward_auth=true in the registry.
	if !svc.forwardAuth {
		r2.Header.Del("Authorization")
	}
	svc.proxy.ServeHTTP(w, r2)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	if registryKey == "" {
		slog.Error("REGISTRY_READ_KEY is not set; refusing to start")
		os.Exit(1)
	}

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

	// Warm the service cache, retrying until the registry is reachable.
	for {
		refreshServiceCache(context.Background())
		servicesMu.RLock()
		populated := len(servicesMap) > 0
		servicesMu.RUnlock()
		if populated {
			break
		}
		slog.Warn("registry not reachable or empty, retrying in 5s")
		time.Sleep(5 * time.Second)
	}

	// Refresh the service registry every 30 seconds.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			refreshServiceCache(context.Background())
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/{path...}", http.HandlerFunc(handleServiceProxy))

	port := envOrDefault("PORT", "8082")

	wrappedMux := otelhttp.NewHandler(newLogger(mux), "conductor",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")
	if certFile != "" && keyFile != "" {
		slog.Info("listening with TLS", "port", port)
		if err := http.ListenAndServeTLS(":"+port, certFile, keyFile, wrappedMux); err != nil {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	} else {
		slog.Info("listening", "port", port)
		if err := http.ListenAndServe(":"+port, wrappedMux); err != nil {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}
}
