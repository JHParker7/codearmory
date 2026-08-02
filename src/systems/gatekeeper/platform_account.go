package main

import (
	"context"
	"errors"
	"log/slog"

	"gorm.io/gorm"
)

// The platform account: the principal that OWNS instance-level configuration.
//
// Platform-owned resources are declared "codearmory/<service>/…" (runner classes,
// allowed images, OIDC settings, the audit log, the default-org baseline). Gatekeeper
// leaves those untouched instead of prefixing the caller, because the first segment
// already names an owner — see scopeResource. That works whether or not a matching user
// row exists: the namespace is just a string.
//
// This row exists so the owner is a real PRINCIPAL rather than a bare literal. With it,
// a change to platform configuration attributes to a subject that can be resolved,
// listed and reported on, instead of an id that resolves to nothing.
//
// It is seeded at migration time rather than through User.Add on purpose. Add refuses
// reserved usernames — "codearmory" among them — and that guard is what stops anyone
// claiming the namespace. Punching a bypass through it for one caller would be the
// weakest point in the whole scheme; seeding beneath it keeps the API-facing rule
// absolute, with exactly one writer of this row and that writer in the startup path.
const (
	// platformUserID is FIXED, never generated. Audit rows reference their actor by id,
	// so a regenerated id on a rebuild would orphan every historical attribution and
	// leave old entries pointing at a user that no longer exists.
	platformUserID = "00000000-0000-0000-0000-000000000001"

	platformUsername = "codearmory"

	// A .invalid address (RFC 2606) can never be routed or registered, so it cannot
	// collide with a real signup and no mail can ever reach it.
	platformEmail = "codearmory@platform.invalid"

	// lockedPasswordHash is deliberately NOT a valid bcrypt hash. bcrypt returns an
	// error rather than a match for malformed input, so no password can ever verify
	// against it — the same trick as "!" in /etc/shadow. isPlatformAccount is the
	// explicit guard; this is the belt to its braces, so the account stays unusable
	// even if a future auth path forgets to call it.
	lockedPasswordHash = "!"
)

// isPlatformAccount reports whether a user row is the platform account, which may never
// authenticate. Checked by id rather than by username or email so that renaming or
// re-addressing the row — however that came about — cannot turn it back into a
// loginable account.
func isPlatformAccount(u User) bool {
	return u.UserID == platformUserID
}

// seedPlatformAccount inserts the platform account if it is absent. Idempotent: it is
// called on every startup and does nothing when the row already exists.
//
// Non-fatal by design, matching the other seeders. A gatekeeper that cannot write this
// row should still start — platform RESOURCES keep working regardless, since their
// namespace is a string and does not depend on this row existing. Only the attribution
// half degrades, and a warning is the proportionate response to that.
func seedPlatformAccount(ctx context.Context) {
	var existing User
	err := connect().WithContext(ctx).Where("user_id = ?", platformUserID).First(&existing).Error
	if err == nil {
		return
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		slog.WarnContext(ctx, "seedPlatformAccount: could not check for the platform account; audit attribution for platform config may be unresolved", "error", err)
		return
	}

	// Created directly rather than via User.Add — see the note above on why the
	// reserved-username guard is not bypassed for this.
	acct := User{
		UserID:         platformUserID,
		Username:       platformUsername,
		Email:          platformEmail,
		HashedPassword: lockedPasswordHash,
		Firstname:      "CodeArmory",
		Lastname:       "Platform",
		Active:         true,
	}
	if err := connect().WithContext(ctx).Create(&acct).Error; err != nil {
		slog.WarnContext(ctx, "seedPlatformAccount: could not create the platform account; audit attribution for platform config may be unresolved", "error", err)
		return
	}
	slog.InfoContext(ctx, "seedPlatformAccount: platform account created", "user_id", platformUserID, "username", platformUsername)
}
