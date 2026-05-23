package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterGetState    metric.Int64Counter
	meterUpdateState metric.Int64Counter
	meterDeleteState metric.Int64Counter
	meterLockState   metric.Int64Counter
	meterUnlockState metric.Int64Counter
	meterPermChecks  metric.Int64Counter
)

// initMetrics registers the application's metric instruments against the global
// MeterProvider. Must be called after setupOTel (which sets the real provider);
// before that it runs against the default no-op provider so calls are safe no-ops.
func initMetrics() {
	meter := otel.Meter("blueprints")

	meterGetState, _ = meter.Int64Counter(
		"blueprints.state.get.total",
		metric.WithDescription("Total state GET requests"),
	)
	meterUpdateState, _ = meter.Int64Counter(
		"blueprints.state.update.total",
		metric.WithDescription("Total state POST (update) requests"),
	)
	meterDeleteState, _ = meter.Int64Counter(
		"blueprints.state.delete.total",
		metric.WithDescription("Total state DELETE requests"),
	)
	meterLockState, _ = meter.Int64Counter(
		"blueprints.state.lock.total",
		metric.WithDescription("Total state LOCK requests"),
	)
	meterUnlockState, _ = meter.Int64Counter(
		"blueprints.state.unlock.total",
		metric.WithDescription("Total state UNLOCK requests"),
	)
	meterPermChecks, _ = meter.Int64Counter(
		"blueprints.permission_checks.total",
		metric.WithDescription("Total permission check results"),
	)
}
