// Package telemetry provides shared OpenTelemetry setup for codearmory services.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	otelslog "go.opentelemetry.io/contrib/bridges/otelslog"
	gotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	otellog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// FanoutHandler fans slog records out to multiple handlers. Use NewFanoutHandler
// to construct one — the handler slice is intentionally not exported.
type FanoutHandler struct {
	handlers []slog.Handler
}

// NewFanoutHandler returns a handler that forwards each record to all provided handlers.
func NewFanoutHandler(handlers ...slog.Handler) *FanoutHandler {
	return &FanoutHandler{handlers: handlers}
}

func (f *FanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (f *FanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range f.handlers {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r)
		}
	}
	return nil
}

func (f *FanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &FanoutHandler{handlers: hs}
}

func (f *FanoutHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &FanoutHandler{handlers: hs}
}

// Setup initialises OTLP exporters for logs, traces, and metrics, sets the
// global TracerProvider and MeterProvider, and returns an slog.Handler that
// forwards records to the collector.
//
// defaultServiceName is used when OTEL_SERVICE_NAME is not set in the environment.
// Returns a non-nil error (and nil handler/shutdown) if OTEL_EXPORTER_OTLP_ENDPOINT
// is unset — callers should fall back to stderr-only logging in that case.
func Setup(ctx context.Context, defaultServiceName string) (slog.Handler, func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return nil, nil, fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT not set")
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = defaultServiceName
	}

	res, _ := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", serviceName)),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)

	logExp, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	logProvider := otellog.NewLoggerProvider(
		otellog.WithProcessor(otellog.NewBatchProcessor(logExp)),
		otellog.WithResource(res),
	)

	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	gotel.SetTracerProvider(traceProvider)
	gotel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	metricProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	gotel.SetMeterProvider(metricProvider)

	shutdown := func(ctx context.Context) error {
		_ = logProvider.Shutdown(ctx)
		_ = metricProvider.Shutdown(ctx)
		return traceProvider.Shutdown(ctx)
	}

	handler := otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(logProvider))
	return handler, shutdown, nil
}
