package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterHooksReceived metric.Int64Counter // total webhooks received
	meterRulesMatched  metric.Int64Counter // rules matched across all events
	meterRunsTriggered metric.Int64Counter // successful workflow triggers
)

func initMetrics() {
	meter := otel.Meter("hooks")

	meterHooksReceived, _ = meter.Int64Counter(
		"hooks.received.total",
		metric.WithDescription("Total webhooks received"),
	)
	meterRulesMatched, _ = meter.Int64Counter(
		"hooks.rules.matched.total",
		metric.WithDescription("Total rules matched across all webhook events"),
	)
	meterRunsTriggered, _ = meter.Int64Counter(
		"hooks.runs.triggered.total",
		metric.WithDescription("Total successful workflow run triggers from hook events"),
	)
}
