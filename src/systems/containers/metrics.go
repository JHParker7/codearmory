package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterManifestsDeleted metric.Int64Counter
)

func initMetrics() {
	meter := otel.Meter("containers")

	meterManifestsDeleted, _ = meter.Int64Counter(
		"containers.manifests.deleted.total",
		metric.WithDescription("Total image manifests deleted via the containers integration"),
	)
}
