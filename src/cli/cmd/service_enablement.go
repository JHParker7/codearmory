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
	registeredServicesOnce sync.Once
	registeredServicesSet  map[string]bool
)

// registeredServices returns the set of platform services currently registered
// and routable in conductor (its live routing table, rebuilt from the registry —
// covering both manifest-registered core services and services builder deployed
// at runtime). The TUI hub uses it to show only services that are actually up
// (see enabledScreensFor).
//
// It FAILS OPEN by returning a NIL map: running under tests, not being logged in,
// an unreachable conductor, a non-2xx, or a decode error all yield nil, which the
// hub filter treats as "unknown — hide nothing". A non-nil (possibly empty) map
// means the set was resolved and modules outside it are hidden. The result is
// cached for the life of the process: the hub is built once per launch, and a
// restart re-reads it.
func registeredServices() map[string]bool {
	registeredServicesOnce.Do(func() { registeredServicesSet = fetchRegisteredServices() })
	return registeredServicesSet
}

func fetchRegisteredServices() map[string]bool {
	// Keep unit tests hermetic and offline: a dev machine may hold a live session
	// in its keychain, but building a hub in a test must never reach out. Nil =
	// fail open, so the live entry points behave like the unfiltered hub.
	if testing.Testing() {
		return nil
	}

	tok := bearerToken()
	if tok == "" {
		return nil // not logged in — hide nothing
	}

	// A 4s budget so a slow/unreachable conductor never stalls synchronous hub
	// startup; uses the short-timeout client, never the 30s default.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", conductorURL()+"/services", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "armory-cli")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := enablementClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	var body struct {
		Services []struct {
			Name string `json:"name"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, s := range body.Services {
		if s.Name != "" {
			out[s.Name] = true
		}
	}
	return out
}
