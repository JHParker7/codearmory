package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterRunsTriggered  metric.Int64Counter
	meterRunsCompleted  metric.Int64Counter
	meterStepsCompleted metric.Int64Counter
)

func initMetrics() {
	meter := otel.Meter("workflows")

	meterRunsTriggered, _ = meter.Int64Counter(
		"workflows.runs.triggered.total",
		metric.WithDescription("Total workflow runs triggered"),
	)
	meterRunsCompleted, _ = meter.Int64Counter(
		"workflows.runs.completed.total",
		metric.WithDescription("Total workflow runs completed (any terminal status)"),
	)
	meterStepsCompleted, _ = meter.Int64Counter(
		"workflows.steps.completed.total",
		metric.WithDescription("Total workflow step runs completed"),
	)
}
