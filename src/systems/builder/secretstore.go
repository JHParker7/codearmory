package main

import (
	"context"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// secretStore is where builder reads and writes a service's secret bundle. The default
// backend stores Kubernetes Secrets; the interface is the seam at which a future
// Vault-backed store (gatekeeper's planned Vault option / global.externalSecrets) drops
// in without the reconciler or provisioner changing. Names are raw bundle names
// (e.g. "codearmory-forge"); keys are the conventional per-service keys
// (database-url, gatekeeper-service-key, redis-url, …).
type secretStore interface {
	// get reads one key from a bundle. ("", nil) when the bundle or key is absent; a
	// transient backend error is returned (never swallowed) so callers can retry rather
	// than mistake it for "no value".
	get(ctx context.Context, name, key string) (string, error)
	// merge upserts data into a bundle, creating it (with labels) when absent and
	// preserving keys it does not set — so it never clobbers values another writer or a
	// prior reconcile stored (notably the rotating gatekeeper-service-key). Keys listed
	// in remove are deleted from the bundle, so an admin secret that was cleared is
	// actually revoked rather than lingering.
	merge(ctx context.Context, name string, labels map[string]string, data map[string][]byte, remove []string) error
	// deleteIfManaged removes a bundle only when it carries managedLabel=managedValue, so
	// builder never deletes a secret it does not own.
	deleteIfManaged(ctx context.Context, name, managedLabel, managedValue string) error
}

// k8sSecretStore is the default secretStore, backed by Kubernetes Secrets.
type k8sSecretStore struct {
	client    kubernetes.Interface
	namespace string
}

func (s *k8sSecretStore) get(ctx context.Context, name, key string) (string, error) {
	if name == "" {
		return "", nil
	}
	sec, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(sec.Data[key]), nil
}

func (s *k8sSecretStore) merge(ctx context.Context, name string, labels map[string]string, data map[string][]byte, remove []string) error {
	api := s.client.CoreV1().Secrets(s.namespace)
	existing, err := api.Get(ctx, name, metav1.GetOptions{})
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		return err
	}
	merged := map[string][]byte{}
	if !create && existing.Data != nil {
		maps.Copy(merged, existing.Data)
	}
	maps.Copy(merged, data)
	for _, k := range remove {
		delete(merged, k)
	}
	if create {
		_, err = api.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace, Labels: labels},
			Type:       corev1.SecretTypeOpaque,
			Data:       merged,
		}, metav1.CreateOptions{})
		return err
	}
	existing.Data = merged
	_, err = api.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func (s *k8sSecretStore) deleteIfManaged(ctx context.Context, name, managedLabel, managedValue string) error {
	sec, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if sec.Labels[managedLabel] != managedValue {
		return nil
	}
	if err := s.client.CoreV1().Secrets(s.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
