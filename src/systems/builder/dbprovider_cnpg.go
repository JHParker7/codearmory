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

// The cnpg backend provisions a database through the CloudNativePG operator: builder
// creates a Database CR (declarative, idempotent) and points the pod's DATABASE_URL at
// a Secret the operator/admin owns. Builder holds only k8s RBAC on the CR — no database
// credential — so a builder compromise cannot read or alter the database directly.
//
// CNPG's Database CR provisions the database against an existing Cluster. The connection
// credential is delivered by a Secret the admin tells builder about (the cluster's app
// secret, or a managed-role passwordSecret) via BUILDER_DB_CNPG_CRED_SECRET_TEMPLATE +
// BUILDER_DB_CNPG_CRED_KEY — builder does not create or read it.

var cnpgDatabaseGVR = schema.GroupVersionResource{
	Group:    "postgresql.cnpg.io",
	Version:  "v1",
	Resource: "databases",
}

type cnpgProvider struct{}

func (cnpgProvider) resolve(ctx context.Context, b *k8sBackend, service, _ string) (dbResolution, error) {
	cfg := b.dbcfg
	if cfg.cnpgCluster == "" {
		return dbResolution{}, fmt.Errorf("cnpg backend requires BUILDER_DB_CNPG_CLUSTER")
	}
	if cfg.cnpgCredSecretTmpl == "" {
		return dbResolution{}, fmt.Errorf("cnpg backend requires BUILDER_DB_CNPG_CRED_SECRET_TEMPLATE")
	}
	dbName := serviceDBName(service)
	if !validDBIdentifier(dbName) {
		return dbResolution{}, fmt.Errorf("service %q is not a valid database identifier", service)
	}
	owner := strings.ToLower(strings.TrimSpace(cfg.sqlOwnerRole))
	if owner == "" {
		owner = roleNameForService(service)
	}

	ns := cfg.cnpgNamespace
	if ns == "" {
		ns = b.namespace
	}
	if err := b.ensureCNPGDatabase(ctx, ns, service, dbName, owner); err != nil {
		return dbResolution{}, err
	}

	key := cfg.cnpgCredKey
	if key == "" {
		key = "uri"
	}
	return dbResolution{ref: &dbSecretRef{
		secretName: cnpgExpand(cfg.cnpgCredSecretTmpl, cfg.cnpgCluster, service),
		key:        key,
	}}, nil
}

// teardown intentionally leaves the Database CR in place: depending on the operator's
// reclaim policy, deleting it could drop the database. Removal is a deliberate admin action.
func (cnpgProvider) teardown(context.Context, *k8sBackend, string) {}

// ensureCNPGDatabase creates the CNPG Database CR for a service when absent. It does not
// overwrite an existing CR, so admin edits to the resource are never clobbered.
func (b *k8sBackend) ensureCNPGDatabase(ctx context.Context, namespace, service, dbName, owner string) error {
	name := b.name(service) + "-db"
	api := b.dynamic.Resource(cnpgDatabaseGVR).Namespace(namespace)

	_, err := api.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil // already exists
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get cnpg database %s: %w", name, err)
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": cnpgDatabaseGVR.Group + "/" + cnpgDatabaseGVR.Version,
		"kind":       "Database",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    labelsToAny(b.labels(service)),
		},
		"spec": map[string]any{
			"cluster": map[string]any{"name": b.dbcfg.cnpgCluster},
			"name":    dbName,
			"owner":   owner,
		},
	}}
	if _, err := api.Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create cnpg database %s: %w", name, err)
	}
	return nil
}

// cnpgExpand substitutes {cluster} and {service} in a Secret-name template. With no
// placeholder it returns the template verbatim (a single shared secret name).
func cnpgExpand(tmpl, cluster, service string) string {
	out := strings.ReplaceAll(tmpl, "{cluster}", cluster)
	out = strings.ReplaceAll(out, "{service}", serviceDBName(service))
	return out
}

// labelsToAny converts a string label map to the map[string]any form unstructured needs.
func labelsToAny(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
