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

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	gatekeeperURL  = envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
	registryURL    = envOrDefault("REGISTRY_URL", "http://localhost:8084")
	registryKey    = os.Getenv("REGISTRY_READ_KEY")
	httpClient     = &http.Client{Timeout: 10 * time.Second}
)

// ── Service registry cache ────────────────────────────────────────────────────

// endpointEntry is a compiled representation of one endpoint declared by a
// backend service. pattern is built from the registered path template by
// replacing {param} segments with [^/]+ so request paths can be matched.
type endpointEntry struct {
	method   string
	pattern  *regexp.Regexp
	action   string
	resource string
}

var (
	servicesMu   sync.RWMutex
	servicesMap  = map[string]string{}                  // name → URL
	proxiesMu    sync.RWMutex
	proxiesMap   = map[string]*httputil.ReverseProxy{} // name → proxy
	endpointsMu  sync.RWMutex
	endpointsMap = map[string][]endpointEntry{}        // name → endpoints
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ── Input validation ──────────────────────────────────────────────────────────

const bodyMax = 64 * 1024 // 64 KB cap on public-route request bodies

var (
	// emailRE is a loose format check; full RFC 5322 parsing is left to Gatekeeper.
	emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	// slugRE allows alphanumeric characters, hyphens, and underscores (1–64 chars).
	// Used for both username body fields and path segments (username, org, team, workspace).
	slugRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
)

// readAndRestore reads the entire request body (up to bodyMax bytes), writes an
// error response and returns (nil, false) if the limit is exceeded or a read
// error occurs, and otherwise resets r.Body so the proxy can forward it unchanged.
func readAndRestore(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, bodyMax)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, true
}

func validateSignupBody(w http.ResponseWriter, r *http.Request) bool {
	data, ok := readAndRestore(w, r)
	if !ok {
		return false
	}
	var req struct {
		Email    string `json:"email"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return false
	}
	if req.Email == "" || req.Username == "" || req.Password == "" {
		http.Error(w, "email, username, and password are required", http.StatusBadRequest)
		return false
	}
	if !emailRE.MatchString(req.Email) {
		http.Error(w, "invalid email format", http.StatusBadRequest)
		return false
	}
	if !slugRE.MatchString(req.Username) {
		http.Error(w, "username must be 1-64 alphanumeric, hyphen, or underscore characters", http.StatusBadRequest)
		return false
	}
	if len(req.Password) < 8 {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return false
	}
	return true
}

func validateLoginBody(w http.ResponseWriter, r *http.Request) bool {
	data, ok := readAndRestore(w, r)
	if !ok {
		return false
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return false
	}
	if req.Email == "" || req.Password == "" {
		http.Error(w, "email and password are required", http.StatusBadRequest)
		return false
	}
	return true
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
	return httputil.NewSingleHostReverseProxy(u)
}

// proxyWith forwards r to p after running all checks in order. Each check is
// responsible for writing its own error response; if any returns false,
// forwarding is aborted.
func proxyWith(p *httputil.ReverseProxy, checks ...func(http.ResponseWriter, *http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, check := range checks {
			if !check(w, r) {
				return
			}
		}
		p.ServeHTTP(w, r)
	})
}

// validUUID returns a check that path value `name` is a well-formed UUID.
func validUUID(name string) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if uuidRE.MatchString(r.PathValue(name)) {
			return true
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
}

// validJSON reads and restores the body for POST/PUT/PATCH requests, rejecting
// it with 400 if it is not well-formed JSON. Empty bodies are allowed through.
func validJSON(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return true
	}
	data, ok := readAndRestore(w, r)
	if !ok {
		return false
	}
	if len(data) > 0 && !json.Valid(data) {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	return true
}

// ── User existence middleware ─────────────────────────────────────────────────

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
// token. Returns true only when Gatekeeper responds 200, which means the token
// is valid and the user record is active.
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
	// Drain before close so the underlying TCP connection returns to the pool.
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

// userMiddleware validates the caller's JWT by forwarding it to Gatekeeper's
// GET /users/{id} endpoint. Conductor decodes the payload locally only to extract
// the user ID for the forwarded request; full signature verification happens inside
// Gatekeeper's authMiddleware, which has access to the per-session ECDSA public key.
func userMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("conductor").Start(r.Context(), "userMiddleware")
		defer span.End()
		r = r.WithContext(ctx)

		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			span.SetStatus(codes.Error, "no bearer token")
			meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "no_token")))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")

		userID, ok := getUserID(token)
		if !ok {
			span.SetStatus(codes.Error, "malformed token")
			meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "malformed_token")))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if !checkUserExists(ctx, token, userID) {
			span.SetStatus(codes.Error, "user not found")
			meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "user_not_found")))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		span.SetStatus(codes.Ok, "")
		meterAllowed.Add(ctx, 1)
		next.ServeHTTP(w, r)
	})
}

// ── Service registry ──────────────────────────────────────────────────────────

// refreshServiceCache fetches GET /services from the registry and rebuilds the
// in-memory proxy map and endpoint table.
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
		Name      string `json:"name"`
		URL       string `json:"url"`
		Endpoints []struct {
			Method   string `json:"method"`
			Path     string `json:"path"`
			Action   string `json:"action"`
			Resource string `json:"resource"`
		} `json:"endpoints"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&svcs); err != nil {
		return
	}

	active := make(map[string]string, len(svcs))
	for _, s := range svcs {
		active[s.Name] = s.URL
	}

	servicesMu.Lock()
	proxiesMu.Lock()
	endpointsMu.Lock()
	for _, s := range svcs {
		if servicesMap[s.Name] != s.URL {
			u, err := url.Parse(s.URL)
			if err != nil {
				slog.Warn("invalid service URL", "name", s.Name, "url", s.URL)
				continue
			}
			proxiesMap[s.Name] = httputil.NewSingleHostReverseProxy(u)
			servicesMap[s.Name] = s.URL
			slog.Info("service cache updated", "name", s.Name)
		}
		entries := make([]endpointEntry, 0, len(s.Endpoints))
		for _, ep := range s.Endpoints {
			entries = append(entries, endpointEntry{
				method:   ep.Method,
				pattern:  compilePathPattern(ep.Path),
				action:   ep.Action,
				resource: ep.Resource,
			})
		}
		endpointsMap[s.Name] = entries
	}
	for name := range servicesMap {
		if _, ok := active[name]; !ok {
			delete(servicesMap, name)
			delete(proxiesMap, name)
			delete(endpointsMap, name)
			slog.Info("service removed from cache", "name", name)
		}
	}
	endpointsMu.Unlock()
	proxiesMu.Unlock()
	servicesMu.Unlock()
}

// methodToAction maps an HTTP method to a coarse RBAC action used as a fallback
// when no registered endpoint matches the request path.
func methodToAction(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead:
		return "read"
	case http.MethodDelete:
		return "delete"
	default:
		return "write"
	}
}

// compilePathPattern converts a service path template such as /executions/{id}
// into a regexp that matches concrete paths (e.g. /executions/abc-123).
// Each {param} segment is replaced with [^/]+ so only a single path segment
// is consumed per wildcard.
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

// resolveEndpoint finds the registered action and resource for method+path.
// Falls back to methodToAction and the first path segment when no match exists.
func resolveEndpoint(entries []endpointEntry, method, path string) (action, resource string) {
	for _, e := range entries {
		if e.method == method && e.pattern.MatchString(path) {
			return e.action, e.resource
		}
	}
	// Fallback: coarse action + first non-empty path segment as resource.
	action = methodToAction(method)
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	if len(parts) > 0 && parts[0] != "" {
		resource = parts[0]
	} else {
		resource = "*"
	}
	return action, resource
}

// checkServicePermission calls GET /check_permissions on Gatekeeper to verify
// the caller has the required action on the target service and resource path.
// It uses registered endpoint metadata when available, falling back to coarse
// method-to-action mapping for paths that weren't declared on registration.
func checkServicePermission(r *http.Request, service, path string) bool {
	ctx, span := otel.Tracer("conductor").Start(r.Context(), "checkServicePermission")
	defer span.End()

	endpointsMu.RLock()
	entries := endpointsMap[service]
	endpointsMu.RUnlock()

	action, resource := resolveEndpoint(entries, r.Method, path)

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

// handleServiceProxy is the dynamic catch-all: it extracts /{service}/{path...},
// looks the service up in the cache, permission-checks the caller, then forwards.
func handleServiceProxy(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	idx := strings.IndexByte(trimmed, '/')
	var serviceName, restPath string
	if idx == -1 {
		serviceName = trimmed
		restPath = "/"
	} else {
		serviceName = trimmed[:idx]
		restPath = trimmed[idx:]
	}
	if serviceName == "" {
		http.NotFound(w, r)
		return
	}

	proxiesMu.RLock()
	proxy, ok := proxiesMap[serviceName]
	proxiesMu.RUnlock()
	if !ok {
		http.Error(w, serviceName+" service is not installed", http.StatusServiceUnavailable)
		return
	}

	if !checkServicePermission(r, serviceName, restPath) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Strip the /{service} prefix before forwarding to the backend.
	r2 := r.Clone(r.Context())
	r2.URL.Path = restPath
	proxy.ServeHTTP(w, r2)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	otelHandler, shutdown, err := setupOTel(context.Background())
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(&fanoutHandler{handlers: []slog.Handler{jsonHandler, otelHandler}}))
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

	gk := newProxy(gatekeeperURL)

	auth := func(h http.Handler) http.Handler { return userMiddleware(h) }
	id := validUUID("id")

	mux := http.NewServeMux()

	// ── Public routes — validated then forwarded to Gatekeeper ───────────────
	mux.Handle("POST /signup", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validateSignupBody(w, r) {
			return
		}
		gk.ServeHTTP(w, r)
	}))
	mux.Handle("POST /login", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validateLoginBody(w, r) {
			return
		}
		gk.ServeHTTP(w, r)
	}))

	// ── Gatekeeper — authenticated routes ────────────────────────────────────
	mux.Handle("GET /check_permissions", auth(proxyWith(gk)))

	mux.Handle("GET /users", auth(proxyWith(gk)))
	mux.Handle("GET /users/{id}", auth(proxyWith(gk, id)))
	mux.Handle("PUT /users/{id}", auth(proxyWith(gk, id, validJSON)))
	mux.Handle("DELETE /users/{id}", auth(proxyWith(gk, id)))

	mux.Handle("GET /orgs", auth(proxyWith(gk)))
	mux.Handle("POST /orgs", auth(proxyWith(gk, validJSON)))
	mux.Handle("GET /orgs/{id}", auth(proxyWith(gk, id)))
	mux.Handle("PUT /orgs/{id}", auth(proxyWith(gk, id, validJSON)))
	mux.Handle("DELETE /orgs/{id}", auth(proxyWith(gk, id)))
	mux.Handle("POST /orgs/{id}/invites", auth(proxyWith(gk, id, validJSON)))

	mux.Handle("GET /teams", auth(proxyWith(gk)))
	mux.Handle("POST /teams", auth(proxyWith(gk, validJSON)))
	mux.Handle("GET /teams/{id}", auth(proxyWith(gk, id)))
	mux.Handle("PUT /teams/{id}", auth(proxyWith(gk, id, validJSON)))
	mux.Handle("DELETE /teams/{id}", auth(proxyWith(gk, id)))
	mux.Handle("POST /teams/{id}/invites", auth(proxyWith(gk, id, validJSON)))

	mux.Handle("POST /roles", auth(proxyWith(gk, validJSON)))
	mux.Handle("GET /roles/{id}", auth(proxyWith(gk, id)))
	mux.Handle("PUT /roles/{id}", auth(proxyWith(gk, id, validJSON)))
	mux.Handle("DELETE /roles/{id}", auth(proxyWith(gk, id)))

	mux.Handle("POST /permissions", auth(proxyWith(gk, validJSON)))
	mux.Handle("GET /permissions/{id}", auth(proxyWith(gk, id)))
	mux.Handle("PUT /permissions/{id}", auth(proxyWith(gk, id, validJSON)))
	mux.Handle("DELETE /permissions/{id}", auth(proxyWith(gk, id)))

	mux.Handle("GET /sessions/{id}", auth(proxyWith(gk, id)))
	mux.Handle("DELETE /sessions/{id}", auth(proxyWith(gk, id)))

	mux.Handle("GET /invites", auth(proxyWith(gk)))
	mux.Handle("GET /invites/{id}", auth(proxyWith(gk, id)))
	mux.Handle("POST /invites/{id}/accept", auth(proxyWith(gk, id, validJSON)))
	mux.Handle("POST /invites/{id}/decline", auth(proxyWith(gk, id, validJSON)))
	mux.Handle("DELETE /invites/{id}", auth(proxyWith(gk, id)))

	// ── Dynamic service proxy — catch-all for registered backend services ─────
	mux.Handle("/{path...}", auth(http.HandlerFunc(handleServiceProxy)))

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
