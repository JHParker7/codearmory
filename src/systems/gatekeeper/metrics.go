package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	meterSignups          metric.Int64Counter
	meterLogins           metric.Int64Counter
	meterPermissionChecks metric.Int64Counter
	meterAuthMiddleware   metric.Int64Counter
)

// initMetrics registers the application's metric instruments against the global
// MeterProvider. Must be called after setupOTel (which sets the real provider);
// in tests it runs against the default no-op provider so calls are safe no-ops.
func initMetrics() {
	meter := otel.Meter("gatekeeper")

	meterSignups, _ = meter.Int64Counter(
		"gatekeeper.signups.total",
		metric.WithDescription("Total signup attempts"),
	)
	meterLogins, _ = meter.Int64Counter(
		"gatekeeper.logins.total",
		metric.WithDescription("Total login attempts"),
	)
	meterPermissionChecks, _ = meter.Int64Counter(
		"gatekeeper.permission_checks.total",
		metric.WithDescription("Total permission check results"),
	)
	meterAuthMiddleware, _ = meter.Int64Counter(
		"gatekeeper.auth.total",
		metric.WithDescription("Total requests processed by auth middleware"),
	)
}
