package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// normalizeSignupEmail lower-cases and trims an address for consistent matching.
func normalizeSignupEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// normalizeAllowlistValue validates and normalises an allowlist entry. It accepts
// either a full email address ("Alice@Example.com" → "alice@example.com") or a
// domain rule that begins with "@" ("@Example.com" → "@example.com", matching any
// address at that domain). It returns an error describing why an input is invalid.
func normalizeAllowlistValue(raw string) (string, error) {
	v := normalizeSignupEmail(raw)
	if v == "" {
		return "", errors.New("email is required")
	}
	if strings.HasPrefix(v, "@") {
		domain := v[1:]
		if domain == "" || !strings.Contains(domain, ".") || strings.ContainsAny(domain, " @") {
			return "", errors.New("invalid domain rule; expected @example.com")
		}
		return "@" + domain, nil
	}
	addr, err := mail.ParseAddress(v)
	if err != nil {
		return "", errors.New("invalid email address")
	}
	// ParseAddress may accept "Name <a@b>" forms; keep only the bare address.
	return strings.ToLower(addr.Address), nil
}

// signupEmailDomainKey returns the "@domain" portion of a normalised email, or ""
// when the address has no domain part.
func signupEmailDomainKey(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return ""
	}
	return email[at:]
}

// isSignupEmailAllowed reports whether email matches any active allowlist entry,
// either as an exact address or via a domain rule. Matching is case-insensitive.
func isSignupEmailAllowed(ctx context.Context, email string) (bool, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.signup_allowlist.match")
	defer span.End()

	e := normalizeSignupEmail(email)
	domainKey := signupEmailDomainKey(e)

	var count int64
	q := connectRead().WithContext(ctx).Model(&SignupAllowlistEntry{}).
		Where("active = ? AND (lower(email) = ? OR lower(email) = ?)", true, e, domainKey)
	if err := q.Count(&count).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return false, err
	}
	span.SetStatus(codes.Ok, "")
	return count > 0, nil
}

// Add inserts the allowlist entry.
func (e SignupAllowlistEntry) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.signup_allowlist.add")
	defer span.End()
	span.SetAttributes(attribute.String("entry.id", e.EntryID))
	e.Active = true
	if err := connect().WithContext(ctx).Create(&e).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all fields of the allowlist entry.
func (e SignupAllowlistEntry) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.signup_allowlist.update")
	defer span.End()
	span.SetAttributes(attribute.String("entry.id", e.EntryID))
	e.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&e).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the allowlist entry by setting active = false.
func (e SignupAllowlistEntry) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.signup_allowlist.remove")
	defer span.End()
	span.SetAttributes(attribute.String("entry.id", e.EntryID))
	if err := connect().WithContext(ctx).Model(&SignupAllowlistEntry{}).
		Where("entry_id = ?", e.EntryID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active allowlist entry by EntryID.
func (e SignupAllowlistEntry) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.signup_allowlist.get")
	defer span.End()
	span.SetAttributes(attribute.String("entry.id", e.EntryID))
	var row SignupAllowlistEntry
	if err := connectRead().WithContext(ctx).First(&row, "entry_id = ? AND active = ?", e.EntryID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return row, nil
}

// List retrieves active allowlist entries, most recent first.
func (e SignupAllowlistEntry) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.signup_allowlist.list")
	defer span.End()
	var rows []SignupAllowlistEntry
	q := connectRead().WithContext(ctx).Where("active = ?", true).Order("created_at DESC")
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

// getSignupPolicy returns the singleton signup policy. When no row exists yet the
// zero-value policy (InviteOnly = false, i.e. open registration) is returned so a
// never-configured instance defaults to open signup.
func getSignupPolicy(ctx context.Context) (SignupPolicy, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.signup_policy.get")
	defer span.End()
	var p SignupPolicy
	err := connectRead().WithContext(ctx).First(&p, "id = ?", signupPolicySingletonID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		span.SetStatus(codes.Ok, "")
		return SignupPolicy{ID: signupPolicySingletonID, InviteOnly: false}, nil
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return SignupPolicy{}, err
	}
	span.SetStatus(codes.Ok, "")
	return p, nil
}

// setSignupPolicy upserts the singleton signup policy. updatedBy records the actor
// that changed it (empty for startup seeding).
func setSignupPolicy(ctx context.Context, inviteOnly bool, updatedBy string) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.signup_policy.set")
	defer span.End()
	p := SignupPolicy{
		ID:         signupPolicySingletonID,
		InviteOnly: inviteOnly,
		UpdatedAt:  time.Now(),
		UpdatedBy:  updatedBy,
	}
	// Save upserts on the fixed primary key. InviteOnly carries no default tag, so
	// storing it as false is honoured (see the SignupPolicy doc comment).
	if err := connect().WithContext(ctx).Save(&p).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// seedSignupPolicy establishes the initial policy row from GATEKEEPER_INVITE_ONLY
// the first time the instance starts. It only writes when no row exists yet, so an
// admin's later runtime change is never overwritten on restart.
func seedSignupPolicy(ctx context.Context) {
	var count int64
	if err := connect().WithContext(ctx).Model(&SignupPolicy{}).
		Where("id = ?", signupPolicySingletonID).Count(&count).Error; err != nil {
		slog.WarnContext(ctx, "seedSignupPolicy: count failed", "error", err)
		return
	}
	if count > 0 {
		return
	}
	inviteOnly := envBool("GATEKEEPER_INVITE_ONLY")
	if err := setSignupPolicy(ctx, inviteOnly, ""); err != nil {
		slog.WarnContext(ctx, "seedSignupPolicy: initial write failed", "error", err)
		return
	}
	slog.InfoContext(ctx, "seedSignupPolicy: initial signup policy set", "invite_only", inviteOnly)
}

// seedSignupAllowlist seeds allowlist entries from GATEKEEPER_SIGNUP_ALLOWLIST (a
// comma-separated list of addresses and/or @domain rules). Each value is inserted
// at most once: if any row (active or soft-deleted) already carries the normalised
// value it is skipped, so an admin who deletes a seeded entry does not see it
// resurrected on the next restart.
func seedSignupAllowlist(ctx context.Context) {
	raw := secret("GATEKEEPER_SIGNUP_ALLOWLIST")
	if strings.TrimSpace(raw) == "" {
		return
	}
	for _, part := range strings.Split(raw, ",") {
		value, err := normalizeAllowlistValue(part)
		if err != nil {
			slog.WarnContext(ctx, "seedSignupAllowlist: skipping invalid entry", "value", part, "error", err)
			continue
		}
		var count int64
		if err := connect().WithContext(ctx).Model(&SignupAllowlistEntry{}).
			Where("lower(email) = ?", value).Count(&count).Error; err != nil {
			slog.WarnContext(ctx, "seedSignupAllowlist: count failed", "value", value, "error", err)
			continue
		}
		if count > 0 {
			continue
		}
		entry := SignupAllowlistEntry{
			EntryID:   uuid.New().String(),
			Email:     value,
			Note:      "seeded from GATEKEEPER_SIGNUP_ALLOWLIST",
			CreatedBy: "system",
		}
		if err := entry.Add(ctx); err != nil {
			slog.WarnContext(ctx, "seedSignupAllowlist: insert failed", "value", value, "error", err)
			continue
		}
		slog.InfoContext(ctx, "seedSignupAllowlist: seeded allowlist entry", "value", value)
	}
}

// duplicateAllowlistErr formats the error returned when an entry already exists.
func duplicateAllowlistErr(value string) error {
	return fmt.Errorf("%q is already on the signup allowlist", value)
}

// signupBootstrapExempt reports whether the current signup is the genuine
// first-user bootstrap that must bypass the invite-only gate so an instance can
// always be initialised. It applies only when no dedicated admin is provisioned
// via env (that path already creates the first account at startup) and the
// instance still has zero users. A count error fails closed (no exemption).
func signupBootstrapExempt(ctx context.Context) bool {
	if adminSeedConfigured() {
		return false
	}
	count, err := instanceUserCount(connect().WithContext(ctx))
	if err != nil {
		slog.WarnContext(ctx, "signupBootstrapExempt: user count failed; denying exemption", "error", err)
		return false
	}
	return count == 0
}
