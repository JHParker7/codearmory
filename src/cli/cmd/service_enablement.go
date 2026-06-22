package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// enablementClient is a short-timeout client for the hub's single enablement
// probe. The hub is built synchronously before the TUI renders, so a slow or
// unreachable builder must not stall startup: time out fast, then fail open.
var enablementClient = &http.Client{Timeout: 4 * time.Second}

var (
	disabledServicesOnce sync.Once
	disabledServicesSet  map[string]bool
)

// disabledServices returns the set of platform services disabled for the
// caller's org, as reported by the builder control plane. The TUI hub uses it
// to hide disabled services entirely (see enabledScreensFor).
//
// It FAILS OPEN: running under tests, not being logged in, an unresolvable org,
// an unreachable builder, a non-2xx, or a decode error all yield an empty set,
// so nothing is hidden — mirroring gatekeeper's own org-service gate, which
// allows when builder is unavailable. The result is cached for the life of the
// process: the hub is built once per launch, and a restart re-reads it.
func disabledServices() map[string]bool {
	disabledServicesOnce.Do(func() { disabledServicesSet = fetchDisabledServices() })
	return disabledServicesSet
}

func fetchDisabledServices() map[string]bool {
	out := map[string]bool{}

	// Keep unit tests hermetic and offline: a dev machine may hold a live
	// session in its keychain, but building a hub in a test must never reach out
	// to a real builder. The pure screensFor (not enabledScreensFor) is what the
	// tests assert against anyway.
	if testing.Testing() {
		return out
	}

	tok := bearerToken()
	if tok == "" {
		return out // not logged in — hide nothing
	}

	// One 4s budget for the whole probe (org-id resolution + the services call), so a
	// slow/unreachable conductor never stalls synchronous hub startup. Both requests
	// share this deadline and the short-timeout client — never the 30s default client.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	// Resolve the caller's org id within the budget; "" => "default", which a
	// non-admin can't read — that 403 fails open below.
	id := fastOrgID(ctx, tok)

	req, err := http.NewRequestWithContext(ctx, "GET", conductorURL()+"/builder/orgs/"+id+"/services", nil)
	if err != nil {
		return out
	}
	req.Header.Set("User-Agent", "armory-cli")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := enablementClient.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return out
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return out
	}
	var recs []osvRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return out
	}
	for _, r := range recs {
		if osvBool(r, "core") {
			continue // core services are always on; never hide them
		}
		if !osvBool(r, "enabled") {
			out[gkStr(r, "service")] = true
		}
	}
	return out
}

// fastOrgID resolves the caller's org id within ctx's deadline, returning "default"
// (the baseline scope, which fails open for a non-admin) if it can't be read in time.
// It deliberately uses the short-timeout enablementClient rather than the shared 30s
// doRequest path, so the hub's fail-fast startup budget actually holds.
func fastOrgID(ctx context.Context, tok string) string {
	sub, err := subjectFromToken(tok)
	if err != nil {
		return "default"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", conductorURL()+"/gatekeeper/users/"+sub, nil)
	if err != nil {
		return "default"
	}
	req.Header.Set("User-Agent", "armory-cli")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := enablementClient.Do(req)
	if err != nil {
		return "default"
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "default"
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "default"
	}
	var profile map[string]any
	if err := json.Unmarshal(data, &profile); err != nil {
		return "default"
	}
	if id, ok := profile["org_id"].(string); ok && id != "" {
		return id
	}
	return "default"
}
