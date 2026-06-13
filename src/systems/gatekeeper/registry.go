package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

var gkRegistryClient = &http.Client{
	Transport: otelhttp.NewTransport(http.DefaultTransport),
	Timeout:   10 * time.Second,
}

// DefaultGrant is a permission template declared by a service in the registry.
// Resources may contain template variables: {user_id}, {username}, {org_id}, {team_id}.
type DefaultGrant struct {
	ServiceName string   `json:"service_name"`
	GrantOn     string   `json:"grant_on"` // "user", "org", or "team"
	Actions     []string `json:"actions"`
	Resources   []string `json:"resources"`
}

var (
	defaultGrantsMu sync.RWMutex
	cachedGrants    []DefaultGrant
)

// defaultGrantsFor returns all cached grants for the given grant_on context.
func defaultGrantsFor(grantOn string) []DefaultGrant {
	defaultGrantsMu.RLock()
	defer defaultGrantsMu.RUnlock()
	var out []DefaultGrant
	for _, g := range cachedGrants {
		if g.GrantOn == grantOn {
			out = append(out, g)
		}
	}
	return out
}

// applyGrantTemplates substitutes placeholder variables in a resource slice.
// Supported variables: {user_id}, {username}, {org_id}, {team_id}.
func applyGrantTemplates(resources []string, vars map[string]string) []string {
	out := make([]string, len(resources))
	for i, r := range resources {
		for k, v := range vars {
			r = strings.ReplaceAll(r, "{"+k+"}", v)
		}
		out[i] = r
	}
	return out
}

// fetchDefaultGrants calls GET /default-grants on the registry and returns the result.
func fetchDefaultGrants(ctx context.Context, registryURL, serviceKey string) ([]DefaultGrant, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL+"/default-grants", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Service-Key", serviceKey)

	resp, err := gkRegistryClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("registry returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var grants []DefaultGrant
	if err := json.NewDecoder(resp.Body).Decode(&grants); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return grants, nil
}

// startDefaultGrantPoller fetches default grants from the registry immediately
// and then refreshes every 5 minutes. If the registry is unreachable at startup,
// gatekeeper logs a warning and retries on the next tick.
func startDefaultGrantPoller(ctx context.Context, registryURL, serviceKey string) {
	refresh := func() {
		grants, err := fetchDefaultGrants(ctx, registryURL, serviceKey)
		if err != nil {
			slog.WarnContext(ctx, "default grants: registry unavailable", "error", err)
			return
		}
		defaultGrantsMu.Lock()
		cachedGrants = grants
		defaultGrantsMu.Unlock()
		slog.DebugContext(ctx, "default grants refreshed", "count", len(grants))
	}

	refresh()
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}
