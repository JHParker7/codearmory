package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// secret reads ${NAME}_FILE first (k8s volume-mounted secrets) then env NAME,
// matching the control-plane convention.
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

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	logLevel := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		_ = logLevel.UnmarshalText([]byte(v))
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	// Tracing/log-forwarding is opt-in: telemetry.Setup is a no-op unless
	// OTEL_EXPORTER_OTLP_ENDPOINT is set. On a customer cluster the endpoint is
	// normally unset, so the outpost stays stderr-only and never ships telemetry
	// off the cluster — point it at a collector only when running it yourself for
	// development. A missing endpoint is the expected case, hence Debug not Warn.
	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "outpost")
	if err != nil {
		slog.Debug("outpost: OpenTelemetry disabled, logging to stderr only", "reason", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(context.Background())
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	controlPlaneURL := envOrDefault("CONTROL_PLANE_URL", "")
	if controlPlaneURL == "" {
		slog.Error("CONTROL_PLANE_URL is required (the outpost-gateway base URL)")
		os.Exit(1)
	}
	// The gateway connection carries the outpost key and authorizes destructive
	// in-cluster actions, so require https. An operator can opt into http only
	// explicitly (e.g. self-hosted, gateway reached over an in-cluster Service).
	if !strings.HasPrefix(controlPlaneURL, "https://") {
		if envOrDefault("OUTPOST_ALLOW_INSECURE_HTTP", "") == "true" {
			slog.Warn("outpost: CONTROL_PLANE_URL is not https — the outpost key and commands travel in cleartext (OUTPOST_ALLOW_INSECURE_HTTP=true)")
		} else {
			slog.Error("CONTROL_PLANE_URL must be https:// (set OUTPOST_ALLOW_INSECURE_HTTP=true to permit http, e.g. an in-cluster self-hosted gateway)")
			os.Exit(1)
		}
	}
	enabled := splitCSV(envOrDefault("OUTPOST_MODULES", "chaos"))
	if len(enabled) == 0 {
		slog.Error("OUTPOST_MODULES is empty — nothing to do")
		os.Exit(1)
	}

	modules, err := buildModules(enabled)
	if err != nil {
		slog.Error("failed to initialise modules", "error", err)
		os.Exit(1)
	}

	client := newGatewayClient(controlPlaneURL, envOrDefault("OUTPOST_STATE_FILE", "/var/lib/outpost/state.json"))
	if err := enrollWithRetry(ctx, client); err != nil {
		slog.Error("enrollment failed", "error", err)
		os.Exit(1)
	}

	// emit delivers a module event to the gateway with a few retries. Verdict
	// events are also re-derivable from the next informer resync, so a dropped
	// emit is recoverable.
	emit := func(ev Event) {
		emitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := postEventWithRetry(emitCtx, client, ev); err != nil {
			slog.Error("outpost: emit event failed", "integration", ev.Integration, "type", ev.Type, "error", err)
		}
	}

	for _, m := range modules {
		if err := m.Start(ctx, emit); err != nil {
			slog.Error("module failed to start", "module", m.Name(), "error", err)
			os.Exit(1)
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); heartbeatLoop(ctx, client) }()
	go func() { defer wg.Done(); commandLoop(ctx, client, modules) }()

	slog.Info("outpost running", "modules", enabled, "control_plane", controlPlaneURL)
	<-ctx.Done()
	slog.Info("outpost shutting down")
	wg.Wait()
}

func buildModules(enabled []string) ([]Module, error) {
	var needsK8s bool
	for _, name := range enabled {
		if name == "chaos" || name == "argo" || name == "deploy" {
			needsK8s = true
		}
	}
	var dynClient = clientOrNil(needsK8s)
	var modules []Module
	for _, name := range enabled {
		switch name {
		case "chaos":
			modules = append(modules, newChaosModule(dynClient))
		case "argo":
			modules = append(modules, newArgoModule(dynClient))
		case "deploy":
			modules = append(modules, newDeployModule(dynClient))
		default:
			slog.Warn("outpost: ignoring unknown module", "module", name)
		}
	}
	if len(modules) == 0 {
		return nil, errNoModules
	}
	return modules, nil
}

// commandLoop long-polls for commands and dispatches each to its module,
// emitting results and acking. Reconnect/backoff on transport failure.
func commandLoop(ctx context.Context, client *gatewayClient, modules []Module) {
	byName := map[string]Module{}
	for _, m := range modules {
		byName[m.Name()] = m
	}
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		cmds, err := client.pollCommands(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("outpost: poll failed, backing off", "error", err, "backoff", backoff)
			sleep(ctx, backoff)
			backoff = minDur(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if len(cmds) == 0 {
			// The gateway holds the long-poll open ~30s, so an empty result means
			// the hold elapsed. Sleep a short floor before re-polling so a gateway
			// (or proxy) that returns empty immediately can't spin this hot.
			sleep(ctx, emptyPollFloor)
			continue
		}
		slog.Debug("outpost: commands received", "count", len(cmds))
		for _, c := range cmds {
			handleCommand(ctx, client, byName, c)
		}
	}
}

// emptyPollFloor is the minimum delay between command polls that returned empty,
// guarding against a busy loop if the server does not actually long-poll.
const emptyPollFloor = 2 * time.Second

func handleCommand(ctx context.Context, client *gatewayClient, byName map[string]Module, c Command) {
	// One span per command roots the in-cluster work and the downstream event/ack
	// client spans under it, so a command is traceable end-to-end and links to the
	// gateway via propagated context. Inert when OpenTelemetry is not configured.
	ctx, span := otel.Tracer("outpost").Start(ctx, "handle_command")
	span.SetAttributes(
		attribute.String("integration", c.Integration),
		attribute.String("command.type", c.Type),
		attribute.String("command.id", c.ID),
	)
	defer span.End()

	slog.Debug("outpost: handling command", "integration", c.Integration, "type", c.Type, "command_id", c.ID)
	m, ok := byName[c.Integration]
	if !ok {
		slog.Warn("outpost: no module for command", "integration", c.Integration, "command_id", c.ID)
		_ = client.ackCommand(ctx, c.ID) // ack so it isn't re-served forever
		return
	}
	events, err := m.HandleCommand(ctx, c)
	if err != nil {
		span.RecordError(err)
		slog.Error("outpost: command failed", "integration", c.Integration, "type", c.Type, "command_id", c.ID, "error", err)
		// Surface the failure as an event so the control plane can react. Only ack
		// once the failure is reported; if we can't even report it, leave the
		// command for redelivery rather than silently dropping it.
		if emitErr := postEventWithRetry(ctx, client, Event{
			Integration: c.Integration,
			Type:        "command-error",
			Payload:     commandErrorPayload(c, err),
		}); emitErr != nil {
			slog.Error("outpost: reporting command failure failed; leaving command for redelivery", "command_id", c.ID, "error", emitErr)
			return
		}
		if ackErr := client.ackCommand(ctx, c.ID); ackErr != nil {
			slog.Warn("outpost: ack failed", "command_id", c.ID, "error", ackErr)
		}
		return
	}
	// Deliver all result events before acking. If any delivery fails, leave the
	// command unacked so it is re-delivered (at-least-once) — acking here would
	// lose the result with no recovery (e.g. an argo sync-started never recorded).
	for _, ev := range events {
		if emitErr := postEventWithRetry(ctx, client, ev); emitErr != nil {
			slog.Error("outpost: emit command result failed; leaving command for redelivery", "command_id", c.ID, "error", emitErr)
			return
		}
	}
	if err := client.ackCommand(ctx, c.ID); err != nil {
		slog.Warn("outpost: ack failed", "command_id", c.ID, "error", err)
		return
	}
	slog.Debug("outpost: command acked", "command_id", c.ID, "events", len(events))
}

func heartbeatLoop(ctx context.Context, client *gatewayClient) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	_ = client.heartbeat(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := client.heartbeat(ctx); err != nil {
				if ctx.Err() == nil {
					slog.Debug("outpost: heartbeat failed", "error", err)
				}
			} else {
				slog.Debug("outpost: heartbeat ok")
			}
		}
	}
}

func enrollWithRetry(ctx context.Context, client *gatewayClient) error {
	id := secret("OUTPOST_ID")
	key := secret("OUTPOST_KEY")
	token := secret("ENROLLMENT_TOKEN")
	backoff := 2 * time.Second
	var lastErr error
	for i := 0; i < 30; i++ {
		if err := client.ensureEnrolled(ctx, id, key, token); err != nil {
			lastErr = err
			slog.Warn("outpost: enrollment attempt failed, retrying", "error", err, "backoff", backoff)
			if !sleep(ctx, backoff) {
				return ctx.Err()
			}
			backoff = minDur(backoff*2, 30*time.Second)
			continue
		}
		return nil
	}
	return lastErr
}

func postEventWithRetry(ctx context.Context, client *gatewayClient, ev Event) error {
	// Assign the id ONCE, before any retry, so every attempt carries the same id
	// and the gateway's at-least-once dedupe collapses them. Assigning inside
	// postEvent (per attempt) would give each retry a fresh id and deliver the
	// event multiple times when a response is lost.
	if ev.EventID == "" {
		ev.EventID = randID()
	}
	backoff := time.Second
	var lastErr error
	for i := 0; i < 5; i++ {
		if err := client.postEvent(ctx, ev); err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !sleep(ctx, backoff) {
				return ctx.Err()
			}
			backoff = minDur(backoff*2, 15*time.Second)
			continue
		}
		return nil
	}
	return lastErr
}

func commandErrorPayload(c Command, err error) map[string]any {
	p := map[string]any{"error": err.Error(), "command_type": c.Type}
	if eid := payloadString(c.Payload, "experiment_id"); eid != "" {
		p["experiment_id"] = eid
	}
	if app := payloadString(c.Payload, "app_name"); app != "" {
		p["app_name"] = app
	}
	return p
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
