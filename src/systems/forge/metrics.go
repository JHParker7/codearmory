package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterSubmit   metric.Int64Counter
	meterComplete metric.Int64Counter
	meterCancel   metric.Int64Counter
)

func initMetrics() {
	meter := otel.Meter("forge")

	meterSubmit, _ = meter.Int64Counter(
		"forge.executions.submitted.total",
		metric.WithDescription("Total executions submitted"),
	)
	meterComplete, _ = meter.Int64Counter(
		"forge.executions.completed.total",
		metric.WithDescription("Total executions completed (any terminal status)"),
	)
	meterCancel, _ = meter.Int64Counter(
		"forge.executions.cancelled.total",
		metric.WithDescription("Total executions cancelled by the user"),
	)
}
