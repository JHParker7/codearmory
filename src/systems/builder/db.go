package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"gorm.io/gorm/logger"
)

var gormDB *gorm.DB
var gormDBRead *gorm.DB
var dbInitMu sync.Mutex

func connect() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/builder")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		slog.Error("unable to connect to database", "error", err)
		os.Exit(1)
	}
	gormDB = conn
	return gormDB
}

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
		readURL = secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/builder")
	}
	conn, err := gorm.Open(postgres.Open(readURL), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		slog.Error("unable to connect to read database", "error", err)
		os.Exit(1)
	}
	gormDBRead = conn
	return gormDBRead
}

func isNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// getOrgService loads the row for one (org, service) scope, or ErrRecordNotFound.
func getOrgService(ctx context.Context, orgID, service string) (OrgService, error) {
	ctx, span := otel.Tracer("builder").Start(ctx, "db.org_service.get")
	defer span.End()
	span.SetAttributes(attribute.String("org.id", orgID), attribute.String("service", service))
	var row OrgService
	if err := connectRead().WithContext(ctx).
		Where("org_id = ? AND service_name = ?", orgID, service).
		First(&row).Error; err != nil {
		return OrgService{}, err
	}
	return row, nil
}

// getOrgServicePrimary loads the row from the primary (not the read replica), so a
// value written earlier in the same admin flow is visible. The enable-time validation
// gate uses it: a just-stored db_url/secret must not be missed to a lagging replica.
func getOrgServicePrimary(ctx context.Context, orgID, service string) (OrgService, error) {
	ctx, span := otel.Tracer("builder").Start(ctx, "db.org_service.get_primary")
	defer span.End()
	span.SetAttributes(attribute.String("org.id", orgID), attribute.String("service", service))
	var row OrgService
	if err := connect().WithContext(ctx).
		Where("org_id = ? AND service_name = ?", orgID, service).
		First(&row).Error; err != nil {
		return OrgService{}, err
	}
	return row, nil
}

// listOrgServices returns every desired-state row for a single scope (no default
// merge — callers that need the merge use the catalog overlay).
func listOrgServices(ctx context.Context, orgID string) ([]OrgService, error) {
	ctx, span := otel.Tracer("builder").Start(ctx, "db.org_service.list")
	defer span.End()
	span.SetAttributes(attribute.String("org.id", orgID))
	var rows []OrgService
	if err := connectRead().WithContext(ctx).
		Where("org_id = ?", orgID).
		Order("service_name ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// configColumnValue encodes Config the way GORM's `serializer:json` tag would.
// That tag is honoured for struct writes (Create/Save), but NOT for a column named
// in the map handed to Updates — the driver then receives a raw map[string]any bound
// to a text column and rejects the whole statement ("cannot find encode plan"), so
// every config change on an existing row failed. A nil map becomes SQL NULL, matching
// what the serializer stores, so reading it back still yields a nil map.
func configColumnValue(config map[string]any) (any, error) {
	if config == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return string(encoded), nil
}

// upsertOrgService creates or updates the row for a (org, service) scope. The
// caller is responsible for setting Enabled/Kind/Config explicitly; we update the
// mutable columns by hand so a false Enabled is never dropped by a GORM default.
func upsertOrgService(ctx context.Context, in OrgService) (OrgService, error) {
	ctx, span := otel.Tracer("builder").Start(ctx, "db.org_service.upsert")
	defer span.End()
	span.SetAttributes(attribute.String("org.id", in.OrgID), attribute.String("service", in.ServiceName))

	existing, err := getOrgService(ctx, in.OrgID, in.ServiceName)
	switch {
	case err == nil:
		now := time.Now().UTC()
		cfg, err := configColumnValue(in.Config)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return OrgService{}, err
		}
		updates := map[string]any{
			"enabled":     in.Enabled,
			"kind":        in.Kind,
			"config":      cfg,
			"image":       in.Image,
			"port":        in.Port,
			"description": in.Description,
			"updated_at":  now,
		}
		// Only overwrite the encrypted DB URL when the caller supplied a new one,
		// so a plain enable/disable/config change never drops it.
		if in.DBURLCiphertext != nil {
			updates["db_url_ct"] = in.DBURLCiphertext
			updates["db_host"] = in.DBHost
		}
		// Likewise the encrypted secrets map: keep the stored one unless re-supplied.
		if in.SecretsCiphertext != nil {
			updates["secrets_ct"] = in.SecretsCiphertext
		}
		if err := connect().WithContext(ctx).Model(&OrgService{}).
			Where("org_service_id = ?", existing.OrgServiceID).
			Updates(updates).Error; err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return OrgService{}, err
		}
		return getOrgService(ctx, in.OrgID, in.ServiceName)
	case isNotFound(err):
		in.OrgServiceID = uuid.New().String()
		in.CreatedAt = time.Now().UTC()
		in.UpdatedAt = in.CreatedAt
		if err := connect().WithContext(ctx).Create(&in).Error; err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return OrgService{}, err
		}
		return in, nil
	default:
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return OrgService{}, err
	}
}

// deleteOrgService hard-deletes the override/custom row for a scope. Hard delete
// (not soft) so the (org_id, service_name) unique slot frees up and effective
// resolution falls back through to the default-scope baseline.
func deleteOrgService(ctx context.Context, orgID, service string) (bool, error) {
	ctx, span := otel.Tracer("builder").Start(ctx, "db.org_service.delete")
	defer span.End()
	span.SetAttributes(attribute.String("org.id", orgID), attribute.String("service", service))
	res := connect().WithContext(ctx).
		Where("org_id = ? AND service_name = ?", orgID, service).
		Delete(&OrgService{})
	if res.Error != nil {
		span.RecordError(res.Error)
		span.SetStatus(codes.Error, res.Error.Error())
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// enabledFrom applies the effective-enabled precedence to already-loaded rows
// (pure, so it is unit-tested without a database):
//   - core services are always enabled;
//   - an explicit org override wins;
//   - else the default-scope baseline wins;
//   - else default-on (nothing configured never breaks existing behaviour).
func enabledFrom(service string, override, dflt *OrgService) bool {
	if coreServices[service] {
		return true
	}
	if override != nil {
		return override.Enabled
	}
	if dflt != nil {
		return dflt.Enabled
	}
	return true
}

// computeDisabled merges the default-scope baseline with an org's overrides and
// returns the set of services explicitly disabled for that org (override wins).
// Pure: callers load the rows. Core services are never included.
func computeDisabled(defaults, overrides []OrgService) []string {
	disabled := map[string]bool{}
	for _, r := range defaults {
		if !coreServices[r.ServiceName] && !r.Enabled {
			disabled[r.ServiceName] = true
		}
	}
	for _, r := range overrides {
		if coreServices[r.ServiceName] {
			continue
		}
		if r.Enabled {
			delete(disabled, r.ServiceName)
		} else {
			disabled[r.ServiceName] = true
		}
	}
	out := make([]string, 0, len(disabled))
	for s := range disabled {
		out = append(out, s)
	}
	return out
}

// effectiveEnabled resolves whether a service is enabled for an org by loading
// the relevant rows and applying enabledFrom.
func effectiveEnabled(ctx context.Context, orgID, service string) bool {
	var override, dflt *OrgService
	if orgID != "" && orgID != defaultOrgID {
		if row, err := getOrgService(ctx, orgID, service); err == nil {
			override = &row
		}
	}
	if row, err := getOrgService(ctx, defaultOrgID, service); err == nil {
		dflt = &row
	}
	return enabledFrom(service, override, dflt)
}

// effectiveDisabledSet returns the set of services explicitly disabled for an org,
// merging the default-scope baseline with the org's overrides.
func effectiveDisabledSet(ctx context.Context, orgID string) ([]string, error) {
	defaults, err := listOrgServices(ctx, defaultOrgID)
	if err != nil {
		return nil, err
	}
	var overrides []OrgService
	if orgID != "" && orgID != defaultOrgID {
		overrides, err = listOrgServices(ctx, orgID)
		if err != nil {
			return nil, err
		}
	}
	return computeDisabled(defaults, overrides), nil
}
