package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
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
	Email     string `json:"email"`
	Username  string `json:"username"`
	Password  string `json:"password"` // optional; kept unchanged when empty
	Firstname string `json:"firstname"`
	Lastname  string `json:"lastname"`
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
		attribute.String("caller.id", callerID),
		attribute.String("target.user_id", id),
	)
	slog.Info("get user request", "caller_id", callerID, "target_user_id", id)

	if !requirePermission(w, r, "getUser", "gatekeeper/users/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (User{UserID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "user not found")
		slog.Warn("get user: not found", "caller_id", callerID, "target_user_id", id)
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("user.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.Info("get user: success", "caller_id", callerID, "target_user_id", id)
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
		attribute.String("caller.id", callerID),
		attribute.String("target.user_id", id),
	)
	slog.Info("update user request", "caller_id", callerID, "target_user_id", id)

	if !requirePermission(w, r, "updateUser", "gatekeeper/users/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	var req updateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("update user: invalid request body", "caller_id", callerID, "target_user_id", id, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Email == "" || req.Username == "" {
		span.SetStatus(codes.Error, "missing required fields")
		slog.Warn("update user: missing required fields", "caller_id", callerID, "target_user_id", id, "email_provided", req.Email != "", "username_provided", req.Username != "")
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
		slog.Warn("update user: not found", "caller_id", callerID, "target_user_id", id)
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
		if len(req.Password) < 8 {
			http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
			return
		}
		if len(req.Password) > 128 {
			http.Error(w, "password must not exceed 128 characters", http.StatusBadRequest)
			return
		}
		slog.Info("update user: changing password", "caller_id", callerID, "target_user_id", id)
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), 12)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "bcrypt failure")
			slog.Error("update user: bcrypt error", "caller_id", callerID, "target_user_id", id, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		u.HashedPassword = string(hash)
		span.AddEvent("password.rehashed")
	}
	if err := u.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("update user: db error", "caller_id", callerID, "target_user_id", id, "error", err)
		http.Error(w, "failed to update user", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.write", trace.WithAttributes(attribute.String("user.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.Info("update user: success", "caller_id", callerID, "target_user_id", id, "new_email", req.Email, "new_username", req.Username)
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
	span.SetAttributes(attribute.String("caller.id", callerID))
	slog.Info("list users request", "caller_id", callerID)

	if !requirePermission(w, r, "listUser", "gatekeeper/users") {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	limit, offset, ok := parsePagination(w, r)
	if !ok {
		span.SetStatus(codes.Error, "invalid pagination")
		return
	}

	q := r.URL.Query()
	var filter User
	if v := q.Get("user_id"); v != "" {
		filter.UserID = v
	}
	if v := q.Get("email"); v != "" {
		filter.Email = v
	}
	if v := q.Get("username"); v != "" {
		filter.Username = v
	}
	if v := q.Get("org_id"); v != "" {
		s := v
		filter.OrgID = &s
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
		slog.Warn("list users: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list users", http.StatusInternalServerError)
		return
	}
	responses := make([]userResponse, len(rows))
	for i, row := range rows {
		responses[i] = toUserResponse(row.(User))
	}
	span.AddEvent("db.read")
	span.SetStatus(codes.Ok, "")
	slog.Info("list users: success", "caller_id", callerID, "count", len(responses))
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
		attribute.String("caller.id", callerID),
		attribute.String("target.user_id", id),
	)
	slog.Info("delete user request", "caller_id", callerID, "target_user_id", id)

	if !requirePermission(w, r, "deleteUser", "gatekeeper/users/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (User{UserID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "user not found")
		slog.Warn("delete user: not found", "caller_id", callerID, "target_user_id", id)
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("user.id", id)))

	if err := row.(User).Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.Error("delete user: db error", "caller_id", callerID, "target_user_id", id, "error", err)
		http.Error(w, "failed to delete user", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.soft_delete", trace.WithAttributes(attribute.String("user.id", id)))

	if err := connect().WithContext(ctx).Model(&Session{}).Where("user_id = ?", id).Update("active", false).Error; err != nil {
		slog.Error("delete user: failed to invalidate sessions", "caller_id", callerID, "target_user_id", id, "error", err)
	} else {
		cacheDelUserSessions(ctx, id)
		span.AddEvent("sessions.invalidated", trace.WithAttributes(attribute.String("user.id", id)))
		slog.Info("delete user: sessions invalidated", "caller_id", callerID, "target_user_id", id)
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("delete user: success", "caller_id", callerID, "target_user_id", id)
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

	slog.Info("signup request received")

	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("signup failed: invalid request body", "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Email == "" || req.Username == "" || req.Password == "" {
		span.SetStatus(codes.Error, "missing required fields")
		slog.Warn("signup failed: missing required fields", "email_provided", req.Email != "", "username_provided", req.Username != "")
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

	span.SetAttributes(attribute.String("user.username", req.Username))
	slog.Info("creating new user", "username", req.Username)

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), 12)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "bcrypt failure")
		slog.Error("signup failed: bcrypt error", "email", req.Email, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("password.hashed")

	userID := uuid.New().String()

	gatekeeperPerm := Permissions{
		Name:          fmt.Sprintf("Basic Permissions for user %s", userID),
		PermissionsID: uuid.New().String(),
		Service:       "gatekeeper",
		Actions:       []string{"getUser", "updateUser", "deleteUser", "createOrg", "createTeam"},
		Resources:     []string{fmt.Sprintf("gatekeeper/users/%s", userID), "gatekeeper/orgs", "gatekeeper/teams"},
	}

	if err = gatekeeperPerm.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to create default permission")
		slog.Error("signup failed: could not create default permission", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("permission.created", trace.WithAttributes(
		attribute.String("permissions.id", gatekeeperPerm.PermissionsID),
		attribute.String("permissions.service", gatekeeperPerm.Service),
	))
	slog.Info("default permission created", "user_id", userID, "permissions_id", gatekeeperPerm.PermissionsID)

	blueprintsPerm := Permissions{
		Name:          fmt.Sprintf("State Permissions for user %s", req.Username),
		PermissionsID: uuid.New().String(),
		Service:       "blueprints",
		Actions:       []string{"getState", "updateState", "deleteState", "lockState", "unlockState"},
		Resources:     []string{fmt.Sprintf("states/%s/*", req.Username)},
	}

	if err = blueprintsPerm.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to create default blueprints permission")
		slog.Error("signup failed: could not create default blueprints permission", "user_id", userID, "error", err)
		gatekeeperPerm.Remove(ctx) //nolint:errcheck
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("permission.created", trace.WithAttributes(
		attribute.String("permissions.id", blueprintsPerm.PermissionsID),
		attribute.String("permissions.service", blueprintsPerm.Service),
	))
	slog.Info("default blueprints permission created", "user_id", userID, "permissions_id", blueprintsPerm.PermissionsID)

	forgePerm := Permissions{
		Name:          fmt.Sprintf("Forge Permissions for user %s", req.Username),
		PermissionsID: uuid.New().String(),
		Service:       "forge",
		Resources:     []string{"forge/executions", "forge/executions/*"},
		Actions:       []string{"createExecution", "listExecution", "getExecution", "deleteExecution"},
	}
	if err = forgePerm.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to create default forge permission")
		slog.Error("signup failed: could not create default forge permission", "user_id", userID, "error", err)
		gatekeeperPerm.Remove(ctx) //nolint:errcheck
		blueprintsPerm.Remove(ctx) //nolint:errcheck
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("permission.created", trace.WithAttributes(
		attribute.String("permissions.id", forgePerm.PermissionsID),
		attribute.String("permissions.service", forgePerm.Service),
	))
	slog.Info("default forge permission created", "user_id", userID, "permissions_id", forgePerm.PermissionsID)

	role := Role{
		RoleID:         uuid.New().String(),
		PermissionsIDs: []string{gatekeeperPerm.PermissionsID, blueprintsPerm.PermissionsID, forgePerm.PermissionsID},
	}
	if err = role.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to create default role")
		slog.Error("signup failed: could not create default role", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("role.created", trace.WithAttributes(
		attribute.String("role.id", role.RoleID),
	))
	slog.Info("default role created", "user_id", userID, "role_id", role.RoleID)

	user := User{
		UserID:         userID,
		HashedPassword: string(hash),
		Email:          req.Email,
		Username:       req.Username,
		Firstname:      req.Firstname,
		Lastname:       req.Lastname,
		RoleID:         &role.RoleID,
	}

	if err := user.Add(ctx); err != nil {
		// Clean up permissions and role that were already committed.
		if cleanErr := gatekeeperPerm.Remove(ctx); cleanErr != nil {
			slog.Error("signup: failed to clean up orphaned permission", "permissions_id", gatekeeperPerm.PermissionsID, "error", cleanErr)
		}
		if cleanErr := blueprintsPerm.Remove(ctx); cleanErr != nil {
			slog.Error("signup: failed to clean up orphaned permission", "permissions_id", blueprintsPerm.PermissionsID, "error", cleanErr)
		}
		if cleanErr := forgePerm.Remove(ctx); cleanErr != nil {
			slog.Error("signup: failed to clean up orphaned permission", "permissions_id", forgePerm.PermissionsID, "error", cleanErr)
		}
		if cleanErr := role.Remove(ctx); cleanErr != nil {
			slog.Error("signup: failed to clean up orphaned role", "role_id", role.RoleID, "error", cleanErr)
		}
		// GORM surfaces the raw DB error string; string-matching "unique" is the
		// portable way to detect unique constraint violations without importing a
		// postgres-specific driver package.
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			span.SetStatus(codes.Error, "email or username conflict")
			slog.Warn("signup failed: email or username already in use", "email", req.Email, "username", req.Username)
			meterSignups.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "conflict")))
			http.Error(w, "email or username already in use", http.StatusConflict)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to insert user")
		slog.Error("signup failed: could not insert user", "email", req.Email, "username", req.Username, "error", err)
		http.Error(w, "failed to create user", http.StatusInternalServerError)
		return
	}

	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("user.created", trace.WithAttributes(
		attribute.String("user.id", userID),
	))
	span.SetStatus(codes.Ok, "")
	meterSignups.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
	slog.Info("user created successfully", "user_id", userID, "email", req.Email, "username", req.Username)
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

	slog.Info("login request received")

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("login failed: invalid request body", "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Email == "" || req.Password == "" {
		span.SetStatus(codes.Error, "missing email or password")
		slog.Warn("login failed: missing email or password")
		http.Error(w, "email and password are required", http.StatusBadRequest)
		return
	}

	slog.Debug("attempting login")

	// dummyHash is a pre-computed bcrypt hash used to keep the response time
	// constant whether or not the email exists, preventing user enumeration via timing.
	const dummyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

	var user User
	if err := connect().WithContext(ctx).Where("email = ? AND active = ?", req.Email, true).First(&user).Error; err != nil {
		bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(req.Password)) //nolint:errcheck
		span.SetStatus(codes.Error, "user not found")
		slog.Warn("login failed: user not found or inactive")
		meterLogins.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "failure")))
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	span.AddEvent("user.found", trace.WithAttributes(attribute.String("user.id", user.UserID)))

	if err := bcrypt.CompareHashAndPassword([]byte(user.HashedPassword), []byte(req.Password)); err != nil {
		span.SetStatus(codes.Error, "password mismatch")
		slog.Warn("login failed: password mismatch", "email", req.Email, "user_id", user.UserID)
		meterLogins.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "failure")))
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	span.AddEvent("credentials.verified")

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "key generation failed")
		slog.Error("login failed: could not generate ECDSA key", "user_id", user.UserID, "error", err)
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
	slog.Info("creating session", "user_id", user.UserID, "session_id", sessionID, "expires_at", expiresAt)

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
		slog.Error("login failed: could not sign JWT", "user_id", user.UserID, "session_id", sessionID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("jwt.signed")

	pubKeyBytes, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "public key marshal failed")
		slog.Error("login failed: could not marshal public key", "user_id", user.UserID, "error", err)
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
		slog.Error("login failed: could not persist session", "user_id", user.UserID, "session_id", sessionID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("session.persisted", trace.WithAttributes(
		attribute.String("session.id", sessionID),
	))
	span.SetStatus(codes.Ok, "")
	meterLogins.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
	slog.Info("login successful", "user_id", user.UserID, "session_id", sessionID)
	writeAudit(ctx, user.UserID, "user", "session.create", sessionID, user.Username)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": tokenString})
}
