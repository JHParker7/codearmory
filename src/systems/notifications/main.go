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

// notifications delivers platform events to human channels through pluggable providers
// (Slack, Discord, generic webhook, email). A CHANNEL is a configured destination — a
// provider plus its config (a webhook URL, an email address) and the set of event types
// it wants. The events service posts matching events to /internal/notify, and this
// service fans each one out to every channel that subscribes to it. Config lives in
// postgres; delivery is best-effort and logged.

var (
	gatekeeperURL    string
	httpClient       *http.Client
	gatekeeperClient *gk.Client
	// eventsTriggerKey is the shared HMAC key that authenticates the events service posting
	// to /internal/notify (the same EVENTS_TRIGGER_KEY events signs "dispatch:<ts>" with).
	eventsTriggerKey string
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
	gatekeeperClient = &gk.Client{URL: gatekeeperURL, Service: "notifications", HTTPClient: httpClient}
	registry.StartKeyRotation(ctx, gatekeeperURL, "notifications", secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)
	eventsTriggerKey = secret("EVENTS_TRIGGER_KEY")

	if err := migrate(ctx); err != nil {
		slog.Error("migrate", "error", err)
		os.Exit(1)
	}

	mux := telemetry.NewMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	// Providers are the static set of integration types a channel can use.
	mux.HandleFunc("GET /providers", handleListProviders)
	// Channels are user/project-configured destinations.
	mux.HandleFunc("GET /channels", handleListChannels)
	mux.HandleFunc("POST /channels", handleCreateChannel)
	mux.HandleFunc("GET /channels/{id}", handleGetChannel)
	mux.HandleFunc("PUT /channels/{id}", handleUpdateChannel)
	mux.HandleFunc("DELETE /channels/{id}", handleDeleteChannel)
	mux.HandleFunc("POST /channels/{id}/test", handleTestChannel)
	// The events service fans matching events here (HMAC-authenticated, not gatekeeper).
	mux.HandleFunc("POST /internal/notify", handleInternalNotify)

	port := envOrDefault("PORT", "8094")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	go func() {
		slog.Info("notifications listening", "port", port)
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
