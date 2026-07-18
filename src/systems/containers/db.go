package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// db is the common interface implemented by all persistent entities, mirroring
// the pattern used by the other GORM services in this repo.
type db interface {
	Add(ctx context.Context) error
	Update(ctx context.Context) error
	Remove(ctx context.Context) error
	Get(ctx context.Context) (db, error)
	List(ctx context.Context, limit, offset int) ([]db, error)
}

var (
	gormDB     *gorm.DB
	gormDBRead *gorm.DB
	dbInitMu   sync.Mutex
)

const defaultDSN = "postgresql://postgres:postgres@localhost:5432/containers"

func connect() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", defaultDSN)), &gorm.Config{
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
		readURL = secretOrDefault("DATABASE_URL", defaultDSN)
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

// migrate creates the registries table on startup. No separate migration step —
// matches the CREATE-on-boot pattern of the other services.
func migrate() error {
	return connect().AutoMigrate(&Registry{})
}

// ── Registry ────────────────────────────────────────────────────────────────

// Registry is one upstream OCI/Docker registry the service can proxy to. The
// service can hold several; exactly one is marked IsDefault and receives all
// docker push/pull (/v2) traffic and any read request that does not name a
// specific registry.
type Registry struct {
	ID        string    `json:"id"         gorm:"column:id;primaryKey"`
	Name      string    `json:"name"       gorm:"column:name;uniqueIndex"`
	URL       string    `json:"url"        gorm:"column:url"`
	Username  string    `json:"username"   gorm:"column:username"`
	Password  string    `json:"-"          gorm:"column:password"`
	IsDefault bool      `json:"is_default" gorm:"column:is_default"`
	CreatedAt time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt time.Time `json:"updated_at" gorm:"column:updated_at"`
}

// TableName sets the GORM table name for Registry.
func (Registry) TableName() string { return "registries" }

func (rg Registry) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("containers").Start(ctx, "db.registry.add")
	defer span.End()
	span.SetAttributes(attribute.String("registry.name", rg.Name))
	if err := connect().WithContext(ctx).Create(&rg).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (rg Registry) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("containers").Start(ctx, "db.registry.update")
	defer span.End()
	span.SetAttributes(attribute.String("registry.name", rg.Name))
	// Select every column so a write of a zero value (e.g. IsDefault=false,
	// cleared username) is persisted rather than skipped by GORM's omit-zero.
	if err := connect().WithContext(ctx).
		Model(&Registry{}).
		Where("id = ?", rg.ID).
		Select("name", "url", "username", "password", "is_default", "updated_at").
		Updates(map[string]any{
			"name":       rg.Name,
			"url":        rg.URL,
			"username":   rg.Username,
			"password":   rg.Password,
			"is_default": rg.IsDefault,
			"updated_at": rg.UpdatedAt,
		}).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (rg Registry) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("containers").Start(ctx, "db.registry.remove")
	defer span.End()
	span.SetAttributes(attribute.String("registry.name", rg.Name))
	if err := connect().WithContext(ctx).Where("id = ?", rg.ID).Delete(&Registry{}).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// errRegistryNotFound is returned when a lookup matches no row.
var errRegistryNotFound = errors.New("registry not found")

// getRegistryByName returns the registry with the given name, or
// errRegistryNotFound. Uses the read connection.
func getRegistryByName(ctx context.Context, name string) (Registry, error) {
	var rg Registry
	err := connectRead().WithContext(ctx).Where("name = ?", name).First(&rg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Registry{}, errRegistryNotFound
	}
	return rg, err
}

// getDefaultRegistry returns the registry currently marked as default, or
// errRegistryNotFound when none is configured.
func getDefaultRegistry(ctx context.Context) (Registry, error) {
	var rg Registry
	err := connectRead().WithContext(ctx).Where("is_default = ?", true).First(&rg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Registry{}, errRegistryNotFound
	}
	return rg, err
}

// listRegistries returns all configured registries, default first then by name.
func listRegistries(ctx context.Context) ([]Registry, error) {
	var rgs []Registry
	err := connectRead().WithContext(ctx).
		Order("is_default DESC, name ASC").
		Find(&rgs).Error
	return rgs, err
}

// countRegistries returns how many registries are configured.
func countRegistries(ctx context.Context) (int64, error) {
	var n int64
	err := connectRead().WithContext(ctx).Model(&Registry{}).Count(&n).Error
	return n, err
}

// addRegistryTx inserts rg and, when rg.IsDefault, atomically demotes every
// other registry so the single-default invariant holds.
func addRegistryTx(ctx context.Context, rg Registry) error {
	ctx, span := otel.Tracer("containers").Start(ctx, "db.registry.add_tx")
	defer span.End()
	span.SetAttributes(attribute.String("registry.name", rg.Name))
	err := connect().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if rg.IsDefault {
			if err := tx.Model(&Registry{}).Where("is_default = ?", true).
				Update("is_default", false).Error; err != nil {
				return err
			}
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&rg).Error
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// setDefaultRegistry marks the named registry default and demotes all others,
// atomically. Returns errRegistryNotFound when no registry has that name.
func setDefaultRegistry(ctx context.Context, name string) error {
	ctx, span := otel.Tracer("containers").Start(ctx, "db.registry.set_default")
	defer span.End()
	span.SetAttributes(attribute.String("registry.name", name))
	err := connect().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rg Registry
		if err := tx.Where("name = ?", name).First(&rg).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errRegistryNotFound
			}
			return err
		}
		if err := tx.Model(&Registry{}).Where("is_default = ?", true).
			Update("is_default", false).Error; err != nil {
			return err
		}
		return tx.Model(&Registry{}).Where("id = ?", rg.ID).
			Update("is_default", true).Error
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// promoteAnyDefault ensures a default exists when registries remain: if none is
// currently marked default, the oldest registry is promoted. Called after a
// delete that may have removed the default.
func promoteAnyDefault(ctx context.Context) error {
	return connect().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&Registry{}).Where("is_default = ?", true).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		var rg Registry
		if err := tx.Order("created_at ASC").First(&rg).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil // no registries left — nothing to promote
			}
			return err
		}
		return tx.Model(&Registry{}).Where("id = ?", rg.ID).Update("is_default", true).Error
	})
}
