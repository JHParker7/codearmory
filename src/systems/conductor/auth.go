package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// ── Auth ──────────────────────────────────────────────────────────────────────

type authOutcome int

const (
	authAllowed authOutcome = iota
	authUnauthorized
	authForbidden
)

// getUserID decodes the JWT payload (without signature verification — that
// happens inside Gatekeeper when we call GET /users/{id}) and returns the sub
// claim, which Gatekeeper sets to the user's UUID.
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
	return canonicalUUID(claims.Sub)
}

// checkUserAuth verifies that the caller has a valid, active account.
// It decodes the JWT locally to extract the user ID, then calls Gatekeeper's
// GET /users/{id} endpoint — which performs full JWT signature verification —
// to confirm the user exists and the token is genuine.
// Permission checking is left to each backend service.
func checkUserAuth(r *http.Request) (authOutcome, string) {
	ctx, span := otel.Tracer("conductor").Start(r.Context(), "checkUserAuth")
	defer span.End()

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "no_token")))
		return authUnauthorized, ""
	}

	userID, ok := getUserID(strings.TrimPrefix(auth, "Bearer "))
	if !ok {
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "malformed_token")))
		return authUnauthorized, ""
	}
	span.SetAttributes(attribute.String("user.id", userID))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		gatekeeperURL+"/users/"+url.PathEscape(userID), nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return authUnauthorized, ""
	}
	req.Header.Set("Authorization", auth)

	resp, err := gatekeeperClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return authUnauthorized, ""
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "unauthorized")))
		return authUnauthorized, userID
	case resp.StatusCode >= 500:
		span.SetAttributes(attribute.Int("http.response_status_code", resp.StatusCode))
		span.SetStatus(codes.Error, "gatekeeper error")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "gatekeeper_error")))
		return authForbidden, ""
	case resp.StatusCode != http.StatusOK:
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "user_not_found")))
		return authForbidden, ""
	}

	span.SetStatus(codes.Ok, "")
	meterAllowed.Add(ctx, 1)
	return authAllowed, userID
}

// ── Suspicious-activity block list ───────────────────────────────────────────

const (
	suspectThreshold = 10
	blockDuration    = time.Hour
)

var (
	suspectMu   sync.Mutex
	suspectHits = map[string]int{}
	blockedIPs  = map[string]time.Time{}
)

// sourceIP returns the immediate peer IP from r.RemoteAddr, stripping the port.
func sourceIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// isBlocked reports whether the IP is on the block list and has not yet expired.
func isBlocked(ip string) bool {
	suspectMu.Lock()
	defer suspectMu.Unlock()
	until, ok := blockedIPs[ip]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(blockedIPs, ip)
		delete(suspectHits, ip)
		return false
	}
	return true
}

// resetSuspect clears the failure counter for the IP on successful authentication.
func resetSuspect(ip string) {
	suspectMu.Lock()
	defer suspectMu.Unlock()
	delete(suspectHits, ip)
}

// recordSuspect logs an auth failure and blocks the IP after suspectThreshold
// failures. Keyed on IP alone so a forged JWT sub claim cannot target a specific
// victim's block state.
func recordSuspect(ip, userID, method, path string) {
	suspectMu.Lock()
	defer suspectMu.Unlock()
	if _, already := blockedIPs[ip]; already {
		return
	}
	suspectHits[ip]++
	n := suspectHits[ip]
	slog.Warn("user passed conductor auth but failed service validation",
		"source_ip", ip, "user_id", userID,
		"method", method, "path", path, "failure_count", n)
	if n >= suspectThreshold {
		until := time.Now().Add(blockDuration)
		blockedIPs[ip] = until
		slog.Warn("IP added to block list",
			"source_ip", ip, "blocked_until", until)
		meterBlocked.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("source_ip", ip)))
	}
}

// signForwardedUserID generates a short-lived HMAC-SHA256 token binding userID
// to a 30-second timestamp window. Any backend service with forward_auth=false
// can verify this token to confirm X-User-ID was injected by conductor.
func signForwardedUserID(userID string) (token, timestamp string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(conductorForwardKey))
	fmt.Fprintf(mac, "conductor:%s:%s", userID, ts)
	return hex.EncodeToString(mac.Sum(nil)), ts
}

// paramTypeFor returns the validation type for a path param by name.
// "id" → UUID, slug-like names → slug pattern.
func paramTypeFor(name string) string {
	switch name {
	case "id":
		return "uuid"
	case "username", "workspace", "org", "team":
		return "slug"
	}
	return ""
}
