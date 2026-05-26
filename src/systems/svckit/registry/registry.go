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

// Register publishes cfg to the registry, retrying every 5 seconds until
// successful or ctx is cancelled.
//
// Two modes are supported, selected by environment variables:
//
//  1. Credential mode (CLIENT_ID + CLIENT_SECRET both set): sends client
//     credentials on every call; expects 204. Use after the first bootstrap.
//
//  2. Bootstrap mode (SERVICE_KEY set): sends the pre-shared key on first
//     contact; expects 200 with {client_id, client_secret} in the response.
//     Logs the returned credentials so the operator can persist them and switch
//     to credential mode on the next deploy.
//
// SERVICE_URL and REGISTRY_URL are read from the environment
// (REGISTRY_URL defaults to http://localhost:8084).
// SERVICE_NAME overrides cfg.Name when set.
func Register(ctx context.Context, cfg ServiceConfig, serviceKey string) {
	clientID := os.Getenv("CLIENT_ID")
	clientSecret := os.Getenv("CLIENT_SECRET")

	credMode := clientID != "" && clientSecret != ""
	if !credMode && serviceKey == "" {
		slog.Warn("neither SERVICE_KEY nor CLIENT_ID+CLIENT_SECRET set, skipping registry registration")
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

	client := &http.Client{Timeout: 5 * time.Second}

	if credMode {
		registerWithCredentials(ctx, client, cfg, name, serviceURL, registryURL, clientID, clientSecret)
	} else {
		registerWithServiceKey(ctx, client, cfg, name, serviceURL, registryURL, serviceKey)
	}
}

func registerWithCredentials(ctx context.Context, client *http.Client, cfg ServiceConfig, name, serviceURL, registryURL, clientID, clientSecret string) {
	payload, _ := json.Marshal(struct {
		Name         string        `json:"name"`
		ClientID     string        `json:"client_id"`
		ClientSecret string        `json:"client_secret"`
		URL          string        `json:"url"`
		Description  string        `json:"description"`
		Roles        []RoleDef     `json:"roles"`
		Endpoints    []EndpointDef `json:"endpoints"`
	}{
		Name:         name,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		URL:          serviceURL,
		Description:  cfg.Description,
		Roles:        cfg.Roles,
		Endpoints:    cfg.Endpoints,
	})

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, registryURL+"/services/register", bytes.NewReader(payload))
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

func registerWithServiceKey(ctx context.Context, client *http.Client, cfg ServiceConfig, name, serviceURL, registryURL, serviceKey string) {
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

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, registryURL+"/services/register", bytes.NewReader(payload))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("registry bootstrap failed, retrying in 5s", "service", name, "error", err)
			sleepFn(5 * time.Second)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			var creds struct {
				ClientID     string `json:"client_id"`
				ClientSecret string `json:"client_secret"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&creds); err == nil && creds.ClientID != "" {
				slog.Info("bootstrap complete — persist these credentials and switch to credential mode",
					"service", name,
					"client_id", creds.ClientID,
					"action", "set CLIENT_ID="+creds.ClientID+" CLIENT_SECRET=<secret> and remove SERVICE_KEY")
				slog.Info("CLIENT_SECRET", "value", creds.ClientSecret)
			}
			resp.Body.Close()
			slog.Info("registered with registry (bootstrap)", "service", name, "endpoints", len(cfg.Endpoints))
			return
		}
		resp.Body.Close()
		slog.Warn("registry bootstrap: unexpected status, retrying in 5s", "service", name, "status", resp.StatusCode)
		sleepFn(5 * time.Second)
	}
}
