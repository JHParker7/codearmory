package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Provisioning lets builder bring a non-core service online with no Helm change.
// Before deploying the workload it ensures the two things Helm would otherwise
// provide: a gatekeeper service identity (registered at runtime) and the service's
// Secret (the admin-supplied DB URL + a generated gatekeeper key + the shared
// conductor-forward key). It never creates the database — the admin supplies a URL
// for a role already scoped to an existing database.

// provisioningConfig is the runtime config for builder's provisioner.
type provisioningConfig struct {
	enabled             bool
	gatekeeperURL       string // e.g. http://codearmory-gatekeeper:8081
	internalKey         string // BUILDER_INTERNAL_KEY (shared with gatekeeper)
	conductorSecretName string // e.g. codearmory-conductor (for conductor-forward-key)
}

func (b *k8sBackend) provisioningOn() bool {
	return b.prov.enabled && b.prov.gatekeeperURL != "" && b.prov.internalKey != ""
}

// provision ensures the gatekeeper identity and Secret exist for a service. dbURL
// is the decrypted admin-supplied database URL ("" when none is configured).
func (b *k8sBackend) provision(ctx context.Context, service, dbURL string) error {
	key, err := b.ensureGatekeeperIdentity(ctx, service)
	if err != nil {
		return fmt.Errorf("gatekeeper identity: %w", err)
	}
	if err := b.ensureServiceSecret(ctx, service, key, dbURL); err != nil {
		return fmt.Errorf("service secret: %w", err)
	}
	return nil
}

// deprovision removes what builder created for a torn-down service: its Secret (if
// builder owns it). The gatekeeper identity is left in place (harmless without a
// running pod, and re-registered idempotently on the next enable).
func (b *k8sBackend) deprovision(ctx context.Context, service string) {
	name := b.name(service)
	sec, err := b.client.CoreV1().Secrets(b.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil && sec.Labels[labelManagedBy] == managedByValue {
		if err := b.client.CoreV1().Secrets(b.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			slog.WarnContext(ctx, "deprovision: delete secret failed", "service", service, "error", err)
		}
	}
}

// ensureGatekeeperIdentity reuses the service's existing gatekeeper key (so a
// reconcile never churns the pod) or generates one, then registers it with
// gatekeeper. Registration is idempotent — it refreshes the bootstrap key.
func (b *k8sBackend) ensureGatekeeperIdentity(ctx context.Context, service string) (string, error) {
	key, err := b.existingSecretKey(ctx, service, "gatekeeper-service-key")
	if err != nil {
		// A transient read error must NOT be treated as "no key" — generating a
		// fresh one here would re-key the running pod out from under itself. Abort
		// the reconcile so it retries with the Secret intact.
		return "", fmt.Errorf("read existing gatekeeper key: %w", err)
	}
	if key == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		key = hex.EncodeToString(raw)
	}
	if err := b.registerGatekeeperIdentity(ctx, service, key); err != nil {
		return "", err
	}
	return key, nil
}

func (b *k8sBackend) registerGatekeeperIdentity(ctx context.Context, service, key string) error {
	body, _ := json.Marshal(map[string]string{"service_name": service, "key": key})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.prov.gatekeeperURL+"/internal/service-accounts", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.prov.internalKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("gatekeeper register returned %s", resp.Status)
	}
	return nil
}

// ensureServiceSecret merges the keys builder manages into the service's Secret,
// creating it (labelled managed-by builder) if absent. It only overwrites the
// admin-supplied DB URL and fills the conductor-forward key when missing, so it
// never clobbers a chart-provisioned Secret's other keys.
func (b *k8sBackend) ensureServiceSecret(ctx context.Context, service, gkKey, dbURL string) error {
	name := b.name(service)
	api := b.client.CoreV1().Secrets(b.namespace)

	existing, err := api.Get(ctx, name, metav1.GetOptions{})
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		return err
	}

	data := map[string][]byte{}
	if !create && existing.Data != nil {
		for k, v := range existing.Data {
			data[k] = v
		}
	}
	data["gatekeeper-service-key"] = []byte(gkKey)
	if dbURL != "" {
		data["database-url"] = []byte(dbURL)
	}
	if _, ok := data["conductor-forward-key"]; !ok {
		// Best-effort fill: a transient read error just skips it this round rather
		// than failing the whole secret reconcile.
		if cfk, err := b.existingSecretKeyIn(ctx, b.prov.conductorSecretName, "conductor-forward-key"); err == nil && cfk != "" {
			data["conductor-forward-key"] = []byte(cfk)
		}
	}

	if create {
		_, err = api.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace, Labels: b.labels(service)},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}, metav1.CreateOptions{})
		return err
	}
	existing.Data = data
	_, err = api.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// existingSecretKey reads one key from the service's own Secret. A missing Secret
// returns ("", nil); a transient API error is propagated so callers can retry
// rather than mistake it for "no value".
func (b *k8sBackend) existingSecretKey(ctx context.Context, service, key string) (string, error) {
	return b.existingSecretKeyIn(ctx, b.name(service), key)
}

func (b *k8sBackend) existingSecretKeyIn(ctx context.Context, secretName, key string) (string, error) {
	if secretName == "" {
		return "", nil
	}
	sec, err := b.client.CoreV1().Secrets(b.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(sec.Data[key]), nil
}
