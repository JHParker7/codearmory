package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterReposCreated metric.Int64Counter
	meterPRsCreated   metric.Int64Counter
	meterPRsMerged    metric.Int64Counter
)

func initMetrics() {
	meter := otel.Meter("gitea")

	meterReposCreated, _ = meter.Int64Counter(
		"gitea.repos.created.total",
		metric.WithDescription("Total repositories created via the gitea integration"),
	)
	meterPRsCreated, _ = meter.Int64Counter(
		"gitea.pulls.created.total",
		metric.WithDescription("Total pull requests created via the gitea integration"),
	)
	meterPRsMerged, _ = meter.Int64Counter(
		"gitea.pulls.merged.total",
		metric.WithDescription("Total pull requests merged via the gitea integration"),
	)
}
