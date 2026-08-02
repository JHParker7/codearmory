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
	"gorm.io/gorm/logger"
)

var (
	gormDB     *gorm.DB
	gormDBRead *gorm.DB
	dbInitMu   sync.Mutex
)

// errBackendNotFound is returned when a lookup matches no backend.
var errBackendNotFound = errors.New("backend not found")

// errRepoNotFound is returned when a manual-repo lookup matches nothing.
var errRepoNotFound = errors.New("repo not found")

func connect() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/git")), &gorm.Config{
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
		readURL = secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/git")
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

// Add inserts a new backend.
func (b GitBackend) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("git").Start(ctx, "db.backend.add")
	defer span.End()
	span.SetAttributes(attribute.String("backend.id", b.ID))
	if err := connect().WithContext(ctx).Create(&b).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update persists base_url/host/auth changes for an existing backend.
func (b GitBackend) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("git").Start(ctx, "db.backend.update")
	defer span.End()
	span.SetAttributes(attribute.String("backend.id", b.ID))
	b.UpdatedAt = time.Now().UTC()
	if err := connect().WithContext(ctx).
		Model(&GitBackend{}).
		Where("id = ? AND owner = ?", b.ID, b.Owner).
		Updates(map[string]any{
			"base_url":      b.BaseURL,
			"host":          b.Host,
			"auth_mode":     b.AuthMode,
			"auth_enc":      b.AuthEnc,
			"prefer_mirror": b.PreferMirror,
			"updated_at":    b.UpdatedAt,
		}).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove hard-deletes a backend owned by the caller.
func (b GitBackend) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("git").Start(ctx, "db.backend.remove")
	defer span.End()
	span.SetAttributes(attribute.String("backend.id", b.ID))
	res := connect().WithContext(ctx).
		Where("id = ? AND owner = ?", b.ID, b.Owner).
		Delete(&GitBackend{})
	if res.Error != nil {
		span.RecordError(res.Error)
		span.SetStatus(codes.Error, res.Error.Error())
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errBackendNotFound
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// getBackendByID fetches a single backend scoped to its owner.
func getBackendByID(ctx context.Context, owner, id string) (GitBackend, error) {
	ctx, span := otel.Tracer("git").Start(ctx, "db.backend.get")
	defer span.End()
	var b GitBackend
	err := connectRead().WithContext(ctx).
		Where("id = ? AND owner = ?", id, owner).
		First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return GitBackend{}, errBackendNotFound
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return GitBackend{}, err
	}
	return b, nil
}

// getBackendByHost resolves the backend a clone host belongs to, for a given owner.
//
// A user's own link always wins. Only when they have none for the host does it fall
// back to a platform-owned backend (see platformOwner) — the in-cluster git-factory
// seeded from GIT_FACTORY_URL — so cloning the platform's own git host needs no per-user setup
// while a user who has deliberately linked that host keeps their own credential.
func getBackendByHost(ctx context.Context, owner, host string) (GitBackend, error) {
	ctx, span := otel.Tracer("git").Start(ctx, "db.backend.get_by_host")
	defer span.End()
	var b GitBackend
	err := connectRead().WithContext(ctx).
		Where("owner = ? AND host = ?", owner, host).
		First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) && owner != platformOwner {
		err = connectRead().WithContext(ctx).
			Where("owner = ? AND host = ?", platformOwner, host).
			First(&b).Error
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return GitBackend{}, errBackendNotFound
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return GitBackend{}, err
	}
	return b, nil
}

// upsertPlatformBackend registers (or refreshes) a platform-owned backend, keyed by
// host so repeated calls converge instead of piling up rows. Both the startup seeder and
// the internal endpoint re-run it on a level-triggered loop, so it must be idempotent and
// must not churn the row's ID — nothing references it, but a stable ID keeps logs and
// traces readable across passes.
func upsertPlatformBackend(ctx context.Context, b GitBackend) (GitBackend, error) {
	ctx, span := otel.Tracer("git").Start(ctx, "db.backend.upsert_platform")
	defer span.End()

	var existing GitBackend
	err := connect().WithContext(ctx).
		Where("owner = ? AND host = ?", platformOwner, b.Host).
		First(&existing).Error
	switch {
	case err == nil:
		existing.Name = b.Name
		existing.Type = b.Type
		existing.BaseURL = b.BaseURL
		existing.AuthMode = b.AuthMode
		existing.AuthEnc = b.AuthEnc
		existing.UpdatedAt = time.Now().UTC()
		if err := connect().WithContext(ctx).Save(&existing).Error; err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return GitBackend{}, err
		}
		span.SetStatus(codes.Ok, "")
		return existing, nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		if err := b.Add(ctx); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return GitBackend{}, err
		}
		span.SetStatus(codes.Ok, "")
		return b, nil
	default:
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return GitBackend{}, err
	}
}

// Add inserts a new manual repo.
func (rp GitRepo) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("git").Start(ctx, "db.repo.add")
	defer span.End()
	span.SetAttributes(attribute.String("repo.id", rp.ID))
	if err := connect().WithContext(ctx).Create(&rp).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove hard-deletes a manual repo owned by the caller.
func (rp GitRepo) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("git").Start(ctx, "db.repo.remove")
	defer span.End()
	span.SetAttributes(attribute.String("repo.id", rp.ID))
	res := connect().WithContext(ctx).
		Where("id = ? AND owner = ?", rp.ID, rp.Owner).
		Delete(&GitRepo{})
	if res.Error != nil {
		span.RecordError(res.Error)
		span.SetStatus(codes.Error, res.Error.Error())
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errRepoNotFound
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// UpdateSync writes the GitOps workflow-sync settings a caller actually supplied on a
// manual repo owned by them, and returns the row as it now stands.
//
// cols names the columns to write — that is what keeps PUT /repos/{id} a PARTIAL
// update. A field the request omitted is absent from cols and keeps its stored value;
// a field it did supply is written EVEN WHEN ZERO, since Select forces GORM to persist
// `false` / an empty allowlist rather than skip them as unset. Writing both columns
// unconditionally (the previous behaviour) silently disabled sync on a branches-only
// PUT and wiped the allowlist — falling back to "main" — on an enabled-only one, which
// would sync from a branch the user had deliberately excluded.
//
// The []string branch allowlist round-trips through the field's json serializer.
func (rp GitRepo) UpdateSync(ctx context.Context, cols []string) (GitRepo, error) {
	ctx, span := otel.Tracer("git").Start(ctx, "db.repo.update_sync")
	defer span.End()
	span.SetAttributes(attribute.String("repo.id", rp.ID))
	db := connect().WithContext(ctx)
	if len(cols) > 0 {
		res := db.
			Model(&GitRepo{}).
			Where("id = ? AND owner = ?", rp.ID, rp.Owner).
			Select(cols).
			Updates(GitRepo{WorkflowSyncEnabled: rp.WorkflowSyncEnabled, WorkflowSyncBranches: rp.WorkflowSyncBranches})
		if res.Error != nil {
			span.RecordError(res.Error)
			span.SetStatus(codes.Error, res.Error.Error())
			return GitRepo{}, res.Error
		}
		if res.RowsAffected == 0 {
			return GitRepo{}, errRepoNotFound
		}
	}
	// Read the row back so the response reflects what is STORED rather than only the
	// fields this request carried — an omitted field keeps its old value and the caller
	// has to see it. It also makes a request that changes nothing still 404 a repo that
	// does not exist (or is not the caller's). Read via the primary connection: a
	// replica could still be behind the write above.
	var stored GitRepo
	err := db.Where("id = ? AND owner = ?", rp.ID, rp.Owner).First(&stored).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return GitRepo{}, errRepoNotFound
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return GitRepo{}, err
	}
	span.SetStatus(codes.Ok, "")
	return stored, nil
}

// reposByURL returns every pinned repo (across all owners) with the given clone URL.
// A GitOps push webhook names a repo by URL, and the same repo may be pinned by more
// than one user; the sync policy is resolved per matching owner.
func reposByURL(ctx context.Context, url string) ([]GitRepo, error) {
	ctx, span := otel.Tracer("git").Start(ctx, "db.repo.by_url")
	defer span.End()
	var repos []GitRepo
	if err := connectRead().WithContext(ctx).Where("url = ?", url).Find(&repos).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	return repos, nil
}

// listRepos returns all manually-registered repos owned by the caller, newest first.
func listRepos(ctx context.Context, owner string) ([]GitRepo, error) {
	ctx, span := otel.Tracer("git").Start(ctx, "db.repo.list")
	defer span.End()
	var repos []GitRepo
	if err := connectRead().WithContext(ctx).
		Where("owner = ?", owner).
		Order("created_at DESC").
		Find(&repos).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	return repos, nil
}

// listBackends returns all backends owned by the caller, newest first.
func listBackends(ctx context.Context, owner string) ([]GitBackend, error) {
	ctx, span := otel.Tracer("git").Start(ctx, "db.backend.list")
	defer span.End()
	var backends []GitBackend
	if err := connectRead().WithContext(ctx).
		Where("owner = ?", owner).
		Order("created_at DESC").
		Find(&backends).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	return backends, nil
}
