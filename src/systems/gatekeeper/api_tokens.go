package main

import (
	"context"
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
)

// Scoped tokens: a credential a user mints for themselves that can do LESS than they
// can.
//
// Until now the only credential a person had was a full session token, and the CLI, a
// pipeline and a git client all carried the same one — so a pipeline that files
// tickets held a credential that could delete repositories, and losing it lost the
// whole account. A scoped token is the same session mechanism pointed at a smaller
// permission set.
//
// Nothing here is new machinery. Session.ScopedRoleID already makes permission
// evaluation consider ONLY that role (see evaluatePermissions), which is exactly "a
// token that can do less than its owner", and the namespace-role path already knows
// how to build an attenuated role safely. This assembles the two and gives them a
// lifecycle: name it, expire it, list it, revoke it.
//
// Why no token hash is stored: the JWT is verified against the per-session public key
// already held in Session.PubKey, so gatekeeper never needs the token itself — not
// even hashed. Revocation deactivates the session, which fails closed at the session
// lookup in authMiddleware, BEFORE any permission is evaluated.

// tokenRoleNamePrefix marks the roles minted for scoped tokens, the way "workflow:"
// marks run-token roles. It keeps them identifiable in the roles table and stops a
// token role being mistaken for something a user assigned to a colleague.
const tokenRoleNamePrefix = "token:"

// Token lifetime bounds. A token with no expiry is a permanent credential in a
// wallet nobody audits, so one is always set; a year is the ceiling because beyond
// that the expiry has stopped being a control.
const (
	defaultTokenLifetime = 90 * 24 * time.Hour
	maxTokenLifetime     = 365 * 24 * time.Hour
)

type createTokenRequest struct {
	Name string `json:"name"`
	// ExpiresInDays is the friendlier half of the pair; ExpiresAt wins when both are
	// given, since it is the more explicit statement.
	ExpiresInDays int              `json:"expires_in_days"`
	ExpiresAt     *time.Time       `json:"expires_at"`
	Permissions   []permissionSpec `json:"permissions"`
}

// tokenView is what the API returns for an existing token: everything except the
// credential, which is unrecoverable by design.
type tokenView struct {
	TokenID    string     `json:"token_id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	Expired    bool       `json:"expired"`
}

func viewOf(t PersonalToken) tokenView {
	return tokenView{
		TokenID:    t.TokenID,
		Name:       t.Name,
		CreatedAt:  t.CreatedAt,
		ExpiresAt:  t.ExpiresAt,
		LastUsedAt: t.LastUsedAt,
		Expired:    time.Now().After(t.ExpiresAt),
	}
}

// resolveTokenExpiry turns the request's expiry into an absolute time, clamped to the
// maximum. An expiry in the past is refused rather than clamped: it is a mistake, and
// silently issuing a dead token would be a confusing way to say so.
func resolveTokenExpiry(req createTokenRequest, now time.Time) (time.Time, string) {
	switch {
	case req.ExpiresAt != nil:
		exp := req.ExpiresAt.UTC().Truncate(time.Second)
		if !exp.After(now) {
			return time.Time{}, "expires_at must be in the future"
		}
		if exp.After(now.Add(maxTokenLifetime)) {
			return time.Time{}, "expires_at is more than a year away"
		}
		return exp, ""
	case req.ExpiresInDays > 0:
		d := time.Duration(req.ExpiresInDays) * 24 * time.Hour
		if d > maxTokenLifetime {
			return time.Time{}, "expires_in_days is more than a year"
		}
		return now.Add(d).UTC().Truncate(time.Second), ""
	case req.ExpiresInDays < 0:
		return time.Time{}, "expires_in_days must be positive"
	default:
		return now.Add(defaultTokenLifetime).UTC().Truncate(time.Second), ""
	}
}

// handleCreateToken mints a scoped token for the calling user. The permission set is
// confined to the caller's own namespace and attenuated against what they hold — the
// same two rules namespace roles enforce, via the same helper, because a token is
// just a role with a credential attached.
func handleCreateToken(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCreateToken")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))

	username, orgNS := callerNamespaces(r, callerID)
	if username == "" {
		http.Error(w, "unknown caller", http.StatusUnauthorized)
		return
	}
	if !requirePermission(w, r, "createToken", username+"/gatekeeper/tokens") {
		return
	}

	var req createTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Permissions) == 0 {
		http.Error(w, "name and at least one permission are required", http.StatusBadRequest)
		return
	}
	expiresAt, problem := resolveTokenExpiry(req, time.Now())
	if problem != "" {
		http.Error(w, problem, http.StatusBadRequest)
		return
	}

	// Note what is deliberately NOT done here: defaulting an omitted resource. A
	// service's resources are per-collection ("alice/tickets/tickets",
	// "alice/codearmory_git_factory/repos"), so any guess wide enough to be useful —
	// "alice/tickets/*" — is wider than the grants a user actually holds, and would be
	// refused by attenuation anyway. Better to ask for the resource than to invent one
	// that always fails.
	// A minting caller may only pass on what they already hold, and only over their
	// own namespace. Failures are rejected, never quietly dropped: a token that
	// silently came back with less than was asked for fails later, somewhere else,
	// as a permission error nobody can trace back to here.
	permIDs, attErr := attenuatedPermissions(ctx, callerID, username, orgNS, tokenRoleNamePrefix+username+":"+req.Name, req.Permissions)
	if attErr != nil {
		http.Error(w, attErr.message, attErr.status)
		return
	}

	role := Role{
		RoleID:         uuid.New().String(),
		Name:           tokenRoleNamePrefix + username + ":" + req.Name,
		PermissionsIDs: permIDs,
		OwnerID:        callerID,
		Active:         true,
	}
	if err := role.Add(ctx); err != nil {
		slog.ErrorContext(ctx, "token: create role", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	tokenString, sessionID, err := mintScopedSession(ctx, callerID, role.RoleID, expiresAt)
	if err != nil {
		slog.ErrorContext(ctx, "token: mint session", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	record := PersonalToken{
		TokenID:   uuid.New().String(),
		UserID:    callerID,
		Name:      req.Name,
		SessionID: sessionID,
		RoleID:    role.RoleID,
		ExpiresAt: expiresAt,
		Active:    true,
	}
	if err := record.Add(ctx); err != nil {
		slog.ErrorContext(ctx, "token: persist record", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.InfoContext(ctx, "scoped token created", "user_id", callerID, "token_id", record.TokenID, "permissions", len(permIDs))
	writeAudit(ctx, callerID, "user", "token.create", record.TokenID, req.Name)
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	// The token appears in this response and nowhere else, ever — gatekeeper keeps
	// only the session's public key, so it cannot show it again even if asked.
	json.NewEncoder(w).Encode(struct { //nolint:errcheck
		tokenView
		Token string `json:"token"`
	}{viewOf(record), tokenString})
}

// handleListTokens returns the caller's tokens, newest first. Never the credentials.
func handleListTokens(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleListTokens")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	username, _ := callerNamespaces(r, callerID)
	if username == "" {
		http.Error(w, "unknown caller", http.StatusUnauthorized)
		return
	}
	if !requirePermission(w, r, "listToken", username+"/gatekeeper/tokens") {
		return
	}

	rows, err := listTokensForUser(ctx, callerID)
	if err != nil {
		slog.ErrorContext(ctx, "token: list", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	views := make([]tokenView, 0, len(rows))
	for _, t := range rows {
		views = append(views, viewOf(t))
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(views) //nolint:errcheck
}

// handleRevokeToken kills a token now, not at expiry: the session is deactivated, so
// the next request carrying it fails at the session lookup in authMiddleware, before
// any permission is evaluated. The role goes too, so a revoked token leaves nothing
// behind that could be reattached to a new session.
func handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleRevokeToken")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	username, _ := callerNamespaces(r, callerID)
	if username == "" {
		http.Error(w, "unknown caller", http.StatusUnauthorized)
		return
	}
	if !requirePermission(w, r, "deleteToken", username+"/gatekeeper/tokens") {
		return
	}

	tokenID := r.PathValue("id")
	record, err := getTokenForUser(ctx, callerID, tokenID)
	if err != nil {
		// Owner-scoped lookup: someone else's token id reads as not-found, never as
		// forbidden, so token ids cannot be probed for existence.
		http.Error(w, "token not found", http.StatusNotFound)
		return
	}

	if err := (Session{SessionID: record.SessionID}).Remove(ctx); err != nil {
		slog.ErrorContext(ctx, "token: revoke session", "token_id", tokenID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// The role is deactivated after the session, so a failure here leaves a dead
	// credential and a live role — harmless — rather than a live credential.
	if err := deactivateRole(ctx, record.RoleID); err != nil {
		slog.WarnContext(ctx, "token: revoke role", "token_id", tokenID, "error", err)
	}
	if err := deactivateToken(ctx, record.TokenID); err != nil {
		slog.ErrorContext(ctx, "token: deactivate record", "token_id", tokenID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.InfoContext(ctx, "scoped token revoked", "user_id", callerID, "token_id", tokenID)
	writeAudit(ctx, callerID, "user", "token.revoke", tokenID, record.Name)
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

// mintScopedSession issues a JWT bound to a fresh session whose permissions are
// exactly roleID's. Extracted from the run-token path so both mint sessions the same
// way — the only differences are who may ask and how long it lives.
func mintScopedSession(ctx context.Context, userID, roleID string, expiresAt time.Time) (token, sessionID string, err error) {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	sessionID = uuid.New().String()
	token, err = jwt.NewWithClaims(jwt.SigningMethodES256, authClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "gatekeeper",
			Subject:   userID,
			ID:        sessionID,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}).SignedString(privKey)
	if err != nil {
		return "", "", err
	}
	pubKeyBytes, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		return "", "", err
	}
	scoped := roleID
	session := Session{
		SessionID:    sessionID,
		UserID:       userID,
		ExpiresAt:    expiresAt,
		PubKey:       string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubKeyBytes})),
		ScopedRoleID: &scoped,
	}
	if err := session.Add(ctx); err != nil {
		return "", "", err
	}
	return token, sessionID, nil
}
