package main

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var meterServiceConfigured metric.Int64Counter

// meterRolloutFailed counts reconcile passes that found a managed workload wedged
// (pods past the deadline in ImagePullBackOff or unschedulable). It is the alertable
// signal for a failure that is otherwise entirely silent — the apiserver accepted the
// spec, so nothing else reports anything wrong.
var meterRolloutFailed metric.Int64Counter

func initMetrics() {
	meter := otel.Meter("builder")
	meterServiceConfigured, _ = meter.Int64Counter(
		"builder.org_services.configured.total",
		metric.WithDescription("Total org-service enable/disable/configure writes"),
	)
	meterRolloutFailed, _ = meter.Int64Counter(
		"builder.rollout.failed.total",
		metric.WithDescription("Reconcile observations of a managed workload wedged past the rollout deadline"),
	)
}

// recordRolloutFailure emits the wedged-rollout metric. Nil-safe: initMetrics runs in
// main, and unit tests construct reconcilers without it.
func recordRolloutFailure(ctx context.Context, service, reason string) {
	if meterRolloutFailed == nil {
		return
	}
	meterRolloutFailed.Add(ctx, 1, metric.WithAttributes(
		attribute.String("service", service),
		attribute.String("reason", reason),
	))
}
