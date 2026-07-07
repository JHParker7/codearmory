package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	stateCacheHits   metric.Int64Counter
	stateCacheMisses metric.Int64Counter
)

// initMetrics wires the workspace-state cache counters to the global OTEL meter.
// Call after telemetry.Setup so instruments bind to the OTLP exporter; before
// setup (or with no exporter) otel.Meter returns a no-op meter, so this is safe
// to call from tests too. HTTP server request durations are recorded
// automatically by the otelhttp handler and need no instrument here.
func initMetrics() {
	meter := otel.Meter("portal-bff")
	stateCacheHits, _ = meter.Int64Counter(
		"portal_bff.state_cache.hits.total",
		metric.WithDescription("Workspace state cache hits"),
	)
	stateCacheMisses, _ = meter.Int64Counter(
		"portal_bff.state_cache.misses.total",
		metric.WithDescription("Workspace state cache misses"),
	)
}
