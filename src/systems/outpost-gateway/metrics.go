package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterOutpostsEnrolled metric.Int64Counter
	meterCommandsEnqueued metric.Int64Counter
	meterEventsIngested   metric.Int64Counter
	meterEventsDelivered  metric.Int64Counter
)

func initMetrics() {
	meter := otel.Meter("outpost-gateway")
	meterOutpostsEnrolled, _ = meter.Int64Counter("outpost.enrolled.total",
		metric.WithDescription("Total outposts enrolled"))
	meterCommandsEnqueued, _ = meter.Int64Counter("outpost.commands.enqueued.total",
		metric.WithDescription("Total commands enqueued for outposts"))
	meterEventsIngested, _ = meter.Int64Counter("outpost.events.ingested.total",
		metric.WithDescription("Total events ingested from outposts"))
	meterEventsDelivered, _ = meter.Int64Counter("outpost.events.delivered.total",
		metric.WithDescription("Total events delivered to consumer services"))
}
