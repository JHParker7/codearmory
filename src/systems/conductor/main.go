package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"crypto/tls"
	"crypto/x509"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
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

const maxRequestBodyBytes = 64 * 1024

var (
	uuidHyphenRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	uuidHexRe    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	slugRe     = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	emailRe    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
)

// paramTypeFor returns the validation type for a path param by name.
// "id" → UUID, slug-like names → slug pattern.
func paramTypeFor(name string) string {
	switch name {
	case "id":
		return "uuid"
	case "username", "workspace", "org", "team":
		return "slug"
	}
	return ""
}

// signForwardedUserID generates a short-lived HMAC-SHA256 token binding userID
// to a 30-second timestamp window. Any backend service with forward_auth=false
// can verify this token to confirm X-User-ID was injected by conductor.
func signForwardedUserID(userID string) (token, timestamp string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(conductorForwardKey))
	fmt.Fprintf(mac, "conductor:%s:%s", userID, ts)
	return hex.EncodeToString(mac.Sum(nil)), ts
}

// ── Service registry cache ────────────────────────────────────────────────────

// endpointEntry is a compiled representation of one endpoint declared by a
// backend service. pattern matches the full request path as registered.
type endpointEntry struct {
	method       string
	pattern      *regexp.Regexp
	paramNames   []string // ordered param names captured by pattern (e.g. "id" for {id})
	action       string
	resource     string // may contain {param} placeholders resolved at request time
	public       bool   // skip user auth and permission check
	serviceName  string // which service owns this endpoint
	originalPath string // path template as declared in the registry (e.g. /users/{id})
}

// serviceState holds the proxy and per-service routing config for one service.
type serviceState struct {
	url         string
	proxy       *httputil.ReverseProxy
	forwardAuth bool   // whether to forward the caller's Authorization header
	description string // human-readable description from the registry manifest
}

// routingMu protects both servicesMap and endpointsList under a single lock so
// readers always see a consistent pair — updates swap both atomically.
var (
	routingMu     sync.RWMutex
	servicesMap   = map[string]serviceState{}
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

// parseParamNames extracts ordered param names from a path pattern like /users/{id}.
func parseParamNames(pattern string) []string {
	var names []string
	for _, seg := range strings.Split(pattern, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			names = append(names, seg[1:len(seg)-1])
		}
	}
	return names
}

// lookupEndpointFull finds the best matching endpoint for method+path across all
// services, and returns any captured path-param values.
func lookupEndpointFull(method, path string) (endpointEntry, []string, bool) {
	routingMu.RLock()
	defer routingMu.RUnlock()
	for _, e := range endpointsList {
		if e.method != method {
			continue
		}
		if m := e.pattern.FindStringSubmatch(path); m != nil {
			return e, m[1:], true
		}
	}
	return endpointEntry{}, nil, false
}

// lookupEndpointForService finds a matching endpoint restricted to a specific service.
func lookupEndpointForService(method, path, service string) (endpointEntry, []string, bool) {
	routingMu.RLock()
	defer routingMu.RUnlock()
	for _, e := range endpointsList {
		if e.serviceName != service || e.method != method {
			continue
		}
		if m := e.pattern.FindStringSubmatch(path); m != nil {
			return e, m[1:], true
		}
	}
	return endpointEntry{}, nil, false
}

func newProxy(target, peerService string) *httputil.ReverseProxy {
	u, err := url.Parse(target)
	if err != nil {
		slog.Error("invalid proxy target", "url", target, "error", err)
		os.Exit(1)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.Transport = otelhttp.NewTransport(http.DefaultTransport,
		otelhttp.WithSpanOptions(trace.WithAttributes(attribute.String("peer.service", peerService))),
	)
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

// getUserID decodes the JWT payload (without signature verification — that
// happens inside Gatekeeper when we call GET /users/{id}) and returns the sub
// claim, which Gatekeeper sets to the user's UUID.
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
	return canonicalUUID(claims.Sub)
}

// checkUserAuth verifies that the caller has a valid, active account.
// It decodes the JWT locally to extract the user ID, then calls Gatekeeper's
// GET /users/{id} endpoint — which performs full JWT signature verification —
// to confirm the user exists and the token is genuine.
// Permission checking is left to each backend service.
func checkUserAuth(r *http.Request) (authOutcome, string) {
	ctx, span := otel.Tracer("conductor").Start(r.Context(), "checkUserAuth")
	defer span.End()

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "no_token")))
		return authUnauthorized, ""
	}

	userID, ok := getUserID(strings.TrimPrefix(auth, "Bearer "))
	if !ok {
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "malformed_token")))
		return authUnauthorized, ""
	}
	span.SetAttributes(attribute.String("user.id", userID))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		gatekeeperURL+"/users/"+url.PathEscape(userID), nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return authUnauthorized, ""
	}
	req.Header.Set("Authorization", auth)

	resp, err := gatekeeperClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return authUnauthorized, ""
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "unauthorized")))
		return authUnauthorized, userID
	case resp.StatusCode >= 500:
		span.SetStatus(codes.Error, "gatekeeper error")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "gatekeeper_error")))
		return authForbidden, ""
	case resp.StatusCode != http.StatusOK:
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "user_not_found")))
		return authForbidden, ""
	}

	span.SetStatus(codes.Ok, "")
	meterAllowed.Add(ctx, 1)
	return authAllowed, userID
}

// ── Suspicious-activity block list ───────────────────────────────────────────

const (
	suspectThreshold = 10
	blockDuration    = time.Hour
)

var (
	suspectMu   sync.Mutex
	suspectHits = map[string]int{}
	blockedIPs  = map[string]time.Time{}
)

// sourceIP returns the immediate peer IP from r.RemoteAddr, stripping the port.
func sourceIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// isBlocked reports whether the IP is on the block list and has not yet expired.
func isBlocked(ip string) bool {
	suspectMu.Lock()
	defer suspectMu.Unlock()
	until, ok := blockedIPs[ip]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(blockedIPs, ip)
		delete(suspectHits, ip)
		return false
	}
	return true
}

// resetSuspect clears the failure counter for the IP on successful authentication.
func resetSuspect(ip string) {
	suspectMu.Lock()
	defer suspectMu.Unlock()
	delete(suspectHits, ip)
}

// recordSuspect logs an auth failure and blocks the IP after suspectThreshold
// failures. Keyed on IP alone so a forged JWT sub claim cannot target a specific
// victim's block state.
func recordSuspect(ip, userID, method, path string) {
	suspectMu.Lock()
	defer suspectMu.Unlock()
	if _, already := blockedIPs[ip]; already {
		return
	}
	suspectHits[ip]++
	n := suspectHits[ip]
	slog.Warn("user passed conductor auth but failed service validation",
		"source_ip", ip, "user_id", userID,
		"method", method, "path", path, "failure_count", n)
	if n >= suspectThreshold {
		until := time.Now().Add(blockDuration)
		blockedIPs[ip] = until
		slog.Warn("IP added to block list",
			"source_ip", ip, "blocked_until", until)
		meterBlocked.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("source_ip", ip)))
	}
}

// ── Service registry ──────────────────────────────────────────────────────────

// refreshMu serialises concurrent calls to refreshServiceCache so a push
// notification and the periodic ticker cannot race when committing the new
// routing table.
var refreshMu sync.Mutex

// refreshServiceCache fetches GET /services from the registry and rebuilds the
// in-memory proxy map and endpoint list.
func refreshServiceCache(ctx context.Context) {
	if getRegistryKey == nil {
		return
	}
	rctx, span := otel.Tracer("conductor").Start(ctx, "registry.refresh")
	defer span.End()
	refreshMu.Lock()
	defer refreshMu.Unlock()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, registryURL+"/services", nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Service-Key", "conductor:"+getRegistryKey())

	resp, err := registryClient.Do(req)
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
		Description string `json:"description"`
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

	routingMu.RLock()
	oldServices := make(map[string]serviceState, len(servicesMap))
	for k, v := range servicesMap {
		oldServices[k] = v
	}
	routingMu.RUnlock()

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
			proxy = newProxy(s.URL, s.Name)
			slog.Info("service cache updated", "name", s.Name)
		}

		newServices[s.Name] = serviceState{
			url:         s.URL,
			proxy:       proxy,
			forwardAuth: s.ForwardAuth,
			description: s.Description,
		}

		for _, ep := range s.Endpoints {
			newEndpoints = append(newEndpoints, endpointEntry{
				method:       ep.Method,
				pattern:      compilePathPattern(ep.Path),
				paramNames:   parseParamNames(ep.Path),
				action:       ep.Action,
				resource:     ep.Resource,
				public:       ep.Public,
				serviceName:  s.Name,
				originalPath: ep.Path,
			})
		}
	}

	for name := range oldServices {
		if _, ok := newServices[name]; !ok {
			slog.Info("service removed from cache", "name", name)
		}
	}

	routingMu.Lock()
	servicesMap = newServices
	endpointsList = newEndpoints
	routingMu.Unlock()
}

// compilePathPattern converts a path template such as /users/{id} into a
// regexp that matches concrete paths. Each {param} segment is replaced with
// ([^/]+) (capturing group) so values can be extracted for resource substitution.
func compilePathPattern(pattern string) *regexp.Regexp {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			parts[i] = `([^/]+)`
		} else {
			parts[i] = regexp.QuoteMeta(p)
		}
	}
	return regexp.MustCompile(`^` + strings.Join(parts, `/`) + `$`)
}

// lookupEndpoint finds the registered endpoint for method+path across all
// services. Returns the entry and true on match, zero value and false otherwise.
func lookupEndpoint(method, path string) (endpointEntry, bool) {
	routingMu.RLock()
	defer routingMu.RUnlock()
	for _, e := range endpointsList {
		if e.method == method && e.pattern.MatchString(path) {
			return e, true
		}
	}
	return endpointEntry{}, false
}

// canonicalUUID normalises a UUID string to lowercase hyphenated form.
// It accepts both the standard 36-char form and the 32-char hex form.
func canonicalUUID(v string) (string, bool) {
	v = strings.ToLower(v)
	if uuidHyphenRe.MatchString(v) {
		return v, true
	}
	if uuidHexRe.MatchString(v) {
		return v[:8] + "-" + v[8:12] + "-" + v[12:16] + "-" + v[16:20] + "-" + v[20:], true
	}
	return "", false
}

// validatePathParams checks path param values against their expected types and
// normalises UUID params to hyphenated form so resource strings are consistent.
// Returns the (possibly updated) paramValues slice and false+400 on failure.
func validatePathParams(w http.ResponseWriter, paramNames []string, paramValues []string) ([]string, bool) {
	out := make([]string, len(paramValues))
	copy(out, paramValues)
	for i, name := range paramNames {
		if i >= len(out) {
			break
		}
		val := out[i]
		switch paramTypeFor(name) {
		case "uuid":
			canonical, ok := canonicalUUID(val)
			if !ok {
				http.Error(w, "invalid path parameter: "+name+" must be a UUID", http.StatusBadRequest)
				return nil, false
			}
			out[i] = canonical
		case "slug":
			if !slugRe.MatchString(val) {
				http.Error(w, "invalid path parameter: "+name+" must be alphanumeric (hyphens/underscores allowed, max 64 chars)", http.StatusBadRequest)
				return nil, false
			}
		}
	}
	return out, true
}

// readAndValidateBody reads the request body, enforcing the global size limit.
// For POST/PUT requests with JSON content type it also checks JSON validity.
// For the signup endpoint it validates field formats.
// Returns the buffered body (or nil if no body), and false if an error response was already written.
func readAndValidateBody(w http.ResponseWriter, r *http.Request, entry endpointEntry) ([]byte, bool) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		return nil, true
	}
	if r.Body == nil {
		return nil, true
	}
	lr := &io.LimitedReader{R: r.Body, N: maxRequestBodyBytes + 1}
	body, err := io.ReadAll(lr)
	if err != nil {
		http.Error(w, "error reading request body", http.StatusBadRequest)
		return nil, false
	}
	if lr.N == 0 {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}

	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		if len(body) > 0 && !json.Valid(body) {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return nil, false
		}
		if entry.action == "signup" {
			if err := validateSignupBody(body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return nil, false
			}
		}
	}
	return body, true
}

func validateSignupBody(body []byte) error {
	var req struct {
		Email    string `json:"email"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("invalid request body")
	}
	if req.Email == "" {
		return fmt.Errorf("email is required")
	}
	if !emailRe.MatchString(req.Email) {
		return fmt.Errorf("invalid email format")
	}
	if req.Username == "" {
		return fmt.Errorf("username is required")
	}
	if !usernameRe.MatchString(req.Username) {
		return fmt.Errorf("invalid username: must be alphanumeric (hyphens/underscores allowed, max 64 chars)")
	}
	if req.Password == "" {
		return fmt.Errorf("password is required")
	}
	if len(req.Password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	return nil
}

// routeAndProxy finds the endpoint for method+path (using hybrid routing), checks
// auth/RBAC, and forwards to the backend. strippedPath is the path to forward
// (may differ from r.URL.Path when a service-name prefix was stripped).
func routeAndProxy(w http.ResponseWriter, r *http.Request, entry endpointEntry, paramValues []string, strippedPath string) {
	trace.SpanFromContext(r.Context()).SetName(entry.serviceName + " " + entry.method + " " + entry.originalPath)
	routingMu.RLock()
	svc, ok := servicesMap[entry.serviceName]
	routingMu.RUnlock()
	if !ok {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Validate path params before auth so invalid inputs get 400, not 401/403.
	// Also normalises UUID params to canonical hyphenated form for consistent
	// resource strings (avoids RBAC/cache divergence between hex and UUID forms).
	paramValues, ok = validatePathParams(w, entry.paramNames, paramValues)
	if !ok {
		return
	}

	// Read and validate the body before auth so malformed input gets 400/413.
	body, ok := readAndValidateBody(w, r, entry)
	if !ok {
		return
	}
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}

	var userID string
	if !entry.public {
		var outcome authOutcome
		outcome, userID = checkUserAuth(r)
		switch outcome {
		case authUnauthorized:
			if userID != "" {
				recordSuspect(sourceIP(r), userID, r.Method, r.URL.Path)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		case authForbidden:
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		default:
			resetSuspect(sourceIP(r))
		}
	}

	if userID != "" {
		trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("user.id", userID))
	}

	r2 := r.Clone(r.Context())
	// Strip headers that could be used to spoof identity or routing metadata.
	// X-Conductor-Token and X-Conductor-Timestamp are stripped here and only
	// re-set below (conditionally), preventing clients from pre-loading them.
	r2.Header.Del("X-User-ID")
	r2.Header.Del("X-Conductor-Token")
	r2.Header.Del("X-Conductor-Timestamp")
	r2.Header.Del("X-Forwarded-Host")
	r2.Header.Del("X-Forwarded-Proto")
	r2.Header.Del("X-Real-IP")

	// Rewrite path when the service-name prefix was stripped.
	if strippedPath != r.URL.Path {
		r2.URL.Path = strippedPath
		r2.URL.RawPath = ""
	}

	// Inject the authenticated user ID so backends don't need to decode the JWT.
	// When CONDUCTOR_FORWARD_KEY is set, also inject an HMAC token so any
	// forward_auth=false backend can verify X-User-ID was set by conductor.
	if userID != "" {
		r2.Header.Set("X-User-ID", userID)
		if !svc.forwardAuth && conductorForwardKey != "" {
			tok, ts := signForwardedUserID(userID)
			r2.Header.Set("X-Conductor-Token", tok)
			r2.Header.Set("X-Conductor-Timestamp", ts)
		}
	}

	r2.Header.Set("X-Forwarded-For", sourceIP(r))

	// Strip the bearer token before forwarding so backend services cannot replay
	// it against other services. Services that need to re-verify the caller
	// (e.g. gatekeeper itself) declare forward_auth=true in the registry.
	if !svc.forwardAuth {
		r2.Header.Del("Authorization")
	}

	svc.proxy.ServeHTTP(w, r2)
}

// handleServiceProxy is the universal handler. It uses hybrid routing:
//  1. If the first path segment is a registered service name, strip it and look
//     up the remaining path within that service's endpoints. Returns 404 if the
//     service is registered but the specific endpoint is not found.
//  2. Otherwise, try matching the full path against all registered endpoints.
//     Returns 404 if no match is found.
func handleServiceProxy(w http.ResponseWriter, r *http.Request) {
	ip := sourceIP(r)
	if isBlocked(ip) {
		slog.Warn("request rejected: IP is blocked", "source_ip", ip, "path", r.URL.Path)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	path := r.URL.Path

	// Step 1: service-name prefix routing (e.g. /blueprints/state/... or /forge/executions)
	trimmed := strings.TrimPrefix(path, "/")
	if slashIdx := strings.Index(trimmed, "/"); slashIdx > 0 {
		svcName := trimmed[:slashIdx]
		restPath := path[slashIdx+1:] // e.g. /state/alice/dev

		routingMu.RLock()
		_, svcRegistered := servicesMap[svcName]
		routingMu.RUnlock()

		if svcRegistered {
			entry, paramVals, ok := lookupEndpointForService(r.Method, restPath, svcName)
			if !ok {
				http.NotFound(w, r)
				return
			}
			routeAndProxy(w, r, entry, paramVals, restPath)
			return
		}
	}

	// Step 2: full-path endpoint matching (e.g. gatekeeper's /signup, /users/{id})
	entry, paramVals, ok := lookupEndpointFull(r.Method, path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	routeAndProxy(w, r, entry, paramVals, path)
}

// handleInternalRefresh is called by the registry after a manifest load to
// trigger an immediate route-table update without waiting for the next poll.
func handleInternalRefresh(w http.ResponseWriter, r *http.Request) {
	expected := "registry:" + conductorNotifyKey
	if conductorNotifyKey == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Service-Key")), []byte(expected)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	go refreshServiceCache(context.Background())
	w.WriteHeader(http.StatusAccepted)
}

// ── Main ──────────────────────────────────────────────────────────────────────

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
