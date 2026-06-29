package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var meterCredentialsMinted metric.Int64Counter

// initMetrics registers the service's OpenTelemetry instruments. Failures are
// non-fatal — the service runs without metrics rather than crashing.
func initMetrics() {
	m := otel.Meter("git")
	meterCredentialsMinted, _ = m.Int64Counter(
		"git.credentials.minted",
		metric.WithDescription("Count of git credentials minted, by backend type"),
	)
}
