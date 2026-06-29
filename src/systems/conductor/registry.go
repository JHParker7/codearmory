package main

import (
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

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

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
	uiPath      string // path serving the service's embedded mini-portal ("" = no UI)
}

// routingMu protects both servicesMap and endpointsList under a single lock so
// readers always see a consistent pair — updates swap both atomically.
var (
	routingMu     sync.RWMutex
	servicesMap   = map[string]serviceState{}
	endpointsList []endpointEntry
)

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
		slog.WarnContext(ctx, "service registry refresh failed", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode == http.StatusUnauthorized {
			// A 401 means the registry rejected conductor's service key. This is a
			// persistent auth failure (e.g. the key fell out of sync after a restart
			// or chaos test) that retrying with the same key cannot resolve, and it
			// keeps conductor from routing any traffic — so it is an error, not a warning.
			slog.ErrorContext(ctx, "service registry refresh: registry rejected conductor service key", "status", resp.StatusCode)
		} else {
			slog.WarnContext(ctx, "service registry refresh: unexpected status", "status", resp.StatusCode)
		}
		return
	}

	var svcs []struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		Description string `json:"description"`
		ForwardAuth bool   `json:"forward_auth"`
		UIPath      string `json:"ui_path"`
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
			slog.WarnContext(ctx, "invalid service URL", "name", s.Name, "url", s.URL)
			continue
		}

		var proxy *httputil.ReverseProxy
		if old, ok := oldServices[s.Name]; ok && old.url == s.URL {
			proxy = old.proxy
		} else {
			proxy = newProxy(s.URL, s.Name)
			slog.DebugContext(ctx, "service cache updated", "name", s.Name)
		}

		newServices[s.Name] = serviceState{
			url:         s.URL,
			proxy:       proxy,
			forwardAuth: s.ForwardAuth,
			description: s.Description,
			uiPath:      s.UIPath,
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
			slog.DebugContext(ctx, "service removed from cache", "name", name)
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
