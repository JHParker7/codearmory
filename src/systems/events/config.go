package main

import (
	"net/http"
	"os"
	"time"
)

// Service identity and the endpoints/keys the events service depends on. Values come from
// the environment (with the secret() indirection so k8s file-mounted secrets work), matching
// every other codearmory service.
const serviceName = "events"

var (
	gatekeeperURL = envOrDefault("GATEKEEPER_URL", "http://localhost:8081")
	workflowsURL  = envOrDefault("WORKFLOWS_URL", "http://localhost:8085")
	ticketsURL    = envOrDefault("TICKETS_URL", "http://localhost:8086")

	// eventsTriggerKey is the shared HMAC key that authenticates trusted emitters posting to
	// /internal/events and that events itself signs internal calls (e.g. dispatch to
	// workflows) with. Formerly HOOKS_TRIGGER_KEY.
	eventsTriggerKey = secret("EVENTS_TRIGGER_KEY")

	// eventsWebhookSecret is the secret configured on the git provider's webhook. It keys the
	// provider HMAC verified on POST /hooks/git, the one endpoint reachable from the public
	// internet. Unset closes that endpoint: an unsigned webhook is never trusted.
	eventsWebhookSecret = secret("EVENTS_WEBHOOK_SECRET")

	// eventsServiceKey returns the current rotated gatekeeper service key. It is wired in
	// main() from registry.StartKeyRotation's accessor so secret lookups authenticate with
	// the live key rather than the bootstrap value.
	eventsServiceKey func() string

	httpClient = &http.Client{Timeout: 20 * time.Second}
)

// envOrDefault returns the env var or a fallback.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// secret reads NAME_FILE first (k8s volume-mounted secret) then the NAME env var.
func secret(name string) string {
	if path := os.Getenv(name + "_FILE"); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return string(b)
		}
	}
	return os.Getenv(name)
}

// secretOrDefault is secret() with a fallback.
func secretOrDefault(name, def string) string {
	if v := secret(name); v != "" {
		return v
	}
	return def
}
