package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const maxRequestBodyBytes = 64 * 1024

var (
	uuidHyphenRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	uuidHexRegex    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	slugRegex       = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	emailRegex      = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	usernameRegex   = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
)

// canonicalUUID normalises a UUID string to lowercase hyphenated form.
// It accepts both the standard 36-char form and the 32-char hex form.
func canonicalUUID(v string) (string, bool) {
	v = strings.ToLower(v)
	if uuidHyphenRegex.MatchString(v) {
		return v, true
	}
	if uuidHexRegex.MatchString(v) {
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
			if !slugRegex.MatchString(val) {
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
	if !emailRegex.MatchString(req.Email) {
		return fmt.Errorf("invalid email format")
	}
	if req.Username == "" {
		return fmt.Errorf("username is required")
	}
	if !usernameRegex.MatchString(req.Username) {
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

// resolveResource substitutes path-param placeholders in a manifest resource
// template with the canonical param values captured during route matching.
// E.g. resolveResource("forge/executions/{id}", ["id"], ["abc-123"]) →
// "forge/executions/abc-123".
func resolveResource(template string, paramNames, paramValues []string) string {
	r := template
	for i, name := range paramNames {
		if i < len(paramValues) {
			r = strings.ReplaceAll(r, "{"+name+"}", paramValues[i])
		}
	}
	return r
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

	var userID, normalizedAuth string
	if !entry.public {
		// The IP-block check only applies to non-public routes so a blocked IP can
		// still reach public recovery endpoints (e.g. /gatekeeper/login,
		// /gatekeeper/signup). Hoisting this to the top of handleServiceProxy would
		// re-block those public routes and revert fix a261f5a.
		if ip := sourceIP(r); isBlocked(ip) {
			slog.WarnContext(r.Context(), "request rejected: IP is blocked", "source_ip", ip, "path", r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		resource := resolveResource(entry.resource, entry.paramNames, paramValues)
		var outcome authOutcome
		var denyReason string
		outcome, userID, normalizedAuth, denyReason = checkUserAuth(r, entry.serviceName, entry.action, resource)
		switch outcome {
		case authUnauthorized:
			if userID != "" {
				recordSuspect(sourceIP(r), userID, r.Method, r.URL.Path)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		case authForbidden:
			if userID != "" {
				recordSuspect(sourceIP(r), userID, r.Method, r.URL.Path)
			}
			if denyReason == "" {
				denyReason = "forbidden"
			}
			http.Error(w, denyReason, http.StatusForbidden)
			return
		case authError:
			http.Error(w, "service unavailable", http.StatusBadGateway)
			return
		default:
			resetSuspect(sourceIP(r))
		}
	}

	span := trace.SpanFromContext(r.Context())
	if userID != "" {
		span.SetAttributes(attribute.String("user.id", userID))
	}
	if ua := r.Header.Get("User-Agent"); strings.HasPrefix(ua, "armory-cli") {
		span.SetAttributes(attribute.String("armory.client", "cli"))
	}

	svc.proxy.ServeHTTP(w, prepareForwardRequest(r, userID, normalizedAuth, svc, strippedPath))
}

// prepareForwardRequest clones r, strips client-supplied identity/routing
// headers that backends must not trust, rewrites the path if a service-name
// prefix was stripped, and injects X-User-ID (plus an HMAC token for
// non-forwardAuth services). The caller's Authorization header is removed
// for non-forwardAuth services so backend tokens cannot be replayed.
// normalizedAuth is the "Bearer <token>" string extracted during auth (may
// differ from r's Authorization header when auth arrived via cookie).
func prepareForwardRequest(r *http.Request, userID, normalizedAuth string, svc serviceState, strippedPath string) *http.Request {
	r2 := r.Clone(r.Context())

	// Drop headers that a client could use to spoof identity. X-Conductor-*
	// are stripped here and only re-added below so clients cannot pre-load them.
	r2.Header.Del("X-User-ID")
	r2.Header.Del("X-Conductor-Token")
	r2.Header.Del("X-Conductor-Timestamp")
	r2.Header.Del("X-Forwarded-Host")
	r2.Header.Del("X-Forwarded-Proto")
	r2.Header.Del("X-Real-IP")

	if strippedPath != r.URL.Path {
		r2.URL.Path = strippedPath
		r2.URL.RawPath = ""
	}

	if userID != "" {
		r2.Header.Set("X-User-ID", userID)
		if !svc.forwardAuth && conductorForwardKey != "" {
			tok, ts := signForwardedUserID(userID)
			r2.Header.Set("X-Conductor-Token", tok)
			r2.Header.Set("X-Conductor-Timestamp", ts)
		}
	}
	r2.Header.Set("X-Forwarded-For", sourceIP(r))

	// If auth arrived via cookie, inject the token as Authorization so that
	// forward_auth=true backends (e.g. gatekeeper) receive it properly.
	if normalizedAuth != "" && r2.Header.Get("Authorization") == "" {
		r2.Header.Set("Authorization", normalizedAuth)
	}

	// Services declaring forward_auth=true (e.g. gatekeeper) receive the
	// original Bearer token. All others get it stripped to prevent replay.
	if !svc.forwardAuth {
		r2.Header.Del("Authorization")
	}
	// Strip the session cookie so it isn't forwarded to backends.
	r2.Header.Del("Cookie")
	return r2
}

// handleServiceProxy is the universal handler. It uses hybrid routing:
//  1. If the first path segment is a registered service name, strip it and look
//     up the remaining path within that service's endpoints. Returns 404 if the
//     service is registered but the specific endpoint is not found.
//  2. Otherwise, try matching the full path against all registered endpoints.
//     Returns 404 if no match is found.
func handleServiceProxy(w http.ResponseWriter, r *http.Request) {
	// Accept an optional "/api" prefix so the SPA can call the API on the same origin
	// it loads from (/api/gatekeeper/... routes as /gatekeeper/...). Mutating the request
	// path here means every downstream step — route lookup and the forwarded path —
	// sees the stripped form.
	if stripped := stripAPIPrefix(r.URL.Path); stripped != r.URL.Path {
		r.URL.Path = stripped
	}
	path := r.URL.Path

	// All routes require a service-name prefix (e.g. /forge/executions, /gatekeeper/login).
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

	// Not an API route. When conductor fronts the web UI (PORTAL_URL set), hand the
	// request to the portal so one origin serves both the API and the SPA (its "/",
	// "/app/...", and "/assets/..." all land here). Otherwise it is a genuine 404.
	if portalProxy != nil {
		portalProxy.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
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
