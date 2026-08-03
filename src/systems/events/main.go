package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	gk "github.com/code-armory-app/codearmory_sdk/gatekeeper"
	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// gatekeeperClient verifies caller permissions on the trigger/event CRUD endpoints.
var gatekeeperClient *gk.Client

// initGithubApp builds the optional GitHub App from the environment. Returns (nil, nil) when
// no App is configured. A half-configured App is an error rather than a silent downgrade: an
// App id with no webhook secret would accept unverified deliveries.
func initGithubApp() (*githubApp, error) {
	rawID := secret("GITHUB_APP_ID")
	if rawID == "" {
		return nil, nil
	}
	appID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("GITHUB_APP_ID %q is not an integer: %w", rawID, err)
	}
	webhookSecret := secret("GITHUB_APP_WEBHOOK_SECRET")
	if webhookSecret == "" {
		return nil, fmt.Errorf("GITHUB_APP_WEBHOOK_SECRET must be set when GITHUB_APP_ID is configured")
	}
	return newGithubApp(appID, secret("GITHUB_APP_PRIVATE_KEY"), webhookSecret)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	if otelHandler, shutdown, err := telemetry.Setup(context.Background(), serviceName); err != nil {
		slog.Error("telemetry setup", "error", err)
	} else {
		defer shutdown(context.Background())
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(slog.NewJSONHandler(os.Stdout, nil), otelHandler)))
	}

	gatekeeperClient = &gk.Client{URL: gatekeeperURL, Service: serviceName, HTTPClient: httpClient}

	if err := migrate(ctx); err != nil {
		slog.Error("migrate", "error", err)
		os.Exit(1)
	}

	if eventsTriggerKey == "" {
		slog.Warn("EVENTS_TRIGGER_KEY not set — /internal/events will reject all emitters")
	}
	if eventsWebhookSecret == "" {
		slog.Warn("EVENTS_WEBHOOK_SECRET not set — /hooks/git will reject all provider webhooks")
	}
	eventsServiceKey = registry.StartKeyRotation(ctx, gatekeeperURL, serviceName, secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	initMetrics()
	startRetryLoop(ctx)

	// The GitHub App is optional: without GITHUB_APP_ID there is no App, so /hooks/github is
	// not registered at all rather than served with nothing to verify deliveries against.
	ghApp, err := initGithubApp()
	if err != nil {
		slog.Error("GitHub App configuration", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /openapi.yaml", handleOpenAPIYAML)

	// Emitters (trusted, HMAC).
	mux.HandleFunc("POST /internal/events", handleInternalEvent)
	// Inbound webhooks (normalized into envelopes).
	mux.HandleFunc("POST /hooks", handleGenericWebhook)
	mux.HandleFunc("POST /hooks/git", handleGitWebhook)
	mux.HandleFunc("POST /hooks/gitea", handleGiteaWebhook)
	if ghApp != nil {
		mux.HandleFunc("POST /hooks/github", handleGitHubWebhook(ghApp))
		slog.Info("GitHub App configured", "app_id", ghApp.appID)
	}

	// Trigger CRUD + the event log (RBAC via conductor → gatekeeper).
	mux.HandleFunc("POST /triggers", handleCreateTrigger)
	mux.HandleFunc("GET /triggers", handleListTriggers)
	mux.HandleFunc("GET /triggers/{id}", handleGetTrigger)
	mux.HandleFunc("PUT /triggers/{id}", handleUpdateTrigger)
	mux.HandleFunc("DELETE /triggers/{id}", handleDeleteTrigger)
	mux.HandleFunc("POST /triggers/test-match", handleTestMatch)
	mux.HandleFunc("GET /events", handleListEvents)
	mux.HandleFunc("GET /events/{id}", handleGetEvent)

	port := envOrDefault("PORT", "8093")
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           otelhttp.NewHandler(mux, serviceName),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	slog.Info("events service listening", "port", port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
