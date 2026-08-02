package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/crypto/bcrypt"
)

// setSessionCookie writes an HttpOnly session cookie carrying the JWT. Defaults
// to Secure=true; set COOKIE_SECURE=false to disable (local HTTP dev only).
func setSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge <= 0 {
		return
	}
	secure := os.Getenv("COOKIE_SECURE") != "false"
	http.SetCookie(w, &http.Cookie{
		Name:     "armory_session",
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})
}

// handleLogout clears the session cookie. Callers that used JWT-only auth can
// delete their own session via DELETE /sessions/{id}.
func handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "armory_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   os.Getenv("COOKIE_SECURE") != "false",
	})
	w.WriteHeader(http.StatusNoContent)
}

// userResponse is the safe public shape of a User — HashedPassword is deliberately omitted.
type userResponse struct {
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	OrgID     *string   `json:"org_id"`
	TeamID    *string   `json:"team_id"`
	RoleID    *string   `json:"role_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Active    bool      `json:"active"`
}

func toUserResponse(u User) userResponse {
	return userResponse{
		UserID: u.UserID, Username: u.Username,
		OrgID: u.OrgID, TeamID: u.TeamID, RoleID: u.RoleID,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
		Active: u.Active,
	}
}

type updateUserRequest struct {
	Email           string `json:"email"`
	Username        string `json:"username"`
	Password        string `json:"password"`         // optional; kept unchanged when empty
	CurrentPassword string `json:"current_password"` // required to authorize a password change
	Firstname       string `json:"firstname"`
	Lastname        string `json:"lastname"`
}

type signupRequest struct {
	Email     string `json:"email"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Firstname string `json:"firstname"`
	Lastname  string `json:"lastname"`
}

type signupResponse struct {
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	Username  string `json:"username"`
	Firstname string `json:"firstname"`
	Lastname  string `json:"lastname"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func handleGetUser(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleGetUser")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("user.id", callerID),
		attribute.String("target.user_id", id),
	)
	slog.InfoContext(ctx, "get user request", "caller_id", callerID, "target_user_id", id)

	if !requirePermission(w, r, "getUser", "gatekeeper/users/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (User{UserID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "user not found")
		slog.WarnContext(ctx, "get user: not found", "caller_id", callerID, "target_user_id", id)
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("user.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "get user: success", "caller_id", callerID, "target_user_id", id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toUserResponse(row.(User)))
}

func handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleUpdateUser")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("user.id", callerID),
		attribute.String("target.user_id", id),
	)
	slog.InfoContext(ctx, "update user request", "caller_id", callerID, "target_user_id", id)

	if !requirePermission(w, r, "updateUser", "gatekeeper/users/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	var req updateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.WarnContext(ctx, "update user: invalid request body", "caller_id", callerID, "target_user_id", id, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Email == "" || req.Username == "" {
		span.SetStatus(codes.Error, "missing required fields")
		slog.WarnContext(ctx, "update user: missing required fields", "caller_id", callerID, "target_user_id", id, "email_provided", req.Email != "", "username_provided", req.Username != "")
		http.Error(w, "email and username are required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(
		attribute.String("new.username", req.Username),
		attribute.Bool("password.change_requested", req.Password != ""),
	)

	row, err := (User{UserID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "user not found")
		slog.WarnContext(ctx, "update user: not found", "caller_id", callerID, "target_user_id", id)
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("user.id", id)))

	u := row.(User)
	u.Email = req.Email
	u.Username = req.Username
	u.Firstname = req.Firstname
	u.Lastname = req.Lastname
	if req.Password != "" {
		// Re-authenticate with the current password before allowing a password
		// change — RBAC alone is not sufficient (mirrors handleTOTPDisable, which
		// also requires the password). Without this, a stolen session cookie or
		// still-valid bearer token is enough to reset the password and take over
		// the account. An empty current_password fails the compare below.
		if err := bcrypt.CompareHashAndPassword([]byte(u.HashedPassword), []byte(req.CurrentPassword)); err != nil {
			span.SetStatus(codes.Error, "current password mismatch")
			slog.WarnContext(ctx, "update user: password change rejected — current_password missing or incorrect", "caller_id", callerID, "target_user_id", id)
			http.Error(w, "current_password is incorrect", http.StatusUnauthorized)
			return
		}
		if len(req.Password) < 8 {
			http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
			return
		}
		if len(req.Password) > 128 {
			http.Error(w, "password must not exceed 128 characters", http.StatusBadRequest)
			return
		}
		slog.InfoContext(ctx, "update user: changing password", "caller_id", callerID, "target_user_id", id)
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), 12)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "bcrypt failure")
			slog.ErrorContext(ctx, "update user: bcrypt error", "caller_id", callerID, "target_user_id", id, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		u.HashedPassword = string(hash)
		span.AddEvent("password.rehashed")
	}
	if err := u.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "update user: db error", "caller_id", callerID, "target_user_id", id, "error", err)
		http.Error(w, "failed to update user", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.write", trace.WithAttributes(attribute.String("user.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "update user: success", "caller_id", callerID, "target_user_id", id, "new_username", req.Username)
	writeAudit(ctx, callerID, "user", "user.update", id, req.Username)
	row, _ = u.Get(ctx)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toUserResponse(row.(User)))
}

func handleListUsers(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleListUsers")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))
	slog.InfoContext(ctx, "list users request", "caller_id", callerID)

	if !requirePermission(w, r, "listUser", "gatekeeper/users") {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	limit, offset, ok := parsePagination(w, r)
	if !ok {
		span.SetStatus(codes.Error, "invalid pagination")
		return
	}

	// Always scope results to the caller's own org regardless of any ?org_id= param.
	var callerOrgID *string
	if callerRow, err := (User{UserID: callerID}).Get(ctx); err == nil {
		callerOrgID = callerRow.(User).OrgID
	}

	q := r.URL.Query()
	var filter User
	filter.OrgID = callerOrgID
	if v := q.Get("user_id"); v != "" {
		filter.UserID = v
	}
	if v := q.Get("email"); v != "" {
		filter.Email = v
	}
	if v := q.Get("username"); v != "" {
		filter.Username = v
	}
	if v := q.Get("team_id"); v != "" {
		s := v
		filter.TeamID = &s
	}
	if v := q.Get("role_id"); v != "" {
		s := v
		filter.RoleID = &s
	}

	rows, err := filter.List(ctx, limit, offset)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list users failed")
		slog.WarnContext(ctx, "list users: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list users", http.StatusInternalServerError)
		return
	}
	responses := make([]userResponse, len(rows))
	for i, row := range rows {
		responses[i] = toUserResponse(row.(User))
	}
	span.AddEvent("db.read")
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "list users: success", "caller_id", callerID, "count", len(responses))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responses)
}

// handleDeleteUser soft-deletes the user and invalidates all their active sessions.
func handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleDeleteUser")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("user.id", callerID),
		attribute.String("target.user_id", id),
	)
	slog.InfoContext(ctx, "delete user request", "caller_id", callerID, "target_user_id", id)

	if !requirePermission(w, r, "deleteUser", "gatekeeper/users/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (User{UserID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "user not found")
		slog.WarnContext(ctx, "delete user: not found", "caller_id", callerID, "target_user_id", id)
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("user.id", id)))

	if err := row.(User).Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.ErrorContext(ctx, "delete user: db error", "caller_id", callerID, "target_user_id", id, "error", err)
		http.Error(w, "failed to delete user", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.soft_delete", trace.WithAttributes(attribute.String("user.id", id)))

	if err := deactivateUserSessions(ctx, id); err != nil {
		slog.ErrorContext(ctx, "delete user: failed to invalidate sessions", "caller_id", callerID, "target_user_id", id, "error", err)
	} else {
		cacheDelUserSessions(ctx, id)
		span.AddEvent("sessions.invalidated", trace.WithAttributes(attribute.String("user.id", id)))
		slog.InfoContext(ctx, "delete user: sessions invalidated", "caller_id", callerID, "target_user_id", id)
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "delete user: success", "caller_id", callerID, "target_user_id", id)
	writeAudit(ctx, callerID, "user", "user.delete", id, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleSignup creates a new user account. On success it also creates a default
// Permissions record granting getUser/updateUser/deleteUser on the new user's own
// resource path, and a Role referencing that record — so the user can manage their
// own account without any additional setup.
func handleSignup(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleSignup")
	defer span.End()
	r = r.WithContext(ctx)

	slog.InfoContext(ctx, "signup request received")

	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.WarnContext(ctx, "signup failed: invalid request body", "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Email == "" || req.Username == "" || req.Password == "" {
		span.SetStatus(codes.Error, "missing required fields")
		slog.WarnContext(ctx, "signup failed: missing required fields", "email_provided", req.Email != "", "username_provided", req.Username != "")
		http.Error(w, "email, username, and password are required", http.StatusBadRequest)
		return
	}
	if len(req.Password) < 8 {
		span.SetStatus(codes.Error, "password too short")
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	if len(req.Password) > 128 {
		span.SetStatus(codes.Error, "password too long")
		http.Error(w, "password must not exceed 128 characters", http.StatusBadRequest)
		return
	}

	// Invite-only registration gate. When enabled, the email must match the signup
	// allowlist (an exact address or a @domain rule). The genuine first-user
	// bootstrap admin is exempt so an invite-only instance can always be
	// initialised; once any account exists — including an env-seeded admin — the
	// gate applies to every subsequent signup.
	policy, err := getSignupPolicy(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "signup policy read failed")
		slog.ErrorContext(ctx, "signup failed: could not read signup policy", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if policy.InviteOnly {
		allowed, err := isSignupEmailAllowed(ctx, req.Email)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "allowlist check failed")
			slog.ErrorContext(ctx, "signup failed: allowlist check error", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if !allowed && !signupBootstrapExempt(ctx) {
			span.SetStatus(codes.Error, "invite-only: email not allowlisted")
			slog.WarnContext(ctx, "signup rejected: invite-only mode and email not on allowlist", "username", req.Username)
			meterSignups.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "invite_denied")))
			http.Error(w, "sign-ups are invite-only; this email is not on the allowlist", http.StatusForbidden)
			return
		}
	}

	span.SetAttributes(attribute.String("user.username", req.Username))
	slog.InfoContext(ctx, "creating new user", "username", req.Username)

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), 12)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "bcrypt failure")
		slog.ErrorContext(ctx, "signup failed: bcrypt error", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("password.hashed")

	userID := uuid.New().String()

	// Whether this signup is eligible to become the bootstrap admin. When a
	// dedicated admin is provisioned via GATEKEEPER_ADMIN_EMAIL/PASSWORD
	// (seedAdminUser at startup), that env-var admin takes precedence and the
	// first-user fallback is disabled — the operator has named the admin explicitly.
	adminCandidate := !adminSeedConfigured()
	// A non-admin account needs default grants or it would be permission-less; the
	// bootstrap admin is exempt (its wildcard grant does not depend on the registry).
	// createUserWithBootstrapAdmin enforces this transactionally (errNoDefaultGrants),
	// which is the single source of truth — handled in the error switch below.
	grantsAvailable := len(defaultGrantsFor("user")) > 0

	// The personal role holds org/team-ownership and custom grants. It starts empty;
	// the user-scoped default grants live in a separately-managed default role (see
	// rebuildDefaultRole). The bootstrap admin is the exception — its personal role
	// is named "admin" and carries the wildcard permission, set by the atomic create.
	personalRole := Role{RoleID: uuid.New().String(), OwnerID: userID, PermissionsIDs: []string{}}
	user := User{
		UserID:         userID,
		HashedPassword: string(hash),
		Email:          req.Email,
		Username:       req.Username,
		Firstname:      req.Firstname,
		Lastname:       req.Lastname,
		RoleID:         &personalRole.RoleID,
	}

	// Insert the personal role and user atomically, deciding first-user-admin inside
	// the transaction so two concurrent signups on an empty DB cannot both win admin.
	isFirstUser, err := createUserWithBootstrapAdmin(ctx, &user, &personalRole, adminCandidate, grantsAvailable)
	if err != nil {
		if errors.Is(err, errNoDefaultGrants) {
			slog.ErrorContext(ctx, "signup: no default grants for 'user' — refusing to create a permission-less account; check that the registry is reachable and has default_grants seeded", "user_id", userID)
			http.Error(w, "service configuration error: permissions not available", http.StatusServiceUnavailable)
			return
		}
		// GORM surfaces the raw DB error string; string-matching "unique" is the
		// portable way to detect unique constraint violations.
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			span.SetStatus(codes.Error, "email or username conflict")
			slog.WarnContext(ctx, "signup failed: email or username already in use", "username", req.Username)
			meterSignups.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "conflict")))
			http.Error(w, "email or username already in use", http.StatusConflict)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to create user")
		slog.ErrorContext(ctx, "signup failed: could not create user", "username", req.Username, "error", err)
		http.Error(w, "failed to create user", http.StatusInternalServerError)
		return
	}
	if isFirstUser {
		slog.InfoContext(ctx, "signup: first user detected — bootstrapped as admin", "user_id", userID, "username", req.Username)
	}

	// Materialise the user-scoped default grants into the user's default role. For
	// the bootstrap admin this is non-fatal — it already has full access via its
	// wildcard grant, so a missing default role is retried on next login rather than
	// rolling back the platform owner. A normal user with no default role would be
	// permission-less, so there we roll back.
	if err := rebuildDefaultRole(ctx, userID); err != nil {
		if isFirstUser {
			slog.WarnContext(ctx, "signup: default role build failed for bootstrap admin (admin still has full access)", "user_id", userID, "error", err)
		} else {
			span.RecordError(err)
			span.SetStatus(codes.Error, "failed to build default role")
			slog.ErrorContext(ctx, "signup failed: could not build default role", "user_id", userID, "error", err)
			if cleanErr := user.Remove(ctx); cleanErr != nil {
				slog.ErrorContext(ctx, "signup: failed to clean up user after default role failure", "user_id", userID, "error", cleanErr)
			}
			if cleanErr := personalRole.Remove(ctx); cleanErr != nil {
				slog.ErrorContext(ctx, "signup: failed to clean up personal role after default role failure", "role_id", personalRole.RoleID, "error", cleanErr)
			}
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("user.created", trace.WithAttributes(
		attribute.String("user.id", userID),
	))
	span.SetStatus(codes.Ok, "")
	meterSignups.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
	slog.InfoContext(ctx, "user created successfully", "user_id", userID, "username", req.Username)
	writeAudit(ctx, userID, "user", "user.signup", userID, req.Username)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(signupResponse{
		UserID:    user.UserID,
		Email:     user.Email,
		Username:  user.Username,
		Firstname: user.Firstname,
		Lastname:  user.Lastname,
	})
}

// handleLogin validates credentials and issues a JWT. A fresh ECDSA P-256 key
// pair is generated per session: the private key signs the token, and the public
// key is stored in the Session row so authMiddleware can verify it without a
// shared secret.
func handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleLogin")
	defer span.End()
	r = r.WithContext(ctx)

	slog.InfoContext(ctx, "login request received")

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.WarnContext(ctx, "login failed: invalid request body", "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Email == "" || req.Password == "" {
		span.SetStatus(codes.Error, "missing email or password")
		slog.WarnContext(ctx, "login failed: missing email or password")
		http.Error(w, "email and password are required", http.StatusBadRequest)
		return
	}

	slog.DebugContext(ctx, "attempting login")

	// dummyHash is a pre-computed bcrypt hash used to keep the response time
	// constant whether or not the email exists, preventing user enumeration via timing.
	const dummyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

	user, userErr := getUserByEmail(ctx, req.Email)
	if userErr != nil {
		bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(req.Password)) //nolint:errcheck
		span.SetStatus(codes.Error, "user not found")
		slog.WarnContext(ctx, "login failed: user not found or inactive")
		meterLogins.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "failure")))
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	span.AddEvent("user.found", trace.WithAttributes(attribute.String("user.id", user.UserID)))

	// The platform account owns instance configuration; it is a subject for audit
	// attribution, never a login. Answered exactly like an unknown email — same status,
	// same dummy comparison to hold the timing — so its existence cannot be probed for.
	// Its stored hash is already unusable (see lockedPasswordHash); this is the explicit
	// rule, so the account stays locked even if that ever changed.
	if isPlatformAccount(user) {
		bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(req.Password)) //nolint:errcheck
		span.SetStatus(codes.Error, "login attempt on the platform account")
		slog.WarnContext(ctx, "login failed: the platform account cannot be logged into", "user_id", user.UserID)
		meterLogins.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "failure")))
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.HashedPassword), []byte(req.Password)); err != nil {
		span.SetStatus(codes.Error, "password mismatch")
		slog.WarnContext(ctx, "login failed: password mismatch", "user_id", user.UserID)
		meterLogins.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "failure")))
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	span.AddEvent("credentials.verified")

	// Refresh the user's default role if the default grant definitions have
	// changed since it was last built. The version compare makes this a no-op on
	// the overwhelming majority of logins; it runs before the MFA branch so it
	// covers both MFA and non-MFA logins. A rebuild failure is non-fatal — the
	// user keeps their existing (stale) default role and we retry next login.
	if v := grantsVersion(); v != "" && user.DefaultGrantsVersion != v {
		if err := rebuildDefaultRole(ctx, user.UserID); err != nil {
			slog.WarnContext(ctx, "login: default role rebuild failed", "user_id", user.UserID, "error", err)
		} else {
			span.AddEvent("default_role.rebuilt")
		}
	}

	if userHasTOTP(ctx, user.UserID) {
		pending, err := newMFAPending(ctx, user.UserID, "", "", "", "")
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "mfa pending creation failed")
			slog.ErrorContext(ctx, "login: failed to create MFA pending", "user_id", user.UserID, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		span.SetStatus(codes.Ok, "")
		slog.InfoContext(ctx, "login: MFA required", "user_id", user.UserID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"mfa_required": true,
			"mfa_token":    pending.Token,
		})
		return
	}

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "key generation failed")
		slog.ErrorContext(ctx, "login failed: could not generate ECDSA key", "user_id", user.UserID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("keypair.generated")

	sessionID := uuid.New().String()
	const maxTTLHours = 720 // 30 days
	ttlHours := 24
	if v := os.Getenv("SESSION_TTL_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > maxTTLHours {
				n = maxTTLHours
			}
			ttlHours = n
		}
	}
	expiresAt := time.Now().Add(time.Duration(ttlHours) * time.Hour).UTC().Truncate(time.Second)

	span.SetAttributes(
		attribute.String("user.id", user.UserID),
		attribute.String("session.id", sessionID),
		attribute.String("session.expires_at", expiresAt.String()),
	)
	slog.InfoContext(ctx, "creating session", "user_id", user.UserID, "session_id", sessionID, "expires_at", expiresAt)

	tokenString, err := jwt.NewWithClaims(jwt.SigningMethodES256, authClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "gatekeeper",
			Subject:   user.UserID,
			ID:        sessionID,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}).SignedString(privKey)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "JWT signing failed")
		slog.ErrorContext(ctx, "login failed: could not sign JWT", "user_id", user.UserID, "session_id", sessionID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("jwt.signed")

	pubKeyBytes, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "public key marshal failed")
		slog.ErrorContext(ctx, "login failed: could not marshal public key", "user_id", user.UserID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	pubKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubKeyBytes})

	session := Session{
		SessionID: sessionID,
		UserID:    user.UserID,
		ExpiresAt: expiresAt,
		PubKey:    string(pubKeyPEM),
	}
	if err := session.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "session persist failed")
		slog.ErrorContext(ctx, "login failed: could not persist session", "user_id", user.UserID, "session_id", sessionID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("session.persisted", trace.WithAttributes(
		attribute.String("session.id", sessionID),
	))
	span.SetStatus(codes.Ok, "")
	meterLogins.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
	slog.InfoContext(ctx, "login successful", "user_id", user.UserID, "session_id", sessionID)
	writeAudit(ctx, user.UserID, "user", "session.create", sessionID, user.Username)

	setSessionCookie(w, tokenString, expiresAt)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": tokenString})
}
