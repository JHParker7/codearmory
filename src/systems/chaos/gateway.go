package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// outpostGatewayURL is the base URL of the outpost-gateway's internal API. The
// chaos service enqueues commands there; the gateway long-polls them out to the
// right outpost. outpostInternalKey is the shared HMAC secret protecting the
// internal command/event plane between control-plane services and the gateway.
var (
	outpostGatewayURL  = strings.TrimRight(envOrDefault("OUTPOST_GATEWAY_URL", ""), "/")
	outpostInternalKey = secret("OUTPOST_INTERNAL_KEY")
)

// signInternal builds the HMAC-SHA256 token over the domain ("command"/"event"),
// a unix timestamp, and the full request body keyed by the shared internal key.
// Signing the whole body means payload/tenant tampering invalidates the token.
// The same scheme verifies events arriving from the dispatcher (see events.go).
func signInternal(domain string, body []byte) (token, timestamp string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	return internalMAC(domain, ts, body), ts
}

// internalMAC computes hex(HMAC-SHA256(key, "domain:timestamp:" || body)).
func internalMAC(domain, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(outpostInternalKey))
	fmt.Fprintf(mac, "%s:%s:", domain, timestamp)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// enqueueCommand asks the outpost-gateway to deliver a command to the given
// outpost. integration selects the outpost module ("chaos"); typ is the module
// command verb. orgID/userID identify the authorizing tenant; the gateway checks
// them against the target outpost's owner so a caller cannot drive another
// tenant's outpost.
func enqueueCommand(ctx context.Context, outpostID, integration, typ, orgID, userID string, payload map[string]any) error {
	if outpostGatewayURL == "" {
		return fmt.Errorf("OUTPOST_GATEWAY_URL is not configured")
	}
	body, err := json.Marshal(map[string]any{
		"outpost_id":  outpostID,
		"integration": integration,
		"type":        typ,
		"org_id":      orgID,
		"user_id":     userID,
		"payload":     payload,
	})
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	token, ts := signInternal("command", body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, outpostGatewayURL+"/internal/commands", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build command request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", token)
	req.Header.Set("X-Internal-Timestamp", ts)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("enqueue command: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("outpost-gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// verifyInternal validates an HMAC token over the domain and the exact body
// bytes received, with a 30s replay window. Used by the /internal/events handler.
func verifyInternal(domain string, body []byte, token, timestamp string) bool {
	if outpostInternalKey == "" {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if d := time.Now().Unix() - ts; d > 30 || d < -30 {
		return false
	}
	return hmac.Equal([]byte(token), []byte(internalMAC(domain, timestamp, body)))
}
