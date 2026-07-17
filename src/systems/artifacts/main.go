// Artifacts — the store for things that must OUTLIVE a run.
//
// Forge volumes are run-scoped and reaped at the end of a run, so a Go build cache
// or a compiled binary has nowhere to persist. A pipeline saves an artifact at the
// end of a run and restores it at the start of the next, which is what turns a cold
// build into a warm one and lets a CLI binary be published after the run that made it.
//
// Every artifact belongs to a user and counts against that user's cap. An admin sets
// the default for everyone (ARTIFACTS_DEFAULT_QUOTA_MB) and can override any single
// user through /quotas — the same default-plus-override shape forge uses for
// concurrency limits.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	gk "github.com/code-armory-app/codearmory_sdk/gatekeeper"
	sdkregistry "github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
)

// contextT aliases context.Context so quotaView can be declared in api_quotas.go
// without importing context there.
type contextT = context.Context

var (
	gatekeeperURL    string
	gatekeeperClient *gk.Client
	httpClient       = &http.Client{Timeout: 30 * time.Second}
	getServiceKey    func() string
)

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

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// dataDir is where blobs live — a persistent volume in a real deployment.
func dataDir() string { return envOrDefault("ARTIFACTS_DATA_DIR", "/data") }

// defaultQuotaMB is the service-wide default allowance, in megabytes. It is the
// admin's one global knob; per-user overrides live in the database.
const defaultQuotaMB = 5120 // 5 GB — enough for a Go module + build cache

// defaultQuotaBytes is the per-user cap when no override exists.
//
// Read per call rather than cached at boot so an admin can change the default by
// restarting with a new value and have it apply to everyone who has no override —
// without a migration or a per-user backfill.
func defaultQuotaBytes() int64 {
	mb := int64(defaultQuotaMB)
	if v := os.Getenv("ARTIFACTS_DEFAULT_QUOTA_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			mb = n
		} else {
			slog.Warn("invalid ARTIFACTS_DEFAULT_QUOTA_MB, using built-in default", "value", v, "default_mb", defaultQuotaMB)
		}
	}
	return mb * 1024 * 1024
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

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "artifacts")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(context.Background())
	}

	// Choose the blob backend. An S3 bucket (ARTIFACTS_S3_BUCKET) selects object
	// storage, which is shared across nodes and lets the service scale past one
	// replica; otherwise the filesystem PVC, which is simplest but single-node.
	if os.Getenv("ARTIFACTS_S3_BUCKET") != "" {
		s3s, err := newS3Store(ctx)
		if err != nil {
			slog.Error("cannot initialise s3 artifact store", "error", err)
			os.Exit(1)
		}
		store = s3s
		slog.Info("artifact store: s3", "bucket", os.Getenv("ARTIFACTS_S3_BUCKET"), "endpoint", os.Getenv("ARTIFACTS_S3_ENDPOINT"))
	} else {
		if err := os.MkdirAll(dataDir(), 0o750); err != nil {
			slog.Error("cannot create artifact data dir", "dir", dataDir(), "error", err)
			os.Exit(1)
		}
		store = newFSStore(dataDir())
		slog.Info("artifact store: filesystem", "dir", dataDir())
	}
	if err := migrate(); err != nil {
		slog.Error("failed to migrate database", "error", err)
		os.Exit(1)
	}

	gatekeeperURL = strings.TrimRight(envOrDefault("GATEKEEPER_URL", "http://gatekeeper:8081"), "/")
	gatekeeperClient = &gk.Client{URL: gatekeeperURL, Service: "artifacts", HTTPClient: httpClient}
	getServiceKey = sdkregistry.StartKeyRotation(ctx, gatekeeperURL, "artifacts",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)
	_ = getServiceKey

	mux := telemetry.NewMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// The caller's own artifacts.
	mux.HandleFunc("GET /artifacts", handleListArtifacts)
	mux.HandleFunc("GET /artifacts/{name}", handleGetArtifact)
	mux.HandleFunc("PUT /artifacts/{name}", handleUploadArtifact)
	mux.HandleFunc("GET /artifacts/{name}/content", handleDownloadArtifact)
	mux.HandleFunc("DELETE /artifacts/{name}", handleDeleteArtifact)
	// A user's own allowance — no admin right needed, so a pipeline can check its
	// headroom before it spends ten minutes building something it cannot store.
	mux.HandleFunc("GET /usage", handleGetUsage)

	// Admin: the default plus per-scope overrides.
	mux.HandleFunc("GET /quotas", handleListQuotas)
	mux.HandleFunc("GET /quotas/{scope}/{scope_id}", handleGetQuota)
	mux.HandleFunc("PUT /quotas/{scope}/{scope_id}", handleSetQuota)
	mux.HandleFunc("DELETE /quotas/{scope}/{scope_id}", handleDeleteQuota)

	port := envOrDefault("PORT", "8097")
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
		// Uploads are large and slow; a write timeout here would kill a legitimate
		// multi-gigabyte cache push mid-stream. The body size is bounded by the
		// quota instead, which is the limit that actually matters.
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx) //nolint:errcheck
	}()

	slog.Info("artifacts listening", "port", port, "data_dir", dataDir(),
		"default_quota_mb", defaultQuotaBytes()/(1024*1024))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
