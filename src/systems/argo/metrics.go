package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterSyncsTriggered metric.Int64Counter
	meterSyncsResolved  metric.Int64Counter
)

func initMetrics() {
	meter := otel.Meter("argo")
	meterSyncsTriggered, _ = meter.Int64Counter("argo.syncs.triggered.total",
		metric.WithDescription("Total Argo app syncs triggered"))
	meterSyncsResolved, _ = meter.Int64Counter("argo.syncs.resolved.total",
		metric.WithDescription("Total Argo app syncs that reached a terminal state"))
}
