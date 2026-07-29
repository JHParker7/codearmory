package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// Builder deploys git-factory, so builder is the only component that knows it exists,
// where it listens, and when it came up. That makes it the right place to also tell
// git_connector about it — otherwise every install needs an operator to link the
// platform's own git host by hand, mint a durable git-factory token for it, and paste
// that token in as a basic-auth password.
//
// The link builder registers carries no credential. git_connector's git_factory backend
// type mints a short-lived gatekeeper token per clone, as the user the clone is for,
// using its own service account (see the git service's gitfactory.go). So the only thing
// builder has to do is name the host — there is no secret to generate or distribute, and
// nothing to rotate.
//
// Auth for the call is git's own east-west key. Builder reads it straight out of git's
// Secret rather than deriving it: git is a core (Helm-deployed) service and its Secret is
// the canonical owner of git-internal-key, so a derived value would not match.

// gitConnectorPort is git_connector's in-cluster port (the git chart's .Values.git.port,
// and the service's own PORT default).
const gitConnectorPort = 8096

// gitConnectorURL overrides where git_connector is reached. Empty — the normal case —
// means derive it from the release prefix, since git is a core service always deployed
// alongside builder under the same name. Set GIT_CONNECTOR_URL only when git runs
// somewhere the convention does not describe.
var gitConnectorURL string

// gitConnectorBase returns git_connector's base URL, honouring the override.
func (b *k8sBackend) gitConnectorBase() string {
	if gitConnectorURL != "" {
		return strings.TrimRight(gitConnectorURL, "/")
	}
	return fmt.Sprintf("http://%s:%d", b.name("git"), gitConnectorPort)
}

// ensureGitConnectorBackend registers the service as a clone source in git_connector.
// Best-effort and level-triggered, like the rest of ensureInfra: git_connector may not be
// reachable yet on the pass that first deploys git-factory, and the next pass retries.
// A failure here must not block the workload rollout — the service itself is healthy
// without the link; only cloning its repos from a pipeline is unavailable until it lands.
func (b *k8sBackend) ensureGitConnectorBackend(ctx context.Context, service string, link svcGitBackend) error {
	key, err := b.existingSecretKey(ctx, "git", "git-internal-key")
	if err != nil {
		return fmt.Errorf("read git-internal-key: %w", err)
	}
	if key == "" {
		// git_connector is not deployed (or predates the key). Nothing to link to.
		slog.DebugContext(ctx, "git connector link: no git-internal-key, skipping", "service", service)
		return nil
	}

	name := link.Name
	if name == "" {
		name = k8sNameFor(service)
	}
	body, _ := json.Marshal(map[string]string{
		"name":     name,
		"base_url": b.gitFactoryBaseURL(service),
	})
	url := b.gitConnectorBase() + "/internal/backends/platform"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", key)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("git connector register: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("git connector register returned %s", resp.Status)
	}
	slog.InfoContext(ctx, "git connector backend registered", "service", service, "name", name)
	return nil
}

// gitFactoryBaseURL is the in-cluster URL git_connector will clone from. It must be the
// address reachable from inside the cluster, not any external ingress: the clone is done
// by a sandboxed runner whose egress policy allows in-cluster traffic only.
func (b *k8sBackend) gitFactoryBaseURL(service string) string {
	port := int32(0)
	if d, ok := embeddedServiceDef(service); ok {
		port = d.Port
	}
	if port == 0 {
		port = 80
	}
	return fmt.Sprintf("http://%s:%d", b.name(service), port)
}
