package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func TestBackendFor(t *testing.T) {
	cfg := dbConfig{backend: dbBackendSQL}
	if got := cfg.backendFor(nil); got != dbBackendSQL {
		t.Errorf("default backend = %q, want sql", got)
	}
	// A valid per-service override wins.
	if got := cfg.backendFor(map[string]any{"dbBackend": "cnpg"}); got != dbBackendCNPG {
		t.Errorf("override = %q, want cnpg", got)
	}
	// An invalid override is ignored (falls back to the global default).
	if got := cfg.backendFor(map[string]any{"dbBackend": "bogus"}); got != dbBackendSQL {
		t.Errorf("invalid override = %q, want sql fallback", got)
	}
	// An empty global default normalizes to manual.
	if got := (dbConfig{}).backendFor(nil); got != dbBackendManual {
		t.Errorf("empty default = %q, want manual", got)
	}
}

func TestBackendUsesForeignSecret(t *testing.T) {
	for b, want := range map[string]bool{
		dbBackendManual:   false,
		dbBackendSQL:      false,
		dbBackendCNPG:     true,
		dbBackendExternal: true,
	} {
		if got := backendUsesForeignSecret(b); got != want {
			t.Errorf("backendUsesForeignSecret(%q) = %v, want %v", b, got, want)
		}
	}
}

// databaseURLEnv returns the DATABASE_URL env var from a rendered pod template.
func databaseURLEnv(t *testing.T, pt corev1.PodTemplateSpec) corev1.EnvVar {
	t.Helper()
	for _, e := range pt.Spec.Containers[0].Env {
		if e.Name == "DATABASE_URL" {
			return e
		}
	}
	t.Fatal("DATABASE_URL env not found")
	return corev1.EnvVar{}
}

func TestTemplatePod_ForeignDBRef(t *testing.T) {
	b := &k8sBackend{prefix: "codearmory", registry: "reg", tag: "latest"}

	// Without a foreign ref, DATABASE_URL reads builder's own Secret.
	own := databaseURLEnv(t, b.templatePod(workloadSpec{Service: "blueprints"}))
	if own.ValueFrom == nil || own.ValueFrom.SecretKeyRef.Name != "codearmory-blueprints" || own.ValueFrom.SecretKeyRef.Key != "database-url" {
		t.Fatalf("own-secret DATABASE_URL ref wrong: %+v", own.ValueFrom)
	}

	// With a foreign ref, DATABASE_URL points at the operator/ESO Secret instead.
	spec := workloadSpec{Service: "blueprints", DBRef: &dbSecretRef{secretName: "pg-main-blueprints", key: "uri"}}
	got := databaseURLEnv(t, b.templatePod(spec))
	if got.ValueFrom == nil || got.ValueFrom.SecretKeyRef.Name != "pg-main-blueprints" || got.ValueFrom.SecretKeyRef.Key != "uri" {
		t.Fatalf("foreign DATABASE_URL ref wrong: %+v", got.ValueFrom)
	}
}

// newDynBackend builds a k8sBackend with a fake dynamic client that knows the CNPG and
// ESO list kinds, plus any seed objects.
func newDynBackend(cfg dbConfig, objs ...runtime.Object) *k8sBackend {
	scheme := runtime.NewScheme()
	lists := map[schema.GroupVersionResource]string{
		cnpgDatabaseGVR:              "DatabaseList",
		externalSecretGVR("v1beta1"): "ExternalSecretList",
	}
	dc := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, lists, objs...)
	return &k8sBackend{namespace: "codearmory", prefix: "codearmory", dynamic: dc, dbcfg: cfg}
}

func TestCNPGProvider_CreatesDatabaseAndRef(t *testing.T) {
	cfg := dbConfig{
		cnpgCluster:        "pg-main",
		cnpgCredSecretTmpl: "{cluster}-{service}",
		cnpgCredKey:        "uri",
	}
	b := newDynBackend(cfg)
	res, err := cnpgProvider{}.resolve(context.Background(), b, "blueprints", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.ref == nil || res.ref.secretName != "pg-main-blueprints" || res.ref.key != "uri" {
		t.Fatalf("ref wrong: %+v", res.ref)
	}
	// The Database CR was created with the right cluster/owner/name.
	got, err := b.dynamic.Resource(cnpgDatabaseGVR).Namespace("codearmory").Get(context.Background(), "codearmory-blueprints-db", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get database CR: %v", err)
	}
	spec, _, _ := unstructured.NestedMap(got.Object, "spec")
	if spec["name"] != "blueprints" || spec["owner"] != "svc_blueprints" {
		t.Errorf("database spec wrong: %v", spec)
	}
	cluster, _, _ := unstructured.NestedString(got.Object, "spec", "cluster", "name")
	if cluster != "pg-main" {
		t.Errorf("cluster = %q, want pg-main", cluster)
	}
}

func TestCNPGProvider_RequiresConfig(t *testing.T) {
	b := newDynBackend(dbConfig{})
	if _, err := (cnpgProvider{}).resolve(context.Background(), b, "blueprints", ""); err == nil {
		t.Fatal("expected error when cnpg cluster unset")
	}
}

func TestExternalProvider_CreatesExternalSecretAndRef(t *testing.T) {
	cfg := dbConfig{
		extStoreRef:     "vault-backend",
		extStoreKind:    "ClusterSecretStore",
		extPathTemplate: "codearmory/{service}/db",
		extProperty:     "url",
		extVersion:      "v1beta1",
		extRefresh:      "1h",
	}
	b := newDynBackend(cfg)
	res, err := externalProvider{}.resolve(context.Background(), b, "tickets", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.ref == nil || res.ref.secretName != "codearmory-tickets-db" || res.ref.key != externalDBSecretKey {
		t.Fatalf("ref wrong: %+v", res.ref)
	}
	got, err := b.dynamic.Resource(externalSecretGVR("v1beta1")).Namespace("codearmory").Get(context.Background(), "codearmory-tickets-db", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get externalsecret: %v", err)
	}
	storeName, _, _ := unstructured.NestedString(got.Object, "spec", "secretStoreRef", "name")
	storeKind, _, _ := unstructured.NestedString(got.Object, "spec", "secretStoreRef", "kind")
	if storeName != "vault-backend" || storeKind != "ClusterSecretStore" {
		t.Errorf("store ref wrong: %s/%s", storeKind, storeName)
	}
	target, _, _ := unstructured.NestedString(got.Object, "spec", "target", "name")
	if target != "codearmory-tickets-db" {
		t.Errorf("target secret = %q", target)
	}
	data, _, _ := unstructured.NestedSlice(got.Object, "spec", "data")
	if len(data) != 1 {
		t.Fatalf("expected one data entry, got %v", data)
	}
	entry := data[0].(map[string]any)
	rr := entry["remoteRef"].(map[string]any)
	if rr["key"] != "codearmory/tickets/db" || rr["property"] != "url" {
		t.Errorf("remoteRef wrong: %v", rr)
	}
}

func TestExternalProvider_Teardown(t *testing.T) {
	cfg := dbConfig{extStoreRef: "s", extPathTemplate: "p/{service}", extVersion: "v1beta1"}
	b := newDynBackend(cfg)
	if _, err := (externalProvider{}).resolve(context.Background(), b, "tickets", ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	externalProvider{}.teardown(context.Background(), b, "tickets")
	_, err := b.dynamic.Resource(externalSecretGVR("v1beta1")).Namespace("codearmory").Get(context.Background(), "codearmory-tickets-db", metav1.GetOptions{})
	if err == nil {
		t.Fatal("expected ExternalSecret to be deleted on teardown")
	}
}

func TestDBProviderFor(t *testing.T) {
	b := newDynBackend(dbConfig{})
	if _, ok := b.dbProviderFor(dbBackendCNPG).(cnpgProvider); !ok {
		t.Error("cnpg backend should yield cnpgProvider")
	}
	if _, ok := b.dbProviderFor(dbBackendExternal).(externalProvider); !ok {
		t.Error("external backend should yield externalProvider")
	}
	if _, ok := b.dbProviderFor(dbBackendSQL).(manualProvider); !ok {
		t.Error("sql backend should yield manualProvider at reconcile")
	}
	// With no dynamic client, cnpg/external degrade to passthrough.
	nob := &k8sBackend{}
	if _, ok := nob.dbProviderFor(dbBackendCNPG).(manualProvider); !ok {
		t.Error("cnpg without dynamic client should degrade to manualProvider")
	}
}
