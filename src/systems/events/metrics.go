package main

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Ingest/dispatch counters. They carry over from the former hooks service so the same
// questions stay answerable, renamed onto the events namespace.
var (
	meterEventsReceived  metric.Int64Counter
	meterTriggersMatched metric.Int64Counter
	meterActionsRun      metric.Int64Counter
)

// initMetrics builds the counters. A failure here is not fatal — the service runs without
// metrics rather than refusing to start — but it is logged, since a silently absent counter
// looks identical on a dashboard to "nothing happened".
func initMetrics() {
	m := otel.Meter(serviceName)
	var err error
	if meterEventsReceived, err = m.Int64Counter("events.received.total",
		metric.WithDescription("Events ingested, by source")); err != nil {
		slog.Warn("metrics: events.received.total", "error", err)
	}
	if meterTriggersMatched, err = m.Int64Counter("events.triggers.matched.total",
		metric.WithDescription("Trigger filters that matched an event")); err != nil {
		slog.Warn("metrics: events.triggers.matched.total", "error", err)
	}
	if meterActionsRun, err = m.Int64Counter("events.actions.run.total",
		metric.WithDescription("Actions executed, by kind and outcome")); err != nil {
		slog.Warn("metrics: events.actions.run.total", "error", err)
	}
}

// The count* helpers exist so call sites never touch a possibly-nil counter: when initMetrics
// failed, recording is a no-op rather than a panic on the request path.

func countEvent(ctx context.Context, source string) {
	if meterEventsReceived != nil {
		meterEventsReceived.Add(ctx, 1, metric.WithAttributes(attribute.String("source", source)))
	}
}

func countTriggerMatched(ctx context.Context, eventType string) {
	if meterTriggersMatched != nil {
		meterTriggersMatched.Add(ctx, 1, metric.WithAttributes(attribute.String("event.type", eventType)))
	}
}

func countActionRun(ctx context.Context, kind string, ok bool) {
	if meterActionsRun != nil {
		meterActionsRun.Add(ctx, 1, metric.WithAttributes(
			attribute.String("kind", kind),
			attribute.Bool("ok", ok),
		))
	}
}
