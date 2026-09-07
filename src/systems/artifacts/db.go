package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	gormDB    *gorm.DB
	dbInitMu  sync.Mutex
	errNoSuch = errors.New("artifact not found")
)

func connect() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/artifacts")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		slog.Error("unable to connect to database", "error", err)
		os.Exit(1)
	}
	gormDB = conn
	return gormDB
}

func migrate() error {
	return connect().AutoMigrate(&Artifact{}, &Quota{})
}

// ── Artifacts ─────────────────────────────────────────────────────────────────

// getArtifact loads one user's artifact by name.
func getArtifact(ctx context.Context, userID, name string) (Artifact, error) {
	var a Artifact
	err := connect().WithContext(ctx).Where("user_id=? AND name=?", userID, name).First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Artifact{}, errNoSuch
	}
	return a, err
}

// listArtifacts returns the artifacts a caller may see, newest first: their own, plus
// any filed into a project they can reach (projectIDs). An optional project slug narrows
// the view to that one project — a view filter, not a security boundary. Mirrors forge's
// listExecutions.
func listArtifacts(ctx context.Context, userID, project string, projectIDs []string) ([]Artifact, error) {
	var out []Artifact
	q := connect().WithContext(ctx).Order("updated_at DESC").Limit(500)
	// Widen to artifacts in any project the caller can reach; otherwise owner-only.
	if len(projectIDs) > 0 {
		q = q.Where("user_id = ? OR project_id IN ?", userID, projectIDs)
	} else {
		q = q.Where("user_id = ?", userID)
	}
	// Project is an optional view filter, not a security boundary.
	if project != "" {
		q = q.Where("project = ?", project)
	}
	err := q.Find(&out).Error
	if out == nil {
		out = []Artifact{}
	}
	return out, err
}

// getArtifactInProject loads an artifact by (project, name), used to reach an artifact a
// caller does not own but may access as a project member. A name is unique per user, not
// per project, so this returns the FIRST match — the disambiguation the caller cares
// about is which project, which they named; a duplicate name across two owners in one
// project is not a case this store distinguishes.
func getArtifactInProject(ctx context.Context, projectID, name string) (Artifact, error) {
	var a Artifact
	err := connect().WithContext(ctx).Where("project_id=? AND name=?", projectID, name).First(&a).Error
	return a, err
}

// upsertArtifact records a stored blob, replacing any same-named one. Uploading a
// name that exists REPLACES it (see Artifact) — the row is keyed by (user, name),
// so the write is an update rather than a second row.
func upsertArtifact(ctx context.Context, a Artifact) error {
	var existing Artifact
	err := connect().WithContext(ctx).Where("user_id=? AND name=?", a.UserID, a.Name).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		a.CreatedAt = time.Now().UTC()
		a.UpdatedAt = a.CreatedAt
		return connect().WithContext(ctx).Create(&a).Error
	}
	if err != nil {
		return err
	}
	return connect().WithContext(ctx).Model(&Artifact{}).
		Where("artifact_id=?", existing.ArtifactID).
		Updates(map[string]any{
			"size_bytes":        a.SizeBytes,
			"content_type":      a.ContentType,
			"sha256":            a.SHA256,
			"org_id":            a.OrgID,
			"project":           a.Project,
			"project_id":        a.ProjectID,
			"project_namespace": a.ProjectNamespace,
			"updated_at":        time.Now().UTC(),
		}).Error
}

func deleteArtifact(ctx context.Context, userID, name string) (int64, error) {
	res := connect().WithContext(ctx).Where("user_id=? AND name=?", userID, name).Delete(&Artifact{})
	return res.RowsAffected, res.Error
}

// usage totals a user's stored bytes and artifact count — the numerator of the quota.
func usage(ctx context.Context, userID string) (bytes int64, count int64, err error) {
	var row struct {
		Total int64
		N     int64
	}
	err = connect().WithContext(ctx).Model(&Artifact{}).
		Select("COALESCE(SUM(size_bytes),0) AS total, COUNT(*) AS n").
		Where("user_id=?", userID).Scan(&row).Error
	return row.Total, row.N, err
}

// ── Quotas ────────────────────────────────────────────────────────────────────

// getQuotaOverride returns an admin-set override for a scope, or false when none
// exists (the caller then falls back to the default).
func getQuotaOverride(ctx context.Context, scope, scopeID string) (Quota, bool, error) {
	var q Quota
	err := connect().WithContext(ctx).Where("scope=? AND scope_id=?", scope, scopeID).First(&q).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Quota{}, false, nil
	}
	if err != nil {
		return Quota{}, false, err
	}
	return q, true, nil
}

// effectiveQuota is the cap that actually applies to a user: their override when an
// admin set one, otherwise the service default. isDefault distinguishes the two, so
// an admin can tell an unconfigured user from one deliberately set to the default.
func effectiveQuota(ctx context.Context, userID string) (maxBytes int64, isDefault bool, setBy string, err error) {
	q, ok, err := getQuotaOverride(ctx, ScopeUser, userID)
	if err != nil {
		return 0, false, "", err
	}
	if ok {
		return q.MaxBytes, false, q.SetBy, nil
	}
	return defaultQuotaBytes(), true, "", nil
}

func setQuota(ctx context.Context, q Quota) error {
	q.UpdatedAt = time.Now().UTC()
	var existing Quota
	err := connect().WithContext(ctx).Where("scope=? AND scope_id=?", q.Scope, q.ScopeID).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		q.CreatedAt = q.UpdatedAt
		return connect().WithContext(ctx).Create(&q).Error
	}
	if err != nil {
		return err
	}
	return connect().WithContext(ctx).Model(&Quota{}).
		Where("scope=? AND scope_id=?", q.Scope, q.ScopeID).
		Updates(map[string]any{"max_bytes": q.MaxBytes, "set_by": q.SetBy, "updated_at": q.UpdatedAt}).Error
}

// deleteQuota removes an override, returning the scope to the service default.
func deleteQuota(ctx context.Context, scope, scopeID string) (int64, error) {
	res := connect().WithContext(ctx).Where("scope=? AND scope_id=?", scope, scopeID).Delete(&Quota{})
	return res.RowsAffected, res.Error
}

func listQuotas(ctx context.Context) ([]Quota, error) {
	var out []Quota
	err := connect().WithContext(ctx).Order("scope_id ASC").Limit(500).Find(&out).Error
	if out == nil {
		out = []Quota{}
	}
	return out, err
}
