package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"golang.org/x/crypto/bcrypt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

type contextKey string

const userIDKey contextKey = "user_id"

// authClaims is the JWT claims type used for all session tokens. Subject holds
// the user ID; ID (jti) holds the session ID used to look up the stored public key.
type authClaims struct {
	jwt.RegisteredClaims
}

type checkPermissionsRequest struct {
	Service  string `json:"service"`
	Resource string `json:"resource"`
	Action   string `json:"action"`
}

// dbLog logs at Warn when err is gorm.ErrRecordNotFound (expected empty SELECT) and at Error
// for any other database failure.
func dbLog(err error, msg string, args ...any) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		slog.Warn(msg, args...)
	} else {
		slog.Error(msg, args...)
	}
}

// requirePermission checks if the authenticated user may perform action on resource within the
// "gatekeeper" service. Writes 403 and returns false on denial.
func requirePermission(w http.ResponseWriter, r *http.Request, action, resource string) bool {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "requirePermission")
	defer span.End()
	r = r.WithContext(ctx)

	userID, _ := r.Context().Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("permission.service", "gatekeeper"),
		attribute.String("permission.action", action),
		attribute.String("permission.resource", resource),
	)
	ok, err := checkPermissions(ctx, userID, "gatekeeper", action, resource)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("permission check error", "user_id", userID, "action", action, "resource", resource, "error", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	if !ok {
		span.SetStatus(codes.Error, "permission denied")
		slog.Warn("permission denied", "user_id", userID, "action", action, "resource", resource)
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	span.SetStatus(codes.Ok, "")
	slog.Debug("permission approved", "user_id", userID, "service", "gatekeeper", "action", action, "resource", resource)
	return true
}

// authMiddleware verifies the Bearer JWT in the Authorization header.
// On success it adds the authenticated user's ID to the request context under userIDKey.
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "authMiddleware")
		defer span.End()
		span.SetAttributes(
			attribute.String("http.method", r.Method),
			attribute.String("http.path", r.URL.Path),
		)

		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			span.SetStatus(codes.Error, "missing or malformed Authorization header")
			slog.Warn("auth rejected: missing or malformed Authorization header", "method", r.Method, "path", r.URL.Path)
			meterAuthMiddleware.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "rejected")))
			http.Error(w, "missing or invalid authorization header", http.StatusUnauthorized)
			return
		}
		tokenString := strings.TrimPrefix(authHeader, "Bearer ")

		// Parse without verification to extract the session ID (JWT jti claim).
		unverified, _, err := jwt.NewParser().ParseUnverified(tokenString, &authClaims{})
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "token parse failed")
			slog.Warn("auth rejected: could not parse token", "method", r.Method, "path", r.URL.Path, "error", err)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		c, ok := unverified.Claims.(*authClaims)
		if !ok || c.ID == "" {
			span.SetStatus(codes.Error, "token missing jti claim")
			slog.Warn("auth rejected: token missing jti claim", "method", r.Method, "path", r.URL.Path)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		span.SetAttributes(attribute.String("session.id", c.ID))
		span.AddEvent("token.parsed", trace.WithAttributes(attribute.String("session.id", c.ID)))

		row, err := (Session{SessionID: c.ID}).Get(ctx)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "session not found")
			slog.Warn("auth rejected: session not found or inactive", "session_id", c.ID, "method", r.Method, "path", r.URL.Path)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		session := row.(Session)
		span.AddEvent("session.loaded", trace.WithAttributes(
			attribute.String("session.id", session.SessionID),
			attribute.String("user.id", session.UserID),
			attribute.String("session.expires_at", session.ExpiresAt.String()),
		))

		if time.Now().After(session.ExpiresAt) {
			span.SetStatus(codes.Error, "token expired")
			slog.Warn("auth rejected: token expired", "session_id", session.SessionID, "user_id", session.UserID, "expired_at", session.ExpiresAt)
			http.Error(w, "token expired", http.StatusUnauthorized)
			return
		}

		pubKey, err := parseECPublicKey(session.PubKey)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "public key parse failed")
			slog.Error("auth failed: could not parse session public key", "session_id", c.ID, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		span.AddEvent("pubkey.parsed")

		verified, err := jwt.ParseWithClaims(tokenString, &authClaims{}, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodECDSA); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return pubKey, nil
		})
		if err != nil || !verified.Valid {
			span.RecordError(err)
			span.SetStatus(codes.Error, "token signature invalid")
			slog.Warn("auth rejected: token signature invalid", "session_id", c.ID, "error", err)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		claims, ok := verified.Claims.(*authClaims)
		if !ok || claims.Subject == "" {
			span.SetStatus(codes.Error, "verified token missing subject")
			slog.Warn("auth rejected: verified token missing subject", "session_id", c.ID)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		span.SetAttributes(attribute.String("user.id", claims.Subject))
		span.AddEvent("auth.accepted", trace.WithAttributes(
			attribute.String("user.id", claims.Subject),
			attribute.String("session.id", session.SessionID),
		))
		span.SetStatus(codes.Ok, "")
		meterAuthMiddleware.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "accepted")))
		slog.Debug("auth accepted", "user_id", claims.Subject, "session_id", session.SessionID, "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, userIDKey, claims.Subject)))
	})
}

// checkPermissions resolves the caller's effective permissions by walking both
// their direct role and their team's role, then returns true on the first
// matching (service, action, resource) triple. Resource matching supports exact
// strings, wildcard "*", prefix "foo/*", and per-segment wildcards like
// "foo/*/bar". Returns (false, nil) — not an error — when no match is found.
func checkPermissions(ctx context.Context, userID string, service string, action string, resource string) (bool, error) {
	permissionCheck := PermissionsCheck{
		PermissionsCheckID: uuid.New().String(),
		Action:             action,
		Resource:           resource,
		UserID:             userID,
	}
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "checkPermissions")
	defer span.End()
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("permission.service", service),
		attribute.String("permission.action", action),
		attribute.String("permission.resource", resource),
	)

	slog.Debug("checking permissions", "user_id", userID, "service", service, "action", action, "resource", resource)

	row, err := (User{UserID: userID}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		dbLog(err, "checkPermissions: failed to load user", "user_id", userID, "error", err)
		permissionCheck.Granted = false
		perErr := permissionCheck.Add(ctx)
		if perErr != nil {
			slog.Debug("permissions check row failed to save", "error", perErr.Error())
		}

		return false, err
	}
	user := row.(User)
	permissionCheck.TeamID = user.TeamID
	permissionCheck.OrgID = user.OrgID
	span.AddEvent("user.loaded", trace.WithAttributes(
		attribute.Bool("user.has_role", user.RoleID != nil),
		attribute.Bool("user.has_team", user.TeamID != nil),
	))

	var permissions []Permissions

	if user.RoleID != nil {
		slog.Debug("loading direct role permissions", "user_id", userID, "role_id", *user.RoleID)
		span.AddEvent("role.loading", trace.WithAttributes(attribute.String("role.id", *user.RoleID)))
		roleRow, err := (Role{RoleID: *user.RoleID}).Get(ctx)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			dbLog(err, "checkPermissions: failed to load user role", "user_id", userID, "role_id", *user.RoleID, "error", err)
			permissionCheck.Granted = false
			perErr := permissionCheck.Add(ctx)
			if perErr != nil {
				slog.Debug("permissions check row failed to save", "error", perErr.Error())
			}
			return false, err
		}
		role := roleRow.(Role)
		span.AddEvent("role.loaded", trace.WithAttributes(
			attribute.String("role.id", role.RoleID),
			attribute.Int("role.permissions_count", len(role.PermissionsIDs)),
		))
		slog.Debug("user role loaded", "user_id", userID, "role_id", role.RoleID, "permissions_count", len(role.PermissionsIDs))
		for _, pid := range role.PermissionsIDs {
			pRow, err := (Permissions{PermissionsID: pid}).Get(ctx)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				dbLog(err, "checkPermissions: failed to load permission", "user_id", userID, "permissions_id", pid, "error", err)
				permissionCheck.Granted = false
				perErr := permissionCheck.Add(ctx)
				if perErr != nil {
					slog.Debug("permissions check row failed to save", "error", perErr.Error())
				}
				return false, err
			}
			permissions = append(permissions, pRow.(Permissions))
		}
		span.AddEvent("direct_role.permissions_loaded", trace.WithAttributes(
			attribute.Int("permissions.count", len(permissions)),
		))
	}

	// Two separate blocks: the first attempts the load and nils out TeamID on failure
	// (stale reference — team was soft-deleted but user.team_id was not yet cleared).
	// The second block only runs when the load succeeded, keeping the happy-path readable.
	if user.TeamID != nil {
		slog.Debug("loading team role permissions", "user_id", userID, "team_id", *user.TeamID)
		span.AddEvent("team.loading", trace.WithAttributes(attribute.String("team.id", *user.TeamID)))
		row, err = (Team{TeamID: *user.TeamID}).Get(ctx)
		if err != nil {
			slog.Debug("checkPermissions: team not found, skipping team permissions", "user_id", userID, "team_id", *user.TeamID)
			user.TeamID = nil
		}
	}
	if user.TeamID != nil {
		team := row.(Team)
		span.AddEvent("team.loaded", trace.WithAttributes(
			attribute.String("team.id", team.TeamID),
			attribute.Bool("team.has_role", team.RoleID != nil),
		))
		if team.RoleID == nil {
			slog.Debug("team has no role assigned, skipping team permission check", "user_id", userID, "team_id", team.TeamID)
		} else {
			slog.Debug("loading team role", "user_id", userID, "team_id", team.TeamID, "role_id", *team.RoleID)
			span.AddEvent("team_role.loading", trace.WithAttributes(attribute.String("role.id", *team.RoleID)))
			row, err = (Role{RoleID: *team.RoleID}).Get(ctx)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				dbLog(err, "checkPermissions: failed to load team role", "user_id", userID, "team_id", team.TeamID, "role_id", *team.RoleID, "error", err)
				permissionCheck.Granted = false
				perErr := permissionCheck.Add(ctx)
				if perErr != nil {
					slog.Debug("permissions check row failed to save", "error", perErr.Error())
				}
				return false, err
			}
			role := row.(Role)
			span.AddEvent("team_role.loaded", trace.WithAttributes(
				attribute.String("role.id", role.RoleID),
				attribute.Int("role.permissions_count", len(role.PermissionsIDs)),
			))
			slog.Debug("team role loaded", "user_id", userID, "team_id", team.TeamID, "role_id", role.RoleID, "permissions_count", len(role.PermissionsIDs))
			for _, pid := range role.PermissionsIDs {
				pRow, err := (Permissions{PermissionsID: pid}).Get(ctx)
				if err != nil {
					span.RecordError(err)
					span.SetStatus(codes.Error, err.Error())
					dbLog(err, "checkPermissions: failed to load team permission", "user_id", userID, "team_id", team.TeamID, "permissions_id", pid, "error", err)
					permissionCheck.Granted = false
					perErr := permissionCheck.Add(ctx)
					if perErr != nil {
						slog.Debug("permissions check row failed to save", "error", perErr.Error())
					}
					return false, err
				}
				permissions = append(permissions, pRow.(Permissions))
			}
			span.AddEvent("team_role.permissions_loaded", trace.WithAttributes(
				attribute.Int("permissions.total", len(permissions)),
			))
		}
	}

	slog.Debug("evaluating permissions", "user_id", userID, "total_permissions", len(permissions), "action", action, "resource", resource)
	span.AddEvent("evaluation.started", trace.WithAttributes(
		attribute.Int("permissions.total", len(permissions)),
	))

	for _, permission := range permissions {
		resourceMatch := false

		// Three matching strategies, tried in order:
		//  1. Exact match or global wildcard ("*")
		//  2. Trailing-star prefix: "blueprints/states/*" matches any path under that prefix
		//  3. Per-segment wildcard: "blueprints/states/*/locks" matches a specific depth with a wildcard segment
		if slices.Contains(permission.Resources, resource) || slices.Contains(permission.Resources, "*") {
			resourceMatch = true
		} else {
			for _, allowedResource := range permission.Resources {
				if len(allowedResource) > 0 && allowedResource[len(allowedResource)-1] == '*' {
					prefix := allowedResource[:len(allowedResource)-1]
					if strings.HasPrefix(resource, prefix) {
						resourceMatch = true
					}
				}
				split_resource_a := strings.Split(resource, "/")
				split_resource_b := strings.Split(allowedResource, "/")
				if len(split_resource_a) == len(split_resource_b) {
					nonMatch := false
					for i := range split_resource_a {
						if split_resource_a[i] != split_resource_b[i] && split_resource_b[i] != "*" {
							nonMatch = true
						}
					}
					if !nonMatch {
						resourceMatch = true
					}
				}
			}
		}

		allowedAction := false
		if slices.Contains(permission.Actions, "*") || slices.Contains(permission.Actions, action) {
			allowedAction = true
		}
		for _, per_action := range permission.Actions {
			if len(per_action) > 0 && per_action[len(per_action)-1] == '*' {
				prefix := per_action[:len(per_action)-1]
				if strings.HasPrefix(action, prefix) {
					allowedAction = true
				}
			}
		}

		if permission.Service == service && allowedAction && resourceMatch {
			span.SetAttributes(attribute.Bool("permission.granted", true))
			span.AddEvent("permission.granted", trace.WithAttributes(
				attribute.String("matched.permissions_id", permission.PermissionsID),
				attribute.String("matched.service", permission.Service),
			))
			span.SetStatus(codes.Ok, "")
			slog.Debug("permission granted", "user_id", userID, "service", service, "action", action, "resource", resource, "matched_permission_id", permission.PermissionsID)
			permissionCheck.Granted = true
			perErr := permissionCheck.Add(ctx)
			if perErr != nil {
				slog.Debug("permissions check row failed to save", "error", perErr.Error())
			}
			return true, nil
		}
	}

	span.SetAttributes(attribute.Bool("permission.granted", false))
	span.AddEvent("permission.denied", trace.WithAttributes(
		attribute.Int("permissions.checked", len(permissions)),
	))
	span.SetStatus(codes.Ok, "")
	slog.Debug("permission denied", "user_id", userID, "service", service, "action", action, "resource", resource, "permissions_checked", len(permissions))
	permissionCheck.Granted = false
	perErr := permissionCheck.Add(ctx)
	if perErr != nil {
		slog.Debug("permissions check row failed to save", "error", perErr.Error())
	}
	return false, nil
}

// handleCheckPermissions is the public GET /check_permissions endpoint. It
// evaluates the (service, action, resource) triple in the request body against
// the authenticated user's resolved permissions and returns {"authorized": bool}.
func handleCheckPermissions(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCheckPermissions")
	defer span.End()
	r = r.WithContext(ctx)

	var req checkPermissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("check_permissions: invalid request body", "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	userID, _ := r.Context().Value(userIDKey).(string)

	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("permission.service", req.Service),
		attribute.String("permission.action", req.Action),
		attribute.String("permission.resource", req.Resource),
	)
	slog.Info("check_permissions request", "user_id", userID, "service", req.Service, "action", req.Action, "resource", req.Resource)

	w.Header().Set("Content-Type", "application/json")
	isAllowed, err := checkPermissions(r.Context(), userID, req.Service, req.Action, req.Resource)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		span.RecordError(err)
		span.SetStatus(codes.Error, "evaluation error")
		slog.Error("check_permissions: evaluation error", "user_id", userID, "service", req.Service, "action", req.Action, "resource", req.Resource, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		slog.Warn("permission check error", "user_id", userID, "action", req.Action, "error", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	span.SetAttributes(attribute.Bool("permission.authorized", isAllowed))
	span.SetStatus(codes.Ok, "")
	meterPermissionChecks.Add(ctx, 1, metric.WithAttributes(attribute.Bool("authorized", isAllowed)))
	slog.Info("check_permissions result", "user_id", userID, "service", req.Service, "action", req.Action, "resource", req.Resource, "authorized", isAllowed)
	json.NewEncoder(w).Encode(map[string]bool{"authorized": isAllowed})
}

// parseECPublicKey decodes a PEM-encoded PKIX public key and asserts it is ECDSA.
func parseECPublicKey(pemStr string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ecKey, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("key is not ECDSA")
	}
	return ecKey, nil
}

// grantPermissions creates a Permissions record and appends it to userID's direct role.
// If the user has no direct role, one is created first. All writes go through db — pass
// connect().WithContext(ctx) for non-transactional callers, or a *gorm.DB transaction.
func grantPermissions(ctx context.Context, db *gorm.DB, userID, name string, actions []string, resource string) error {
	return grantServicePermissions(ctx, db, "gatekeeper", userID, name, actions, resource)
}

func grantServicePermissions(ctx context.Context, db *gorm.DB, service, userID, name string, actions []string, resource string) error {
	var user User
	if err := db.Where("user_id = ? AND active = ?", userID, true).First(&user).Error; err != nil {
		return err
	}

	var role Role
	if user.RoleID == nil {
		role = Role{RoleID: uuid.New().String(), OwnerID: userID, Active: true, PermissionsIDs: []string{}}
		if err := db.Create(&role).Error; err != nil {
			return err
		}
		user.RoleID = &role.RoleID
		if err := db.Save(&user).Error; err != nil {
			return err
		}
		cacheDel(ctx, "gk:user:"+user.UserID)
	} else {
		if err := db.Where("role_id = ? AND active = ?", *user.RoleID, true).First(&role).Error; err != nil {
			return err
		}
	}

	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Name:          name,
		Service:       service,
		Actions:       actions,
		Resources:     []string{resource},
		OwnerID:       userID,
		Active:        true,
	}
	if err := db.Create(&perm).Error; err != nil {
		return err
	}

	if role.PermissionsIDs == nil {
		role.PermissionsIDs = []string{}
	}
	role.PermissionsIDs = append(role.PermissionsIDs, perm.PermissionsID)
	role.UpdatedAt = time.Now()
	if err := db.Save(&role).Error; err != nil {
		return err
	}
	cacheDel(ctx, "gk:role:"+role.RoleID)
	return nil
}

// writeAudit appends an immutable audit log entry. Failures are logged but
// never propagate to the caller — a missing audit entry is better than a
// failed request.
func writeAudit(ctx context.Context, actorID, actorType, action, resourceID, detail string) {
	entry := AuditLog{
		AuditLogID: uuid.New().String(),
		ActorID:    actorID,
		ActorType:  actorType,
		Action:     action,
		ResourceID: resourceID,
		Detail:     detail,
	}
	if err := entry.Add(ctx); err != nil {
		slog.Warn("audit log write failed", "action", action, "resource_id", resourceID, "error", err)
	}
}

// requireServiceAuth validates the X-Service-Key header (format "name:key") against the
// stored bcrypt hash for the named ServiceAccount. Returns the account on success.
func requireServiceAuth(w http.ResponseWriter, r *http.Request) (ServiceAccount, bool) {
	header := r.Header.Get("X-Service-Key")
	if header == "" {
		http.Error(w, "missing X-Service-Key header", http.StatusUnauthorized)
		return ServiceAccount{}, false
	}
	idx := strings.Index(header, ":")
	if idx < 1 {
		http.Error(w, "invalid X-Service-Key format, expected name:key", http.StatusUnauthorized)
		return ServiceAccount{}, false
	}
	name, key := header[:idx], header[idx+1:]

	row, err := (ServiceAccount{ServiceName: name}).Get(r.Context())
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return ServiceAccount{}, false
	}
	svc := row.(ServiceAccount)
	if err := bcrypt.CompareHashAndPassword([]byte(svc.HashedKey), []byte(key)); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return ServiceAccount{}, false
	}

	// IP allowlist: if the service account has allowed CIDRs configured, the
	// request source IP must fall within one of them.
	if len(svc.AllowedCIDRs) > 0 {
		remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			remoteIP = r.RemoteAddr
		}
		ip := net.ParseIP(remoteIP)
		allowed := false
		for _, cidr := range svc.AllowedCIDRs {
			_, ipNet, err := net.ParseCIDR(cidr)
			if err == nil && ip != nil && ipNet.Contains(ip) {
				allowed = true
				break
			}
		}
		if !allowed {
			slog.Warn("service auth rejected: source IP not in allowlist", "service", name, "remote_addr", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return ServiceAccount{}, false
		}
	}

	// TLS client certificate binding: if the service account has registered
	// certificate fingerprints, the client must present a matching certificate.
	// This requires gatekeeper to be started with mTLS (TLS_CLIENT_AUTH=require).
	if len(svc.ClientCertFingerprints) > 0 {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			slog.Warn("service auth rejected: client certificate required but not presented", "service", name)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return ServiceAccount{}, false
		}
		sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
		fingerprint := fmt.Sprintf("%x", sum[:])
		allowed := false
		for _, f := range svc.ClientCertFingerprints {
			if subtle.ConstantTimeCompare([]byte(f), []byte(fingerprint)) == 1 {
				allowed = true
				break
			}
		}
		if !allowed {
			slog.Warn("service auth rejected: client certificate fingerprint not recognised", "service", name)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return ServiceAccount{}, false
		}
	}

	return svc, true
}

// parsePagination reads ?limit=N&offset=N from the request. Returns 400 and
// false if limit is present but not a positive integer.
func parsePagination(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset = 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return 0, 0, false
		}
		if n > 500 {
			n = 500 // hard cap prevents a single request from dumping the entire table
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "offset must be a non-negative integer", http.StatusBadRequest)
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}
