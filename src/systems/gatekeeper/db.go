package main

import (
	"context"
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

// Add inserts the user. Nil OrgID, TeamID, and RoleID are written as NULL.
func (user User) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.user.add")
	defer span.End()
	span.SetAttributes(attribute.String("user.id", user.UserID))
	user.Active = true
	if err := connect().WithContext(ctx).Create(&user).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all user fields. Nil OrgID, TeamID, and RoleID are written as NULL.
// Callers must pass a fully-populated struct: GORM Save writes every field including
// zero values, so a partial struct will blank out username, email, and other columns.
// Always Get() the user first, mutate the desired fields, then call Update().
func (user User) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.user.update")
	defer span.End()
	span.SetAttributes(attribute.String("user.id", user.UserID))
	user.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&user).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:user:"+user.UserID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the user by setting active = false.
func (user User) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.user.remove")
	defer span.End()
	span.SetAttributes(attribute.String("user.id", user.UserID))
	if err := connect().WithContext(ctx).Model(&User{}).Where("user_id = ?", user.UserID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:user:"+user.UserID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active user by UserID or Username (whichever is non-empty).
// The OR lookup lets callers find a user by either field without separate methods.
func (user User) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.user.get")
	defer span.End()
	span.SetAttributes(attribute.String("user.id", user.UserID))
	if user.UserID != "" {
		if cached, ok := cacheGet[User](ctx, "gk:user:"+user.UserID); ok {
			span.SetStatus(codes.Ok, "")
			return cached, nil
		}
	}
	var newUser User
	if err := connectRead().WithContext(ctx).Where("(user_id = ? OR username = ?) AND active = ?", user.UserID, user.Username, true).First(&newUser).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	cacheSet(ctx, "gk:user:"+newUser.UserID, newUser, entityTTL)
	span.SetStatus(codes.Ok, "")
	return newUser, nil
}

// List retrieves all active users matching the non-zero fields of the receiver.
func (user User) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.user.list")
	defer span.End()
	span.SetAttributes(attribute.String("user.id", user.UserID))
	var users []User
	user.Active = true
	q := connectRead().WithContext(ctx).Where(user)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&users).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(users))
	for i, u := range users {
		result[i] = u
	}
	return result, nil
}

// Add inserts the org.
func (org Org) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.org.add")
	defer span.End()
	span.SetAttributes(attribute.String("org.id", org.OrgID))
	org.Active = true
	if err := connect().WithContext(ctx).Create(&org).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all org fields.
func (org Org) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.org.update")
	defer span.End()
	span.SetAttributes(attribute.String("org.id", org.OrgID))
	org.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&org).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:org:"+org.OrgID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the org by setting active = false.
func (org Org) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.org.remove")
	defer span.End()
	span.SetAttributes(attribute.String("org.id", org.OrgID))
	if err := connect().WithContext(ctx).Model(&Org{}).Where("org_id = ?", org.OrgID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:org:"+org.OrgID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active org by OrgID.
func (org Org) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.org.get")
	defer span.End()
	span.SetAttributes(attribute.String("org.id", org.OrgID))
	if cached, ok := cacheGet[Org](ctx, "gk:org:"+org.OrgID); ok {
		span.SetStatus(codes.Ok, "")
		return cached, nil
	}
	var newOrg Org
	if err := connectRead().WithContext(ctx).First(&newOrg, "org_id = ? AND active = ?", org.OrgID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	cacheSet(ctx, "gk:org:"+newOrg.OrgID, newOrg, entityTTL)
	span.SetStatus(codes.Ok, "")
	return newOrg, nil
}

// List retrieves all active orgs matching the non-zero fields of the receiver.
func (org Org) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.org.list")
	defer span.End()
	span.SetAttributes(attribute.String("org.id", org.OrgID))
	var orgs []Org
	org.Active = true
	q := connectRead().WithContext(ctx).Where(org)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&orgs).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(orgs))
	for i, o := range orgs {
		result[i] = o
	}
	return result, nil
}

// Add inserts the team.
func (team Team) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.team.add")
	defer span.End()
	span.SetAttributes(attribute.String("team.id", team.TeamID))
	team.Active = true
	if err := connect().WithContext(ctx).Create(&team).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all team fields.
func (team Team) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.team.update")
	defer span.End()
	span.SetAttributes(attribute.String("team.id", team.TeamID))
	team.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&team).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:team:"+team.TeamID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the team by setting active = false.
func (team Team) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.team.remove")
	defer span.End()
	span.SetAttributes(attribute.String("team.id", team.TeamID))
	if err := connect().WithContext(ctx).Model(&Team{}).Where("team_id = ?", team.TeamID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:team:"+team.TeamID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active team by TeamID.
func (team Team) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.team.get")
	defer span.End()
	span.SetAttributes(attribute.String("team.id", team.TeamID))
	if cached, ok := cacheGet[Team](ctx, "gk:team:"+team.TeamID); ok {
		span.SetStatus(codes.Ok, "")
		return cached, nil
	}
	var newTeam Team
	if err := connectRead().WithContext(ctx).First(&newTeam, "team_id = ? AND active = ?", team.TeamID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	cacheSet(ctx, "gk:team:"+newTeam.TeamID, newTeam, entityTTL)
	span.SetStatus(codes.Ok, "")
	return newTeam, nil
}

// List retrieves all active teams matching the non-zero fields of the receiver.
func (team Team) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.team.list")
	defer span.End()
	span.SetAttributes(attribute.String("team.id", team.TeamID))
	var teams []Team
	team.Active = true
	q := connectRead().WithContext(ctx).Where(team)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&teams).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(teams))
	for i, t := range teams {
		result[i] = t
	}
	return result, nil
}

// Add inserts the role.
func (role Role) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.role.add")
	defer span.End()
	span.SetAttributes(attribute.String("role.id", role.RoleID))
	role.Active = true
	if err := connect().WithContext(ctx).Create(&role).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all role fields.
func (role Role) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.role.update")
	defer span.End()
	span.SetAttributes(attribute.String("role.id", role.RoleID))
	role.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&role).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:role:"+role.RoleID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the role by setting active = false.
func (role Role) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.role.remove")
	defer span.End()
	span.SetAttributes(attribute.String("role.id", role.RoleID))
	if err := connect().WithContext(ctx).Model(&Role{}).Where("role_id = ?", role.RoleID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:role:"+role.RoleID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active role by RoleID.
func (role Role) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.role.get")
	defer span.End()
	span.SetAttributes(attribute.String("role.id", role.RoleID))
	if cached, ok := cacheGet[Role](ctx, "gk:role:"+role.RoleID); ok {
		span.SetStatus(codes.Ok, "")
		return cached, nil
	}
	var newRole Role
	if err := connectRead().WithContext(ctx).First(&newRole, "role_id = ? AND active = ?", role.RoleID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	cacheSet(ctx, "gk:role:"+newRole.RoleID, newRole, entityTTL)
	span.SetStatus(codes.Ok, "")
	return newRole, nil
}

// List retrieves all active roles matching the non-zero fields of the receiver.
func (role Role) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.role.list")
	defer span.End()
	span.SetAttributes(attribute.String("role.id", role.RoleID))
	var roles []Role
	role.Active = true
	q := connectRead().WithContext(ctx).Where(role)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&roles).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(roles))
	for i, r := range roles {
		result[i] = r
	}
	return result, nil
}

// Add inserts the session.
func (session Session) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.session.add")
	defer span.End()
	span.SetAttributes(attribute.String("session.id", session.SessionID))
	session.Active = true
	if err := connect().WithContext(ctx).Create(&session).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	if session.UserID != "" {
		cacheTrackUserSession(ctx, session.UserID, session.SessionID)
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all session fields.
func (session Session) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.session.update")
	defer span.End()
	span.SetAttributes(attribute.String("session.id", session.SessionID))
	session.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&session).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:session:"+session.SessionID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the session by setting active = false.
func (session Session) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.session.remove")
	defer span.End()
	span.SetAttributes(attribute.String("session.id", session.SessionID))
	if err := connect().WithContext(ctx).Model(&Session{}).Where("session_id = ?", session.SessionID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:session:"+session.SessionID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active session by SessionID.
func (session Session) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.session.get")
	defer span.End()
	span.SetAttributes(attribute.String("session.id", session.SessionID))
	if cached, ok := cacheGet[Session](ctx, "gk:session:"+session.SessionID); ok {
		span.SetStatus(codes.Ok, "")
		return cached, nil
	}
	var newSession Session
	if err := connectRead().WithContext(ctx).First(&newSession, "session_id = ? AND active = ?", session.SessionID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	// Cache until the session's own expiry, not the default entityTTL. Using a
	// longer TTL would let authMiddleware accept already-expired sessions from cache.
	if ttl := time.Until(newSession.ExpiresAt); ttl > 0 {
		cacheSet(ctx, "gk:session:"+newSession.SessionID, newSession, ttl)
	}
	span.SetStatus(codes.Ok, "")
	return newSession, nil
}

// List retrieves all active sessions matching the non-zero fields of the receiver.
func (session Session) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.session.list")
	defer span.End()
	span.SetAttributes(attribute.String("session.id", session.SessionID))
	var sessions []Session
	session.Active = true
	q := connectRead().WithContext(ctx).Where(session)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&sessions).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(sessions))
	for i, s := range sessions {
		result[i] = s
	}
	return result, nil
}

// Add inserts the permissions record.
func (p Permissions) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions.add")
	defer span.End()
	span.SetAttributes(attribute.String("permissions.id", p.PermissionsID))
	p.Active = true
	if err := connect().WithContext(ctx).Create(&p).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all permissions fields.
func (p Permissions) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions.update")
	defer span.End()
	span.SetAttributes(attribute.String("permissions.id", p.PermissionsID))
	p.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&p).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:perm:"+p.PermissionsID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the permissions record by setting active = false.
func (p Permissions) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions.remove")
	defer span.End()
	span.SetAttributes(attribute.String("permissions.id", p.PermissionsID))
	if err := connect().WithContext(ctx).Model(&Permissions{}).Where("permissions_id = ?", p.PermissionsID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:perm:"+p.PermissionsID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active permissions record by PermissionsID.
func (p Permissions) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions.get")
	defer span.End()
	span.SetAttributes(attribute.String("permissions.id", p.PermissionsID))
	if cached, ok := cacheGet[Permissions](ctx, "gk:perm:"+p.PermissionsID); ok {
		span.SetStatus(codes.Ok, "")
		return cached, nil
	}
	var newP Permissions
	if err := connectRead().WithContext(ctx).First(&newP, "permissions_id = ? AND active = ?", p.PermissionsID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	cacheSet(ctx, "gk:perm:"+newP.PermissionsID, newP, entityTTL)
	span.SetStatus(codes.Ok, "")
	return newP, nil
}

// List retrieves all active permissions records matching the non-zero fields of the receiver.
func (p Permissions) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions.list")
	defer span.End()
	span.SetAttributes(attribute.String("permissions.id", p.PermissionsID))
	var perms []Permissions
	p.Active = true
	q := connectRead().WithContext(ctx).Where(p)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&perms).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(perms))
	for i, perm := range perms {
		result[i] = perm
	}
	return result, nil
}

// Add inserts the invite.
func (invite Invite) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.invite.add")
	defer span.End()
	span.SetAttributes(attribute.String("invite.id", invite.InviteID))
	invite.Active = true
	if err := connect().WithContext(ctx).Create(&invite).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all invite fields.
func (invite Invite) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.invite.update")
	defer span.End()
	span.SetAttributes(attribute.String("invite.id", invite.InviteID))
	invite.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&invite).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the invite by setting active = false.
func (invite Invite) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.invite.remove")
	defer span.End()
	span.SetAttributes(attribute.String("invite.id", invite.InviteID))
	if err := connect().WithContext(ctx).Model(&Invite{}).Where("invite_id = ?", invite.InviteID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// List retrieves all active invites matching the non-zero fields of the receiver.
func (invite Invite) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.invite.list")
	defer span.End()
	span.SetAttributes(attribute.String("invite.id", invite.InviteID))
	var invites []Invite
	invite.Active = true
	q := connectRead().WithContext(ctx).Where(invite)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&invites).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(invites))
	for i, inv := range invites {
		result[i] = inv
	}
	return result, nil
}

// listInvitesForCaller returns active invites where callerID is the inviter or
// callerEmail is the invitee. Optional filters narrow the result further.
func listInvitesForCaller(ctx context.Context, callerID, callerEmail, inviteID, resourceType, resourceID, status string, limit, offset int) ([]db, error) {
	q := connectRead().WithContext(ctx).
		Where("active = ? AND (inviter_id = ? OR invitee_email = ?)", true, callerID, callerEmail)
	if inviteID != "" {
		q = q.Where("invite_id = ?", inviteID)
	}
	if resourceType != "" {
		q = q.Where("resource_type = ?", resourceType)
	}
	if resourceID != "" {
		q = q.Where("resource_id = ?", resourceID)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	var invites []Invite
	if err := q.Find(&invites).Error; err != nil {
		return nil, err
	}
	result := make([]db, len(invites))
	for i, inv := range invites {
		result[i] = inv
	}
	return result, nil
}

// Add inserts the service account.
func (svc ServiceAccount) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_account.add")
	defer span.End()
	span.SetAttributes(attribute.String("service_account.name", svc.ServiceName))
	svc.Active = true
	if err := connect().WithContext(ctx).Create(&svc).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all service account fields.
func (svc ServiceAccount) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_account.update")
	defer span.End()
	span.SetAttributes(attribute.String("service_account.name", svc.ServiceName))
	svc.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&svc).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the service account.
func (svc ServiceAccount) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_account.remove")
	defer span.End()
	span.SetAttributes(attribute.String("service_account.id", svc.ServiceAccountID))
	if err := connect().WithContext(ctx).Model(&ServiceAccount{}).Where("service_account_id = ?", svc.ServiceAccountID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active service account by ServiceAccountID or ServiceName.
func (svc ServiceAccount) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_account.get")
	defer span.End()
	span.SetAttributes(attribute.String("service_account.name", svc.ServiceName))
	var result ServiceAccount
	if err := connectRead().WithContext(ctx).
		Where("(service_account_id = ? OR service_name = ?) AND active = ?", svc.ServiceAccountID, svc.ServiceName, true).
		First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

// List retrieves all active service accounts.
func (svc ServiceAccount) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_account.list")
	defer span.End()
	var rows []ServiceAccount
	q := connectRead().WithContext(ctx).Where("active = ?", true)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&rows).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(rows))
	for i, r := range rows {
		result[i] = r
	}
	return result, nil
}

// Add inserts the service permission request.
func (req ServicePermissionRequest) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_permission_request.add")
	defer span.End()
	span.SetAttributes(attribute.String("request.id", req.RequestID))
	req.Active = true
	if err := connect().WithContext(ctx).Create(&req).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all service permission request fields.
func (req ServicePermissionRequest) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_permission_request.update")
	defer span.End()
	span.SetAttributes(attribute.String("request.id", req.RequestID))
	req.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&req).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the service permission request.
func (req ServicePermissionRequest) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_permission_request.remove")
	defer span.End()
	span.SetAttributes(attribute.String("request.id", req.RequestID))
	if err := connect().WithContext(ctx).Model(&ServicePermissionRequest{}).Where("request_id = ?", req.RequestID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active service permission request by RequestID.
func (req ServicePermissionRequest) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_permission_request.get")
	defer span.End()
	span.SetAttributes(attribute.String("request.id", req.RequestID))
	var result ServicePermissionRequest
	if err := connectRead().WithContext(ctx).First(&result, "request_id = ? AND active = ?", req.RequestID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

// List retrieves active service permission requests matching the non-zero fields of the receiver.
func (req ServicePermissionRequest) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_permission_request.list")
	defer span.End()
	var rows []ServicePermissionRequest
	req.Active = true
	q := connectRead().WithContext(ctx).Where(req).Order("created_at DESC")
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&rows).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(rows))
	for i, r := range rows {
		result[i] = r
	}
	return result, nil
}

// Add inserts the TOTP credential.
func (c TOTPCredential) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.totp_credential.add")
	defer span.End()
	span.SetAttributes(attribute.String("credential.id", c.CredentialID))
	if err := connect().WithContext(ctx).Create(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all TOTP credential fields.
func (c TOTPCredential) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.totp_credential.update")
	defer span.End()
	span.SetAttributes(attribute.String("credential.id", c.CredentialID))
	c.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the TOTP credential.
func (c TOTPCredential) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.totp_credential.remove")
	defer span.End()
	span.SetAttributes(attribute.String("credential.id", c.CredentialID))
	if err := connect().WithContext(ctx).Model(&TOTPCredential{}).Where("credential_id = ?", c.CredentialID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active TOTP credential by CredentialID.
func (c TOTPCredential) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.totp_credential.get")
	defer span.End()
	span.SetAttributes(attribute.String("credential.id", c.CredentialID))
	var result TOTPCredential
	if err := connectRead().WithContext(ctx).First(&result, "credential_id = ? AND active = ?", c.CredentialID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

// List retrieves active TOTP credentials matching the non-zero fields of the receiver.
func (c TOTPCredential) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.totp_credential.list")
	defer span.End()
	var rows []TOTPCredential
	c.Active = true
	q := connectRead().WithContext(ctx).Where(c)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&rows).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(rows))
	for i, r := range rows {
		result[i] = r
	}
	return result, nil
}

// Add inserts the audit log entry.
func (a AuditLog) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.audit_log.add")
	defer span.End()
	span.SetAttributes(attribute.String("audit_log.id", a.AuditLogID))
	if err := connect().WithContext(ctx).Create(&a).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves an audit log entry by AuditLogID.
func (a AuditLog) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.audit_log.get")
	defer span.End()
	span.SetAttributes(attribute.String("audit_log.id", a.AuditLogID))
	var result AuditLog
	if err := connectRead().WithContext(ctx).First(&result, "audit_log_id = ?", a.AuditLogID).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

// List retrieves audit log entries matching the non-zero fields of the receiver.
func (a AuditLog) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.audit_log.list")
	defer span.End()
	var rows []AuditLog
	q := connectRead().WithContext(ctx).Where(a).Order("created_at DESC")
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&rows).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(rows))
	for i, r := range rows {
		result[i] = r
	}
	return result, nil
}

// Add inserts the permissions check audit record.
func (pc PermissionsCheck) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions_check.add")
	defer span.End()
	span.SetAttributes(attribute.String("permissions_check.id", pc.PermissionsCheckID))
	pc.Active = true
	if err := connect().WithContext(ctx).Create(&pc).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all permissions check fields.
func (pc PermissionsCheck) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions_check.update")
	defer span.End()
	span.SetAttributes(attribute.String("permissions_check.id", pc.PermissionsCheckID))
	pc.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&pc).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the permissions check record by setting active = false.
func (pc PermissionsCheck) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions_check.remove")
	defer span.End()
	span.SetAttributes(attribute.String("permissions_check.id", pc.PermissionsCheckID))
	if err := connect().WithContext(ctx).Model(&PermissionsCheck{}).Where("permissions_check_id = ?", pc.PermissionsCheckID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active permissions check record by PermissionsCheckID.
func (pc PermissionsCheck) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions_check.get")
	defer span.End()
	span.SetAttributes(attribute.String("permissions_check.id", pc.PermissionsCheckID))
	var newPC PermissionsCheck
	if err := connectRead().WithContext(ctx).First(&newPC, "permissions_check_id = ? AND active = ?", pc.PermissionsCheckID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return newPC, nil
}

// List retrieves all active permissions check records matching the non-zero fields of the receiver.
func (pc PermissionsCheck) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions_check.list")
	defer span.End()
	span.SetAttributes(attribute.String("permissions_check.id", pc.PermissionsCheckID))
	var checks []PermissionsCheck
	pc.Active = true
	q := connectRead().WithContext(ctx).Where(pc)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&checks).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(checks))
	for i, c := range checks {
		result[i] = c
	}
	return result, nil
}

// Get retrieves the active invite by InviteID.
func (invite Invite) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.invite.get")
	defer span.End()
	span.SetAttributes(attribute.String("invite.id", invite.InviteID))
	var newInvite Invite
	if err := connectRead().WithContext(ctx).First(&newInvite, "invite_id = ? AND active = ?", invite.InviteID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return newInvite, nil
}

// ── Lookup helpers ────────────────────────────────────────────────────────────

// getUserByEmail returns the active user with the given email address.
func getUserByEmail(ctx context.Context, email string) (User, error) {
	var user User
	if err := connectRead().WithContext(ctx).Where("email = ? AND active = ?", email, true).First(&user).Error; err != nil {
		return User{}, err
	}
	return user, nil
}

// ── OAuthClient ───────────────────────────────────────────────────────────────

func (c OAuthClient) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.oauth_client.add")
	defer span.End()
	span.SetAttributes(attribute.String("client.id", c.ClientID))
	if err := connect().WithContext(ctx).Create(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c OAuthClient) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.oauth_client.update")
	defer span.End()
	if err := connect().WithContext(ctx).Save(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c OAuthClient) Remove(ctx context.Context) error { return nil }

func (c OAuthClient) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.oauth_client.get")
	defer span.End()
	span.SetAttributes(attribute.String("client.id", c.ClientID))
	var result OAuthClient
	if err := connectRead().WithContext(ctx).Where("client_id = ? AND active = ?", c.ClientID, true).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (c OAuthClient) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.oauth_client.list")
	defer span.End()
	var clients []OAuthClient
	q := connectRead().WithContext(ctx).Where("active = ?", true)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&clients).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(clients))
	for i, cl := range clients {
		result[i] = cl
	}
	return result, nil
}

// getOAuthClientByClientID returns the active OAuth client with the given client ID.
func getOAuthClientByClientID(ctx context.Context, clientID string) (OAuthClient, error) {
	row, err := (OAuthClient{ClientID: clientID}).Get(ctx)
	if err != nil {
		return OAuthClient{}, err
	}
	return row.(OAuthClient), nil
}

// listOAuthClients returns all active OAuth clients.
func listOAuthClients(ctx context.Context) ([]OAuthClient, error) {
	rows, err := (OAuthClient{}).List(ctx, 0, 0)
	if err != nil {
		return nil, err
	}
	clients := make([]OAuthClient, len(rows))
	for i, r := range rows {
		clients[i] = r.(OAuthClient)
	}
	return clients, nil
}

// deactivateOAuthClient soft-deletes an OAuth client by client ID.
// Returns the number of rows affected.
func deactivateOAuthClient(ctx context.Context, id string) (int64, error) {
	result := connect().WithContext(ctx).Model(&OAuthClient{}).Where("client_id = ?", id).Update("active", false)
	return result.RowsAffected, result.Error
}

// ── OAuthCode ─────────────────────────────────────────────────────────────────

func (c OAuthCode) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.oauth_code.add")
	defer span.End()
	if err := connect().WithContext(ctx).Create(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c OAuthCode) Update(ctx context.Context) error  { return nil }
func (c OAuthCode) Remove(ctx context.Context) error  { return nil }
func (c OAuthCode) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.oauth_code.get")
	defer span.End()
	var result OAuthCode
	if err := connectRead().WithContext(ctx).Where("code = ?", c.Code).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}
func (c OAuthCode) List(ctx context.Context, limit, offset int) ([]db, error) { return nil, nil }

// getOAuthCode returns an unused authorization code for the given client.
func getOAuthCode(ctx context.Context, code, clientID string) (OAuthCode, error) {
	var result OAuthCode
	if err := connectRead().WithContext(ctx).
		Where("code = ? AND client_id = ? AND used = ?", code, clientID, false).
		First(&result).Error; err != nil {
		return OAuthCode{}, err
	}
	return result, nil
}

// redeemOAuthCode atomically marks an unused authorization code as used.
// Returns the number of rows affected (0 if already redeemed by a peer request).
func redeemOAuthCode(ctx context.Context, code string) (int64, error) {
	result := connect().WithContext(ctx).
		Model(&OAuthCode{}).
		Where("code = ? AND used = ?", code, false).
		Update("used", true)
	return result.RowsAffected, result.Error
}

// ── MFAPending ────────────────────────────────────────────────────────────────

func (m MFAPending) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.mfa_pending.add")
	defer span.End()
	if err := connect().WithContext(ctx).Create(&m).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (m MFAPending) Update(ctx context.Context) error  { return nil }
func (m MFAPending) Remove(ctx context.Context) error  { return nil }
func (m MFAPending) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.mfa_pending.get")
	defer span.End()
	var result MFAPending
	if err := connectRead().WithContext(ctx).Where("token = ?", m.Token).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}
func (m MFAPending) List(ctx context.Context, limit, offset int) ([]db, error) { return nil, nil }

// redeemMFAPending atomically marks an unused MFA pending token as used.
// Returns the number of rows affected.
func redeemMFAPending(ctx context.Context, token string) (int64, error) {
	result := connect().WithContext(ctx).
		Model(&MFAPending{}).
		Where("token = ? AND used = ?", token, false).
		Update("used", true)
	return result.RowsAffected, result.Error
}

// ── Secret ────────────────────────────────────────────────────────────────────

func (s Secret) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.secret.add")
	defer span.End()
	span.SetAttributes(attribute.String("secret.id", s.SecretID))
	if err := connect().WithContext(ctx).Create(&s).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s Secret) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.secret.update")
	defer span.End()
	span.SetAttributes(attribute.String("secret.id", s.SecretID))
	if err := connect().WithContext(ctx).Save(&s).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s Secret) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.secret.remove")
	defer span.End()
	span.SetAttributes(attribute.String("secret.id", s.SecretID))
	if err := connect().WithContext(ctx).Model(&s).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s Secret) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.secret.get")
	defer span.End()
	span.SetAttributes(attribute.String("secret.id", s.SecretID))
	var result Secret
	if err := connectRead().WithContext(ctx).Where("secret_id = ? AND active = true", s.SecretID).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (s Secret) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.secret.list")
	defer span.End()
	var secrets []Secret
	q := connectRead().WithContext(ctx).Where(s)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&secrets).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(secrets))
	for i, sec := range secrets {
		result[i] = sec
	}
	return result, nil
}

// getSecretByID returns an active secret by ID.
func getSecretByID(ctx context.Context, id string) (Secret, error) {
	row, err := (Secret{SecretID: id}).Get(ctx)
	if err != nil {
		return Secret{}, err
	}
	return row.(Secret), nil
}

// ── OrgSecretProvider ─────────────────────────────────────────────────────────

func (p OrgSecretProvider) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.org_secret_provider.add")
	defer span.End()
	if err := connect().WithContext(ctx).Create(&p).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p OrgSecretProvider) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.org_secret_provider.update")
	defer span.End()
	if err := connect().WithContext(ctx).Save(&p).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p OrgSecretProvider) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.org_secret_provider.remove")
	defer span.End()
	if err := connect().WithContext(ctx).Delete(&OrgSecretProvider{}, "org_id = ?", p.OrgID).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p OrgSecretProvider) Get(ctx context.Context) (db, error)               { return nil, nil }
func (p OrgSecretProvider) List(ctx context.Context, limit, offset int) ([]db, error) {
	return nil, nil
}

// ── Misc write helpers ────────────────────────────────────────────────────────

// syncServiceAccountBootstrapKey updates the hashed_key column to match the
// bootstrap hash after the bootstrap key fallback path succeeds in requireServiceAuth.
func syncServiceAccountBootstrapKey(ctx context.Context, name, hash string) error {
	return connect().WithContext(ctx).Model(&ServiceAccount{}).
		Where("service_name = ?", name).
		Update("hashed_key", hash).Error
}

// deleteUnconfirmedTOTP removes any pending (unconfirmed) TOTP enrollment for a user.
func deleteUnconfirmedTOTP(ctx context.Context, userID string) error {
	return connect().WithContext(ctx).
		Where("user_id = ? AND confirmed = ?", userID, false).
		Delete(&TOTPCredential{}).Error
}

// deactivateTOTP deactivates all active TOTP credentials for a user.
// Returns the number of rows affected.
func deactivateTOTP(ctx context.Context, userID string) (int64, error) {
	result := connect().WithContext(ctx).
		Model(&TOTPCredential{}).
		Where("user_id = ? AND active = ?", userID, true).
		Update("active", false)
	return result.RowsAffected, result.Error
}

// clearOrgMembership clears the org_id field on all users that belong to orgID.
func clearOrgMembership(ctx context.Context, orgID string) error {
	return connect().WithContext(ctx).Model(&User{}).Where("org_id = ?", orgID).Update("org_id", nil).Error
}

// getUserIDsByTeam returns the user_id of all users in a team.
func getUserIDsByTeam(ctx context.Context, teamID string) ([]string, error) {
	var ids []string
	if err := connect().WithContext(ctx).Model(&User{}).Where("team_id = ?", teamID).Pluck("user_id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// clearTeamMembership clears the team_id field on all users that belong to teamID.
func clearTeamMembership(ctx context.Context, teamID string) error {
	return connect().WithContext(ctx).Model(&User{}).Where("team_id = ?", teamID).Update("team_id", nil).Error
}

// countUsersByRole returns the number of active users assigned to roleID.
func countUsersByRole(ctx context.Context, roleID string) (int64, error) {
	var count int64
	if err := connect().WithContext(ctx).Model(&User{}).Where("role_id = ? AND active = ?", roleID, true).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// countTeamsByRole returns the number of active teams assigned to roleID.
func countTeamsByRole(ctx context.Context, roleID string) (int64, error) {
	var count int64
	if err := connect().WithContext(ctx).Model(&Team{}).Where("role_id = ? AND active = ?", roleID, true).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// deactivateUserSessions sets active=false on all sessions for a user.
func deactivateUserSessions(ctx context.Context, userID string) error {
	return connect().WithContext(ctx).Model(&Session{}).Where("user_id = ?", userID).Update("active", false).Error
}

// applyGrantsForResource grants service permissions to userID using the write connection.
// This is a non-transactional convenience wrapper; use grantServicePermissions directly
// when inside a *gorm.DB transaction.
func applyGrantsForResource(ctx context.Context, service, userID, name string, actions []string, resource string) error {
	return grantServicePermissions(ctx, connect().WithContext(ctx), service, userID, name, actions, resource)
}

// upsertServiceAccountDB upserts a gatekeeper service account by name.
// If the account already exists only HashedBootstrapKey is refreshed;
// HashedKey is preserved so keys rotated at runtime survive restarts.
func upsertServiceAccountDB(ctx context.Context, name, hash string) {
	var existing ServiceAccount
	err := connect().WithContext(ctx).Where("service_name = ?", name).First(&existing).Error
	if err == nil {
		if err2 := connect().WithContext(ctx).Model(&ServiceAccount{}).Where("service_name = ?", name).
			Update("hashed_bootstrap_key", hash).Error; err2 != nil {
			slog.Error("seedServiceAccounts: update bootstrap key failed", "name", name, "error", err2)
		} else {
			slog.Debug("seedServiceAccounts: account exists, bootstrap key refreshed", "name", name)
		}
		return
	}
	svc := ServiceAccount{
		ServiceAccountID:   uuid.New().String(),
		ServiceName:        name,
		HashedKey:          hash,
		HashedBootstrapKey: hash,
		Active:             true,
	}
	if err := connect().WithContext(ctx).Create(&svc).Error; err != nil {
		slog.Error("seedServiceAccounts: create failed", "name", name, "error", err)
	} else {
		slog.Info("seedServiceAccounts: created", "name", name)
	}
}

// ── Complex transactions ──────────────────────────────────────────────────────

// approveServicePermissionRequestAtomic approves a pending service permission
// request inside a serialised FOR UPDATE transaction.
// Returns the approved request (for audit logging) or an error.
// A "request is not pending" error means a concurrent approver won the race.
func approveServicePermissionRequestAtomic(ctx context.Context, requestID, callerID string) (ServicePermissionRequest, error) {
	var spr ServicePermissionRequest
	now := time.Now()
	err := connect().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("request_id = ? AND active = ?", requestID, true).
			First(&spr).Error; err != nil {
			return err
		}
		if spr.Status != "pending" {
			return fmt.Errorf("request is not pending")
		}

		var svcAcct ServiceAccount
		if err := tx.Where("service_name = ? AND active = ?", spr.ServiceName, true).First(&svcAcct).Error; err != nil {
			return err
		}

		var role Role
		if svcAcct.RoleID == nil {
			role = Role{
				RoleID:         uuid.New().String(),
				OwnerID:        callerID,
				Active:         true,
				PermissionsIDs: []string{},
			}
			if err := tx.Create(&role).Error; err != nil {
				return err
			}
			svcAcct.RoleID = &role.RoleID
			svcAcct.UpdatedAt = now
			if err := tx.Save(&svcAcct).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Where("role_id = ? AND active = ?", *svcAcct.RoleID, true).First(&role).Error; err != nil {
				return err
			}
		}

		perm := Permissions{
			PermissionsID: uuid.New().String(),
			Name:          spr.Name,
			Service:       spr.Service,
			Actions:       spr.Actions,
			Resources:     spr.Resources,
			OwnerID:       callerID,
			Active:        true,
		}
		if err := tx.Create(&perm).Error; err != nil {
			return err
		}

		if role.PermissionsIDs == nil {
			role.PermissionsIDs = []string{}
		}
		role.PermissionsIDs = append(role.PermissionsIDs, perm.PermissionsID)
		role.UpdatedAt = now
		if err := tx.Save(&role).Error; err != nil {
			return err
		}
		cacheDel(ctx, "gk:role:"+role.RoleID)

		spr.Status = "approved"
		spr.ResolvedBy = &callerID
		spr.ResolvedAt = &now
		spr.UpdatedAt = now
		return tx.Save(&spr).Error
	})
	return spr, err
}

// acceptInviteAtomic accepts an invite atomically: locks the invite and user
// rows, updates membership, marks the invite accepted, and grants the member
// permission — all in a single transaction.
// Returns sentinel errors "invite is not pending", "already in org", "already in team"
// for concurrency conflicts that map to HTTP 409.
func acceptInviteAtomic(ctx context.Context, inviteID, callerID string, invite Invite, permName, memberAction, permResource string) error {
	return connect().Transaction(func(tx *gorm.DB) error {
		var fresh Invite
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("invite_id = ? AND active = ?", inviteID, true).
			First(&fresh).Error; err != nil {
			return err
		}
		if fresh.Status != "pending" {
			return errors.New("invite is not pending")
		}

		var freshCaller User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND active = ?", callerID, true).
			First(&freshCaller).Error; err != nil {
			return err
		}
		switch invite.ResourceType {
		case "org":
			if freshCaller.OrgID != nil && *freshCaller.OrgID != invite.ResourceID {
				return errors.New("already in org")
			}
			freshCaller.OrgID = &invite.ResourceID
		case "team":
			if freshCaller.TeamID != nil && *freshCaller.TeamID != invite.ResourceID {
				return errors.New("already in team")
			}
			freshCaller.TeamID = &invite.ResourceID
		}
		freshCaller.UpdatedAt = time.Now()

		fresh.Status = "accepted"
		if err := tx.Save(&freshCaller).Error; err != nil {
			return err
		}
		if err := tx.Save(&fresh).Error; err != nil {
			return err
		}
		return grantPermissions(ctx, tx, callerID, permName, []string{memberAction}, permResource)
	})
}
