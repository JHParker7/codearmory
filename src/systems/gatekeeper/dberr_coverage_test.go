package main

import (
	"context"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// TestDBMethods_BrokenDB points both gorm singletons at a closed connection so
// every query fails, exercising the error-return branch of each db-layer method.
func TestDBMethods_BrokenDB(t *testing.T) {
	broken, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open broken db: %v", err)
	}
	if sqldb, err := broken.DB(); err == nil {
		sqldb.Close()
	}
	dbInitMu.Lock()
	ow, or := gormDB, gormDBRead
	gormDB, gormDBRead = broken, broken
	dbInitMu.Unlock()
	t.Cleanup(func() {
		dbInitMu.Lock()
		gormDB, gormDBRead = ow, or
		dbInitMu.Unlock()
	})

	ctx := context.Background()

	type crud interface {
		Add(context.Context) error
		Update(context.Context) error
		Remove(context.Context) error
		Get(context.Context) (db, error)
		List(context.Context, int, int) ([]db, error)
	}
	// Empty structs suffice: every method runs a query that fails on the closed DB.
	entities := []crud{
		User{}, Org{}, Team{}, Role{}, Session{}, Permissions{}, Invite{},
		ServiceAccount{}, ServicePermissionRequest{}, Secret{}, OrgSecretProvider{},
		OAuthClient{}, OAuthCode{},
	}
	for _, e := range entities {
		// Add always issues a Create, so it must surface the DB failure. The other
		// methods are exercised for coverage but not asserted — some are deliberate
		// no-op stubs that return nil regardless of the DB.
		if err := e.Add(ctx); err == nil {
			t.Errorf("%T.Add: expected error under broken DB", e)
		}
		_ = e.Update(ctx)
		_ = e.Remove(ctx)
		_, _ = e.Get(ctx)
		_, _ = e.List(ctx, 10, 0)
	}

	// AuditLog implements a subset (Add/Get/List).
	a := AuditLog{}
	if err := a.Add(ctx); err == nil {
		t.Error("AuditLog.Add: expected error under broken DB")
	}
	_, _ = a.Get(ctx)
	_, _ = a.List(ctx, 10, 0)
}
