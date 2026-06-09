package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	AccountID string    `gorm:"column:account_id;primaryKey"`
	Name      string    `gorm:"column:name;not null;uniqueIndex"`
	HashedKey string    `gorm:"column:hashed_key;not null"`
	Role      string    `gorm:"column:role;not null;default:read"`
	CreatedAt time.Time `gorm:"column:created_at;not null;default:now()"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;default:now()"`
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

// ── db interface ──────────────────────────────────────────────────────────────

type db interface {
	Add(ctx context.Context) error
	Update(ctx context.Context) error
	Remove(ctx context.Context) error
	Get(ctx context.Context) (db, error)
	List(ctx context.Context, limit, offset int) ([]db, error)
}

// manifestRoleSpec and manifestEndpointSpec are parameter types shared between
// replaceServiceManifest (called from handleUpdateServiceEndpoints) and
// loadManifestEntry (called from loadManifest).
type manifestRoleSpec struct {
	Name        string
	Description string
}

type manifestEndpointSpec struct {
	Method   string
	Path     string
	Action   string
	Resource string
	Public   bool
}

// ── ServiceModel ──────────────────────────────────────────────────────────────

func (s ServiceModel) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("registry").Start(ctx, "db.service.add")
	defer span.End()
	span.SetAttributes(attribute.String("service.name", s.Name))
	if err := connect().WithContext(ctx).Create(&s).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s ServiceModel) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("registry").Start(ctx, "db.service.update")
	defer span.End()
	if err := connect().WithContext(ctx).Save(&s).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the service (sets active=false). Returns gorm.ErrRecordNotFound
// when the service does not exist or is already inactive.
func (s ServiceModel) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("registry").Start(ctx, "db.service.remove")
	defer span.End()
	span.SetAttributes(attribute.String("service.id", s.ServiceID))
	result := connect().WithContext(ctx).Exec(
		`UPDATE services SET active = false, updated_at = now() WHERE service_id = ? AND active = true`, s.ServiceID)
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, result.Error.Error())
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves a service. Looks up by ServiceID when set, otherwise by Name.
func (s ServiceModel) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("registry").Start(ctx, "db.service.get")
	defer span.End()
	var out ServiceModel
	var q *gorm.DB
	if s.ServiceID != "" {
		span.SetAttributes(attribute.String("service.id", s.ServiceID))
		q = connect().WithContext(ctx).Where("service_id = ?", s.ServiceID)
	} else {
		span.SetAttributes(attribute.String("service.name", s.Name))
		q = connect().WithContext(ctx).Where("name = ?", s.Name)
	}
	if err := q.First(&out).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

func (s ServiceModel) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("registry").Start(ctx, "db.service.list")
	defer span.End()
	var services []ServiceModel
	q := connect().WithContext(ctx).Where("active = true").Order("name")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if offset > 0 {
		q = q.Offset(offset)
	}
	if err := q.Find(&services).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	rows := make([]db, len(services))
	for i, svc := range services {
		rows[i] = svc
	}
	return rows, nil
}

// ── ServiceAccountModel ────────────────────────────────────────────────────────

func (a ServiceAccountModel) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("registry").Start(ctx, "db.serviceaccount.add")
	defer span.End()
	span.SetAttributes(attribute.String("account.name", a.Name))
	if err := connect().WithContext(ctx).Create(&a).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (a ServiceAccountModel) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("registry").Start(ctx, "db.serviceaccount.update")
	defer span.End()
	if err := connect().WithContext(ctx).Save(&a).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (a ServiceAccountModel) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("registry").Start(ctx, "db.serviceaccount.remove")
	defer span.End()
	result := connect().WithContext(ctx).Where("account_id = ?", a.AccountID).Delete(&ServiceAccountModel{})
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, result.Error.Error())
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves a service account by Name.
func (a ServiceAccountModel) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("registry").Start(ctx, "db.serviceaccount.get")
	defer span.End()
	span.SetAttributes(attribute.String("account.name", a.Name))
	var out ServiceAccountModel
	if err := connect().WithContext(ctx).Where("name = ?", a.Name).First(&out).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

func (a ServiceAccountModel) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("registry").Start(ctx, "db.serviceaccount.list")
	defer span.End()
	var accounts []ServiceAccountModel
	q := connect().WithContext(ctx).Order("name")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if offset > 0 {
		q = q.Offset(offset)
	}
	if err := q.Find(&accounts).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	rows := make([]db, len(accounts))
	for i, a := range accounts {
		rows[i] = a
	}
	return rows, nil
}

// ── Stub interfaces for batch-managed entities ────────────────────────────────
// These entities are only ever managed in bulk via replaceServiceManifest or
// loadManifestEntry; individual CRUD methods are not called directly.

func (m ServiceRoleModel) Add(_ context.Context) error          { return errors.New("not implemented") }
func (m ServiceRoleModel) Update(_ context.Context) error       { return errors.New("not implemented") }
func (m ServiceRoleModel) Remove(_ context.Context) error       { return errors.New("not implemented") }
func (m ServiceRoleModel) Get(_ context.Context) (db, error)    { return nil, errors.New("not implemented") }
func (m ServiceRoleModel) List(_ context.Context, _, _ int) ([]db, error) {
	return nil, errors.New("not implemented")
}

func (m ServiceEndpointModel) Add(_ context.Context) error       { return errors.New("not implemented") }
func (m ServiceEndpointModel) Update(_ context.Context) error    { return errors.New("not implemented") }
func (m ServiceEndpointModel) Remove(_ context.Context) error    { return errors.New("not implemented") }
func (m ServiceEndpointModel) Get(_ context.Context) (db, error) { return nil, errors.New("not implemented") }
func (m ServiceEndpointModel) List(_ context.Context, _, _ int) ([]db, error) {
	return nil, errors.New("not implemented")
}

func (m ServiceActionModel) Add(_ context.Context) error       { return errors.New("not implemented") }
func (m ServiceActionModel) Update(_ context.Context) error    { return errors.New("not implemented") }
func (m ServiceActionModel) Remove(_ context.Context) error    { return errors.New("not implemented") }
func (m ServiceActionModel) Get(_ context.Context) (db, error) { return nil, errors.New("not implemented") }
func (m ServiceActionModel) List(_ context.Context, _, _ int) ([]db, error) {
	return nil, errors.New("not implemented")
}

func (m ServiceDefaultGrantModel) Add(_ context.Context) error       { return errors.New("not implemented") }
func (m ServiceDefaultGrantModel) Update(_ context.Context) error    { return errors.New("not implemented") }
func (m ServiceDefaultGrantModel) Remove(_ context.Context) error    { return errors.New("not implemented") }
func (m ServiceDefaultGrantModel) Get(_ context.Context) (db, error) { return nil, errors.New("not implemented") }
func (m ServiceDefaultGrantModel) List(_ context.Context, _, _ int) ([]db, error) {
	return nil, errors.New("not implemented")
}

// ── Named package-level helpers ───────────────────────────────────────────────

// lookupServiceAccount retrieves a service account by name.
// Used for authentication in requireAuthWithRole and handleRotateServiceKey.
func lookupServiceAccount(ctx context.Context, name string) (ServiceAccountModel, error) {
	var acct ServiceAccountModel
	if err := connect().WithContext(ctx).Where("name = ?", name).First(&acct).Error; err != nil {
		return ServiceAccountModel{}, err
	}
	return acct, nil
}

// upsertServiceAccount creates or refreshes a service account by name, updating
// the hashed key and role on conflict. Called from seedServiceAccounts.
func upsertServiceAccount(ctx context.Context, acct ServiceAccountModel) error {
	return connect().WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "name"}},
			DoUpdates: clause.Assignments(map[string]any{
				"hashed_key": acct.HashedKey,
				"role":       acct.Role,
				"updated_at": gorm.Expr("now()"),
			}),
		}).Create(&acct).Error
}

// rotateServiceKeyDB updates the stored bcrypt hash for a service account by name.
func rotateServiceKeyDB(ctx context.Context, name, newHash string) error {
	return connect().WithContext(ctx).
		Exec(`UPDATE registry_service_accounts SET hashed_key = ?, updated_at = now() WHERE name = ?`, newHash, name).
		Error
}

// upsertServiceModelByName creates or updates a service by its unique name.
// Used when seeding services from the SERVICES environment variable.
func upsertServiceModelByName(ctx context.Context, svc ServiceModel) error {
	return connect().WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "name"}},
			DoUpdates: clause.Assignments(map[string]any{
				"url":        svc.URL,
				"active":     true,
				"updated_at": gorm.Expr("now()"),
			}),
		}).Create(&svc).Error
}

// listServicesWithEndpoints returns all active services with their roles, endpoints,
// and default grants in a batch (avoids N+1 round-trips).
func listServicesWithEndpoints(ctx context.Context) ([]serviceWithEndpoints, error) {
	svcRows, err := connect().WithContext(ctx).Raw(
		`SELECT service_id, name, url, description, forward_auth, active, created_at, updated_at
		 FROM services WHERE active = true ORDER BY name`).Rows()
	if err != nil {
		return nil, err
	}
	defer svcRows.Close()

	var svcs []Service
	for svcRows.Next() {
		var s Service
		if err := svcRows.Scan(&s.ServiceID, &s.Name, &s.URL, &s.Description, &s.ForwardAuth, &s.Active, &s.CreatedAt, &s.UpdatedAt); err != nil {
			slog.Error("listServicesWithEndpoints: scan", "error", err)
			continue
		}
		svcs = append(svcs, s)
	}
	if svcRows.Err() != nil {
		return nil, svcRows.Err()
	}

	svcIndex := make(map[string]int, len(svcs))
	svcIDs := make([]string, len(svcs))
	result := make([]serviceWithEndpoints, len(svcs))
	for i, s := range svcs {
		result[i] = serviceWithEndpoints{Service: s, Roles: []ServiceRole{}, Endpoints: []ServiceEndpoint{}}
		svcIDs[i] = s.ServiceID
		svcIndex[s.ServiceID] = i
	}

	if len(svcIDs) > 0 {
		roleRows, err := connect().WithContext(ctx).Raw(
			`SELECT role_id, service_id, name, description, created_at
			 FROM service_roles WHERE service_id IN (?) ORDER BY service_id, name`,
			svcIDs).Rows()
		if err != nil {
			slog.Error("listServicesWithEndpoints: query roles", "error", err)
		} else {
			for roleRows.Next() {
				var sr ServiceRole
				if err := roleRows.Scan(&sr.RoleID, &sr.ServiceID, &sr.Name, &sr.Description, &sr.CreatedAt); err != nil {
					slog.Error("listServicesWithEndpoints: scan role", "error", err)
					continue
				}
				if i, ok := svcIndex[sr.ServiceID]; ok {
					result[i].Roles = append(result[i].Roles, sr)
				}
			}
			roleRows.Close()
		}

		epRows, err := connect().WithContext(ctx).Raw(
			`SELECT endpoint_id, service_id, method, path, action, resource, public, active, created_at, updated_at
			 FROM service_endpoints WHERE service_id IN (?) AND active = true ORDER BY service_id`,
			svcIDs).Rows()
		if err != nil {
			slog.Error("listServicesWithEndpoints: query endpoints", "error", err)
		} else {
			for epRows.Next() {
				var ep ServiceEndpoint
				if err := epRows.Scan(&ep.EndpointID, &ep.ServiceID, &ep.Method, &ep.Path,
					&ep.Action, &ep.Resource, &ep.Public, &ep.Active, &ep.CreatedAt, &ep.UpdatedAt); err != nil {
					slog.Error("listServicesWithEndpoints: scan endpoint", "error", err)
					continue
				}
				if i, ok := svcIndex[ep.ServiceID]; ok {
					result[i].Endpoints = append(result[i].Endpoints, ep)
				}
			}
			epRows.Close()
		}
	}

	return result, nil
}

// listAllActions returns all active workflow actions joined with their service info.
func listAllActions(ctx context.Context) ([]ServiceAction, error) {
	sqlRows, err := connect().WithContext(ctx).Raw(`
		SELECT sa.action_id, sa.service_id, s.name, s.url,
		       sa.name, sa.method, sa.path,
		       sa.body_transforms, sa.async_config,
		       sa.active, sa.created_at, sa.updated_at,
		       COALESCE(se.action, ''), COALESCE(se.resource, '')
		FROM service_actions sa
		JOIN services s ON sa.service_id = s.service_id
		LEFT JOIN service_endpoints se
		       ON se.service_id = sa.service_id
		      AND se.method     = sa.method
		      AND se.path       = sa.path
		      AND se.active     = true
		WHERE sa.active = true AND s.active = true
		ORDER BY sa.name
	`).Rows()
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()

	var actions []ServiceAction
	for sqlRows.Next() {
		var a ServiceAction
		var bodyTransforms, asyncConfig []byte
		if err := sqlRows.Scan(
			&a.ActionID, &a.ServiceID, &a.ServiceName, &a.ServiceURL,
			&a.Name, &a.Method, &a.Path,
			&bodyTransforms, &asyncConfig,
			&a.Active, &a.CreatedAt, &a.UpdatedAt,
			&a.GkAction, &a.GkResource,
		); err != nil {
			slog.Error("listAllActions: scan", "error", err)
			continue
		}
		a.BodyTransforms = json.RawMessage(bodyTransforms)
		a.AsyncConfig = json.RawMessage(asyncConfig)
		if a.GkAction != "" {
			a.GkService = a.ServiceName
		}
		actions = append(actions, a)
	}
	if sqlRows.Err() != nil {
		return nil, sqlRows.Err()
	}
	return actions, nil
}

// listAllDefaultGrants returns all active default grants across all services,
// enriched with the service name.
func listAllDefaultGrants(ctx context.Context) ([]ServiceDefaultGrant, error) {
	grantRows, err := connect().WithContext(ctx).Raw(
		`SELECT g.grant_id, g.service_id, s.name, g.grant_on, g.actions, g.resources, g.created_at, g.updated_at
		 FROM service_default_grants g
		 JOIN services s ON s.service_id = g.service_id AND s.active = true
		 ORDER BY s.name, g.grant_on`,
	).Rows()
	if err != nil {
		return nil, err
	}
	defer grantRows.Close()

	var grants []ServiceDefaultGrant
	for grantRows.Next() {
		var g ServiceDefaultGrant
		var actionsRaw, resourcesRaw []byte
		if err := grantRows.Scan(&g.GrantID, &g.ServiceID, &g.ServiceName, &g.GrantOn,
			&actionsRaw, &resourcesRaw, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal(actionsRaw, &g.Actions)     //nolint:errcheck
		json.Unmarshal(resourcesRaw, &g.Resources) //nolint:errcheck
		grants = append(grants, g)
	}
	return grants, grantRows.Err()
}

// queryActiveServiceURLs returns the name and URL of every active service.
// Used by the health collector.
func queryActiveServiceURLs(ctx context.Context) ([]struct{ Name, URL string }, error) {
	rows, err := connect().WithContext(ctx).Raw(`SELECT name, url FROM services WHERE active = true`).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var svcs []struct{ Name, URL string }
	for rows.Next() {
		var s struct{ Name, URL string }
		if err := rows.Scan(&s.Name, &s.URL); err == nil {
			svcs = append(svcs, s)
		}
	}
	return svcs, rows.Err()
}

// replaceServiceManifest atomically replaces a service's roles, endpoints, actions,
// and default grants inside a single transaction.
// Returns gorm.ErrRecordNotFound when the service does not exist.
func replaceServiceManifest(ctx context.Context, id, url, description string,
	roles []manifestRoleSpec, endpoints []manifestEndpointSpec,
	actions []manifestActionEntry, grants []manifestDefaultGrant) error {

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback() //nolint:errcheck

	var exists bool
	existsResult := tx.Raw(`SELECT true FROM services WHERE service_id = ? AND active = true`, id).Scan(&exists)
	if existsResult.Error != nil || existsResult.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}

	if url != "" {
		if err := tx.Exec(`UPDATE services SET url = ?, updated_at = now() WHERE service_id = ?`, url, id).Error; err != nil {
			return err
		}
	}
	if description != "" {
		if err := tx.Exec(`UPDATE services SET description = ?, updated_at = now() WHERE service_id = ?`, description, id).Error; err != nil {
			return err
		}
	}

	if err := tx.Exec(`DELETE FROM service_roles WHERE service_id = ?`, id).Error; err != nil {
		return err
	}
	for _, role := range roles {
		if role.Name == "" {
			continue
		}
		if err := tx.Exec(
			`INSERT INTO service_roles (role_id, service_id, name, description) VALUES (?, ?, ?, ?)`,
			uuid.New().String(), id, role.Name, role.Description).Error; err != nil {
			return err
		}
	}

	if err := tx.Exec(`DELETE FROM service_endpoints WHERE service_id = ?`, id).Error; err != nil {
		return err
	}
	for _, ep := range endpoints {
		if ep.Method == "" || ep.Path == "" || ep.Action == "" || ep.Resource == "" {
			continue
		}
		if err := tx.Exec(
			`INSERT INTO service_endpoints (endpoint_id, service_id, method, path, action, resource, public) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			uuid.New().String(), id, ep.Method, ep.Path, ep.Action, ep.Resource, ep.Public).Error; err != nil {
			return err
		}
	}

	if err := tx.Exec(`DELETE FROM service_actions WHERE service_id = ?`, id).Error; err != nil {
		return err
	}
	for _, a := range actions {
		if a.Name == "" || a.Method == "" || a.Path == "" {
			continue
		}
		if err := tx.Exec(
			`INSERT INTO service_actions (action_id, service_id, name, method, path, body_transforms, async_config) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			uuid.New().String(), id, a.Name, a.Method, a.Path,
			jsonbBytes(a.BodyTransforms), jsonbBytes(a.Async)).Error; err != nil {
			return err
		}
	}

	if len(grants) > 0 {
		if err := tx.Exec(`DELETE FROM service_default_grants WHERE service_id = ?`, id).Error; err != nil {
			return err
		}
		for _, g := range grants {
			if g.GrantOn == "" || len(g.Actions) == 0 || len(g.Resources) == 0 {
				continue
			}
			actionsJSON, _ := json.Marshal(g.Actions)
			resourcesJSON, _ := json.Marshal(g.Resources)
			if err := tx.Exec(
				`INSERT INTO service_default_grants (grant_id, service_id, grant_on, actions, resources) VALUES (?, ?, ?, ?, ?)`,
				uuid.New().String(), id, g.GrantOn, actionsJSON, resourcesJSON).Error; err != nil {
				return err
			}
		}
	}

	return tx.Commit().Error
}

// loadManifestEntry upserts one manifest entry (service + all sub-resources).
// Called for each entry during startup from loadManifest.
func loadManifestEntry(ctx context.Context, e manifestEntry) {
	hashedKey, err := hashServiceKey(e.ServiceKey)
	if err != nil {
		slog.Error("manifest: failed to hash service key", "name", e.Name, "error", err)
		return
	}

	conn := connect().WithContext(ctx)
	var svcModel ServiceModel
	err = conn.Where("name = ?", e.Name).First(&svcModel).Error
	if err != nil {
		svcModel = ServiceModel{
			ServiceID:   uuid.New().String(),
			Name:        e.Name,
			URL:         e.URL,
			Description: e.Description,
			ForwardAuth: e.ForwardAuth,
			ServiceKey:  hashedKey,
		}
		if err := conn.Create(&svcModel).Error; err != nil {
			slog.Error("manifest: failed to insert service", "name", e.Name, "error", err)
			return
		}
		slog.Info("manifest: service created", "name", e.Name)
	} else {
		if err := conn.Exec(
			`UPDATE services SET description = ?, forward_auth = ?, service_key = ?, active = true, updated_at = now() WHERE service_id = ?`,
			e.Description, e.ForwardAuth, hashedKey, svcModel.ServiceID,
		).Error; err != nil {
			slog.Error("manifest: failed to update service", "name", e.Name, "error", err)
			return
		}
		slog.Info("manifest: service updated", "name", e.Name)
	}

	serviceID := svcModel.ServiceID
	conn.Exec(`DELETE FROM service_endpoints WHERE service_id = ?`, serviceID)      //nolint:errcheck
	conn.Exec(`DELETE FROM service_roles WHERE service_id = ?`, serviceID)          //nolint:errcheck
	conn.Exec(`DELETE FROM service_actions WHERE service_id = ?`, serviceID)        //nolint:errcheck
	conn.Exec(`DELETE FROM service_default_grants WHERE service_id = ?`, serviceID) //nolint:errcheck

	for _, ep := range e.Endpoints {
		m := ServiceEndpointModel{
			EndpointID: uuid.New().String(),
			ServiceID:  serviceID,
			Method:     ep.Method,
			Path:       ep.Path,
			Action:     ep.Action,
			Resource:   ep.Resource,
			Public:     ep.Public,
		}
		if err := conn.Create(&m).Error; err != nil {
			slog.Error("manifest: failed to insert endpoint", "service", e.Name, "path", ep.Path, "error", err)
		}
	}
	slog.Info("manifest: endpoints registered", "name", e.Name, "count", len(e.Endpoints))

	for _, a := range e.Actions {
		if a.Name == "" || a.Method == "" || a.Path == "" {
			continue
		}
		m := ServiceActionModel{
			ActionID:       uuid.New().String(),
			ServiceID:      serviceID,
			Name:           a.Name,
			Method:         a.Method,
			Path:           a.Path,
			BodyTransforms: jsonbBytes(a.BodyTransforms),
			AsyncConfig:    jsonbBytes(a.Async),
		}
		if err := conn.Create(&m).Error; err != nil {
			slog.Error("manifest: failed to insert action", "service", e.Name, "action", a.Name, "error", err)
		}
	}
	if len(e.Actions) > 0 {
		slog.Info("manifest: actions registered", "name", e.Name, "count", len(e.Actions))
	}

	for _, g := range e.DefaultGrants {
		if g.GrantOn == "" || len(g.Actions) == 0 || len(g.Resources) == 0 {
			continue
		}
		actionsJSON, _ := json.Marshal(g.Actions)
		resourcesJSON, _ := json.Marshal(g.Resources)
		m := ServiceDefaultGrantModel{
			GrantID:   uuid.New().String(),
			ServiceID: serviceID,
			GrantOn:   g.GrantOn,
			Actions:   actionsJSON,
			Resources: resourcesJSON,
		}
		if err := conn.Create(&m).Error; err != nil {
			slog.Error("manifest: failed to insert default grant", "service", e.Name, "grant_on", g.GrantOn, "error", err)
		}
	}
	if len(e.DefaultGrants) > 0 {
		slog.Info("manifest: default grants registered", "name", e.Name, "count", len(e.DefaultGrants))
	}
}

func isDbNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
