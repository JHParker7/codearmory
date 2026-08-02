package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// reservedUsernames are names no user may hold, because each is already a NAMESPACE
// PREFIX in the RBAC resource grammar. A resource reads "<owner>/<service>/…", and the
// owner is either a username, or one of these literals:
//
//	codearmory/…        the instance itself — platform-owned config (runner classes,
//	                    allowed images, OIDC settings, the audit log)
//	org/<name>/…        an organisation
//	project/<slug>/…    a project
//
// A user holding one of these names would own that namespace. Registering as
// "codearmory" would make every platform resource evaluate inside that user's space,
// which is a privilege-escalation route, not a cosmetic clash.
//
// Enforced HERE rather than in the signup handler on purpose: users are created by
// signup, by invite acceptance, by the OIDC/OAuth identity path, and by the env-seeded
// bootstrap admin. A check per handler is a check someone adds a fourth path without —
// which is the same "one missed filter" shape as the cross-tenant leaks this codebase
// has already had. Add and Update are the only ways a username reaches a row, so this
// is the narrowest point that covers all of them, rename included.
//
// Matched case-insensitively. "CodeArmory" would not collide in the resource string
// (matching is exact), but it is an impersonation of the platform owner and there is no
// legitimate reason to allow it.
var reservedUsernames = map[string]bool{
	"codearmory": true,
	"org":        true,
	"project":    true,
}

// checkReservedUsername rejects a username that would claim a reserved namespace.
func checkReservedUsername(username string) error {
	if reservedUsernames[strings.ToLower(strings.TrimSpace(username))] {
		return fmt.Errorf("username %q is reserved: it names an RBAC namespace prefix", username)
	}
	return nil
}

// Add inserts the user. Nil OrgID, TeamID, and RoleID are written as NULL.
func (user User) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.user.add")
	defer span.End()
	span.SetAttributes(attribute.String("user.id", user.UserID))
	if err := checkReservedUsername(user.Username); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	user.Active = true
	if err := connect().WithContext(ctx).Create(&user).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves the user's mutable fields. Nil OrgID, TeamID, and RoleID are written
// as NULL, so a caller can genuinely clear them.
//
// Two columns are deliberately NOT writable here:
//
//   - active — deactivating is Remove()'s job. This used to be a Save() of the whole
//     struct, so a caller who built a User literal rather than Get()ing one wrote
//     active=false (Go's zero value) and silently deleted the account from every
//     lookup: the row still existed, but Get filters on active, so the user simply
//     ceased to exist. Nothing in the call failed.
//   - created_at — likewise zeroed by a literal, and never a thing an update means.
//
// The identity guard below turns the other half of that failure — blanking username
// and email — into a loud error instead of silent corruption. It is cheap insurance:
// no legitimate update sets either to empty, and both carry unique indexes, so the
// blanking only surfaces later as a confusing collision on the NEXT user.
func (user User) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.user.update")
	defer span.End()
	span.SetAttributes(attribute.String("user.id", user.UserID))
	if user.UserID == "" {
		return errors.New("user update: UserID is required")
	}
	if user.Username == "" || user.Email == "" {
		err := fmt.Errorf("user update: refusing to blank username/email for %s — Get() the user, mutate it, then Update()", user.UserID)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	// Rename is the other way into a reserved namespace, and the easier one to forget:
	// signup is the path everyone thinks to guard.
	if err := checkReservedUsername(user.Username); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	user.UpdatedAt = time.Now()
	// Named columns, so a zero value in the struct writes that column (clearing a
	// pointer works) while anything unnamed — active, created_at — is left untouched.
	if err := connect().WithContext(ctx).Model(&User{}).
		Where("user_id = ?", user.UserID).
		Select("hashed_password", "updated_at", "firstname", "lastname", "email",
			"org_id", "role_id", "team_id", "default_role_id", "default_grants_version", "username").
		Updates(user).Error; err != nil {
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

// Remove soft-deletes the org by setting active = false. The org_name is also
// mangled to "__deleted__<id>" so the original name is freed for reuse — without
// this, the unique index on org_name would block recreating the same org after
// deletion (name is immutable from the org's perspective once freed).
func (org Org) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.org.remove")
	defer span.End()
	span.SetAttributes(attribute.String("org.id", org.OrgID))
	if err := connect().WithContext(ctx).Model(&Org{}).Where("org_id = ?", org.OrgID).
		Updates(map[string]any{"active": false, "org_name": "__deleted__" + org.OrgID}).Error; err != nil {
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
