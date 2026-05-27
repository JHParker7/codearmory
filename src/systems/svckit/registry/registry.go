// Package registry provides shared types for service configuration and the
// background key-rotation client used by services that hold a gatekeeper service key.
package registry

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// StartKeyRotation launches a background goroutine that rotates the service's
// gatekeeper key every interval by calling POST /service-accounts/rotate-key.
// Gatekeeper generates the new key server-side and returns it; the goroutine
// updates the in-memory current key atomically. On rotation failure the old key
// is kept and the next tick retries.
//
// Returns a getter function that always returns the current key. If any of
// gatekeeperURL, serviceName, or initialKey is empty, the goroutine is not
// started and the getter always returns initialKey.
func StartKeyRotation(ctx context.Context, gatekeeperURL, serviceName, initialKey string, interval time.Duration) func() string {
	if gatekeeperURL == "" || serviceName == "" || initialKey == "" {
		return func() string { return initialKey }
	}

	var mu sync.RWMutex
	current := initialKey

	getKey := func() string {
		mu.RLock()
		defer mu.RUnlock()
		return current
	}

	rotate := func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/service-accounts/rotate-key", nil)
		if err != nil {
			slog.Warn("key rotation: build request failed", "service", serviceName, "error", err)
			return
		}
		req.Header.Set("X-Service-Key", serviceName+":"+getKey())

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			slog.Warn("key rotation: request failed", "service", serviceName, "error", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			slog.Warn("key rotation: unexpected status", "service", serviceName, "status", resp.StatusCode)
			return
		}

		var result struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			slog.Warn("key rotation: decode failed", "service", serviceName, "error", err)
			return
		}
		if result.Key == "" {
			slog.Warn("key rotation: empty key in response", "service", serviceName)
			return
		}

		mu.Lock()
		current = result.Key
		mu.Unlock()
		slog.Info("service key rotated", "service", serviceName)
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rotate()
			}
		}
	}()

	return getKey
}
