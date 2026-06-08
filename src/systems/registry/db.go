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

// ── GORM model structs (table schema definitions) ─────────────────────────────

type ServiceModel struct {
	ServiceID   string    `gorm:"column:service_id;primaryKey"`
	Name        string    `gorm:"column:name;not null;uniqueIndex"`
	URL         string    `gorm:"column:url;not null"`
	Description string    `gorm:"column:description;not null;default:''"`
	ForwardAuth bool      `gorm:"column:forward_auth;not null;default:false"`
	ServiceKey  string    `gorm:"column:service_key;not null;default:''"`
	Active      bool      `gorm:"column:active;not null;default:true"`
	CreatedAt   time.Time `gorm:"column:created_at;not null;default:now()"`
	UpdatedAt   time.Time `gorm:"column:updated_at;not null;default:now()"`
}

func (ServiceModel) TableName() string { return "services" }

type ServiceRoleModel struct {
	RoleID      string    `gorm:"column:role_id;primaryKey"`
	ServiceID   string    `gorm:"column:service_id;not null;uniqueIndex:service_roles_service_id_name_key"`
	Name        string    `gorm:"column:name;not null;uniqueIndex:service_roles_service_id_name_key"`
	Description string    `gorm:"column:description;not null;default:''"`
	CreatedAt   time.Time `gorm:"column:created_at;not null;default:now()"`
}

func (ServiceRoleModel) TableName() string { return "service_roles" }

type ServiceEndpointModel struct {
	EndpointID string    `gorm:"column:endpoint_id;primaryKey"`
	ServiceID  string    `gorm:"column:service_id;not null"`
	Method     string    `gorm:"column:method;not null"`
	Path       string    `gorm:"column:path;not null"`
	Action     string    `gorm:"column:action;not null"`
	Resource   string    `gorm:"column:resource;not null"`
	Public     bool      `gorm:"column:public;not null;default:false"`
	Active     bool      `gorm:"column:active;not null;default:true"`
	CreatedAt  time.Time `gorm:"column:created_at;not null;default:now()"`
	UpdatedAt  time.Time `gorm:"column:updated_at;not null;default:now()"`
}

func (ServiceEndpointModel) TableName() string { return "service_endpoints" }

type ServiceActionModel struct {
	ActionID       string    `gorm:"column:action_id;primaryKey"`
	ServiceID      string    `gorm:"column:service_id;not null;uniqueIndex:service_actions_service_id_name_key"`
	Name           string    `gorm:"column:name;not null;uniqueIndex:service_actions_service_id_name_key"`
	Method         string    `gorm:"column:method;not null"`
	Path           string    `gorm:"column:path;not null"`
	BodyTransforms []byte    `gorm:"column:body_transforms;type:jsonb"`
	AsyncConfig    []byte    `gorm:"column:async_config;type:jsonb"`
	Active         bool      `gorm:"column:active;not null;default:true"`
	CreatedAt      time.Time `gorm:"column:created_at;not null;default:now()"`
	UpdatedAt      time.Time `gorm:"column:updated_at;not null;default:now()"`
}

func (ServiceActionModel) TableName() string { return "service_actions" }

type ServiceAccountModel struct {
	AccountID  string    `gorm:"column:account_id;primaryKey"`
	Name       string    `gorm:"column:name;not null;uniqueIndex"`
	HashedKey  string    `gorm:"column:hashed_key;not null"`
	Role       string    `gorm:"column:role;not null;default:read"`
	CreatedAt  time.Time `gorm:"column:created_at;not null;default:now()"`
	UpdatedAt  time.Time `gorm:"column:updated_at;not null;default:now()"`
}

func (ServiceAccountModel) TableName() string { return "registry_service_accounts" }

type ServiceDefaultGrantModel struct {
	GrantID   string    `gorm:"column:grant_id;primaryKey"`
	ServiceID string    `gorm:"column:service_id;not null"`
	GrantOn   string    `gorm:"column:grant_on;not null"`
	Actions   []byte    `gorm:"column:actions;type:jsonb;not null;default:'[]'"`
	Resources []byte    `gorm:"column:resources;type:jsonb;not null;default:'[]'"`
	CreatedAt time.Time `gorm:"column:created_at;not null;default:now()"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;default:now()"`
}

func (ServiceDefaultGrantModel) TableName() string { return "service_default_grants" }

// ── Connection ────────────────────────────────────────────────────────────────

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
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgresql://postgres:postgres@localhost:5432/registry"
	}
	conn, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "registry: connect to database: %v\n", err)
		os.Exit(1)
	}
	gormDB = conn
	return gormDB
}
