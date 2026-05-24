package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
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

// connect returns the shared write database connection (DATABASE_URL), opening
// it on first call.
func connect() *gorm.DB {
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
	cacheTrackUserSession(ctx, session.UserID, session.SessionID)
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
