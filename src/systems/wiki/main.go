package main

import (
	"context"
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
)

// wiki is a project's source of truth as a first-class platform service. Content lives in
// a git repo reached through a Store (git-factory today; the git connector's backends,
// e.g. a migrated user's GitHub, later), so history/diffs are git's. This service adds the
// page schema, the read-scoping manifest, project-scoped authz, and (later) drift checks.

var (
	gatekeeperURL    string
	httpClient       *http.Client
	gatekeeperClient *gk.Client
	store            Store
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// secret reads NAME, preferring the file at ${NAME}_FILE (mounted secrets).
func secret(name string) string {
	if path := os.Getenv(name + "_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("cannot read secret file", "var", name+"_FILE", "error", err)
			os.Exit(1)
		}
		return strings.TrimRight(string(data), "\n")
	}
	return os.Getenv(name)
}

func main() {
	lvl := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		lvl = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	httpClient = &http.Client{Timeout: 30 * time.Second}
	gatekeeperURL = envOrDefault("GATEKEEPER_URL", "http://localhost:8081")
	gatekeeperClient = &gk.Client{URL: gatekeeperURL, Service: "wiki", HTTPClient: httpClient}
	registry.StartKeyRotation(ctx, gatekeeperURL, "wiki", secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	// Model B: the wiki owns its repos through a bot identity, and it simply IS that bot —
	// it logs in as the bot and uses that session for git. No privileged token minting: the
	// bot only ever touches repos in its own namespace, so an ordinary login is enough, and
	// users still need only wiki permissions.
	botEmail := os.Getenv("WIKI_BOT_EMAIL")
	botPass := secret("WIKI_BOT_PASSWORD")
	if botEmail == "" || botPass == "" {
		slog.Error("WIKI_BOT_EMAIL and WIKI_BOT_PASSWORD are required (the bot the wiki writes as)")
		os.Exit(1)
	}
	store = newGitFactoryStore(
		envOrDefault("GIT_FACTORY_URL", "http://localhost:9002"),
		gatekeeperURL,
		botEmail,
		botPass,
		envOrDefault("WIKI_NAMESPACE", "ops"),
		httpClient,
	)

	mux := telemetry.NewMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	// project-scoped pages
	mux.HandleFunc("GET /projects/{project}/pages", handleListPages)
	mux.HandleFunc("GET /projects/{project}/manifest", handleListPages)
	mux.HandleFunc("GET /projects/{project}/pages/{id}", handleGetPage)
	mux.HandleFunc("PUT /projects/{project}/pages/{id}", handleWritePage)
	mux.HandleFunc("DELETE /projects/{project}/pages/{id}", handleDeletePage)
	mux.HandleFunc("GET /projects/{project}/pages/{id}/history", handlePageHistory)

	port := envOrDefault("PORT", "8110")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	go func() {
		slog.Info("wiki listening", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
