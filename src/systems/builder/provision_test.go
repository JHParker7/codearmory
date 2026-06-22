package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// stubGatekeeperRegister records the bodies POSTed to /internal/service-accounts.
type registerRecorder struct {
	mu      sync.Mutex
	bodies  []map[string]string
	authHdr string
}

func newStubGatekeeper(t *testing.T, rec *registerRecorder) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, body)
		rec.authHdr = r.Header.Get("Authorization")
		rec.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newTestBackend(t *testing.T, rec *registerRecorder, objs ...runtime.Object) *k8sBackend {
	t.Helper()
	cs := fake.NewSimpleClientset(objs...)
	return &k8sBackend{
		client:    cs,
		namespace: "codearmory",
		prefix:    "codearmory",
		prov: provisioningConfig{
			enabled:             true,
			gatekeeperURL:       newStubGatekeeper(t, rec),
			internalKey:         "internal-key",
			conductorSecretName: "codearmory-conductor",
		},
	}
}

func TestProvision_CreatesIdentityAndSecret(t *testing.T) {
	httpClient = initHTTPClient()
	rec := &registerRecorder{}
	// conductor secret holds the shared forward key builder copies into services.
	conductor := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "codearmory-conductor", Namespace: "codearmory"},
		Data:       map[string][]byte{"conductor-forward-key": []byte("fwd-key")},
	}
	b := newTestBackend(t, rec, conductor)

	ctx := context.Background()
	if err := b.provision(ctx, "forge", "postgres://forge:pw@db:5432/forge"); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// gatekeeper identity registered for forge, with the shared internal key.
	if len(rec.bodies) != 1 || rec.bodies[0]["service_name"] != "forge" || rec.bodies[0]["key"] == "" {
		t.Fatalf("unexpected register calls: %+v", rec.bodies)
	}
	if rec.authHdr != "Bearer internal-key" {
		t.Fatalf("register auth = %q, want Bearer internal-key", rec.authHdr)
	}

	// the service Secret now carries db url + gatekeeper key + conductor-forward-key.
	sec, err := b.client.CoreV1().Secrets("codearmory").Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service secret: %v", err)
	}
	if string(sec.Data["database-url"]) != "postgres://forge:pw@db:5432/forge" {
		t.Errorf("database-url = %q", sec.Data["database-url"])
	}
	if string(sec.Data["conductor-forward-key"]) != "fwd-key" {
		t.Errorf("conductor-forward-key = %q", sec.Data["conductor-forward-key"])
	}
	gkKey := string(sec.Data["gatekeeper-service-key"])
	if gkKey == "" {
		t.Error("gatekeeper-service-key missing")
	}
	if sec.Labels[labelManagedBy] != managedByValue {
		t.Errorf("secret not labelled managed-by builder: %v", sec.Labels)
	}

	// Re-provisioning must reuse the existing key (no pod churn) and register again.
	if err := b.provision(ctx, "forge", "postgres://forge:pw@db:5432/forge"); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	sec2, _ := b.client.CoreV1().Secrets("codearmory").Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if string(sec2.Data["gatekeeper-service-key"]) != gkKey {
		t.Error("re-provision changed the gatekeeper key (would churn the pod)")
	}
	if len(rec.bodies) != 2 {
		t.Errorf("expected 2 register calls, got %d", len(rec.bodies))
	}
}

func TestDeprovision_DeletesManagedSecret(t *testing.T) {
	httpClient = initHTTPClient()
	rec := &registerRecorder{}
	b := newTestBackend(t, rec)
	ctx := context.Background()
	if err := b.provision(ctx, "tickets", ""); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := b.client.CoreV1().Secrets("codearmory").Get(ctx, "codearmory-tickets", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected secret to exist: %v", err)
	}
	b.deprovision(ctx, "tickets")
	if _, err := b.client.CoreV1().Secrets("codearmory").Get(ctx, "codearmory-tickets", metav1.GetOptions{}); err == nil {
		t.Fatal("expected the builder-managed secret to be deleted")
	}
}
