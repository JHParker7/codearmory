package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	gk "github.com/code-armory-app/codearmory_sdk/gatekeeper"
	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// gatekeeperClient verifies caller permissions on the trigger/event CRUD endpoints.
var gatekeeperClient *gk.Client

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
	registry.StartKeyRotation(ctx, gatekeeperURL, serviceName, secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	startRetryLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// Emitters (trusted, HMAC).
	mux.HandleFunc("POST /internal/events", handleInternalEvent)
	// External git webhooks (normalized).
	mux.HandleFunc("POST /hooks/git", handleGitWebhook)

	// Trigger CRUD + the event log (RBAC via conductor → gatekeeper).
	mux.HandleFunc("POST /triggers", handleCreateTrigger)
	mux.HandleFunc("GET /triggers", handleListTriggers)
	mux.HandleFunc("GET /triggers/{id}", handleGetTrigger)
	mux.HandleFunc("PUT /triggers/{id}", handleUpdateTrigger)
	mux.HandleFunc("DELETE /triggers/{id}", handleDeleteTrigger)
	mux.HandleFunc("POST /triggers/test-match", handleTestMatch)
	mux.HandleFunc("GET /events", handleListEvents)

	port := envOrDefault("PORT", "8087")
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
