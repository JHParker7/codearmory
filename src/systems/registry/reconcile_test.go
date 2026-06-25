package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// reconcileManifest must re-ingest a manifest service that has gone missing from
// the DB (e.g. the database was cleared without restarting the registry), so the
// catalog self-heals without a manual restart — and must be a no-op when the
// service is already present, to avoid churning the catalog every tick.
func TestReconcileManifest_HealsMissingService(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := "reconcile-test-" + uuid.New().String()[:8]

	manifest := `[{"name":"` + svc + `","url":"http://x:1","description":"d",` +
		`"forward_auth":false,"service_key":"k",` +
		`"endpoints":[{"method":"GET","path":"/h","action":"health","resource":"` + svc + `/health","public":true}],` +
		`"actions":[{"name":"` + svc + `/run","method":"POST","path":"/run"}]}]`
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	t.Cleanup(func() {
		var id string
		connect().Raw(`SELECT service_id FROM services WHERE name = ?`, svc).Scan(&id)
		if id != "" {
			connect().Exec(`DELETE FROM service_actions WHERE service_id = ?`, id)   //nolint:errcheck
			connect().Exec(`DELETE FROM service_endpoints WHERE service_id = ?`, id) //nolint:errcheck
			connect().Exec(`DELETE FROM services WHERE service_id = ?`, id)          //nolint:errcheck
		}
	})

	// Service absent (DB never had it) → reconcile re-ingests it.
	healed := reconcileManifest(ctx, path)
	if len(healed) != 1 || healed[0] != svc {
		t.Fatalf("reconcile should heal %q, got %v", svc, healed)
	}

	// The action is now served by the exact join listAllActions uses to build the
	// workflows catalog (active service AND active action) — this also guards the
	// default:true active behavior that a fresh-DB ingest depends on.
	var n int64
	if err := connect().Raw(
		`SELECT count(1) FROM service_actions sa JOIN services s ON s.service_id = sa.service_id
		 WHERE s.name = ? AND sa.name = ? AND sa.active = true AND s.active = true`,
		svc, svc+"/run").Scan(&n).Error; err != nil {
		t.Fatalf("query action: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected %s/run active in the catalog after heal, got %d", svc, n)
	}

	// Present now → reconcile is a no-op (no churn when healthy).
	if again := reconcileManifest(ctx, path); len(again) != 0 {
		t.Errorf("reconcile should be a no-op when the service is present, got %v", again)
	}
}
