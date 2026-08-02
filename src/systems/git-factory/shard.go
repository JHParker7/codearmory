package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"gorm.io/gorm"
)

// The routing table (ARCHITECTURE §5 Step 2). Every git operation resolves
// repo → shard → node address before any byte is touched, even though there is
// exactly one node today and the resolution therefore always answers "local".
//
// The point is not the indirection itself but WHEN it is added: with the lookup already
// on every path, Step 3 is swapping the body of three gitplane functions for a reverse
// proxy. Added later, it is a new concept threaded through every call site instead.
//
// An empty table means every shard is local, so a single-node install needs no
// configuration and behaves byte-identically to no routing at all.

// Placement roles (ARCHITECTURE §5 Step 4). Writes go to the primary; reads may fan
// out to a replica that has caught up.
const (
	rolePrimary = "primary"
	roleReplica = "replica"
)

// ShardNode places one git-node in a shard's replica set. A shard has exactly one
// primary row and zero or more replica rows; the composite (Shard, Address) key is what
// lets a shard have several. A single legacy row defaults to role=primary, so the
// pre-replica table (one node per shard) keeps working unchanged.
type ShardNode struct {
	// Shard is the 2-char id prefix (see shardOf) — 256 buckets, stable across renames.
	Shard string `gorm:"primaryKey" json:"shard"`
	// Address is the node's base URL, e.g. "http://git-node-2:9002". Empty means this
	// process owns the shard (the local node).
	Address string `gorm:"primaryKey" json:"address"`
	// Role is "primary" (authoritative, takes writes) or "replica" (read fan-out).
	Role string `gorm:"default:primary" json:"role"`
	// Healthy gates a replica out of read selection when it is known-down, without
	// deleting its placement row.
	Healthy   bool      `gorm:"default:true" json:"healthy"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ReplicaState tracks, per (repo, replica), the highest repo Version that replica has
// fetched. Replica lag is per-repo — a shard holds many repos, each pushed independently
// — so this cannot live on ShardNode (which is per-shard). A read is served from a
// replica only when its Applied >= the repo's current Version (see pickReadNode), which
// is what prevents a just-pushed commit from being served stale.
type ReplicaState struct {
	RepoID    string    `gorm:"primaryKey" json:"repo_id"`
	Address   string    `gorm:"primaryKey" json:"address"`
	Applied   int64     `json:"applied"`
	UpdatedAt time.Time `json:"updated_at"`
}

// errRemoteNodeUnsupported is returned when a shard resolves to a node that is not this
// process. Serving it needs the reverse proxy from Step 3; until that exists, failing
// loudly beats silently reading a local path that holds no such repository.
var errRemoteNodeUnsupported = errors.New("shard is placed on a remote git-node; reverse proxy (ARCHITECTURE §5 Step 3) is not implemented")

// nodeRef is the resolved placement of one repo's bytes.
type nodeRef struct {
	Shard   string
	Address string
	Local   bool
}

// selfNodeAddress is this node's own address, so a routing row pointing at us resolves
// to the local path rather than a proxy loop back into ourselves. Unset (the norm for a
// single-node install) simply means only empty-address rows are local.
func selfNodeAddress() string {
	return strings.TrimRight(envOrDefault("GIT_NODE_ADDRESS", ""), "/")
}

// resolveNode answers which node holds a repo's bytes. Unmapped shards are local, which
// is what makes the table optional: a fresh install routes everything here.
func resolveNode(ctx context.Context, repoID, shard string) (nodeRef, error) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "shard.resolveNode")
	defer span.End()

	if shard == "" {
		// Rows written before Shard was populated (or by a caller that did not set it)
		// still resolve, since the key is derivable from the id at any time.
		shard = shardOf(repoID)
	}
	span.SetAttributes(attribute.String("Repo.id", repoID), attribute.String("shard", shard))

	var sn ShardNode
	// The PRIMARY owns the bytes for the shard — the write target and the node
	// localDirFor serves from. A shard with no primary row is unmapped → local.
	err := connectRead().WithContext(ctx).
		Where("shard = ? AND role = ?", shard, rolePrimary).First(&sn).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nodeRef{Shard: shard, Local: true}, nil
	}
	if err != nil {
		return nodeRef{}, err
	}
	return newNodeRef(shard, sn.Address), nil
}

// newNodeRef normalizes an address and marks whether it is this process.
func newNodeRef(shard, address string) nodeRef {
	addr := strings.TrimRight(address, "/")
	return nodeRef{Shard: shard, Address: addr, Local: addr == "" || addr == selfNodeAddress()}
}

// resolvePlacement returns every node in a shard's replica set (primary first). An empty
// result means the shard is unmapped (single-node install) — the caller serves locally.
func resolvePlacement(ctx context.Context, shard string) ([]ShardNode, error) {
	var nodes []ShardNode
	// primary sorts before replica alphabetically, so ORDER BY role puts it first.
	err := connectRead().WithContext(ctx).
		Where("shard = ?", shard).Order("role").Find(&nodes).Error
	return nodes, err
}

// pickReadNode chooses where to serve a READ (clone/fetch) of repoID, given its current
// Version. Selection, in order of preference:
//  1. the local node, if it is the primary or a caught-up healthy replica (no hop);
//  2. any caught-up healthy replica (offload the primary);
//  3. the primary (always authoritative — never stale).
//
// "Caught up" means the replica's ReplicaState.Applied >= version, which is the gate
// that stops a replica serving a clone that predates the latest push.
func pickReadNode(ctx context.Context, repoID string, version int64) (nodeRef, error) {
	shard := shardOf(repoID)
	nodes, err := resolvePlacement(ctx, shard)
	if err != nil {
		return nodeRef{}, err
	}
	if len(nodes) == 0 {
		return nodeRef{Shard: shard, Local: true}, nil // unmapped → local
	}

	applied := repoAppliedVersions(ctx, repoID) // address -> applied
	var primary nodeRef
	var eligibleReplicas []nodeRef
	for _, n := range nodes {
		ref := newNodeRef(shard, n.Address)
		if n.Role == rolePrimary {
			primary = ref
			continue
		}
		if n.Healthy && applied[ref.Address] >= version {
			eligibleReplicas = append(eligibleReplicas, ref)
		}
	}
	// 1. Prefer serving locally with no proxy hop.
	if primary.Local {
		return primary, nil
	}
	for _, r := range eligibleReplicas {
		if r.Local {
			return r, nil
		}
	}
	// 2. Any caught-up replica offloads the primary.
	if len(eligibleReplicas) > 0 {
		return eligibleReplicas[0], nil
	}
	// 3. Fall back to the authoritative primary.
	return primary, nil
}

// repoAppliedVersions returns each replica's applied version for a repo, keyed by the
// normalized address. A missing row reads as 0 (never synced), so it is never eligible
// until it catches up.
func repoAppliedVersions(ctx context.Context, repoID string) map[string]int64 {
	var states []ReplicaState
	if err := connectRead().WithContext(ctx).Where("repo_id = ?", repoID).Find(&states).Error; err != nil {
		return map[string]int64{}
	}
	out := make(map[string]int64, len(states))
	for _, s := range states {
		out[strings.TrimRight(s.Address, "/")] = s.Applied
	}
	return out
}
