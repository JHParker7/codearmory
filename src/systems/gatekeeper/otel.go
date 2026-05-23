package main

import (
	"context"
	"log/slog"
	"os"

	otelslog "go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
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

// fanoutHandler fans slog records out to multiple handlers.
type fanoutHandler struct {
	handlers []slog.Handler
}

func (f *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (f *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range f.handlers {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r)
		}
	}
	return nil
}

func (f *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: hs}
}

func (f *fanoutHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &fanoutHandler{handlers: hs}
}

// setupOTel initialises OTLP exporters for both logs and traces, sets the
// global TracerProvider and TextMapPropagator, and returns an slog.Handler
// that forwards records to the collector plus a combined shutdown function.
// Endpoints are configured via OTEL_EXPORTER_OTLP_ENDPOINT (or the signal-
// specific variants OTEL_EXPORTER_OTLP_LOGS_ENDPOINT /
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT).
func setupOTel(ctx context.Context) (slog.Handler, func(context.Context) error, error) {
	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "gatekeeper"
	}

	res, _ := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", serviceName)),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)

	// Logs
	logExp, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	logProvider := otellog.NewLoggerProvider(
		otellog.WithProcessor(otellog.NewBatchProcessor(logExp)),
		otellog.WithResource(res),
	)

	// Traces
	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(traceProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Metrics
	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	metricProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(metricProvider)

	shutdown := func(ctx context.Context) error {
		_ = logProvider.Shutdown(ctx)
		_ = metricProvider.Shutdown(ctx)
		return traceProvider.Shutdown(ctx)
	}

	handler := otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(logProvider))
	return handler, shutdown, nil
}
