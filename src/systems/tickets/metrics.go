package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterTicketsCreated  metric.Int64Counter
	meterTicketsResolved metric.Int64Counter
	meterCommentsAdded   metric.Int64Counter
)

func initMetrics() {
	meter := otel.Meter("tickets")

	meterTicketsCreated, _ = meter.Int64Counter(
		"tickets.created.total",
		metric.WithDescription("Total tickets created"),
	)
	meterTicketsResolved, _ = meter.Int64Counter(
		"tickets.resolved.total",
		metric.WithDescription("Total tickets moved to resolved or closed status"),
	)
	meterCommentsAdded, _ = meter.Int64Counter(
		"tickets.comments.total",
		metric.WithDescription("Total comments added to tickets"),
	)
}
