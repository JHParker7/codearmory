package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Storage follows the codearmory GORM pattern: a lazy connect() singleton and CREATE-TABLE
// auto-migration on startup.
var (
	dbInitMu sync.Mutex
	gormDB   *gorm.DB
)

func secretOrDefault(name, def string) string {
	if path := os.Getenv(name + "_FILE"); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimRight(string(b), "\n")
		}
	}
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func connect() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/notifications")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		slog.Error("unable to connect to database", "error", err)
		os.Exit(1)
	}
	gormDB = conn
	return gormDB
}

func migrate(ctx context.Context) error {
	return connect().WithContext(ctx).AutoMigrate(&Channel{}, &Delivery{})
}
