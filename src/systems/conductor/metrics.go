package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterAllowed  metric.Int64Counter
	meterRejected metric.Int64Counter
)

func initMetrics() {
	meter := otel.Meter("conductor")

	meterAllowed, _ = meter.Int64Counter(
		"conductor.requests.allowed.total",
		metric.WithDescription("Requests that passed the user-existence check"),
	)
	meterRejected, _ = meter.Int64Counter(
		"conductor.requests.rejected.total",
		metric.WithDescription("Requests rejected by the user-existence check"),
	)
}
