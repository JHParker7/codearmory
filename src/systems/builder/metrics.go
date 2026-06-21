package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var meterServiceConfigured metric.Int64Counter

func initMetrics() {
	meter := otel.Meter("builder")
	meterServiceConfigured, _ = meter.Int64Counter(
		"builder.org_services.configured.total",
		metric.WithDescription("Total org-service enable/disable/configure writes"),
	)
}
