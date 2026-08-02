package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var meterReposCreated metric.Int64Counter

// initMetrics registers the service's OpenTelemetry instruments. Failures are
// non-fatal — the service runs without metrics rather than crashing.
func initMetrics() {
	m := otel.Meter(serviceName)
	meterReposCreated, _ = m.Int64Counter(
		"codearmory_git_factory.Repos.created",
		metric.WithDescription("Count of Repos created"),
	)
}
