package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
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
	authError // upstream unavailable or returned 5xx — map to 502, not 401/403
)

// jwtExpClaim decodes the JWT payload and returns the exp claim value.
// Returns (0, false) if the token is not a valid JWT or has no exp claim.
func jwtExpClaim(token string) (int64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return 0, false
	}
	return claims.Exp, true
}

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

// checkUserAuth verifies that the caller has a valid token and is authorised for
// the given (service, action, resource) triple by calling Gatekeeper's
// POST /check_permissions. Gatekeeper scopes the resource to the authenticated
// user or org internally before evaluating. Accepts both Bearer tokens and the
// armory_session cookie. Returns (outcome, subjectID, normalizedAuth, denyReason)
// where normalizedAuth is the "Bearer <token>" string callers forward, and
// denyReason is a human-readable explanation (role, resource, missing permission)
// set when gatekeeper reports the request is forbidden; it is empty otherwise.
func checkUserAuth(r *http.Request, service, action, resource string) (authOutcome, string, string, string) {
	ctx, span := otel.Tracer("conductor").Start(r.Context(), "checkUserAuth")
	defer span.End()

	// Accept Bearer token from Authorization header or armory_session cookie.
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		if cookie, err := r.Cookie("armory_session"); err == nil && cookie.Value != "" {
			auth = "Bearer " + cookie.Value
		}
	}
	if !strings.HasPrefix(auth, "Bearer ") {
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "no_token")))
		return authUnauthorized, "", "", ""
	}

	// Decode JWT locally (no sig verify) to validate basic structure and extract
	// the sub claim for suspicious-activity tracking. Tokens that aren't even
	// valid JWTs are rejected here to avoid unnecessary gatekeeper calls.
	token := strings.TrimPrefix(auth, "Bearer ")
	suspectID, ok := getUserID(token)
	if !ok {
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "malformed_token")))
		return authUnauthorized, "", "", ""
	}

	// Expired tokens are not suspicious — the user just needs to re-authenticate.
	// Return early without calling gatekeeper and without incrementing the
	// suspect counter so a run of TUI retries with a stale token never triggers
	// the IP block.
	if exp, ok := jwtExpClaim(token); ok && time.Now().Unix() > exp {
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "expired_token")))
		return authUnauthorized, "", "", ""
	}

	body, _ := json.Marshal(map[string]string{
		"service":  service,
		"action":   action,
		"resource": resource,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gatekeeperURL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return authUnauthorized, "", "", ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)

	resp, err := gatekeeperClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "gatekeeper_unreachable")))
		return authError, "", "", ""
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "unauthorized")))
		return authUnauthorized, suspectID, "", ""
	case resp.StatusCode >= 500:
		span.SetAttributes(attribute.Int("http.response_status_code", resp.StatusCode))
		span.SetStatus(codes.Error, "gatekeeper error")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "gatekeeper_error")))
		return authError, "", "", ""
	case resp.StatusCode != http.StatusOK:
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "auth_failed")))
		return authForbidden, "", "", ""
	}

	var result struct {
		Authorized bool   `json:"authorized"`
		UserID     string `json:"user_id"`
		Reason     string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid check_permissions response")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid_response")))
		return authForbidden, "", "", ""
	}
	if result.UserID == "" {
		span.SetStatus(codes.Error, "check_permissions returned empty user_id")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid_response")))
		return authForbidden, "", "", ""
	}
	if !result.Authorized {
		span.SetStatus(codes.Ok, "")
		meterRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "forbidden")))
		return authForbidden, suspectID, "", result.Reason
	}

	span.SetAttributes(attribute.String("user.id", result.UserID))
	span.SetStatus(codes.Ok, "")
	meterAllowed.Add(ctx, 1)
	return authAllowed, result.UserID, auth, ""
}

// ── Suspicious-activity block list ───────────────────────────────────────────

// suspectThreshold and blockDuration are read from the environment at startup
// (CONDUCTOR_SUSPECT_THRESHOLD, CONDUCTOR_BLOCK_DURATION), defaulting to the values
// below.
//
// They are configurable because the block is keyed on SOURCE IP alone, and an IP is
// not a user. Everyone behind one NAT egress — an office, a CI runner, a
// kubectl port-forward — shares a single counter, so ten failures from any one of
// them locks out all of them for an hour. A deployment whose users share an egress
// needs to raise this; a browser test suite, which deliberately exercises
// logged-out and rejected-session states, needs to raise it a lot.
var (
	suspectThreshold = defaultSuspectThreshold
	blockDuration    = defaultBlockDuration
)

const (
	defaultSuspectThreshold = 10
	defaultBlockDuration    = time.Hour
	// suspectWindow is how long a failure counts toward the streak. It bounds the
	// state below — entries are otherwise only ever removed on a SUCCESSFUL auth from
	// the same IP, so a scanner that fails a few times and never returns is remembered
	// forever and the map grows without limit. It is also the more correct rule: nine
	// failures a week ago should not make today's first failure the tenth.
	suspectWindow = time.Hour
	// suspectPruneAt is the tracked-IP count past which a sweep runs. Pruning is O(n),
	// so it is amortised behind a threshold rather than done on every failure.
	suspectPruneAt = 1024
)

// initSuspectLimits applies the environment overrides. Invalid or absent values keep
// the defaults, so a typo degrades to the shipped behaviour rather than to no
// protection at all.
func initSuspectLimits() {
	if v := os.Getenv("CONDUCTOR_SUSPECT_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			suspectThreshold = n
		} else {
			slog.Warn("invalid CONDUCTOR_SUSPECT_THRESHOLD, using default", "value", v, "default", defaultSuspectThreshold)
		}
	}
	if v := os.Getenv("CONDUCTOR_BLOCK_DURATION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			blockDuration = d
		} else {
			slog.Warn("invalid CONDUCTOR_BLOCK_DURATION, using default", "value", v, "default", defaultBlockDuration)
		}
	}
}

// suspectEntry is a failure streak and when it was last added to.
type suspectEntry struct {
	count int
	last  time.Time
}

var (
	suspectMu   sync.Mutex
	suspectHits = map[string]suspectEntry{}
	blockedIPs  = map[string]time.Time{}
)

// pruneSuspects drops stale failure streaks and expired blocks. Callers must hold
// suspectMu.
func pruneSuspects(now time.Time) {
	for ip, e := range suspectHits {
		if now.Sub(e.last) > suspectWindow {
			delete(suspectHits, ip)
		}
	}
	for ip, until := range blockedIPs {
		if now.After(until) {
			delete(blockedIPs, ip)
		}
	}
}

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
	now := time.Now()
	if len(suspectHits)+len(blockedIPs) > suspectPruneAt {
		pruneSuspects(now)
	}
	e := suspectHits[ip]
	// A streak that has gone quiet for longer than the window starts over rather than
	// resuming where it left off.
	if now.Sub(e.last) > suspectWindow {
		e.count = 0
	}
	e.count++
	e.last = now
	suspectHits[ip] = e
	n := e.count
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
	// "ns" is the owner namespace in owner-first per-record routes
	// (/tickets/{ns}/{id} → "{ns}/tickets/tickets/{id}"). It is interpolated straight
	// into the resource string the RBAC check is evaluated against, so it is validated
	// for the same reason "id" is normalised: an unconstrained value lets two spellings
	// of the same namespace produce two different resource strings, and the permission
	// cache is keyed on that string.
	//
	// Deliberately not "namespace", which container routes use for a docker namespace —
	// those legitimately contain characters the slug pattern rejects, and tightening
	// them here would start 400ing pulls.
	case "ns", "username", "workspace", "org", "team":
		return "slug"
	}
	return ""
}
