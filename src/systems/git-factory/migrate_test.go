package main

import (
	"context"
	"testing"
)

// On a fresh database the composite key comes straight from the struct tags, so the
// fixup must be a no-op — and, crucially, a shard must be able to hold a primary AND
// a replica at once. That second assertion is the actual contract the pre-replica
// single-column key violated; see widenShardNodePK.
func TestShardNodePK_HoldsPrimaryAndReplicas(t *testing.T) {
	setupTestDB(t)

	// The fixup is dialect-guarded; on the SQLite test DB it must succeed and change
	// nothing.
	if err := widenShardNodePK(connect()); err != nil {
		t.Fatalf("widenShardNodePK on a fresh db: %v", err)
	}

	const shard = "ab"
	seedShardPlacement(t, shard, "http://primary:9002")
	seedReplica(t, shard, "http://replica-1:9002")
	seedReplica(t, shard, "http://replica-2:9002")

	nodes, err := resolvePlacement(context.Background(), shard)
	if err != nil {
		t.Fatalf("resolvePlacement: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("placement holds %d rows, want 3 (one primary + two replicas)", len(nodes))
	}
	// primary sorts first (see resolvePlacement's ORDER BY role).
	if nodes[0].Role != rolePrimary {
		t.Errorf("first row role = %q, want %q", nodes[0].Role, rolePrimary)
	}

	// The primary is still resolved unambiguously with replicas present.
	ref, err := resolveNode(context.Background(), "ab000000-0000-0000-0000-000000000001", shard)
	if err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	if ref.Address != "http://primary:9002" {
		t.Errorf("resolveNode address = %q, want the primary", ref.Address)
	}
}
