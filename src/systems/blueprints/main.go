package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/registry"
	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	gatekeeperURL = envOrDefault("GATEKEEPER_URL", "http://localhost:8081")
	httpClient    = &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   10 * time.Second,
	}
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// secret reads the named environment variable. If <NAME>_FILE is set, the
// value is read from that file instead (trailing whitespace stripped), so that
// Docker Compose secrets mounts and Kubernetes Secret volumes work without any
// code changes. The file path takes precedence over the plain env var. If the
// file is specified but unreadable the process exits immediately.
func secret(name string) string {
	if path := os.Getenv(name + "_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("cannot read secret file", "var", name+"_FILE", "path", path, "error", err)
			os.Exit(1)
		}
		return strings.TrimSpace(string(data))
	}
	return os.Getenv(name)
}

func secretOrDefault(name, def string) string {
	if v := secret(name); v != "" {
		return v
	}
	return def
}

const maxBodyBytes = 64 * 1024 * 1024 // 64 MB — generous upper bound for Terraform state

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

// ── Auth ──────────────────────────────────────────────────────────────────────

func loginToGatekeeper(ctx context.Context, email, password string) (string, bool) {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "loginToGatekeeper")
	defer span.End()

	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/login", bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("failed to build gatekeeper login request", "error", err)
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("gatekeeper login request failed", "error", err)
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		span.SetStatus(codes.Error, "login rejected")
		slog.Warn("gatekeeper login rejected", "status", resp.StatusCode)
		return "", false
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", false
	}

	span.SetStatus(codes.Ok, "")
	return result.Token, true
}

func extractToken(ctx context.Context, r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if tok, ok := strings.CutPrefix(h, "Bearer "); ok {
		return tok, true
	}
	if encoded, ok := strings.CutPrefix(h, "Basic "); ok {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", false
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 {
			return "", false
		}
		return loginToGatekeeper(ctx, parts[0], parts[1])
	}
	return "", false
}

func checkPermissions(ctx context.Context, token, resource, action string) bool {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "checkPermissions")
	defer span.End()
	span.SetAttributes(
		attribute.String("permission.resource", resource),
		attribute.String("permission.action", action),
	)

	body, _ := json.Marshal(map[string]string{
		"service":  "blueprints",
		"resource": resource,
		"action":   action,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("failed to build gatekeeper check_permissions request", "error", err)
		meterPermChecks.Add(ctx, 1, metric.WithAttributes(attribute.Bool("authorized", false)))
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("gatekeeper check_permissions failed", "error", err)
		meterPermChecks.Add(ctx, 1, metric.WithAttributes(attribute.Bool("authorized", false)))
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		span.SetStatus(codes.Error, "denied")
		slog.Warn("permission denied", "resource", resource, "action", action, "status", resp.StatusCode)
		meterPermChecks.Add(ctx, 1, metric.WithAttributes(attribute.Bool("authorized", false)))
		return false
	}

	var result struct {
		Authorized bool `json:"authorized"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return false
	}

	span.SetAttributes(attribute.Bool("permission.authorized", result.Authorized))
	span.SetStatus(codes.Ok, "")
	meterPermChecks.Add(ctx, 1, metric.WithAttributes(attribute.Bool("authorized", result.Authorized)))
	if !result.Authorized {
		slog.Warn("permission denied", "resource", resource, "action", action)
	}
	return result.Authorized
}

// requireAuth extracts the token and writes 401 on failure. Returns (token, true) on success.
func requireAuth(ctx context.Context, w http.ResponseWriter, r *http.Request) (string, bool) {
	token, ok := extractToken(ctx, r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Basic")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
	return token, ok
}

// ── State handlers ────────────────────────────────────────────────────────────

func handleGetState(w http.ResponseWriter, r *http.Request, workspaceKey string) {
	ctx, span := otel.Tracer("blueprints").Start(r.Context(), "getState")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", workspaceKey))

	if !requireWorkspaceAuth(ctx, w, r, workspaceKey, "getState") {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}

	if cached, ok := stateCacheGet(ctx, workspaceKey); ok {
		slog.Info("state cache hit", "workspace", workspaceKey)
		// The cache stores ciphertext so Redis never holds plaintext state.
		if plaintext, err := decrypt(cached); err == nil {
			span.SetStatus(codes.Ok, "")
			meterGetState.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "found")))
			w.Header().Set("Content-Type", "application/json")
			w.Write(plaintext)
			return
		}
		// Decrypt failure means a stale or corrupt cache entry; evict and re-read.
		stateCacheDel(ctx, workspaceKey)
		slog.Warn("state cache: decrypt failed, evicting", "workspace", workspaceKey)
	}

	var state State
	result := connect().WithContext(ctx).Where("workspace = ?", workspaceKey).First(&state)
	if result.RowsAffected == 0 {
		slog.Info("state not found", "workspace", workspaceKey)
		span.SetStatus(codes.Ok, "")
		meterGetState.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "not_found")))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, result.Error.Error())
		slog.Error("state read failed", "workspace", workspaceKey, "error", result.Error)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data := state.Data

	plaintext, err := decrypt(data)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("state decrypt failed", "workspace", workspaceKey, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	stateCacheSet(ctx, workspaceKey, data)
	slog.Info("state retrieved", "workspace", workspaceKey)
	span.SetStatus(codes.Ok, "")
	meterGetState.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "found")))
	w.Header().Set("Content-Type", "application/json")
	w.Write(plaintext)
}

func handleUpdateState(w http.ResponseWriter, r *http.Request, workspaceKey string) {
	ctx, span := otel.Tracer("blueprints").Start(r.Context(), "updateState")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", workspaceKey))

	if !requireWorkspaceAuth(ctx, w, r, workspaceKey, "updateState") {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}

	plaintext, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body, err := encrypt(plaintext)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("state encrypt failed", "workspace", workspaceKey, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	lockID := r.URL.Query().Get("ID")

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		span.RecordError(tx.Error)
		span.SetStatus(codes.Error, tx.Error.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	var existingLock string
	// FOR UPDATE serializes concurrent requests on the same workspace row, preventing
	// TOCTOU races between the lock check and the subsequent state write.
	lockResult := tx.Raw("SELECT lock_data FROM locks WHERE workspace = ? FOR UPDATE", workspaceKey).Scan(&existingLock)
	if lockResult.Error != nil {
		span.RecordError(lockResult.Error)
		span.SetStatus(codes.Error, lockResult.Error.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if lockResult.RowsAffected > 0 {
		// Workspace is locked — caller must supply the matching lock ID.
		if lockID == "" {
			slog.Warn("state update rejected: workspace is locked", "workspace", workspaceKey)
			span.SetStatus(codes.Error, "locked")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(existingLock)) //nolint:errcheck
			return
		}
		var lockObj map[string]any
		if err := json.Unmarshal([]byte(existingLock), &lockObj); err != nil {
			slog.Error("state update rejected: corrupt lock data", "workspace", workspaceKey, "error", err)
			span.SetStatus(codes.Error, "corrupt lock data")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if id, _ := lockObj["ID"].(string); subtle.ConstantTimeCompare([]byte(id), []byte(lockID)) != 1 {
			slog.Warn("state update rejected: lock id mismatch", "workspace", workspaceKey)
			span.SetStatus(codes.Error, "lock id mismatch")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(existingLock)) //nolint:errcheck
			return
		}
	}

	if err := tx.Exec(
		`INSERT INTO states (workspace, data) VALUES (?, ?) ON CONFLICT (workspace) DO UPDATE SET data = ?, updated_at = now()`,
		workspaceKey, body, body,
	).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("state update failed", "workspace", workspaceKey, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit().Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	stateCacheDel(ctx, workspaceKey)
	slog.Info("state updated", "workspace", workspaceKey)
	span.SetStatus(codes.Ok, "")
	meterUpdateState.Add(ctx, 1)
	w.WriteHeader(http.StatusOK)
}

func handleDeleteState(w http.ResponseWriter, r *http.Request, workspaceKey string) {
	ctx, span := otel.Tracer("blueprints").Start(r.Context(), "deleteState")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", workspaceKey))

	if !requireWorkspaceAuth(ctx, w, r, workspaceKey, "deleteState") {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}

	if err := connect().WithContext(ctx).Exec("DELETE FROM states WHERE workspace = ?", workspaceKey).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("state delete failed", "workspace", workspaceKey, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	stateCacheDel(ctx, workspaceKey)
	slog.Info("state deleted", "workspace", workspaceKey)
	span.SetStatus(codes.Ok, "")
	meterDeleteState.Add(ctx, 1)
	w.WriteHeader(http.StatusOK)
}

func handleLockState(w http.ResponseWriter, r *http.Request, workspaceKey string) {
	ctx, span := otel.Tracer("blueprints").Start(r.Context(), "lockState")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", workspaceKey))

	if !requireWorkspaceAuth(ctx, w, r, workspaceKey, "lockState") {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !json.Valid(body) {
		span.SetStatus(codes.Error, "invalid lock data")
		http.Error(w, "bad request: lock data must be valid JSON", http.StatusBadRequest)
		return
	}

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		span.RecordError(tx.Error)
		span.SetStatus(codes.Error, tx.Error.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	var existingLock string
	// FOR UPDATE serializes concurrent lock acquisitions on the same workspace, so two
	// callers racing to lock the same workspace can't both see it as unlocked.
	lockResult := tx.Raw("SELECT lock_data FROM locks WHERE workspace = ? FOR UPDATE", workspaceKey).Scan(&existingLock)
	if lockResult.Error != nil {
		span.RecordError(lockResult.Error)
		span.SetStatus(codes.Error, lockResult.Error.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if lockResult.RowsAffected > 0 {
		slog.Warn("lock conflict", "workspace", workspaceKey)
		span.SetStatus(codes.Error, "lock conflict")
		meterLockState.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "conflict")))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		w.Write([]byte(existingLock)) //nolint:errcheck
		return
	}

	if err := tx.Exec("INSERT INTO locks (workspace, lock_data) VALUES (?, ?)", workspaceKey, string(body)).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("lock insert failed", "workspace", workspaceKey, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit().Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("state locked", "workspace", workspaceKey)
	span.SetStatus(codes.Ok, "")
	meterLockState.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "ok")))
	w.WriteHeader(http.StatusOK)
}

func handleUnlockState(w http.ResponseWriter, r *http.Request, workspaceKey string) {
	ctx, span := otel.Tracer("blueprints").Start(r.Context(), "unlockState")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", workspaceKey))

	if !requireWorkspaceAuth(ctx, w, r, workspaceKey, "unlockState") {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		span.RecordError(tx.Error)
		span.SetStatus(codes.Error, tx.Error.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	var existingLock string
	// FOR UPDATE serializes concurrent unlock attempts on the same workspace row.
	// RowsAffected == 0 (workspace already unlocked) falls through to a no-op commit,
	// making unlock idempotent — Terraform expects a 200 even when the lock is already gone.
	lockResult := tx.Raw("SELECT lock_data FROM locks WHERE workspace = ? FOR UPDATE", workspaceKey).Scan(&existingLock)
	if lockResult.Error != nil {
		span.RecordError(lockResult.Error)
		span.SetStatus(codes.Error, lockResult.Error.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if lockResult.RowsAffected > 0 {
		// Workspace is locked — caller must supply the matching lock ID.
		var reqID string
		if len(body) > 0 {
			var reqData map[string]any
			if json.Unmarshal(body, &reqData) == nil {
				reqID, _ = reqData["ID"].(string)
			}
		}
		var lockData map[string]any
		if err := json.Unmarshal([]byte(existingLock), &lockData); err != nil {
			slog.Error("unlock rejected: corrupt lock data", "workspace", workspaceKey, "error", err)
			span.SetStatus(codes.Error, "corrupt lock data")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		storedID, _ := lockData["ID"].(string)
		if reqID == "" || reqID != storedID {
			slog.Warn("unlock rejected: lock id mismatch", "workspace", workspaceKey)
			span.SetStatus(codes.Error, "lock id mismatch")
			http.Error(w, "lock ID mismatch", http.StatusConflict)
			return
		}

		if err := tx.Exec("DELETE FROM locks WHERE workspace = ?", workspaceKey).Error; err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.Error("lock delete failed", "workspace", workspaceKey, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit().Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("state unlocked", "workspace", workspaceKey)
	span.SetStatus(codes.Ok, "")
	meterUnlockState.Add(ctx, 1)
	w.WriteHeader(http.StatusOK)
}

// ── Route helpers ─────────────────────────────────────────────────────────────

func userKey(r *http.Request) (string, string) {
	u, w := r.PathValue("username"), r.PathValue("workspace")
	return u + "/" + w, "states/" + u + "/" + w
}

// lockUnlock dispatches LOCK/UNLOCK custom methods to their handlers.
// Go's ServeMux only accepts standard HTTP methods as route prefixes, so LOCK
// and UNLOCK (WebDAV/Terraform protocol) must be caught by a method-agnostic
// pattern and dispatched manually here.
func lockUnlock(keyFn func(*http.Request) (string, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		k, _ := keyFn(r)
		switch r.Method {
		case "LOCK":
			handleLockState(w, r, k)
		case "UNLOCK":
			handleUnlockState(w, r, k)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "blueprints")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(context.Background())
	}
	initMetrics()

	if err := initEncryption(); err != nil {
		slog.Error("encryption init failed", "error", err)
		os.Exit(1)
	}
	if err := initClientCA(); err != nil {
		slog.Error("client CA init failed", "error", err)
		os.Exit(1)
	}
	initCache()

	if err := connect().AutoMigrate(&State{}, &StateLock{}, &BackendCredential{}); err != nil {
		slog.Error("failed to migrate database", "error", err)
		os.Exit(1)
	}
	slog.Info("database initialized")

	// Rotate the gatekeeper service key every 25 minutes so credentials are always
	// short-lived. GATEKEEPER_SERVICE_KEY must match the key in GATEKEEPER_SERVICES
	// on gatekeeper. The loop is a no-op if the variable is unset.
	registry.StartKeyRotation(ctx, gatekeeperURL, "blueprints",
		secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)

	mux := telemetry.NewMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /backend", handleCreateBackend)

	// User-scoped: /state/{username}/{workspace}
	mux.HandleFunc("GET /state/{username}/{workspace}", func(w http.ResponseWriter, r *http.Request) {
		k, _ := userKey(r)
		handleGetState(w, r, k)
	})
	mux.HandleFunc("POST /state/{username}/{workspace}", func(w http.ResponseWriter, r *http.Request) {
		k, _ := userKey(r)
		handleUpdateState(w, r, k)
	})
	mux.HandleFunc("DELETE /state/{username}/{workspace}", func(w http.ResponseWriter, r *http.Request) {
		k, _ := userKey(r)
		handleDeleteState(w, r, k)
	})
	mux.HandleFunc("/state/{username}/{workspace}", lockUnlock(userKey))

	port := envOrDefault("PORT", "8084")

	wrappedMux := otelhttp.NewHandler(newLogger(mux), "blueprints",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")
	caFile := os.Getenv("CA_CERT_FILE")

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      wrappedMux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if certFile != "" && keyFile != "" {
		tlsConfig := &tls.Config{}
		if caFile != "" {
			caCert, err := os.ReadFile(caFile)
			if err != nil {
				slog.Error("failed to read CA cert", "error", err)
				os.Exit(1)
			}
			caPool := x509.NewCertPool()
			caPool.AppendCertsFromPEM(caCert)
			tlsConfig.ClientCAs = caPool
			tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		}
		srv.TLSConfig = tlsConfig
	}

	go func() {
		var err error
		if certFile != "" && keyFile != "" {
			slog.Info("listening with TLS", "port", port)
			err = srv.ListenAndServeTLS(certFile, keyFile)
		} else {
			slog.Info("listening", "port", port)
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	stop()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
}
