package main

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The external backend delegates credential custody to the External Secrets Operator.
// Builder creates an ExternalSecret CR that tells ESO to materialize a k8s Secret from a
// store path the admin populated (a static db_url, or a Vault DB-engine dynamic role for
// short-lived auto-revoked credentials). Builder holds only the reference — no database
// credential lives in builder at all. The pod's DATABASE_URL reads the materialized Secret.

const externalSecretsGroup = "external-secrets.io"

func externalSecretGVR(version string) schema.GroupVersionResource {
	if version == "" {
		version = "v1beta1"
	}
	return schema.GroupVersionResource{Group: externalSecretsGroup, Version: version, Resource: "externalsecrets"}
}

// externalDBSecretKey is the key ESO writes the URL under in the materialized Secret, and
// that the pod's DATABASE_URL ref reads.
const externalDBSecretKey = "database-url"

type externalProvider struct{}

func (externalProvider) resolve(ctx context.Context, b *k8sBackend, service, _ string) (dbResolution, error) {
	cfg := b.dbcfg
	if cfg.extStoreRef == "" {
		return dbResolution{}, fmt.Errorf("external backend requires BUILDER_DB_EXTERNAL_STORE_REF")
	}
	if cfg.extPathTemplate == "" {
		return dbResolution{}, fmt.Errorf("external backend requires BUILDER_DB_EXTERNAL_PATH_TEMPLATE")
	}
	targetSecret := b.name(service) + "-db"
	if err := b.ensureExternalSecret(ctx, service, targetSecret, cfg); err != nil {
		return dbResolution{}, err
	}
	return dbResolution{ref: &dbSecretRef{secretName: targetSecret, key: externalDBSecretKey}}, nil
}

// teardown deletes the builder-owned ExternalSecret on service removal. With ESO's Owner
// creation policy this also removes the materialized Secret — a credential reference, not
// data — which is safe once the pod is gone. The remote store is never touched.
func (externalProvider) teardown(ctx context.Context, b *k8sBackend, service string) {
	if b.dynamic == nil {
		return
	}
	name := b.name(service) + "-db"
	ns := b.namespace
	api := b.dynamic.Resource(externalSecretGVR(b.dbcfg.extVersion)).Namespace(ns)
	existing, err := api.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return // absent or unreadable — nothing to do
	}
	if existing.GetLabels()[labelManagedBy] != managedByValue {
		return // not ours
	}
	_ = api.Delete(ctx, name, metav1.DeleteOptions{})
}

// ensureExternalSecret creates the ExternalSecret CR for a service when absent (it does
// not overwrite an existing one, so admin edits survive).
func (b *k8sBackend) ensureExternalSecret(ctx context.Context, service, targetSecret string, cfg dbConfig) error {
	name := b.name(service) + "-db"
	ns := b.namespace
	api := b.dynamic.Resource(externalSecretGVR(cfg.extVersion)).Namespace(ns)

	_, err := api.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil // already exists
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get externalsecret %s: %w", name, err)
	}

	remoteRef := map[string]any{"key": externalExpand(cfg.extPathTemplate, service)}
	if cfg.extProperty != "" {
		remoteRef["property"] = cfg.extProperty
	}
	spec := map[string]any{
		"secretStoreRef": map[string]any{
			"name": cfg.extStoreRef,
			"kind": cfg.extStoreKind,
		},
		"target": map[string]any{
			"name":           targetSecret,
			"creationPolicy": "Owner",
		},
		"data": []any{
			map[string]any{
				"secretKey": externalDBSecretKey,
				"remoteRef": remoteRef,
			},
		},
	}
	if cfg.extRefresh != "" {
		spec["refreshInterval"] = cfg.extRefresh
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": externalSecretsGroup + "/" + externalSecretGVR(cfg.extVersion).Version,
		"kind":       "ExternalSecret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    labelsToAny(b.labels(service)),
		},
		"spec": spec,
	}}
	if _, err := api.Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create externalsecret %s: %w", name, err)
	}
	return nil
}

// externalExpand substitutes {service} in a remote-path template.
func externalExpand(tmpl, service string) string {
	return strings.ReplaceAll(tmpl, "{service}", serviceDBName(service))
}
