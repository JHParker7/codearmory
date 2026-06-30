package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

var ociProxy *httputil.ReverseProxy

func initOCIProxy() {
	target, err := url.Parse(registry.baseURL)
	if err != nil {
		slog.Error("invalid REGISTRY_URL", "error", err)
		os.Exit(1)
	}
	ociProxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// Swap codearmory token for registry credentials. Per-org credentials
			// (set via context by handleV2) take precedence over the global fallback.
			req.Header.Del("Authorization")
			if creds, ok := req.Context().Value(ctxCredsKey{}).(registryCreds); ok && creds.username != "" {
				req.SetBasicAuth(creds.username, creds.password)
			} else if registry.username != "" {
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
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, action, resource)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	// Discovery ping — respond directly rather than round-tripping upstream.
	// Must be before credential resolution to avoid a Gatekeeper round-trip on
	// every docker login probe when the fetched credentials would be discarded.
	if r.URL.Path == "/v2" || r.URL.Path == "/v2/" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		span.SetStatus(codes.Ok, "")
		return
	}

	// Credential resolution priority (highest to lowest):
	//   1. Per-user Gitea token (when GITEA_INTEGRATION_URL is set)
	//   2. Per-org secret from Gatekeeper (when REGISTRY_ORG_SECRET_NAME is set)
	//   3. Global REGISTRY_USERNAME/REGISTRY_PASSWORD (proxy Director fallback)
	if creds, err := resolveGiteaUserCreds(ctx, userID, token); err == nil {
		r = r.WithContext(context.WithValue(ctx, ctxCredsKey{}, creds))
	} else if creds, err := resolveOrgCreds(ctx, orgID); err == nil {
		r = r.WithContext(context.WithValue(ctx, ctxCredsKey{}, creds))
	}

	span.SetStatus(codes.Ok, "")
	ociProxy.ServeHTTP(w, r)
}
