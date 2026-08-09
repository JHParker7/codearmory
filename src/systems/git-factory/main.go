package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	gk "github.com/code-armory-app/codearmory_sdk/gatekeeper"
	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

// serviceName is this service's registered gatekeeper/registry name. It is the
// first segment of every RBAC resource ("codearmory_git_factory/repos") and the identity
// used for service-key rotation. The generator rewrites it to your service name.
const serviceName = "codearmory_git_factory"

var (
	gatekeeperURL = envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
	httpClient    *http.Client
	// gatekeeperClient delegates per-request authorisation to gatekeeper's
	// /check_permissions endpoint (forward_auth: true).
	gatekeeperClient *gk.Client
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// secret reads NAME, preferring the file at ${NAME}_FILE (k8s/Docker mounted
// secrets) and falling back to the plain env var.
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

func secretOrDefault(name, def string) string {
	if v := secret(name); v != "" {
		return v
	}
	return def
}

// initHTTPClient builds the outbound client used for gatekeeper calls, wiring in
// optional mTLS (TLS_CLIENT_CERT_FILE/TLS_CLIENT_KEY_FILE) and a custom CA
// (TLS_CA_FILE), and propagating trace context via otelhttp.
func initHTTPClient() *http.Client {
	tlsCfg := &tls.Config{}
	if certFile, keyFile := os.Getenv("TLS_CLIENT_CERT_FILE"), os.Getenv("TLS_CLIENT_KEY_FILE"); certFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			slog.Error("failed to load client TLS certificate", "error", err)
			os.Exit(1)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	if caFile := os.Getenv("TLS_CA_FILE"); caFile != "" {
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			slog.Error("failed to read TLS_CA_FILE", "error", err)
			os.Exit(1)
		}
		caPool := x509.NewCertPool()
		caPool.AppendCertsFromPEM(caCert)
		tlsCfg.RootCAs = caPool
	}
	return &http.Client{
		Transport: otelhttp.NewTransport(&http.Transport{TLSClientConfig: tlsCfg}),
		Timeout:   15 * time.Second,
	}
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *statusResponseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

type requestLogger struct{ handler http.Handler }

func (l *requestLogger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	l.handler.ServeHTTP(rw, r)
	sc := trace.SpanFromContext(r.Context()).SpanContext()
	msg := r.Method + " " + r.URL.Path
	attrs := []any{
		"status", rw.status,
		"duration", time.Since(start),
		"trace_id", sc.TraceID().String(),
		"span_id", sc.SpanID().String(),
	}
	switch r.URL.Path {
	case "/healthz", "/health", "/readyz", "/livez":
		if rw.status >= 500 {
			slog.Warn(msg, attrs...)
		} else {
			slog.Debug(msg, attrs...)
		}
	default:
		slog.InfoContext(r.Context(), "http request", append([]any{"method", r.Method, "path", r.URL.Path}, attrs...)...)
	}
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The git wire routes are exempt: a push POST is the whole packfile, which is
		// routinely far past maxBodyBytes, so capping it fails every non-trivial push
		// instantly. The cap stays on for the JSON API, where it belongs.
		if r.Body != nil && !isGitWirePath(r.URL.Path) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// isGitWirePath reports whether a path is one of the three Smart-HTTP endpoints. It
// matches on the suffix the protocol fixes, so it stays correct for any {ns}/{repo}.
func isGitWirePath(p string) bool {
	return strings.HasSuffix(p, "/info/refs") ||
		strings.HasSuffix(p, "/"+string(svcUploadPack)) ||
		strings.HasSuffix(p, "/"+string(svcReceivePack))
}

func main() {
	logLevel := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		_ = logLevel.UnmarshalText([]byte(v))
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), serviceName)
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(context.Background())
	}
	initMetrics()
	httpClient = initHTTPClient()
	// Built after httpClient so the emitter reuses the instrumented transport.
	initEventEmitter()
	if !eventsEnabled() {
		slog.Info("events integration disabled — set EVENTS_URL and EVENTS_TRIGGER_KEY to enable")
	}

	// Before anything can accept a push: confirm the storage root actually provides
	// the properties git's correctness rests on. Runs first because a filesystem that
	// fails these corrupts repositories rather than erroring, and this store is the
	// only copy of pushed source (ARCHITECTURE §5).
	verifyStorage()

	if err := connect().AutoMigrate(&Repo{}, &ShardNode{}, &ReplicaState{}, &RepoShare{},
		&PullRequest{}, &PullReview{}, &PullComment{}, &CommitStatus{}, &RepoWebhook{},
		&BranchProtection{}, &MaintenanceLease{}); err != nil {
		slog.Error("failed to migrate database", "error", err)
		os.Exit(1)
	}
	// AutoMigrate never alters an existing PRIMARY KEY, so an upgraded install keeps the
	// pre-replica single-column shard_nodes key and physically cannot hold a primary
	// plus its replicas. Fresh installs are already correct; this is a no-op there.
	if err := widenShardNodePK(connect()); err != nil {
		slog.Error("failed to migrate shard_nodes primary key", "error", err)
		os.Exit(1)
	}
	slog.Info("database initialized")

	gatekeeperClient = &gk.Client{URL: gatekeeperURL, Service: serviceName, HTTPClient: httpClient}

	// Registers this service with gatekeeper and rotates the shared service key
	// every 25 minutes. The initial key must match this service's entry in
	// gatekeeper's GATEKEEPER_SERVICES env var. No-op if the key is empty.
	// The returned accessor is the CURRENT key after each rotation — audit writes
	// authenticate with it, so they keep working across a rotation.
	serviceKey = registry.StartKeyRotation(ctx, gatekeeperURL, serviceName,
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	// Periodic repack. In THIS process, not a sidecar: the storage volume is
	// ReadWriteOnce, so this pod is the only thing that may write to the bytes.
	startMaintenance(ctx)

	mux := telemetry.NewMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /openapi.yaml", handleOpenAPIYAML)

	// Embedded mini-portal. Both spellings: the shell frames "<ui_path>/" but a user
	// following a link without the trailing slash must not get a 404. The {$} anchor
	// matters — a bare "/ui/" would also match "/ui/x/info/refs", which the git wire
	// pattern below claims, and ServeMux panics on that ambiguity rather than guessing.
	mux.HandleFunc("GET /ui", handleUI)
	mux.HandleFunc("GET /ui/{$}", handleUI)

	// Example resource — rename/replace with your own. Each handler authorises
	// via gatekeeperClient.CheckPermissions(action, resource) before doing work.
	mux.HandleFunc("GET /repos", handleListRepos)
	mux.HandleFunc("POST /repos", handleCreateRepo)
	mux.HandleFunc("GET /repos/{id}", handleGetRepo)
	mux.HandleFunc("PATCH /repos/{id}", handleUpdateRepo)
	mux.HandleFunc("DELETE /repos/{id}", handleDeleteRepo)
	mux.HandleFunc("GET /repos/{id}/commits", handleListCommits)
	mux.HandleFunc("GET /repos/{id}/commits/{sha}", handleCommit)
	mux.HandleFunc("GET /repos/{id}/readme", handleGetReadme)
	mux.HandleFunc("GET /repos/{id}/branches", handleListBranches)
	mux.HandleFunc("GET /repos/{id}/tags", handleListTags)
	mux.HandleFunc("PUT /repos/{id}/default-branch", handleSetDefaultBranch)
	mux.HandleFunc("GET /repos/{id}/tree", handleTree)
	mux.HandleFunc("GET /repos/{id}/blob", handleBlob)
	mux.HandleFunc("PUT /repos/{id}/blob", handleWriteBlob)
	mux.HandleFunc("GET /repos/{id}/archive", handleArchive)
	mux.HandleFunc("GET /repos/{id}/protections", handleListProtections)
	mux.HandleFunc("PUT /repos/{id}/protections", handleSetProtection)
	mux.HandleFunc("DELETE /repos/{id}/protections/{pattern}", handleDeleteProtection)
	mux.HandleFunc("GET /repos/{id}/pulls", handleListPulls)
	mux.HandleFunc("POST /repos/{id}/pulls", handleCreatePull)
	mux.HandleFunc("GET /repos/{id}/pulls/{number}", handleGetPull)
	mux.HandleFunc("POST /repos/{id}/pulls/{number}/merge", handleMergePull)
	mux.HandleFunc("POST /repos/{id}/pulls/{number}/close", handleClosePull)
	// Review layer — the verdicts branch protection counts, and the discussion it does
	// not. Separate actions so "may review" is grantable without "may merge".
	mux.HandleFunc("GET /repos/{id}/pulls/{number}/reviews", handleListReviews)
	mux.HandleFunc("POST /repos/{id}/pulls/{number}/reviews", handleCreateReview)
	mux.HandleFunc("GET /repos/{id}/pulls/{number}/comments", handleListComments)
	mux.HandleFunc("POST /repos/{id}/pulls/{number}/comments", handleCreateComment)
	mux.HandleFunc("DELETE /repos/{id}/pulls/{number}/comments/{comment_id}", handleDeleteComment)
	// Commit statuses (checks) — where CI reports back, and what a required check reads.
	mux.HandleFunc("POST /repos/{id}/statuses/{sha}", handleSetStatus)
	mux.HandleFunc("GET /repos/{id}/commits/{sha}/statuses", handleListStatuses)
	// Per-repo outbound webhooks.
	mux.HandleFunc("GET /repos/{id}/webhooks", handleListWebhooks)
	mux.HandleFunc("POST /repos/{id}/webhooks", handleCreateWebhook)
	mux.HandleFunc("DELETE /repos/{id}/webhooks/{hook_id}", handleDeleteWebhook)
	// Forks — what makes a contribution possible without write access to the target.
	mux.HandleFunc("POST /repos/{id}/fork", handleForkRepo)
	mux.HandleFunc("GET /repos/{id}/forks", handleListForks)
	// Transfer — change of owner/namespace with the id (and so every grant, PR and
	// byte on disk) left alone.
	mux.HandleFunc("POST /repos/{id}/transfer", handleTransferRepo)
	// Tags and releases: a tag is creatable now, and an annotated one is a release.
	mux.HandleFunc("POST /repos/{id}/tags", handleCreateTag)
	mux.HandleFunc("GET /repos/{id}/tags/{name}", handleGetTag)
	mux.HandleFunc("DELETE /repos/{id}/tags/{name}", handleDeleteTag)
	// CODEOWNERS (read from the ref, never stored) and in-repo code search.
	mux.HandleFunc("GET /repos/{id}/codeowners", handleGetCodeowners)
	mux.HandleFunc("GET /repos/{id}/search", handleSearchCode)
	mux.HandleFunc("GET /repos/{id}/collaborators", handleListCollaborators)
	mux.HandleFunc("PUT /repos/{id}/collaborators", handleAddCollaborator)
	mux.HandleFunc("DELETE /repos/{id}/collaborators/{user}", handleRemoveCollaborator)

	// Git wire surface (ARCHITECTURE §2b). The shape is fixed by the protocol, not
	// chosen: git appends these paths to the clone URL. {repo} carries an optional
	// ".git" suffix, which repoPathParts strips.
	mux.HandleFunc("GET /{ns}/{repo}/info/refs", handleInfoRefs)
	mux.HandleFunc("POST /{ns}/{repo}/"+string(svcUploadPack), handleUploadPack)
	mux.HandleFunc("POST /{ns}/{repo}/"+string(svcReceivePack), handleReceivePack)

	// Git LFS. Part of the WIRE surface, not the JSON API: an LFS client is a git client
	// and authenticates the same way, so these are authorised by authorizeGitRepo (read
	// for download, write for upload) and never routed through conductor's RBAC.
	mux.HandleFunc("POST /{ns}/{repo}/info/lfs/objects/batch", handleLFSBatch)
	mux.HandleFunc("PUT /{ns}/{repo}/info/lfs/objects/{oid}", handleLFSUpload)
	mux.HandleFunc("GET /{ns}/{repo}/info/lfs/objects/{oid}", handleLFSDownload)

	// Internal pull-through-mirror surface (DESIGN-read-replicas.md §7). Called by
	// git-connector, authenticated by GIT_FACTORY_INTERNAL_KEY — NOT part of the
	// gatekeeper RBAC surface and never routed through Conductor.
	mux.HandleFunc("POST /internal/mirrors", handleEnsureMirror)
	mux.HandleFunc("POST /internal/clone-token", handleMintCloneToken)
	// Node-to-node replication (ARCHITECTURE §5 Step 4): a replica fetches from the
	// primary on request. Trusted by the node forward key, like a proxied wire request.
	mux.HandleFunc("POST /internal/replicate", handleReplicate)

	port := envOrDefault("PORT", "9002")
	wrapped := otelhttp.NewHandler(limitBody(&requestLogger{mux}), serviceName,
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      wrapped,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	go func() {
		slog.Info("listening", "port", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
