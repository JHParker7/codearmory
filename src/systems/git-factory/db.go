package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
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

// errRepoNotFound is returned when a lookup matches no Repo owned by the caller.
var errRepoNotFound = errors.New("Repo not found")

// gitHTTPBaseURL is the externally reachable base for clone URLs, e.g.
// "https://git.example.com". Resolved per call rather than cached in a package
// var so tests (and a reconfigured deployment) can override the environment.
// Any trailing slash is trimmed so callers always join with a single "/".
func gitHTTPBaseURL() string {
	base := envOrDefault("GIT_HTTP_BASE_URL", "http://localhost:"+envOrDefault("PORT", "9002"))
	return strings.TrimRight(base, "/")
}

// cloneURL is the single place the clone URL is composed: the base configured right
// now, plus the repo's current path segments.
func cloneURL(namespace, name string) string {
	return gitHTTPBaseURL() + "/" + namespace + "/" + name + ".git"
}

// AfterFind derives HttpUrl on every load. GORM runs it for First and Find, which is
// every read path there is (getRepo, getRepoByID, getRepoByPath, listRepos), so no
// caller has to remember to fill it in — and a repo created before GIT_HTTP_BASE_URL
// was set, or renamed since, still advertises a URL that resolves.
func (re *Repo) AfterFind(*gorm.DB) error {
	re.HttpUrl = cloneURL(re.Namespace, re.Name)
	return nil
}

// connect lazily opens the write DB connection. The database itself must already
// exist (see infra init.sql / the tests stack); GORM AutoMigrate creates tables.
func connect() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/codearmory_git_factory")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		slog.Error("unable to connect to database", "error", err)
		os.Exit(1)
	}
	gormDB = conn
	return gormDB
}

// dbReady reports whether a database handle already exists, WITHOUT opening one.
//
// It exists because connect/connectRead exit the process when they cannot reach the
// database — correct at startup, fatal anywhere optional. Background fan-out (push and
// pull-request notifications) must be able to ask "is there a database to read?" and do
// nothing when the answer is no, rather than taking the process down from a goroutine.
// In a running service this is always true: main connects and migrates before serving.
func dbReady() bool {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	return gormDB != nil || gormDBRead != nil
}

// connectRead lazily opens a read connection, using DATABASE_READ_URL when set
// (a read replica) and otherwise sharing the primary.
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
		readURL = secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/codearmory_git_factory")
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

// Add inserts a new Repo.
func (re Repo) Add(ctx context.Context) error {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "db.Repo.add")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", re.ID))
	// Shard is the placement key (§5-Step2). It derives from the STABLE id, never the
	// name: keying placement by name would move a repo's bytes on rename, which is
	// exactly the property §4 keeps by deriving the on-disk path from the id.
	re.Shard = shardOf(re.ID)
	// Namespace is set by the caller — the org name for an org repo, otherwise the
	// caller's username. It is NOT the owner: Owner is the gatekeeper user_id used
	// as the authorization filter, and must never leak into a clone URL.
	re.HttpUrl = cloneURL(re.Namespace, re.Name)
	if err := CreateGitRepo(ctx, re); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	if err := connect().WithContext(ctx).Create(&re).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update persists name/description changes for a Repo owned by the caller.
func (re Repo) Update(ctx context.Context) error {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "db.Repo.update")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", re.ID))
	re.UpdatedAt = time.Now().UTC()
	updates := map[string]any{
		"name":        re.Name,
		"description": re.Description,
		"updated_at":  re.UpdatedAt,
	}
	// Visibility is updated only when the caller named one. Publishing a repo has to be
	// an explicit act, so an omitted field leaves it as it was rather than resetting it
	// to the zero value the way name and description are handled.
	if re.Visibility != "" {
		updates["visibility"] = re.Visibility
	}
	// Filing into a project is likewise explicit — set only when the caller named one,
	// so a plain rename never moves a repo between projects.
	if re.Project != "" {
		updates["project"] = re.Project
		updates["project_id"] = re.ProjectID
		updates["project_namespace"] = re.ProjectNamespace
	}
	res := connect().WithContext(ctx).
		Model(&Repo{}).
		Where("id = ? AND owner = ?", re.ID, re.Owner).
		Updates(updates)
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

// Remove hard-deletes a Repo owned by the caller.
func (re Repo) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "db.Repo.remove")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", re.ID))

	// Delete the owner-scoped row FIRST. This WHERE clause is the only per-record
	// ownership check in the delete path — gatekeeper authorises the action, not
	// the specific record — so RowsAffected is the proof of ownership. Removing
	// the bytes before this ran would let any authenticated caller destroy another
	// user's repository with a guessed id, while the victim's row survived and the
	// API still answered 404.
	res := connect().WithContext(ctx).
		Where("id = ? AND owner = ?", re.ID, re.Owner).
		Delete(&Repo{})
	if res.Error != nil {
		span.RecordError(res.Error)
		span.SetStatus(codes.Error, res.Error.Error())
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errRepoNotFound
	}

	// Ownership proven. A failure here orphans the directory rather than losing the
	// row — the recoverable direction, and the one a reconciler can sweep up
	// (ARCHITECTURE §6: pick an order, then reconcile).
	if err := DeleteGitRepo(ctx, re); err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "repo row deleted but on-disk repo remains",
			"Repo_id", re.ID, "error", err)
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

// touchRepo bumps updated_at after a successful push. Without it "last updated" only
// ever reflects a metadata edit, so a repo pushed to daily looks untouched since the day
// it was created — the opposite of what the field is read as. Not owner-scoped: the
// caller has already authorised the push.
func touchRepo(ctx context.Context, id string) error {
	return connect().WithContext(ctx).Model(&Repo{}).
		Where("id = ?", id).
		Update("updated_at", time.Now().UTC()).Error
}

// setRepoSize stores the measured size of a repo. Not owner-scoped: it is called
// after a push or a maintenance sweep, both of which have already been authorised,
// and the id comes from a row we just loaded.
func setRepoSize(ctx context.Context, id string, sizeBytes int64) error {
	return connect().WithContext(ctx).Model(&Repo{}).
		Where("id = ?", id).
		Update("size_bytes", sizeBytes).Error
}

// getRepoSize reads the last measured size. Selecting the one column keeps the quota
// check off the hot path of the push it gates.
func getRepoSize(ctx context.Context, id string) (int64, error) {
	var size int64
	err := connectRead().WithContext(ctx).Model(&Repo{}).
		Where("id = ?", id).
		Select("size_bytes").
		Scan(&size).Error
	return size, err
}

// listAllRepoIDs returns every repo id, for the maintenance sweep. Ids only: the
// sweep needs nothing else, and a full scan of a growing table is the one query here
// that has to stay cheap.
func listAllRepoIDs(ctx context.Context) ([]string, error) {
	var ids []string
	err := connectRead().WithContext(ctx).Model(&Repo{}).
		Order("created_at ASC").
		Pluck("id", &ids).Error
	return ids, err
}

// getRepoByPath fetches a Repo by its clone-URL path, {namespace}/{name}. Unlike
// getRepo this is deliberately NOT owner-scoped: the git wire routes address a repo by
// the path in the URL and only then learn who is calling, so the record has to be loaded
// before it can be authorized (see authorizeGitRepo, which asks gatekeeper about the
// repo's OWN namespace and answers 404 on a denial so existence is not leaked).
// (Namespace, Name) is unique, so this matches at most one.
func getRepoByPath(ctx context.Context, namespace, name string) (Repo, error) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "db.Repo.getByPath")
	defer span.End()
	var re Repo
	err := connectRead().WithContext(ctx).
		Where("namespace = ? AND name = ?", namespace, name).
		First(&re).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Repo{}, errRepoNotFound
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Repo{}, err
	}
	return re, nil
}

// getRepoByID fetches a Repo without an owner filter. Per-record authorization is now
// gatekeeper's answer on an owner-first resource (resRepoOf), so the caller must load
// the repo FIRST to learn whose namespace to ask about — an owner-scoped load would
// hide exactly the repos a grant is meant to reveal.
func getRepoByID(ctx context.Context, id string) (Repo, error) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "db.Repo.getByID")
	defer span.End()
	var re Repo
	err := connectRead().WithContext(ctx).Where("id = ?", id).First(&re).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Repo{}, errRepoNotFound
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Repo{}, err
	}
	return re, nil
}

// getRepo fetches a single Repo scoped to its owner.
func getRepo(ctx context.Context, owner, id string) (Repo, error) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "db.Repo.get")
	defer span.End()
	var re Repo
	err := connectRead().WithContext(ctx).
		Where("id = ? AND owner = ?", id, owner).
		First(&re).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Repo{}, errRepoNotFound
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Repo{}, err
	}
	return re, nil
}

// listRepos returns all Repos owned by the caller, newest first.
func listRepos(ctx context.Context, owner string, projectIDs []string) ([]Repo, error) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "db.Repo.list")
	defer span.End()
	var Repos []Repo
	q := connectRead().WithContext(ctx)
	if len(projectIDs) > 0 {
		// Widen to repos filed into a project the caller can reach, not just their own.
		q = q.Where("owner = ? OR project_id IN ?", owner, projectIDs)
	} else {
		q = q.Where("owner = ?", owner)
	}
	if err := q.
		Order("created_at DESC").
		Find(&Repos).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	return Repos, nil
}
