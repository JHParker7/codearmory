// Package registry provides shared types for service configuration and the
// background key-rotation client used by services that hold a gatekeeper service key.
package registry

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// StartKeyRotation launches a background goroutine that rotates the service's
// gatekeeper key every interval by calling POST /service-accounts/rotate-key.
// Gatekeeper generates the new key server-side and returns it; the goroutine
// updates the in-memory current key atomically.
//
// Recovery: the bootstrap (initial) key is used only for the first (spin-up)
// rotation and is then discarded from memory — it is not retained or reused for
// the rest of the process's lifetime. A 401 before that first success retries the
// bootstrap key; a 401 afterwards can no longer self-heal and requires a pod
// restart (which re-reads the key from the environment). Any failed attempt is
// retried within ~30s rather than waiting a full interval.
//
// Returns a getter function that always returns the current key. If any of
// gatekeeperURL, serviceName, or initialKey is empty, the goroutine is not
// started and the getter always returns initialKey.
func StartKeyRotation(ctx context.Context, gatekeeperURL, serviceName, initialKey string, interval time.Duration) func() string {
	if gatekeeperURL == "" || serviceName == "" || initialKey == "" {
		return func() string { return initialKey }
	}

	// Derive peer.service from the target hostname so Tempo's service-graph
	// processor can label the edge even when SERVER spans arrive late.
	peerService := ""
	if u, err := url.Parse(gatekeeperURL); err == nil {
		peerService = u.Hostname()
	}

	var mu sync.RWMutex
	current := initialKey
	// bootstrap is the spin-up recovery key. It is discarded (zeroed) after the
	// first successful rotation so the long-lived key is not retained in memory or
	// reused past spin-up; recovery from a later desync is a pod restart, which
	// re-reads the key from the environment.
	bootstrap := initialKey

	getKey := func() string {
		mu.RLock()
		defer mu.RUnlock()
		return current
	}

	// rotate performs one rotation attempt and reports whether it succeeded so the
	// caller can retry sooner after a failure.
	rotate := func() bool {
		// Create a CLIENT span so Gatekeeper's SERVER span becomes a child of it
		// rather than an orphan root span. Orphan SERVER spans appear as "user →
		// gatekeeper" in Grafana's service map; a proper CLIENT span fixes that.
		rctx, span := otel.Tracer("codearmory_sdk/registry").Start(ctx, "key_rotation",
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("peer.service", peerService),
				attribute.String("http.method", "POST"),
				attribute.String("http.url", gatekeeperURL+"/service-accounts/rotate-key"),
			),
		)
		defer span.End()

		req, err := http.NewRequestWithContext(rctx, http.MethodPost, gatekeeperURL+"/service-accounts/rotate-key", nil)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.Warn("key rotation: build request failed", "service", serviceName, "error", err)
			return false
		}
		req.Header.Set("X-Service-Key", serviceName+":"+getKey())
		otel.GetTextMapPropagator().Inject(rctx, propagation.HeaderCarrier(req.Header))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.Warn("key rotation: request failed", "service", serviceName, "error", err)
			return false
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			span.SetStatus(codes.Error, "unexpected status")
			if resp.StatusCode == http.StatusUnauthorized {
				// The server rejected our key. While we still hold the bootstrap key
				// (the spin-up window) fall back to it so the next attempt can
				// re-authenticate and roll the key forward. Once the bootstrap key has
				// been discarded after the first successful rotation there is nothing to
				// fall back to: the process can no longer self-heal and must be restarted
				// (which re-reads the key from the environment).
				mu.Lock()
				boot := bootstrap
				onBootstrap := boot != "" && current == boot
				if boot != "" && !onBootstrap {
					current = boot
				}
				mu.Unlock()
				switch {
				case boot == "":
					slog.Error("key rotation: key rejected and bootstrap key already discarded after spin-up; restart required to recover", "service", serviceName, "status", resp.StatusCode)
				case onBootstrap:
					// The bootstrap key itself was rejected during spin-up: a genuine
					// misconfiguration (or the server has not finished seeding yet). The
					// next attempt retries the bootstrap key.
					slog.Error("key rotation: bootstrap key rejected", "service", serviceName, "status", resp.StatusCode)
				default:
					slog.Warn("key rotation: rotated key rejected, falling back to bootstrap key", "service", serviceName, "status", resp.StatusCode)
				}
			} else {
				slog.Warn("key rotation: unexpected status", "service", serviceName, "status", resp.StatusCode)
			}
			return false
		}

		var result struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.Warn("key rotation: decode failed", "service", serviceName, "error", err)
			return false
		}
		if result.Key == "" {
			span.SetStatus(codes.Error, "empty key in response")
			slog.Warn("key rotation: empty key in response", "service", serviceName)
			return false
		}

		mu.Lock()
		current = result.Key
		// Discard the spin-up bootstrap key after the first successful rotation so it
		// is not retained in memory or reused for the rest of this process's lifetime.
		bootstrap = ""
		mu.Unlock()
		span.SetStatus(codes.Ok, "")
		slog.Debug("service key rotated", "service", serviceName)
		return true
	}

	// After a failed attempt, retry well before the full interval so a service
	// recovers from a transient desync (e.g. a dependency restart) in seconds
	// rather than waiting out the rotation period.
	retryInterval := 30 * time.Second
	if interval < retryInterval {
		retryInterval = interval
	}

	go func() {
		// Fire immediately so a restarted server re-syncs before the first tick.
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if rotate() {
					timer.Reset(interval)
				} else {
					timer.Reset(retryInterval)
				}
			}
		}
	}()

	return getKey
}
