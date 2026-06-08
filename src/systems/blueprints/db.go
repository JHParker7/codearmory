package main

import (
	"fmt"
	"os"
	"sync"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// State stores encrypted Terraform state blobs keyed by workspace.
type State struct {
	Workspace string    `gorm:"column:workspace;primaryKey"`
	Data      []byte    `gorm:"column:data;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;default:now()"`
}

// StateLock holds the exclusive lock record for a workspace.
type StateLock struct {
	Workspace string    `gorm:"column:workspace;primaryKey"`
	LockData  string    `gorm:"column:lock_data;not null"`
	CreatedAt time.Time `gorm:"column:created_at;default:now()"`
	UpdatedAt time.Time `gorm:"column:updated_at;default:now()"`
}

func (StateLock) TableName() string { return "locks" }

// BackendCredential is an ephemeral mTLS/token credential for a workspace backend.
type BackendCredential struct {
	CredentialID string    `gorm:"column:credential_id;primaryKey"`
	Workspace    string    `gorm:"column:workspace;not null"`
	CertFP       string    `gorm:"column:cert_fp;not null;uniqueIndex:idx_backend_creds_cert_fp"`
	TokenHash    string    `gorm:"column:token_hash;not null;uniqueIndex:idx_backend_creds_token_hash"`
	CreatedBy    string    `gorm:"column:created_by;not null"`
	ExpiresAt    time.Time `gorm:"column:expires_at;not null;index:idx_backend_creds_expires"`
	CreatedAt    time.Time `gorm:"column:created_at;not null;default:now()"`
}

var (
	gormDB   *gorm.DB
	gormDBMu sync.Mutex
)

func connect() *gorm.DB {
	gormDBMu.Lock()
	defer gormDBMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	dsn := secret("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(1)
	}
	conn, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "blueprints: connect to database: %v\n", err)
		os.Exit(1)
	}
	gormDB = conn
	return gormDB
}
