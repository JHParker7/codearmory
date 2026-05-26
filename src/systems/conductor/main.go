package main

import (
	"bytes"
	"context"
	"encoding/base64"
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

// ── User auth ─────────────────────────────────────────────────────────────────

// uuidRE matches the UUID format Gatekeeper uses for user IDs.
var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// getUserID decodes the JWT payload (without signature verification) and returns
// the sub claim, which Gatekeeper sets to the user's ID.
func getUserID(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Sub == "" {
		return "", false
	}
	if !uuidRE.MatchString(claims.Sub) {
		return "", false
	}
	return claims.Sub, true
}

// checkUserExists calls GET /users/{id} on Gatekeeper with the caller's bearer
// token. Returns true only when Gatekeeper responds 200.
func checkUserExists(ctx context.Context, token, userID string) bool {
	ctx, span := otel.Tracer("conductor").Start(ctx, "checkUserExists")
	defer span.End()
	span.SetAttributes(attribute.String("user.id", userID))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatekeeperURL+"/users/"+url.PathEscape(userID), nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	exists := resp.StatusCode == http.StatusOK
	span.SetAttributes(attribute.Bool("user.exists", exists))
	if exists {
		span.SetStatus(codes.Ok, "")
	} else {
		span.SetStatus(codes.Error, "user not found or token invalid")
	}
	return exists
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

// checkServicePermission calls GET /check_permissions on Gatekeeper to verify
// the caller has the required action on the target service and resource.
func checkServicePermission(r *http.Request, service, action, resource string) bool {
	ctx, span := otel.Tracer("conductor").Start(r.Context(), "checkServicePermission")
	defer span.End()

	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	body, _ := json.Marshal(map[string]string{
		"service":  service,
		"action":   action,
		"resource": resource,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatekeeperURL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		span.SetStatus(codes.Error, "permission denied")
		return false
	}
	var result struct {
		Authorized bool `json:"authorized"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false
	}
	if result.Authorized {
		span.SetStatus(codes.Ok, "")
	} else {
		span.SetStatus(codes.Error, "not authorized")
	}
	return result.Authorized
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
		ctx, span := otel.Tracer("conductor").Start(r.Context(), "authCheck")

		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			span.SetStatus(codes.Error, "no bearer token")
			meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "no_token")))
			span.End()
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")

		userID, ok := getUserID(token)
		if !ok {
			span.SetStatus(codes.Error, "malformed token")
			meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "malformed_token")))
			span.End()
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if !checkUserExists(ctx, token, userID) {
			span.SetStatus(codes.Error, "user not found")
			meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "user_not_found")))
			span.End()
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		span.SetStatus(codes.Ok, "")
		meterAllowed.Add(ctx, 1)
		span.End()
		r = r.WithContext(ctx)

		if !checkServicePermission(r, entry.serviceName, entry.action, entry.resource) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	r2 := r.Clone(r.Context())
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
