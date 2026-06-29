package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Builder registers each non-core service it deploys with the registry: its routes,
// actions and RBAC default-grants — so conductor routes to it and gatekeeper grants its
// default permissions — and, for services that pull from the registry, a read service
// account. The chart's registry manifest ships only the core services; everything else
// is registered here on enable and removed on disable. Builder authenticates as a
// registry ADMIN (seeded in the Helm registry secret).

// registerServicesOn reports whether builder should register/deregister services. It
// requires a configured registry client and is on by default; BUILDER_REGISTER_SERVICES=false
// disables it (e.g. when the chart pre-registers every service).
func registerServicesOn() bool {
	if registryURL == "" || getRegistryKey == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BUILDER_REGISTER_SERVICES"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

func registryAuthHeader() string { return "builder:" + getRegistryKey() }

// findRegistryService returns the active registry service id for name (or "") and
// whether it already carries endpoints, so the caller can repair a service that was
// created but never got its manifest (e.g. a crash between POST and PUT).
func findRegistryService(ctx context.Context, name string) (id string, hasEndpoints bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL+"/services", nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("X-Service-Key", registryAuthHeader())
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return "", false, fmt.Errorf("registry GET /services: %s", resp.Status)
	}
	// GET /services returns active services only, each with its endpoints.
	var svcs []struct {
		ServiceID string            `json:"service_id"`
		Name      string            `json:"name"`
		Endpoints []json.RawMessage `json:"endpoints"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&svcs); err != nil {
		return "", false, err
	}
	for _, s := range svcs {
		if s.Name == name {
			return s.ServiceID, len(s.Endpoints) > 0, nil
		}
	}
	return "", false, nil
}

// ensureRegistryService registers def with the registry: it POSTs the service when
// absent (the Phase-0 reactivate-or-create path handles a previously-disabled name)
// and PUTs its endpoint manifest. It is a no-op when the service is already active with
// endpoints, so a steady-state reconcile does not churn the registry or conductor.
func ensureRegistryService(ctx context.Context, def serviceDef, prefix string) error {
	url := fmt.Sprintf("http://%s-%s:%d", prefix, def.K8sName, def.Port)
	id, hasEndpoints, err := findRegistryService(ctx, def.RegistryName)
	if err != nil {
		return fmt.Errorf("lookup %s: %w", def.RegistryName, err)
	}
	if id != "" && hasEndpoints {
		return nil
	}
	if id == "" {
		if id, err = createRegistryService(ctx, def, url); err != nil {
			return fmt.Errorf("create %s: %w", def.RegistryName, err)
		}
	}
	if err := putRegistryEndpoints(ctx, id, def, url); err != nil {
		return fmt.Errorf("put endpoints %s: %w", def.RegistryName, err)
	}
	return nil
}

func createRegistryService(ctx context.Context, def serviceDef, url string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"name":         def.RegistryName,
		"url":          url,
		"description":  def.Description,
		"forward_auth": def.ForwardAuth,
		"ui_path":      def.UIPath,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registryURL+"/services", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", registryAuthHeader())
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// 409 means another writer registered it active between our lookup and POST —
	// re-resolve the id and proceed to PUT endpoints.
	if resp.StatusCode == http.StatusConflict {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		id, _, err := findRegistryService(ctx, def.RegistryName)
		if err != nil {
			return "", err
		}
		if id == "" {
			return "", fmt.Errorf("registry POST /services conflicted but service not found")
		}
		return id, nil
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("registry POST /services: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var svc struct {
		ServiceID string `json:"service_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&svc); err != nil {
		return "", err
	}
	if svc.ServiceID == "" {
		return "", fmt.Errorf("registry POST /services returned empty service_id")
	}
	return svc.ServiceID, nil
}

func putRegistryEndpoints(ctx context.Context, id string, def serviceDef, url string) error {
	body, _ := json.Marshal(map[string]any{
		"url":            url,
		"description":    def.Description,
		"endpoints":      def.Endpoints,
		"actions":        def.Actions,
		"default_grants": def.DefaultGrants,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, registryURL+"/services/"+id+"/endpoints", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", registryAuthHeader())
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("registry PUT endpoints: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// removeRegistryService soft-deletes a service so conductor stops routing to it and
// gatekeeper stops granting its default permissions. A missing service is a no-op.
func removeRegistryService(ctx context.Context, registryName string) error {
	id, _, err := findRegistryService(ctx, registryName)
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, registryURL+"/services/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Service-Key", registryAuthHeader())
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("registry DELETE /services/%s: %s: %s", id, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// ensureRegistryAccount upserts a registry READ service-account so a runtime-deployed
// service that pulls from the registry (e.g. workflows → GET /actions) can authenticate.
// The key is deterministic (derived), so repeated calls are idempotent.
func ensureRegistryAccount(ctx context.Context, name, key string) error {
	body, _ := json.Marshal(map[string]string{"name": name, "key": key, "role": "read"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registryURL+"/service-accounts", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", registryAuthHeader())
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("registry POST /service-accounts: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
