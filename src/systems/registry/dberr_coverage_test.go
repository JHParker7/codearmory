package main

import (
	"context"
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// TestDBMethods_BrokenDB opens a real connection then closes it, so every query
// fails ("database is closed"), exercising each db-layer method's error branch.
func TestDBMethods_BrokenDB(t *testing.T) {
	requireDB(t)
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgresql://postgres:postgres@localhost:5432/registry"
	}
	broken, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if sqldb, err := broken.DB(); err == nil {
		sqldb.Close() // subsequent queries → "sql: database is closed"
	}
	gormDBMu.Lock()
	orig := gormDB
	gormDB = broken
	gormDBMu.Unlock()
	t.Cleanup(func() {
		gormDBMu.Lock()
		gormDB = orig
		gormDBMu.Unlock()
	})

	ctx := context.Background()
	mustErr := func(name string, err error) {
		if err == nil {
			t.Errorf("%s: expected error under broken DB", name)
		}
	}

	mustErr("ServiceModel.Add", (ServiceModel{ServiceID: "x", Name: "n", URL: "http://x"}).Add(ctx))
	mustErr("ServiceModel.Update", (ServiceModel{ServiceID: "x"}).Update(ctx))
	mustErr("ServiceModel.Remove", (ServiceModel{ServiceID: "x"}).Remove(ctx))
	if _, err := (ServiceModel{ServiceID: "x"}).Get(ctx); err == nil {
		t.Error("ServiceModel.Get: expected error")
	}
	if _, err := (ServiceModel{}).List(ctx, 10, 0); err == nil {
		t.Error("ServiceModel.List: expected error")
	}

	mustErr("ServiceAccountModel.Add", (ServiceAccountModel{AccountID: "x", Name: "n"}).Add(ctx))
	mustErr("ServiceAccountModel.Update", (ServiceAccountModel{AccountID: "x"}).Update(ctx))
	mustErr("ServiceAccountModel.Remove", (ServiceAccountModel{AccountID: "x"}).Remove(ctx))
	if _, err := (ServiceAccountModel{Name: "n"}).Get(ctx); err == nil {
		t.Error("ServiceAccountModel.Get: expected error")
	}
	if _, err := (ServiceAccountModel{}).List(ctx, 10, 0); err == nil {
		t.Error("ServiceAccountModel.List: expected error")
	}

	if _, err := lookupServiceAccount(ctx, "n"); err == nil {
		t.Error("lookupServiceAccount: expected error")
	}
	mustErr("upsertServiceAccount", upsertServiceAccount(ctx, ServiceAccountModel{AccountID: "x", Name: "n"}))
	mustErr("rotateServiceKeyDB", rotateServiceKeyDB(ctx, "n", "hash"))
	mustErr("reactivateServiceByName", reactivateServiceByName(ctx, "n", "http://x", "", false, "", false))
	mustErr("upsertServiceModelByName", upsertServiceModelByName(ctx, ServiceModel{ServiceID: "x", Name: "n", URL: "http://x"}))
	if _, err := listServicesWithEndpoints(ctx); err == nil {
		t.Error("listServicesWithEndpoints: expected error")
	}
	if _, err := listAllActions(ctx); err == nil {
		t.Error("listAllActions: expected error")
	}
	if _, err := listAllDefaultGrants(ctx); err == nil {
		t.Error("listAllDefaultGrants: expected error")
	}
	if _, err := queryActiveServiceURLs(ctx); err == nil {
		t.Error("queryActiveServiceURLs: expected error")
	}
}
