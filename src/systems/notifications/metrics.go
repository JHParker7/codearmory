package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterChannelsCreated metric.Int64Counter
	meterEnqueued        metric.Int64Counter
	meterSent            metric.Int64Counter
	meterFailed          metric.Int64Counter
)

func initMetrics() {
	meter := otel.Meter("notifications")

	meterChannelsCreated, _ = meter.Int64Counter(
		"notifications.channels.created.total",
		metric.WithDescription("Total notification channels created"),
	)
	meterEnqueued, _ = meter.Int64Counter(
		"notifications.enqueued.total",
		metric.WithDescription("Total notifications enqueued for delivery"),
	)
	meterSent, _ = meter.Int64Counter(
		"notifications.sent.total",
		metric.WithDescription("Total notifications delivered successfully"),
	)
	meterFailed, _ = meter.Int64Counter(
		"notifications.failed.total",
		metric.WithDescription("Total notifications that exhausted all delivery attempts"),
	)
}
