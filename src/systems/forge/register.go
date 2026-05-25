package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"
)

type endpointDef struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Action   string `json:"action"`
	Resource string `json:"resource"`
}

var forgeEndpoints = []endpointDef{
	{Method: "POST", Path: "/executions", Action: "createExecution", Resource: "executions"},
	{Method: "GET", Path: "/executions", Action: "listExecution", Resource: "executions"},
	{Method: "GET", Path: "/executions/{id}", Action: "getExecution", Resource: "executions"},
	{Method: "DELETE", Path: "/executions/{id}", Action: "deleteExecution", Resource: "executions"},
}

// registerWithGatekeeper authenticates with the service key and declares forge's
// endpoints so Conductor can enforce the correct RBAC action per route.
// Retries until successful so a slow Gatekeeper startup doesn't block forge.
func registerWithGatekeeper(ctx context.Context) {
	gatekeeperURL := envOrDefault("GATEKEEPER_URL", "http://localhost:8080")
	serviceName := envOrDefault("SERVICE_NAME", "forge")
	serviceKey := os.Getenv("SERVICE_KEY")
	serviceURL := os.Getenv("SERVICE_URL")

	if serviceKey == "" {
		slog.Warn("SERVICE_KEY not set, skipping gatekeeper registration")
		return
	}

	payload, _ := json.Marshal(struct {
		Name       string        `json:"name"`
		ServiceKey string        `json:"service_key"`
		URL        string        `json:"url"`
		Endpoints  []endpointDef `json:"endpoints"`
	}{
		Name:       serviceName,
		ServiceKey: serviceKey,
		URL:        serviceURL,
		Endpoints:  forgeEndpoints,
	})

	client := &http.Client{Timeout: 5 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			gatekeeperURL+"/services/register", bytes.NewReader(payload))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("gatekeeper registration failed, retrying in 5s", "error", err)
			time.Sleep(5 * time.Second)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusNoContent {
			slog.Info("registered with gatekeeper", "name", serviceName, "endpoints", len(forgeEndpoints))
			return
		}
		slog.Warn("gatekeeper registration: unexpected status, retrying in 5s", "status", resp.StatusCode)
		time.Sleep(5 * time.Second)
	}
}
