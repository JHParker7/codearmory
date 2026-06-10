package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

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
