package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

// httpClient is the instrumented client used for every upstream call to
// conductor. It has no overall timeout — long-lived responses (streams, long
// polls) are bounded instead by the per-request context, so a client disconnect
// cancels the upstream call.
var httpClient *http.Client

// initHTTPClient builds an OTEL-instrumented HTTP client. When TLS_CA_FILE is set
// its certificate authority is trusted for outbound TLS to conductor.
func initHTTPClient() *http.Client {
	tlsCfg := &tls.Config{}
	if caFile := os.Getenv("TLS_CA_FILE"); caFile != "" {
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			slog.Error("failed to read TLS_CA_FILE", "error", err)
			os.Exit(1)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caCert)
		tlsCfg.RootCAs = pool
	}
	return &http.Client{Transport: otelhttp.NewTransport(&http.Transport{TLSClientConfig: tlsCfg})}
}

// apiHandler dispatches everything mounted under /api: the normalized /state/*
// routes, the /:svc/ui/* mini-portal streaming, and — for everything else — a
// verbatim passthrough to conductor. Ordering mirrors the Express router: state
// and ui win over the JSON catch-all.
func apiHandler(cache *stateCache) http.HandlerFunc {
	stateH := handleState(cache)
	return func(w http.ResponseWriter, r *http.Request) {
		apiPath := strings.TrimPrefix(r.URL.Path, "/api")
		switch {
		case strings.HasPrefix(apiPath, "/state/"):
			stateH(w, r)
		case isServiceUIPath(apiPath):
			handleServiceUI(w, r)
		default:
			// RequestURI (not Path) so the query string and raw encoding survive —
			// conductor needs filter/pagination params. The /api prefix is stripped.
			target := strings.TrimPrefix(r.URL.RequestURI(), "/api")
			if err := proxyToUpstream(w, r, target); err != nil {
				writeJSONError(w, http.StatusBadGateway, "upstream unavailable")
			}
		}
	}
}

// statusResponseWriter captures the status code (and forwards Flush) so the
// request logger can record it and streaming responses still flush.
type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *statusResponseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *statusResponseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// requestLogger logs one structured line per request, correlated with the OTEL
// trace/span, demoting health-probe noise to debug.
type requestLogger struct{ handler http.Handler }

func (l *requestLogger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	case "/healthz", "/readyz", "/livez":
		if rw.status >= 500 {
			slog.WarnContext(r.Context(), "http request", attrs...)
		} else {
			slog.DebugContext(r.Context(), "http request", attrs...)
		}
	default:
		slog.InfoContext(r.Context(), "http request", attrs...)
	}
}

func main() {
	logLevel := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		_ = logLevel.UnmarshalText([]byte(v))
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "portal-bff")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(ctx)
	}

	initMetrics()
	httpClient = initHTTPClient()
	cache := newStateCache(stateCacheTTL)

	limiter := newIPRateLimiter(100, 15*time.Minute)
	go func() {
		t := time.NewTicker(15 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				limiter.cleanup(30 * time.Minute)
			}
		}
	}()

	mux := telemetry.NewMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/", apiHandler(cache))

	// Serve the SPA on the same port as the API. In production the build lands in
	// publicDir and is served statically, with /api winning over the SPA catch-all.
	// In dev publicDir is absent — Vite serves the SPA on its own port and proxies
	// /api here — so this server runs API-only.
	if _, statErr := os.Stat(publicDir); statErr == nil {
		mux.Handle("/", newSPAHandler(publicDir, limiter))
		slog.Info("serving SPA statically", "dir", publicDir)
	} else {
		slog.Warn("SPA directory not found; serving API only (dev: run Vite separately and proxy /api here)", "dir", publicDir)
	}

	wrapped := otelhttp.NewHandler(&requestLogger{mux}, "portal-bff",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	srv := &http.Server{
		Addr:              ":" + listenPort,
		Handler:           wrapped,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		slog.Info("portal-bff started", "port", listenPort, "conductor", conductorURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
}
