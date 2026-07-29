package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// sessionResponse is the safe public shape of a Session — JWT and PubKey are
// deliberately omitted so the token material is never returned via the API.
type sessionResponse struct {
	SessionID string    `json:"session_id"`
	UserID    string    `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Active    bool      `json:"active"`
}

func toSessionResponse(s Session) sessionResponse {
	return sessionResponse{
		SessionID: s.SessionID,
		UserID:    s.UserID,
		ExpiresAt: s.ExpiresAt,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
		Active:    s.Active,
	}
}

func handleGetSession(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleGetSession")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("user.id", callerID),
		attribute.String("session.id", id),
	)
	slog.InfoContext(ctx, "get session request", "caller_id", callerID, "session_id", id)

	if !requirePermission(w, r, "getSession", "gatekeeper/sessions/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Session{SessionID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "session not found")
		slog.WarnContext(ctx, "get session: not found", "caller_id", callerID, "session_id", id)
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s := row.(Session)
	if s.UserID != callerID {
		span.SetStatus(codes.Ok, "")
		slog.WarnContext(ctx, "get session: cross-user attempt", "caller_id", callerID, "session_id", id, "session_user_id", s.UserID)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(
		attribute.String("session.id", id),
		attribute.String("session.user_id", s.UserID),
		attribute.String("session.expires_at", s.ExpiresAt.String()),
	))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "get session: success", "caller_id", callerID, "session_id", id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toSessionResponse(s))
}

// handleCreateRunToken mints a short-lived (24 h) session JWT for a user on
// behalf of the workflows service. The returned token carries the same
// permissions as a normal user login — the caller's own session token is never
// stored in the workflows database. Only the "workflows" service account may
// call this endpoint.
func handleCreateRunToken(w http.ResponseWriter, r *http.Request) {
	svc, ok := requireServiceAuth(w, r)
	if !ok {
		return
	}
	if !canMintScopedRoles(svc.ServiceName) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		UserID string  `json:"user_id"`
		RoleID *string `json:"role_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
		http.Error(w, "user_id is required", http.StatusBadRequest)
		return
	}

	if _, err := (User{UserID: req.UserID}).Get(r.Context()); err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	// Validate that role_id, if supplied, references a workflow-provisioned role.
	// This prevents a compromised caller from escalating privileges by supplying
	// an arbitrary org-admin or system role UUID.
	if req.RoleID != nil {
		roleRow, err := (Role{RoleID: *req.RoleID}).Get(r.Context())
		if err != nil {
			http.Error(w, "role not found", http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(roleRow.(Role).Name, "workflow:") {
			http.Error(w, "role_id must reference a workflow-provisioned role", http.StatusBadRequest)
			return
		}
	}

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		slog.ErrorContext(r.Context(), "run token: key generation failed", "user_id", req.UserID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	sessionID := uuid.New().String()
	// 1 h matches the worker rotation window (30–60 min) so a token is always
	// replaced before it can expire.
	expiresAt := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)

	tokenString, err := jwt.NewWithClaims(jwt.SigningMethodES256, authClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "gatekeeper",
			Subject:   req.UserID,
			ID:        sessionID,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}).SignedString(privKey)
	if err != nil {
		slog.ErrorContext(r.Context(), "run token: JWT signing failed", "user_id", req.UserID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	pubKeyBytes, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		slog.ErrorContext(r.Context(), "run token: public key marshal failed", "user_id", req.UserID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	session := Session{
		SessionID:    sessionID,
		UserID:       req.UserID,
		ExpiresAt:    expiresAt,
		PubKey:       string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubKeyBytes})),
		ScopedRoleID: req.RoleID,
	}
	if err := session.Add(r.Context()); err != nil {
		slog.ErrorContext(r.Context(), "run token: session persist failed", "user_id", req.UserID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.InfoContext(r.Context(), "run token created", "user_id", req.UserID, "session_id", sessionID)
	writeAudit(r.Context(), svc.ServiceName, "service", "run_token.create", sessionID, req.UserID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"token":      tokenString,
		"session_id": sessionID,
	})
}

// handleRevokeRunToken deactivates a run session created by handleCreateRunToken.
// Only the "workflows" service account may call this endpoint.
func handleRevokeRunToken(w http.ResponseWriter, r *http.Request) {
	svc, ok := requireServiceAuth(w, r)
	if !ok {
		return
	}
	if !canMintScopedRoles(svc.ServiceName) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	sessionID := r.PathValue("session_id")
	if err := (Session{SessionID: sessionID}).Remove(r.Context()); err != nil {
		slog.ErrorContext(r.Context(), "run token: revoke failed", "session_id", sessionID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.InfoContext(r.Context(), "run token revoked", "session_id", sessionID)
	writeAudit(r.Context(), svc.ServiceName, "service", "run_token.revoke", sessionID, "")
	w.WriteHeader(http.StatusNoContent)
}

// ── Workflow service roles ────────────────────────────────────────────────────

// permissionSpec is a single gatekeeper permission triple sent by the workflows
// service when provisioning a workflow's service role.
type permissionSpec struct {
	Name     string `json:"name"`
	Service  string `json:"service"`
	Action   string `json:"action"`
	Resource string `json:"resource"`
}

// handleCreateWorkflowRole provisions a minimal gatekeeper Role for a workflow.
// Only the "workflows" service account may call this endpoint.
//
// For each permission spec in the request, gatekeeper verifies that user_id
// actually holds that permission before including it — so the workflow role can
// never exceed the owner's own access. The resulting role_id is stored in the
// Workflow row and used to scope run tokens.
// canMintScopedRoles reports whether a service may provision a minimal-permission
// role and mint run tokens against it.
//
// workflows does this for a run; forge does it for an artifact step, where a sandbox
// needs a bearer for the artifact store; git_connector does it to authorize a clone
// against git-factory as the user the clone is for, instead of holding a shared
// east-west key. All three are safe for the same reason: every permission in the
// request is verified against the OWNER's own access before it is included, so a
// minted role can never exceed what the user already has — the caller is choosing a
// subset, not granting itself authority.
func canMintScopedRoles(service string) bool {
	return service == "workflows" || service == "forge" || service == "git_connector"
}

func handleCreateWorkflowRole(w http.ResponseWriter, r *http.Request) {
	svc, ok := requireServiceAuth(w, r)
	if !ok {
		return
	}
	if !canMintScopedRoles(svc.ServiceName) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		WorkflowID  string           `json:"workflow_id"`
		UserID      string           `json:"user_id"`
		OrgID       string           `json:"org_id"`
		Permissions []permissionSpec `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.WorkflowID == "" || req.UserID == "" {
		http.Error(w, "workflow_id and user_id are required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Resolve the owner's username and org name so scoped-role permissions are
	// stored in the same "<username>/..." form that checkPermissions scopes the
	// request resource to at run time. Storing the raw resource would never match
	// the scoped resource conductor sends for a run step (e.g. "alice/forge/...").
	var ownerUsername, ownerOrgName string
	// ownerOrgID is the authoritative org for the scoped role/permissions. Trusting
	// the caller-supplied req.OrgID verbatim violates fk_roles_org when it's empty
	// (org-less users) or stale, which makes the whole creation 500 and silently
	// drops the run back to the user's full permissions. Resolve it from the owner
	// instead; a nil pointer stores NULL (valid) rather than a dangling "".
	var ownerOrgID *string
	if userRow, err := (User{UserID: req.UserID}).Get(ctx); err == nil {
		owner := userRow.(User)
		ownerUsername = owner.Username
		ownerOrgID = owner.OrgID
		if owner.OrgID != nil {
			if orgRow, err2 := (Org{OrgID: *owner.OrgID}).Get(ctx); err2 == nil {
				ownerOrgName = orgRow.(Org).OrgName
			}
		}
	}

	var permIDs []string
	for _, p := range req.Permissions {
		if p.Service == "" || p.Action == "" || p.Resource == "" {
			continue
		}
		// Only include permissions the owner actually holds (no privilege escalation).
		granted, err := checkPermissions(ctx, req.UserID, p.Service, p.Action, p.Resource)
		if err != nil || !granted {
			// WARN, not DEBUG. Dropping a permission silently is the worst possible
			// outcome here: the pipeline is created successfully and then fails at run
			// time as a 403 from some other service, with nothing linking the two. At
			// the default LOG_LEVEL=info a Debug line does not exist, so the only
			// evidence of the decision was invisible in every real deployment.
			//
			// Still a skip rather than a rejection — unlike attenuatedPermissions,
			// which refuses — because this runs on every create AND update, so failing
			// closed would make a pipeline uneditable the moment one declared grant
			// stopped resolving. The log line is what makes the skip traceable.
			slog.WarnContext(ctx, "workflow role: owner lacks declared permission, dropping it from the run role",
				"workflow_id", req.WorkflowID, "service", p.Service, "action", p.Action,
				"resource", p.Resource, "error", err)
			continue
		}
		name := p.Name
		if name == "" {
			name = p.Service + "." + p.Action
		}
		perm := Permissions{
			PermissionsID: uuid.New().String(),
			Name:          "workflow:" + req.WorkflowID + ":" + name,
			Service:       p.Service,
			Actions:       []string{p.Action},
			Resources:     []string{scopeResource(p.Resource, ownerUsername, ownerOrgName, p.Service)},
			OwnerID:       req.UserID,
			OrgID:         ownerOrgID,
			Active:        true,
		}
		if err := perm.Add(ctx); err != nil {
			slog.ErrorContext(ctx, "workflow role: create permission", "workflow_id", req.WorkflowID, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		permIDs = append(permIDs, perm.PermissionsID)
	}

	role := Role{
		RoleID:         uuid.New().String(),
		Name:           "workflow:" + req.WorkflowID,
		OwnerID:        req.UserID,
		OrgID:          ownerOrgID,
		PermissionsIDs: permIDs,
		Active:         true,
	}
	if role.PermissionsIDs == nil {
		role.PermissionsIDs = []string{}
	}
	if err := role.Add(ctx); err != nil {
		slog.ErrorContext(ctx, "workflow role: create role", "workflow_id", req.WorkflowID, "error", err)
		for _, pid := range permIDs {
			(Permissions{PermissionsID: pid}).Remove(ctx) //nolint:errcheck
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.InfoContext(ctx, "workflow role created", "workflow_id", req.WorkflowID, "role_id", role.RoleID, "permissions", len(permIDs))
	writeAudit(ctx, svc.ServiceName, "service", "workflow_role.create", role.RoleID, req.WorkflowID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"role_id": role.RoleID}) //nolint:errcheck
}

// handleDeleteWorkflowRole removes the workflow's service role and all its
// associated permissions. Only the "workflows" service account may call this.
func handleDeleteWorkflowRole(w http.ResponseWriter, r *http.Request) {
	svc, ok := requireServiceAuth(w, r)
	if !ok {
		return
	}
	if !canMintScopedRoles(svc.ServiceName) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	roleID := r.PathValue("role_id")
	ctx := r.Context()

	roleRow, err := (Role{RoleID: roleID}).Get(ctx)
	if err != nil {
		w.WriteHeader(http.StatusNoContent) // already gone — idempotent
		return
	}
	role := roleRow.(Role)
	if !strings.HasPrefix(role.Name, "workflow:") {
		slog.WarnContext(ctx, "workflow role: delete rejected — role not workflow-provisioned", "role_id", roleID, "role_name", role.Name)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	for _, pid := range role.PermissionsIDs {
		(Permissions{PermissionsID: pid}).Remove(ctx) //nolint:errcheck
	}
	if err := role.Remove(ctx); err != nil {
		slog.ErrorContext(ctx, "workflow role: delete role", "role_id", roleID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.InfoContext(ctx, "workflow role deleted", "role_id", roleID)
	writeAudit(ctx, svc.ServiceName, "service", "workflow_role.delete", roleID, "")
	w.WriteHeader(http.StatusNoContent)
}

func handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleDeleteSession")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("user.id", callerID),
		attribute.String("session.id", id),
	)
	slog.InfoContext(ctx, "delete session request", "caller_id", callerID, "session_id", id)

	if !requirePermission(w, r, "deleteSession", "gatekeeper/sessions/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Session{SessionID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "session not found")
		slog.WarnContext(ctx, "delete session: not found", "caller_id", callerID, "session_id", id)
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s := row.(Session)
	if s.UserID != callerID {
		span.SetStatus(codes.Ok, "")
		slog.WarnContext(ctx, "delete session: cross-user attempt", "caller_id", callerID, "session_id", id, "session_user_id", s.UserID)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("session.id", id)))

	if err := s.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.ErrorContext(ctx, "delete session: db error", "caller_id", callerID, "session_id", id, "error", err)
		http.Error(w, "failed to delete session", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.soft_delete", trace.WithAttributes(attribute.String("session.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "delete session: success", "caller_id", callerID, "session_id", id)
	w.WriteHeader(http.StatusNoContent)
}
