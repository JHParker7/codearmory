package main

import "time"

// Sync lifecycle states. The terminal states Synced/Failed are what the
// argo/sync async action contract in registry-manifest.json polls for.
const (
	SyncPending = "pending"
	SyncRunning = "running"
	SyncSynced  = "Synced"
	SyncFailed  = "Failed"
)

var syncSuccessStates = []string{SyncSynced}
var syncFailureStates = []string{SyncFailed}

const maxBodyBytes = 64 * 1024

// App is an Argo CD Application the control plane has learned about from
// app-state events. The control plane holds no Argo credentials — an outpost
// reports state and performs syncs.
type App struct {
	AppID          string    `json:"app_id"          gorm:"column:app_id;primaryKey"`
	OrgID          string    `json:"org_id"          gorm:"column:org_id;default:''"`
	UserID         string    `json:"user_id"         gorm:"column:user_id;default:''"`
	OutpostID      string    `json:"outpost_id"      gorm:"column:outpost_id"`
	Name           string    `json:"name"            gorm:"column:name;index:idx_apps_org_name,unique"`
	OrgKey         string    `json:"-"               gorm:"column:org_key;index:idx_apps_org_name,unique"`
	SyncStatus     string    `json:"sync_status"     gorm:"column:sync_status;default:''"`
	HealthStatus   string    `json:"health_status"   gorm:"column:health_status;default:''"`
	Revision       string    `json:"revision"        gorm:"column:revision;default:''"`
	OperationPhase string    `json:"operation_phase" gorm:"column:operation_phase;default:''"`
	Active         bool      `json:"-"               gorm:"column:active;default:true"`
	CreatedAt      time.Time `json:"created_at"      gorm:"column:created_at"`
	UpdatedAt      time.Time `json:"updated_at"      gorm:"column:updated_at"`
}

func (App) TableName() string { return "apps" }

// Sync is a single sync operation triggered through an outpost.
type Sync struct {
	SyncID    string     `json:"sync_id"   gorm:"column:sync_id;primaryKey"`
	OrgID     string     `json:"org_id"    gorm:"column:org_id;default:''"`
	UserID    string     `json:"user_id"   gorm:"column:user_id"`
	OutpostID string     `json:"outpost_id" gorm:"column:outpost_id"`
	AppName   string     `json:"app_name"  gorm:"column:app_name"`
	Revision  string     `json:"revision"  gorm:"column:revision;default:''"`
	Status    string     `json:"status"    gorm:"column:status;default:'pending'"`
	Message   string     `json:"message"   gorm:"column:message;default:''"`
	Active    bool       `json:"-"         gorm:"column:active;default:true"`
	CreatedAt time.Time  `json:"created_at" gorm:"column:created_at"`
	StartedAt *time.Time `json:"started_at,omitempty" gorm:"column:started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"   gorm:"column:ended_at"`
}

func (Sync) TableName() string { return "syncs" }

type syncRequest struct {
	OutpostID string `json:"outpost_id"`
	Revision  string `json:"revision"`
}
