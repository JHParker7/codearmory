package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

var ociProxy *httputil.ReverseProxy

func initOCIProxy() {
	ociProxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// The upstream registry is resolved per request (the default
			// registry) and passed via context by handleV2 — there is no longer
			// a single boot-time target.
			reg, _ := req.Context().Value(ctxRegistryKey{}).(*registryClient)
			if reg == nil {
				return
			}
			target, err := url.Parse(reg.baseURL)
			if err != nil {
				return
			}
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// Swap codearmory token for registry credentials. Per-org credentials
			// (set via context by handleV2) take precedence over the global fallback.
			req.Header.Del("Authorization")
			if creds, ok := req.Context().Value(ctxCredsKey{}).(registryCreds); ok && creds.username != "" {
				req.SetBasicAuth(creds.username, creds.password)
			} else if reg.username != "" {
				req.SetBasicAuth(reg.username, reg.password)
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

// v2RepoName extracts the repository name ("<namespace>/<image>") from an OCI
// distribution path. Returns "" for paths that are not scoped to a repository:
// the discovery ping (/v2, /v2/) and the catalog (/v2/_catalog).
func v2RepoName(path string) string {
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok || rest == "" || rest == "_catalog" {
		return ""
	}
	// Everything before the first known OCI segment marker is the repo name.
	for _, marker := range []string{"/manifests/", "/tags/list", "/blobs/uploads", "/blobs/"} {
		if idx := strings.Index(rest, marker); idx >= 0 {
			return rest[:idx]
		}
	}
	return rest
}

// v2Namespace returns the registry namespace owning an OCI request — the first
// segment of the repository name — or "" when the path is not repository-scoped.
func v2Namespace(path string) string {
	ns, _, _ := strings.Cut(v2RepoName(path), "/")
	return ns
}

// v2ActionResource maps an OCI request to a gatekeeper action + resource pair.
func v2ActionResource(method, path string) (action, resource string) {
	repo := v2RepoName(path)
	// Catalog and top-level discovery — no repository in the path.
	if repo == "" {
		return "listRepository", "containers/repositories"
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
//  3. Enforces that the caller owns the namespace the request addresses.
//  4. Streams the request to the upstream registry with service credentials.
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

	// RBAC alone is not a tenant boundary on these routes: the default grant
	// {username}/containers/repositories/* wildcards across every namespace (see
	// namespaceAllowed), and when no per-user credentials resolve the Director
	// falls back to the global service account, so the upstream registry does not
	// scope the request either. Enforce namespace ownership for every verb — pull,
	// push and delete alike — before anything is proxied. The discovery ping
	// returned above carries no namespace, and /v2/_catalog is not
	// repository-scoped either: it stays governed by the listRepository action,
	// exactly like handleListRepositories.
	deny := func(namespace string) {
		span.SetStatus(codes.Ok, "")
		slog.WarnContext(ctx, "v2 request: namespace not owned by caller",
			"user_id", userID, "namespace", namespace, "method", r.Method, "path", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"errors":[{"code":"DENIED","message":"requested access to the resource is denied"}]}`)) //nolint:errcheck
	}
	// authorizeRepo is the namespace-ownership check widened with the project
	// fallback: a caller who does not own the namespace is still allowed when the
	// repository is linked to a project they hold (action) on. Unlinked repositories
	// fall through to the identical namespaceAllowed decision as before.
	if repo := v2RepoName(r.URL.Path); repo != "" {
		ns, img, _ := strings.Cut(repo, "/")
		if !authorizeRepo(ctx, r, userID, orgID, action, ns, img) {
			deny(ns)
			return
		}
	}
	// A cross-repository blob mount (?from=<other-repo>) reads from a second
	// repository, so the caller must be authorized for that one as well.
	if from := r.URL.Query().Get("from"); from != "" {
		fromNS, fromImg, _ := strings.Cut(from, "/")
		if !authorizeRepo(ctx, r, userID, orgID, action, fromNS, fromImg) {
			deny(fromNS)
			return
		}
	}

	// Resolve the upstream (default) registry. The service may boot with none
	// configured, so respond with a clear OCI error rather than proxying to
	// nowhere.
	reg, err := resolveDefaultRegistry(ctx)
	if err != nil {
		span.SetStatus(codes.Ok, "")
		slog.WarnContext(ctx, "v2 request with no registry configured", "path", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"errors":[{"code":"UNAVAILABLE","message":"no container registry configured"}]}`)) //nolint:errcheck
		return
	}
	r = r.WithContext(context.WithValue(ctx, ctxRegistryKey{}, reg))
	ctx = r.Context()

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
