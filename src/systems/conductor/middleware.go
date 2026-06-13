package main

import (
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// ── Logger middleware ─────────────────────────────────────────────────────────

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *statusResponseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

type Logger struct{ handler http.Handler }

func (l *Logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	l.handler.ServeHTTP(rw, r)
	sc := trace.SpanFromContext(r.Context()).SpanContext()
	attrs := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"status", rw.status,
		"duration", time.Since(start),
		"trace_id", sc.TraceID().String(),
		"span_id", sc.SpanID().String(),
	}
	switch r.URL.Path {
	case "/healthz", "/health", "/system_health", "/readyz", "/livez":
		// Background liveness/readiness probes are noise at info; log them at
		// debug, escalating to warn only when the probe itself fails.
		if rw.status >= 500 {
			slog.WarnContext(r.Context(), "http request", attrs...)
		} else {
			slog.DebugContext(r.Context(), "http request", attrs...)
		}
	default:
		slog.InfoContext(r.Context(), "http request", attrs...)
	}
}

func newLogger(h http.Handler) *Logger { return &Logger{h} }
