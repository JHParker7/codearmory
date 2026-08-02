package main

import (
	"fmt"

	"gorm.io/gorm"
)

// Schema fixups that AutoMigrate cannot perform itself. GORM adds columns and
// indexes, but it never alters an existing table's PRIMARY KEY — so a constraint
// that was correct under an older model stays wrong forever on an upgraded install
// while looking perfectly healthy on a fresh one.

// widenShardNodePK upgrades shard_nodes from the pre-replica primary key (shard) to
// the composite (shard, address).
//
// Before read replicas a shard had exactly one node, so `shard` alone was a valid
// key. The replica set model (ARCHITECTURE §5 Step 4) puts one primary row and N
// replica rows in the SAME shard, which a single-column key physically forbids:
// inserting a replica collides with the primary on the PK. AutoMigrate adds the new
// role/healthy columns but leaves the old constraint in place, so an upgraded
// cluster would accept the Phase 4 binary and then silently fail every replica
// registration — while a fresh install (composite key created from the struct tags)
// works. This closes that gap.
//
// Idempotent: it inspects the live constraint and returns immediately once the key
// already spans both columns, so it is safe on every boot and on fresh databases.
func widenShardNodePK(db *gorm.DB) error {
	// information_schema and ALTER TABLE ... DROP CONSTRAINT are Postgres-specific.
	// SQLite (tests) builds the table from the struct tags every time, so it always
	// has the composite key and needs no fixup.
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	if !db.Migrator().HasTable(&ShardNode{}) {
		return nil // fresh install: AutoMigrate creates the composite key from the tags
	}

	var cols int64
	// Count the columns in the table's PRIMARY KEY. 2 means the composite key is
	// already in place (fresh install, or a previous run of this migration).
	const countPKCols = `
		SELECT count(*)
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name
		 AND tc.table_schema = kcu.table_schema
		WHERE tc.table_name = 'shard_nodes'
		  AND tc.constraint_type = 'PRIMARY KEY'`
	if err := db.Raw(countPKCols).Scan(&cols).Error; err != nil {
		return fmt.Errorf("inspect shard_nodes primary key: %w", err)
	}
	if cols == 0 || cols >= 2 {
		// 0: no PK at all (a hand-made table) — leave it alone rather than guess.
		// >=2: already composite.
		return nil
	}

	// A NULL address cannot participate in a primary key. Empty string is the
	// model's "this node" sentinel (see newNodeRef), so it is the correct fill.
	if err := db.Exec(`UPDATE shard_nodes SET address = '' WHERE address IS NULL`).Error; err != nil {
		return fmt.Errorf("normalize shard_nodes.address: %w", err)
	}
	// Drop by lookup rather than the conventional name: a table created by hand may
	// carry a differently-named constraint.
	var pkName string
	const pkNameQuery = `
		SELECT tc.constraint_name
		FROM information_schema.table_constraints tc
		WHERE tc.table_name = 'shard_nodes' AND tc.constraint_type = 'PRIMARY KEY'
		LIMIT 1`
	if err := db.Raw(pkNameQuery).Scan(&pkName).Error; err != nil {
		return fmt.Errorf("resolve shard_nodes primary key name: %w", err)
	}
	if pkName == "" {
		return nil
	}
	if err := db.Exec(`ALTER TABLE shard_nodes DROP CONSTRAINT ` + quoteIdent(pkName)).Error; err != nil {
		return fmt.Errorf("drop shard_nodes primary key: %w", err)
	}
	// Postgres marks both columns NOT NULL as part of adding the key.
	if err := db.Exec(`ALTER TABLE shard_nodes ADD PRIMARY KEY (shard, address)`).Error; err != nil {
		return fmt.Errorf("add composite shard_nodes primary key: %w", err)
	}
	return nil
}

// quoteIdent double-quotes a SQL identifier read back from the catalog, so a
// non-conventional constraint name cannot break the statement.
func quoteIdent(s string) string {
	out := make([]rune, 0, len(s)+2)
	out = append(out, '"')
	for _, r := range s {
		if r == '"' {
			out = append(out, '"')
		}
		out = append(out, r)
	}
	return string(append(out, '"'))
}
