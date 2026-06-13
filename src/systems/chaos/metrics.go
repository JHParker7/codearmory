package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterExperimentsCreated  metric.Int64Counter
	meterExperimentsResolved metric.Int64Counter
)

func initMetrics() {
	meter := otel.Meter("chaos")

	meterExperimentsCreated, _ = meter.Int64Counter(
		"chaos.experiments.created.total",
		metric.WithDescription("Total chaos experiments created"),
	)
	meterExperimentsResolved, _ = meter.Int64Counter(
		"chaos.experiments.resolved.total",
		metric.WithDescription("Total chaos experiments that reached a terminal verdict"),
	)
}

func attrStatus(status string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("status", status))
}
