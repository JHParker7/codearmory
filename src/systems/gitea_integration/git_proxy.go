package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

var gitProxy *httputil.ReverseProxy

func initGitProxy() {
	target, err := url.Parse(gitea.baseURL)
	if err != nil {
		panic("invalid GITEA_URL: " + err.Error())
	}
	gitProxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// Bearer token is forwarded as-is; Forgejo validates it via
			// gatekeeper OIDC — no credential swap needed.
		},
		// Flush immediately so git's interactive protocol (pack negotiation,
		// sideband progress) is not held in a write buffer.
		FlushInterval: -1,
	}
}

// bearerToken extracts a codearmory Bearer token from the request.
// Accepts either:
//   - Authorization: Bearer <token>
//   - Authorization: Basic base64(username:<token>)  — git credential helpers
func bearerToken(r *http.Request) string {
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	if _, password, ok := r.BasicAuth(); ok && password != "" {
		return password
	}
	return ""
}

// requireBearer extracts and returns the Bearer token, writing 401 and
// returning "" if none is present.
func requireBearer(w http.ResponseWriter, r *http.Request) string {
	token := bearerToken(r)
	if token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="codearmory"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
	return token
}

// isGitProxyPath reports whether path belongs to the HTTP git smart protocol.
// Used by limitBody to skip the body size cap on these routes.
func isGitProxyPath(path string) bool {
	return strings.HasSuffix(path, "/git-upload-pack") ||
		strings.HasSuffix(path, "/git-receive-pack") ||
		strings.HasSuffix(path, "/info/refs")
}

// repoName strips a trailing ".git" suffix git clients append to repo names.
func repoName(s string) string { return strings.TrimSuffix(s, ".git") }

// checkAndProxy validates the codearmory token via gatekeeper for the given
// action and resource, then streams the request to Forgejo.
func checkAndProxy(w http.ResponseWriter, r *http.Request, action, resource string) bool {
	ctx := r.Context()
	_, span := otel.Tracer("gitea").Start(ctx, action)
	defer span.End()

	token := requireBearer(w, r)
	if token == "" {
		span.SetStatus(codes.Error, "unauthorized")
		return false
	}

	r.Header.Set("Authorization", "Bearer "+token)
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, action, resource); !ok {
		span.SetStatus(codes.Ok, "")
		return false
	}

	span.SetStatus(codes.Ok, "")
	gitProxy.ServeHTTP(w, r)
	return true
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// handleGitInfoRefs serves GET /{owner}/{name}/info/refs — the initial
// capability advertisement for both clone (git-upload-pack) and push
// (git-receive-pack).
func handleGitInfoRefs(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := repoName(r.PathValue("name"))

	action := "pullRepo"
	if r.URL.Query().Get("service") == "git-receive-pack" {
		action = "pushRepo"
	}

	checkAndProxy(w, r, action, "gitea/repos/"+owner+"/"+name)
}

// handleGitUploadPack serves POST /{owner}/{name}/git-upload-pack — the
// pack-objects exchange used by git clone/fetch.
func handleGitUploadPack(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := repoName(r.PathValue("name"))
	checkAndProxy(w, r, "pullRepo", "gitea/repos/"+owner+"/"+name)
}

// handleGitReceivePack serves POST /{owner}/{name}/git-receive-pack — the
// pack-objects exchange used by git push.
func handleGitReceivePack(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := repoName(r.PathValue("name"))
	checkAndProxy(w, r, "pushRepo", "gitea/repos/"+owner+"/"+name)
}
