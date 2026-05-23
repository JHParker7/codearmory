package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	gatekeeperURL = envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
	blueprintsURL = envOrDefault("BLUEPRINTS_URL", "http://localhost:8081")
	httpClient    = &http.Client{Timeout: 10 * time.Second}
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

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
	slog.Info(r.Method+" "+r.URL.Path,
		"status", rw.status,
		"duration", time.Since(start),
		"trace_id", sc.TraceID().String(),
		"span_id", sc.SpanID().String(),
	)
}

func newLogger(h http.Handler) *Logger { return &Logger{h} }

// ── Proxy helpers ─────────────────────────────────────────────────────────────

func newProxy(target string) *httputil.ReverseProxy {
	u, err := url.Parse(target)
	if err != nil {
		slog.Error("invalid proxy target", "url", target, "error", err)
		os.Exit(1)
	}
	return httputil.NewSingleHostReverseProxy(u)
}

// isBlueprints reports whether the request path should be routed to Blueprints.
// User-scoped paths start with /state/; org-scoped paths have "state" as the
// second segment (/{org}/state/{team}/{workspace}).
func isBlueprints(path string) bool {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) >= 1 && parts[0] == "state" {
		return true
	}
	if len(parts) >= 2 && parts[1] == "state" {
		return true
	}
	return false
}

// ── User existence middleware ─────────────────────────────────────────────────

// getUserID decodes the JWT payload (without signature verification) and returns
// the sub claim, which Gatekeeper sets to the user's ID.
func getUserID(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Sub == "" {
		return "", false
	}
	return claims.Sub, true
}

// checkUserExists calls GET /users/{id} on Gatekeeper with the caller's bearer
// token. Returns true only when Gatekeeper responds 200, which means the token
// is valid and the user record is active.
func checkUserExists(ctx context.Context, token, userID string) bool {
	ctx, span := otel.Tracer("conductor").Start(ctx, "checkUserExists")
	defer span.End()
	span.SetAttributes(attribute.String("user.id", userID))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatekeeperURL+"/users/"+userID, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	exists := resp.StatusCode == http.StatusOK
	span.SetAttributes(attribute.Bool("user.exists", exists))
	if exists {
		span.SetStatus(codes.Ok, "")
	} else {
		span.SetStatus(codes.Error, "user not found or token invalid")
	}
	return exists
}

func userMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("conductor").Start(r.Context(), "userMiddleware")
		defer span.End()
		r = r.WithContext(ctx)

		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			span.SetStatus(codes.Error, "no bearer token")
			meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "no_token")))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")

		userID, ok := getUserID(token)
		if !ok {
			span.SetStatus(codes.Error, "malformed token")
			meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "malformed_token")))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if !checkUserExists(ctx, token, userID) {
			span.SetStatus(codes.Error, "user not found")
			meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "user_not_found")))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		span.SetStatus(codes.Ok, "")
		meterAllowed.Add(ctx, 1)
		next.ServeHTTP(w, r)
	})
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	otelHandler, shutdown, err := setupOTel(context.Background())
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(&fanoutHandler{handlers: []slog.Handler{jsonHandler, otelHandler}}))
		defer shutdown(context.Background())
	}
	initMetrics()

	gk := newProxy(gatekeeperURL)
	bp := newProxy(blueprintsURL)

	mux := http.NewServeMux()

	// Public routes — forwarded to Gatekeeper without a user check.
	mux.Handle("POST /signup", gk)
	mux.Handle("POST /login", gk)

	// All other routes go through the user-existence middleware, then are
	// dispatched to Blueprints or Gatekeeper based on the request path.
	mux.Handle("/", userMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isBlueprints(r.URL.Path) {
			bp.ServeHTTP(w, r)
		} else {
			gk.ServeHTTP(w, r)
		}
	})))

	port := envOrDefault("PORT", "8082")

	wrappedMux := otelhttp.NewHandler(newLogger(mux), "conductor",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")
	if certFile != "" && keyFile != "" {
		slog.Info("listening with TLS", "port", port)
		if err := http.ListenAndServeTLS(":"+port, certFile, keyFile, wrappedMux); err != nil {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	} else {
		slog.Info("listening", "port", port)
		if err := http.ListenAndServe(":"+port, wrappedMux); err != nil {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}
}
