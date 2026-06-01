package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

var ociProxy *httputil.ReverseProxy

func initOCIProxy() {
	target, err := url.Parse(registry.baseURL)
	if err != nil {
		panic("invalid REGISTRY_URL: " + err.Error())
	}
	ociProxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// Swap codearmory token for registry service account credentials
			// so the upstream registry accepts the request.
			req.Header.Del("Authorization")
			if registry.username != "" {
				req.SetBasicAuth(registry.username, registry.password)
			}
		},
		// Flush immediately — blob layers can be gigabytes and must stream
		// rather than buffer.
		FlushInterval: -1,
	}
}

// bearerToken extracts a codearmory Bearer token from the request.
// Accepts either:
//   - Authorization: Bearer <token>
//   - Authorization: Basic base64(username:<token>)  — docker login stores this
func bearerToken(r *http.Request) string {
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	if _, password, ok := r.BasicAuth(); ok && password != "" {
		return password
	}
	return ""
}

// isOCIPath reports whether path belongs to the OCI distribution API.
// Used by limitBody to skip the body size cap on these routes.
func isOCIPath(path string) bool {
	return path == "/v2" || strings.HasPrefix(path, "/v2/")
}

// v2ActionResource maps an OCI request to a gatekeeper action + resource pair.
func v2ActionResource(method, path string) (action, resource string) {
	rest, _ := strings.CutPrefix(path, "/v2/")

	// Catalog and top-level discovery
	if rest == "" || rest == "_catalog" {
		return "listRepository", "containers/repositories"
	}

	// Extract repo name — everything before the first known OCI segment marker
	repo := rest
	for _, marker := range []string{"/manifests/", "/tags/list", "/blobs/uploads", "/blobs/"} {
		if idx := strings.Index(rest, marker); idx >= 0 {
			repo = rest[:idx]
			break
		}
	}

	resource = "containers/repositories/" + repo

	if strings.HasSuffix(path, "/tags/list") {
		return "listTag", resource
	}

	switch method {
	case http.MethodDelete:
		return "deleteImage", resource
	case http.MethodGet, http.MethodHead:
		// Resumable upload status checks are GET but belong to push flow
		if strings.Contains(path, "/blobs/uploads/") {
			return "pushImage", resource
		}
		return "pullImage", resource
	default: // POST, PUT, PATCH
		return "pushImage", resource
	}
}

// handleV2 is the catch-all handler for all OCI distribution API traffic
// (/v2 and /v2/*). It:
//  1. Returns 401 with a Bearer challenge for unauthenticated requests.
//  2. Validates the codearmory token via gatekeeper.
//  3. Streams the request to the upstream registry with service credentials.
func handleV2(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("containers").Start(r.Context(), "handleV2")
	defer span.End()

	token := bearerToken(r)
	if token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="codearmory"`)
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`)) //nolint:errcheck
		return
	}

	action, resource := v2ActionResource(r.Method, r.URL.Path)

	r.Header.Set("Authorization", "Bearer "+token)
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, action, resource); !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	// Discovery ping — respond directly rather than round-tripping upstream.
	if r.URL.Path == "/v2" || r.URL.Path == "/v2/" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		span.SetStatus(codes.Ok, "")
		return
	}

	span.SetStatus(codes.Ok, "")
	ociProxy.ServeHTTP(w, r)
}
