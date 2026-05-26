// Package registry provides shared service self-registration for codearmory services.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// sleepFn is the sleep implementation used between retries. It is a variable so
// tests can replace it with a no-op to avoid slow retries.
var sleepFn = time.Sleep

// Register publishes cfg to the registry, retrying every 5 seconds until the
// registry responds 204 or ctx is cancelled. serviceKey is the pre-shared key
// that proves identity; SERVICE_URL and REGISTRY_URL are read from the environment
// (REGISTRY_URL defaults to http://localhost:8084).
//
// The service name is taken from cfg.Name unless the SERVICE_NAME env var is set,
// which takes precedence to support multi-environment deployments.
func Register(ctx context.Context, cfg ServiceConfig, serviceKey string) {
	if serviceKey == "" {
		slog.Warn("SERVICE_KEY not set, skipping registry registration")
		return
	}

	name := os.Getenv("SERVICE_NAME")
	if name == "" {
		name = cfg.Name
	}
	registryURL := os.Getenv("REGISTRY_URL")
	if registryURL == "" {
		registryURL = "http://localhost:8084"
	}
	serviceURL := os.Getenv("SERVICE_URL")

	payload, _ := json.Marshal(struct {
		Name        string        `json:"name"`
		ServiceKey  string        `json:"service_key"`
		URL         string        `json:"url"`
		Description string        `json:"description"`
		Roles       []RoleDef     `json:"roles"`
		Endpoints   []EndpointDef `json:"endpoints"`
	}{
		Name:        name,
		ServiceKey:  serviceKey,
		URL:         serviceURL,
		Description: cfg.Description,
		Roles:       cfg.Roles,
		Endpoints:   cfg.Endpoints,
	})

	client := &http.Client{Timeout: 5 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			registryURL+"/services/register", bytes.NewReader(payload))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("registry registration failed, retrying in 5s", "service", name, "error", err)
			sleepFn(5 * time.Second)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusNoContent {
			slog.Info("registered with registry", "service", name, "endpoints", len(cfg.Endpoints))
			return
		}
		slog.Warn("registry registration: unexpected status, retrying in 5s", "service", name, "status", resp.StatusCode)
		sleepFn(5 * time.Second)
	}
}
