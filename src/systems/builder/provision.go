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
)

// Provisioning lets builder bring a non-core service online with no Helm change.
// Before deploying the workload it ensures the two things Helm would otherwise
// provide: a gatekeeper service identity (registered at runtime) and the service's
// Secret (the admin-supplied DB URL + a generated gatekeeper key + the shared
// conductor-forward key). It never creates the database — the admin supplies a URL
// for a role already scoped to an existing database. All secret reads/writes go
// through the secretStore seam (see secretstore.go) so a Vault backend can replace
// the default k8s-Secret store without touching this orchestration.

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

// provision ensures the gatekeeper identity and Secret exist for a service. dbURL is
// the decrypted admin-supplied database URL ("" when none); secrets is the decrypted
// admin-supplied sensitive config (env-key → value), written under conventional keys.
func (b *k8sBackend) provision(ctx context.Context, service, dbURL string, secrets map[string]string) error {
	key, err := b.ensureGatekeeperIdentity(ctx, service)
	if err != nil {
		return fmt.Errorf("gatekeeper identity: %w", err)
	}
	if err := b.ensureServiceSecret(ctx, service, key, dbURL, secrets); err != nil {
		return fmt.Errorf("service secret: %w", err)
	}
	return nil
}

// deprovision removes what builder created for a torn-down service: its Secret (if
// builder owns it). The gatekeeper identity is left in place (harmless without a
// running pod, and re-registered idempotently on the next enable).
func (b *k8sBackend) deprovision(ctx context.Context, service string) {
	if err := b.store().deleteIfManaged(ctx, b.name(service), labelManagedBy, managedByValue); err != nil {
		slog.WarnContext(ctx, "deprovision: delete secret failed", "service", service, "error", err)
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

// ensureServiceSecret merges the keys builder manages into the service's Secret via
// the secretStore (creating it, labelled managed-by builder, when absent). The store's
// merge preserves keys it does not set, so it never clobbers a chart-provisioned
// Secret's other keys nor the rotating gatekeeper-service-key.
func (b *k8sBackend) ensureServiceSecret(ctx context.Context, service, gkKey, dbURL string, secrets map[string]string) error {
	name := b.name(service)
	data := map[string][]byte{"gatekeeper-service-key": []byte(gkKey)}
	if dbURL != "" {
		data["database-url"] = []byte(dbURL)
	}
	// Admin-supplied sensitive config, stored under its conventional Secret key
	// (REDIS_URL → redis-url, GITEA_ADMIN_TOKEN → gitea-admin-token). templatePod wires
	// these as secret refs for the service's secretConfig env vars.
	for envKey, val := range secrets {
		data[secretKeyForEnv(envKey)] = []byte(val)
	}
	// Revoke any of the service's declared secretConfig keys the admin is no longer
	// supplying, so clearing a secret actually removes it from the live Secret rather
	// than leaving the stale value wired into the pod. DATABASE_URL has its own write
	// path (database-url, set above when dbURL is present) and is not pruned here.
	var remove []string
	if def, ok := embeddedServiceDef(service); ok {
		for _, envKey := range def.SecretConfig {
			if envKey == "DATABASE_URL" {
				continue
			}
			_, supplied := secrets[envKey]
			// Managed Redis: with no external REDIS_URL supplied, builder points the
			// service at the in-cluster store ensureManagedRedis deploys and keeps the
			// key wired (rather than pruning it as a cleared admin secret).
			if envKey == "REDIS_URL" && def.Infra.ManagedRedis && !supplied {
				data["redis-url"] = []byte(b.managedRedisURL(service))
				continue
			}
			if !supplied {
				remove = append(remove, secretKeyForEnv(envKey))
			}
		}
	}
	// Write the service's derived secrets (cross-service shared keys + per-service
	// crypto keys) and, when it pulls from the registry, its derived registry read-key.
	// Deterministic, so every reconcile writes byte-identical values (no pod churn).
	if def, ok := embeddedServiceDef(service); ok && secretDerivationEnabled() {
		for _, ds := range def.DerivedSecrets {
			if ds.Kind != "shared" {
				data[ds.Name] = []byte(derivePrivateKey(service, ds.Name))
				continue
			}
			// A shared key is only shared if BOTH sides hold the same bytes. Derivation
			// alone guarantees that only among services builder provisions: a peer the
			// Helm chart deployed (hooks, say) carries a chart-generated key instead, so
			// a derived value would be silently wrong — the emitter signs, the receiver
			// rejects with 401, and nothing reports a misconfiguration. So prefer the
			// owning service's existing value and derive only when there is none.
			if peer := sharedKeyOwner(ds.Name); peer != "" && peer != service {
				if existing, err := b.store().get(ctx, b.name(peer), ds.Name); err == nil && existing != "" {
					data[ds.Name] = []byte(existing)
					continue
				}
			}
			data[ds.Name] = []byte(deriveSharedKey(ds.Name))
		}
		if def.RegistryAccount {
			data["registry-service-key"] = []byte(derivePrivateKey(service, "registry-service-key"))
		}
	}
	// Fill the shared conductor-forward-key only when the service's bundle lacks it,
	// reading it from conductor's own secret. Best-effort: a transient read just skips
	// it this round rather than failing the whole secret reconcile (merge below still
	// aborts on a hard error reading/writing the service bundle).
	if cur, err := b.store().get(ctx, name, "conductor-forward-key"); err == nil && cur == "" {
		if cfk, err := b.store().get(ctx, b.prov.conductorSecretName, "conductor-forward-key"); err == nil && cfk != "" {
			data["conductor-forward-key"] = []byte(cfk)
		}
	}
	return b.store().merge(ctx, name, b.labels(service), data, remove)
}

// existingSecretKey reads one key from the service's own Secret. A missing Secret
// returns ("", nil); a transient backend error is propagated so callers can retry
// rather than mistake it for "no value".
func (b *k8sBackend) existingSecretKey(ctx context.Context, service, key string) (string, error) {
	return b.store().get(ctx, b.name(service), key)
}
