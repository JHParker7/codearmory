package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// db is the common interface implemented by all persistent entities.
// Each method operates on the receiver's fields to identify the target row.
type db interface {
	// Add inserts the entity as a new row.
	Add(ctx context.Context) error
	// Update saves all fields of the entity to its existing row.
	Update(ctx context.Context) error
	// Remove deletes the entity's row from the database.
	Remove(ctx context.Context) error
	// Get retrieves the entity's row and returns it as a db value.
	Get(ctx context.Context) (db, error)
	// List gets all matching rows and returns them. A limit of 0 returns all rows.
	List(ctx context.Context, limit, offset int) ([]db, error)
}

// gormDB holds the shared write connection. Initialised on the first call to
// connect(); tests set it directly in TestMain.
var gormDB *gorm.DB

// gormDBRead holds the shared read-only connection. Initialised on the first
// call to connectRead(); falls back to gormDB when that is already set (tests).
var gormDBRead *gorm.DB

// dbInitMu serialises lazy initialisation of gormDB and gormDBRead so that
// concurrent requests at startup cannot race on the nil-check + assignment.
var dbInitMu sync.Mutex

// connect returns the shared write database connection (DATABASE_URL), opening
// it on first call.
func connect() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	conn, err := gorm.Open(postgres.Open(secret("DATABASE_URL")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to connect to database: %v\n", err)
		os.Exit(1)
	}
	gormDB = conn
	return gormDB
}

// connectRead returns the shared read database connection. It uses
// DATABASE_READ_URL when set, falling back to DATABASE_URL. When gormDB has
// been set directly (e.g. in tests) and gormDBRead has not, it reuses gormDB
// so that tests need no additional setup.
func connectRead() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDBRead != nil {
		return gormDBRead
	}
	if gormDB != nil {
		return gormDB
	}
	readURL := secret("DATABASE_READ_URL")
	if readURL == "" {
		readURL = secret("DATABASE_URL")
	}
	conn, err := gorm.Open(postgres.Open(readURL), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to connect to read database: %v\n", err)
		os.Exit(1)
	}
	gormDBRead = conn
	return gormDBRead
}
