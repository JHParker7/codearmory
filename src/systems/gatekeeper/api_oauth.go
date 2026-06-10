package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

var totpFormTmpl = template.Must(template.New("totp").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Two-factor authentication — Codearmory</title>
  <style>
    body{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f5f5f5}
    .card{background:#fff;padding:2rem;border-radius:8px;box-shadow:0 2px 8px rgba(0,0,0,.12);width:100%;max-width:360px}
    h2{margin:0 0 1.5rem;font-size:1.25rem}
    label{display:block;margin-bottom:1rem;font-size:.9rem}
    input{display:block;width:100%;margin-top:.25rem;padding:.5rem;border:1px solid #ccc;border-radius:4px;font-size:1rem;box-sizing:border-box}
    button{width:100%;padding:.65rem;background:#0066cc;color:#fff;border:none;border-radius:4px;font-size:1rem;cursor:pointer;margin-top:.5rem}
    button:hover{background:#0052a3}
    .app-name{color:#555;font-size:.85rem;margin-bottom:1.5rem}
    .error{color:#c00;font-size:.85rem;margin-bottom:1rem}
  </style>
</head>
<body>
<div class="card">
  <h2>Two-factor authentication</h2>
  {{if .AppName}}<p class="app-name">Authorizing <strong>{{.AppName}}</strong></p>{{end}}
  {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
  <form method="POST" action="/oauth/mfa">
    <input type="hidden" name="token" value="{{.Token}}">
    <label>Authentication code<input type="text" name="code" inputmode="numeric" pattern="[0-9]{6}" autocomplete="one-time-code" required autofocus></label>
    <button type="submit">Verify</button>
  </form>
</div>
</body>
</html>`))

type totpFormData struct {
	Token   string
	AppName string
	Error   string
}

// ── Signing key ───────────────────────────────────────────────────────────────

var (
	oidcSigningKey *ecdsa.PrivateKey
	oidcKeyID      string // stable fingerprint used as JWKS kid
	oidcIssuer     string
)

func initOIDC() {
	oidcIssuer = secret("OIDC_ISSUER")
	if oidcIssuer == "" {
		oidcIssuer = "http://localhost:8081"
		slog.Warn("OIDC_ISSUER not set — defaulting to localhost; set for production deployments")
	}

	var key *ecdsa.PrivateKey
	if keyPEM := secret("OIDC_SIGNING_KEY"); keyPEM != "" {
		block, _ := pem.Decode([]byte(keyPEM))
		if block == nil {
			slog.Error("OIDC_SIGNING_KEY: failed to decode PEM block")
			return
		}
		var err error
		key, err = x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			slog.Error("OIDC_SIGNING_KEY: failed to parse EC private key", "error", err)
			return
		}
	} else {
		slog.Warn("OIDC_SIGNING_KEY not set — generating ephemeral key (not stable across restarts; set for production)")
		var err error
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			slog.Error("OIDC: ephemeral key generation failed", "error", err)
			return
		}
	}

	pubBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		slog.Error("OIDC: marshal public key failed", "error", err)
		return
	}
	h := sha256.Sum256(pubBytes)
	oidcKeyID = fmt.Sprintf("%x", h[:8])
	oidcSigningKey = key
	slog.Info("OIDC signing key loaded", "kid", oidcKeyID, "issuer", oidcIssuer)
}

// ── ID token claims ───────────────────────────────────────────────────────────

type oidcClaims struct {
	jwt.RegisteredClaims
	Email             string   `json:"email"`
	EmailVerified     bool     `json:"email_verified"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Groups            []string `json:"groups,omitempty"`
}

// ── Discovery and JWKS ────────────────────────────────────────────────────────

func handleOIDCDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"issuer":                                oidcIssuer,
		"authorization_endpoint":                oidcIssuer + "/oauth/authorize",
		"token_endpoint":                        oidcIssuer + "/oauth/token",
		"userinfo_endpoint":                     oidcIssuer + "/oauth/userinfo",
		"jwks_uri":                              oidcIssuer + "/oauth/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"ES256"},
		"scopes_supported":                      []string{"openid", "email", "profile", "groups"},
		"grant_types_supported":                 []string{"authorization_code", "client_credentials"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		"claims_supported":                      []string{"sub", "iss", "aud", "exp", "iat", "email", "name", "preferred_username", "groups"},
	})
}

func handleJWKS(w http.ResponseWriter, r *http.Request) {
	if oidcSigningKey == nil {
		http.Error(w, "OIDC not configured", http.StatusServiceUnavailable)
		return
	}
	pub := &oidcSigningKey.PublicKey
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"keys": []any{map[string]any{
			"kty": "EC",
			"use": "sig",
			"crv": "P-256",
			"kid": oidcKeyID,
			"alg": "ES256",
			"x":   base64.RawURLEncoding.EncodeToString(zeroPad(pub.X.Bytes(), 32)),
			"y":   base64.RawURLEncoding.EncodeToString(zeroPad(pub.Y.Bytes(), 32)),
		}},
	})
}

func zeroPad(b []byte, size int) []byte {
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}

// ── Authorization endpoint ────────────────────────────────────────────────────

var loginFormTmpl = template.Must(template.New("login").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Sign in — Codearmory</title>
  <style>
    body{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f5f5f5}
    .card{background:#fff;padding:2rem;border-radius:8px;box-shadow:0 2px 8px rgba(0,0,0,.12);width:100%;max-width:360px}
    h2{margin:0 0 1.5rem;font-size:1.25rem}
    label{display:block;margin-bottom:1rem;font-size:.9rem}
    input{display:block;width:100%;margin-top:.25rem;padding:.5rem;border:1px solid #ccc;border-radius:4px;font-size:1rem;box-sizing:border-box}
    button{width:100%;padding:.65rem;background:#0066cc;color:#fff;border:none;border-radius:4px;font-size:1rem;cursor:pointer;margin-top:.5rem}
    button:hover{background:#0052a3}
    .app-name{color:#555;font-size:.85rem;margin-bottom:1.5rem}
    .error{color:#c00;font-size:.85rem;margin-bottom:1rem}
  </style>
</head>
<body>
<div class="card">
  <h2>Sign in to Codearmory</h2>
  {{if .AppName}}<p class="app-name">Authorizing <strong>{{.AppName}}</strong></p>{{end}}
  {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
  <form method="POST" action="/oauth/authorize">
    <input type="hidden" name="client_id"     value="{{.ClientID}}">
    <input type="hidden" name="redirect_uri"  value="{{.RedirectURI}}">
    <input type="hidden" name="state"         value="{{.State}}">
    <input type="hidden" name="scope"         value="{{.Scope}}">
    <input type="hidden" name="response_type" value="{{.ResponseType}}">
    <label>Email<input type="email" name="email" required autocomplete="username"></label>
    <label>Password<input type="password" name="password" required autocomplete="current-password"></label>
    <button type="submit">Sign in</button>
  </form>
</div>
</body>
</html>`))

type loginFormData struct {
	ClientID     string
	RedirectURI  string
	State        string
	Scope        string
	ResponseType string
	AppName      string
	Error        string
}

// oauthParams holds validated OAuth2 request parameters.
type oauthParams struct {
	client       OAuthClient
	redirectURI  string
	state        string
	scope        string
	responseType string
}

// parseOAuthParams extracts and validates OAuth2 parameters from the request.
// Errors before redirect_uri is validated are returned as plain HTTP errors;
// errors after that redirect with an error parameter per RFC 6749.
func parseOAuthParams(w http.ResponseWriter, r *http.Request, fromForm bool) (oauthParams, bool) {
	get := r.URL.Query().Get
	if fromForm {
		get = r.FormValue
	}

	clientID := get("client_id")
	if clientID == "" {
		http.Error(w, "client_id is required", http.StatusBadRequest)
		return oauthParams{}, false
	}

	client, err := getOAuthClientByClientID(r.Context(), clientID)
	if err != nil {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return oauthParams{}, false
	}

	redirectURI := get("redirect_uri")
	if redirectURI == "" {
		if len(client.RedirectURIs) == 1 {
			redirectURI = client.RedirectURIs[0]
		} else {
			http.Error(w, "redirect_uri is required", http.StatusBadRequest)
			return oauthParams{}, false
		}
	}
	if !slices.Contains(client.RedirectURIs, redirectURI) {
		http.Error(w, "redirect_uri not registered for this client", http.StatusBadRequest)
		return oauthParams{}, false
	}

	state := get("state")
	responseType := get("response_type")
	if responseType != "code" {
		oauthRedirectError(w, r, redirectURI, state, "unsupported_response_type", "only 'code' is supported")
		return oauthParams{}, false
	}

	return oauthParams{
		client:       client,
		redirectURI:  redirectURI,
		state:        state,
		scope:        get("scope"),
		responseType: responseType,
	}, true
}

// handleAuthorize displays the login form for the OAuth2 authorization_code flow.
func handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if oidcSigningKey == nil {
		http.Error(w, "OIDC not configured", http.StatusServiceUnavailable)
		return
	}
	params, ok := parseOAuthParams(w, r, false)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	loginFormTmpl.Execute(w, loginFormData{ //nolint:errcheck
		ClientID:     params.client.ClientID,
		RedirectURI:  params.redirectURI,
		State:        params.state,
		Scope:        params.scope,
		ResponseType: params.responseType,
		AppName:      params.client.Name,
	})
}

// handleAuthorizeSubmit processes the login form, authenticates the user, and
// redirects back to the client with an authorization code.
func handleAuthorizeSubmit(w http.ResponseWriter, r *http.Request) {
	if oidcSigningKey == nil {
		http.Error(w, "OIDC not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	params, ok := parseOAuthParams(w, r, true)
	if !ok {
		return
	}

	renderError := func(msg string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		loginFormTmpl.Execute(w, loginFormData{ //nolint:errcheck
			ClientID:     params.client.ClientID,
			RedirectURI:  params.redirectURI,
			State:        params.state,
			Scope:        params.scope,
			ResponseType: params.responseType,
			AppName:      params.client.Name,
			Error:        msg,
		})
	}

	email := r.FormValue("email")
	password := r.FormValue("password")

	// Constant-time dummy hash prevents user-enumeration via timing differences.
	const dummyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	user, err := getUserByEmail(r.Context(), email)
	if err != nil {
		bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(password)) //nolint:errcheck
		slog.Warn("oauth authorize: user not found", "email", email)
		renderError("Invalid email or password.")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.HashedPassword), []byte(password)); err != nil {
		slog.Warn("oauth authorize: bad password", "user_id", user.UserID)
		renderError("Invalid email or password.")
		return
	}

	if userHasTOTP(r.Context(), user.UserID) {
		pending, err := newMFAPending(r.Context(), user.UserID,
			params.client.ClientID, params.redirectURI, params.state, params.scope)
		if err != nil {
			slog.Error("oauth authorize: failed to create MFA pending", "user_id", user.UserID, "error", err)
			renderError("Internal server error.")
			return
		}
		slog.Info("oauth authorize: MFA required", "user_id", user.UserID)
		http.Redirect(w, r, "/oauth/mfa?token="+url.QueryEscape(pending.Token), http.StatusFound)
		return
	}

	code := OAuthCode{
		Code:        uuid.New().String(),
		ClientID:    params.client.ClientID,
		UserID:      user.UserID,
		RedirectURI: params.redirectURI,
		Scopes:      strings.Fields(params.scope),
		ExpiresAt:   time.Now().Add(10 * time.Minute).UTC(),
		CreatedAt:   time.Now().UTC(),
	}
	if err := code.Add(r.Context()); err != nil {
		slog.Error("oauth authorize: persist code failed", "error", err)
		oauthRedirectError(w, r, params.redirectURI, params.state, "server_error", "failed to create authorization code")
		return
	}

	slog.Info("oauth: authorization code issued", "client_id", params.client.ClientID, "user_id", user.UserID)
	oauthRedirectCode(w, r, params.redirectURI, code.Code, params.state)
}

// ── Token endpoint ────────────────────────────────────────────────────────────

func handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request", "cannot parse request body")
		return
	}
	switch r.FormValue("grant_type") {
	case "client_credentials":
		handleClientCredentialsGrant(w, r)
		return
	case "authorization_code":
		// handled below
	default:
		tokenError(w, "unsupported_grant_type", "supported: authorization_code, client_credentials")
		return
	}

	if oidcSigningKey == nil {
		tokenError(w, "server_error", "OIDC not configured")
		return
	}

	clientID, clientSecret, ok := extractClientCredentials(r)
	if !ok {
		tokenError(w, "invalid_client", "client credentials missing")
		return
	}

	client, err := getOAuthClientByClientID(r.Context(), clientID)
	if err != nil {
		tokenError(w, "invalid_client", "client not found")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(client.SecretHash), []byte(clientSecret)); err != nil {
		tokenError(w, "invalid_client", "invalid client secret")
		return
	}

	authCode, err := getOAuthCode(r.Context(), r.FormValue("code"), clientID)
	if err != nil {
		tokenError(w, "invalid_grant", "authorization code not found or already used")
		return
	}
	if time.Now().After(authCode.ExpiresAt) {
		tokenError(w, "invalid_grant", "authorization code expired")
		return
	}
	if authCode.RedirectURI != r.FormValue("redirect_uri") {
		tokenError(w, "invalid_grant", "redirect_uri mismatch")
		return
	}

	// Atomically mark the code as used. RowsAffected == 0 means a concurrent
	// request already redeemed it, preventing double-issuance of sessions.
	n, redeemErr := redeemOAuthCode(r.Context(), authCode.Code)
	if redeemErr != nil || n == 0 {
		tokenError(w, "invalid_grant", "authorization code already used")
		return
	}

	userRow, err := (User{UserID: authCode.UserID}).Get(r.Context())
	if err != nil {
		tokenError(w, "server_error", "user not found")
		return
	}
	user := userRow.(User)

	accessToken, sessionID, expiresAt, err := createOAuthSession(r.Context(), user.UserID)
	if err != nil {
		slog.Error("oauth token: session creation failed", "user_id", user.UserID, "error", err)
		tokenError(w, "server_error", "failed to create session")
		return
	}

	idToken, err := mintIDToken(user, sessionID, client.ClientID, expiresAt, buildGroups(r.Context(), user))
	if err != nil {
		slog.Error("oauth token: id_token signing failed", "user_id", user.UserID, "error", err)
		tokenError(w, "server_error", "failed to sign id_token")
		return
	}

	slog.Info("oauth: tokens issued", "client_id", clientID, "user_id", user.UserID, "session_id", sessionID)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   int(time.Until(expiresAt).Seconds()),
		"id_token":     idToken,
		"scope":        strings.Join(authCode.Scopes, " "),
	})
}

// handleClientCredentialsGrant exchanges a valid client_id + client_secret for a
// Bearer token. The token's permissions are determined by the client's assigned role.
func handleClientCredentialsGrant(w http.ResponseWriter, r *http.Request) {
	clientID, clientSecret, ok := extractClientCredentials(r)
	if !ok {
		tokenError(w, "invalid_client", "client credentials missing")
		return
	}
	client, err := getOAuthClientByClientID(r.Context(), clientID)
	if err != nil {
		tokenError(w, "invalid_client", "client not found")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(client.SecretHash), []byte(clientSecret)); err != nil {
		tokenError(w, "invalid_client", "invalid client secret")
		return
	}

	accessToken, sessionID, expiresAt, err := issueClientSession(r.Context(), clientID)
	if err != nil {
		slog.Error("client_credentials: session creation failed", "client_id", clientID, "error", err)
		tokenError(w, "server_error", "failed to create session")
		return
	}

	slog.Info("oauth: client_credentials token issued", "client_id", clientID, "session_id", sessionID)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   int(time.Until(expiresAt).Seconds()),
	})
}

// issueClientSession creates a session for an OAuth client (not a user) and returns
// the signed JWT, sessionID, and expiry. The JWT sub is set to the client_id.
func issueClientSession(ctx context.Context, clientID string) (token, sessionID string, expiresAt time.Time, err error) {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", time.Time{}, err
	}
	sessionID = uuid.New().String()
	const maxTTLHours = 720
	ttlHours := 24
	if v := os.Getenv("SESSION_TTL_HOURS"); v != "" {
		if n, err2 := strconv.Atoi(v); err2 == nil && n > 0 {
			if n > maxTTLHours {
				n = maxTTLHours
			}
			ttlHours = n
		}
	}
	expiresAt = time.Now().Add(time.Duration(ttlHours) * time.Hour).UTC().Truncate(time.Second)

	var tokenString string
	tokenString, err = jwt.NewWithClaims(jwt.SigningMethodES256, authClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "gatekeeper",
			Subject:   clientID,
			ID:        sessionID,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}).SignedString(privKey)
	if err != nil {
		return "", "", time.Time{}, err
	}

	pubKeyBytes, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		return "", "", time.Time{}, err
	}
	session := Session{
		SessionID: sessionID,
		UserID:    "",
		ClientID:  &clientID,
		ExpiresAt: expiresAt,
		PubKey:    string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubKeyBytes})),
	}
	if err := session.Add(ctx); err != nil {
		return "", "", time.Time{}, err
	}
	return tokenString, sessionID, expiresAt, nil
}

// ── Userinfo endpoint ─────────────────────────────────────────────────────────

// handleUserinfo returns standard OIDC claims for the authenticated user.
// The access_token is a standard gatekeeper JWT verified by authMiddleware.
func handleUserinfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, _ := ctx.Value(userIDKey).(string)

	userRow, err := (User{UserID: userID}).Get(ctx)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	user := userRow.(User)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"sub":                user.UserID,
		"email":              user.Email,
		"email_verified":     true,
		"name":               strings.TrimSpace(user.Firstname + " " + user.Lastname),
		"preferred_username": user.Username,
		"groups":             buildGroups(ctx, user),
	})
}

// ── OAuth client management (internal, service-auth) ─────────────────────────

func handleCreateOAuthClient(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireServiceAuth(w, r); !ok {
		return
	}

	var req struct {
		Name         string   `json:"name"`
		RedirectURIs []string `json:"redirect_uris"`
		OrgID        string   `json:"org_id"`
		RoleID       string   `json:"role_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.RoleID != "" {
		roleRow, err := (Role{RoleID: req.RoleID}).Get(r.Context())
		if err != nil {
			http.Error(w, "role not found", http.StatusBadRequest)
			return
		}
		role := roleRow.(Role)
		// Prevent cross-org privilege escalation: the role must belong to the same
		// org as the client being created.
		if role.OrgID != nil && *role.OrgID != req.OrgID {
			http.Error(w, "role does not belong to the specified org", http.StatusForbidden)
			return
		}
	}

	rawSecret := make([]byte, 32)
	if _, err := rand.Read(rawSecret); err != nil {
		slog.Error("oauth client: secret generation failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	clientSecret := base64.RawURLEncoding.EncodeToString(rawSecret)
	secretHash, err := bcrypt.GenerateFromPassword([]byte(clientSecret), 12)
	if err != nil {
		slog.Error("oauth client: bcrypt failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	client := OAuthClient{
		ClientID:     uuid.New().String(),
		Name:         req.Name,
		SecretHash:   string(secretHash),
		RedirectURIs: req.RedirectURIs,
		OrgID:        req.OrgID,
		Active:       true,
		CreatedAt:    time.Now().UTC(),
	}
	if req.RoleID != "" {
		client.RoleID = &req.RoleID
	}
	if err := client.Add(r.Context()); err != nil {
		slog.Error("oauth client: create failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("oauth client registered", "client_id", client.ClientID, "name", client.Name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"client_id":     client.ClientID,
		"client_secret": clientSecret, // returned once only
		"name":          client.Name,
		"redirect_uris": client.RedirectURIs,
		"org_id":        client.OrgID,
	})
}

func handleListOAuthClients(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireServiceAuth(w, r); !ok {
		return
	}
	clients, err := listOAuthClients(r.Context())
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(clients) //nolint:errcheck
}

func handleDeleteOAuthClient(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireServiceAuth(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	n, err := deactivateOAuthClient(r.Context(), id)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "client not found", http.StatusNotFound)
		return
	}
	slog.Info("oauth client deleted", "client_id", id)
	w.WriteHeader(http.StatusNoContent)
}

// ── OAuth MFA handlers ────────────────────────────────────────────────────────

// handleOAuthMFAGet renders the TOTP input form during an OAuth authorization flow.
func handleOAuthMFAGet(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}

	var pending MFAPending
	if err := connectRead().WithContext(r.Context()).
		Where("token = ? AND used = ?", token, false).
		First(&pending).Error; err != nil || time.Now().After(pending.ExpiresAt) {
		http.Error(w, "invalid or expired MFA token", http.StatusUnauthorized)
		return
	}

	var appName string
	if pending.OAuthClientID != "" {
		var client OAuthClient
		if err := connectRead().WithContext(r.Context()).
			Where("client_id = ?", pending.OAuthClientID).First(&client).Error; err == nil {
			appName = client.Name
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	totpFormTmpl.Execute(w, totpFormData{Token: token, AppName: appName}) //nolint:errcheck
}

// handleOAuthMFAPost verifies the TOTP code submitted via the OAuth MFA form and,
// on success, issues an authorization code and redirects back to the client.
func handleOAuthMFAPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	token := r.FormValue("token")
	code := r.FormValue("code")
	if token == "" || code == "" {
		http.Error(w, "token and code are required", http.StatusBadRequest)
		return
	}

	pending, ok := consumeMFAPending(ctx, w, token)
	if !ok {
		return
	}
	if pending.OAuthClientID == "" {
		http.Error(w, "use POST /mfa/verify for direct login", http.StatusBadRequest)
		return
	}

	var client OAuthClient
	connectRead().WithContext(ctx).Where("client_id = ?", pending.OAuthClientID).First(&client) //nolint:errcheck

	renderTOTPError := func(msg string) {
		// Re-issue a fresh pending token so the user can retry without starting over.
		newPending, err := newMFAPending(ctx, pending.UserID,
			pending.OAuthClientID, pending.OAuthRedirectURI, pending.OAuthState, pending.OAuthScope)
		if err != nil {
			slog.Error("oauth mfa: re-issue pending failed", "user_id", pending.UserID, "error", err)
			oauthRedirectError(w, r, pending.OAuthRedirectURI, pending.OAuthState, "server_error", "internal error")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		totpFormTmpl.Execute(w, totpFormData{ //nolint:errcheck
			Token:   newPending.Token,
			AppName: client.Name,
			Error:   msg,
		})
	}

	valid, _, msg := totpValidateForUser(ctx, pending.UserID, code)
	if !valid {
		slog.Warn("oauth mfa: invalid code", "user_id", pending.UserID)
		renderTOTPError(msg)
		return
	}

	authCode := OAuthCode{
		Code:        uuid.New().String(),
		ClientID:    pending.OAuthClientID,
		UserID:      pending.UserID,
		RedirectURI: pending.OAuthRedirectURI,
		Scopes:      mfaPendingOAuthScopes(pending.OAuthScope),
		ExpiresAt:   time.Now().Add(10 * time.Minute).UTC(),
		CreatedAt:   time.Now().UTC(),
	}
	if err := authCode.Add(ctx); err != nil {
		slog.Error("oauth mfa: persist auth code failed", "error", err)
		oauthRedirectError(w, r, pending.OAuthRedirectURI, pending.OAuthState, "server_error", "failed to create authorization code")
		return
	}

	slog.Info("oauth mfa: authorization code issued", "client_id", pending.OAuthClientID, "user_id", pending.UserID)
	oauthRedirectCode(w, r, pending.OAuthRedirectURI, authCode.Code, pending.OAuthState)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// createOAuthSession creates a gatekeeper session and returns its JWT as the
// OAuth access_token. The token is a standard gatekeeper JWT, so it works
// transparently with authMiddleware and all downstream services.
func createOAuthSession(ctx context.Context, userID string) (token, sessionID string, expiresAt time.Time, err error) {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", time.Time{}, err
	}
	sessionID = uuid.New().String()
	expiresAt = time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	tokenString, err := jwt.NewWithClaims(jwt.SigningMethodES256, authClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "gatekeeper",
			Subject:   userID,
			ID:        sessionID,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}).SignedString(privKey)
	if err != nil {
		return "", "", time.Time{}, err
	}

	pubBytes, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		return "", "", time.Time{}, err
	}
	session := Session{
		SessionID: sessionID,
		UserID:    userID,
		ExpiresAt: expiresAt,
		PubKey:    string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})),
		Active:    true,
	}
	if err := session.Add(ctx); err != nil {
		return "", "", time.Time{}, err
	}
	return tokenString, sessionID, expiresAt, nil
}

// mintIDToken signs an OIDC ID token using the stable oidcSigningKey.
func mintIDToken(user User, sessionID, audience string, expiresAt time.Time, groups []string) (string, error) {
	t := jwt.NewWithClaims(jwt.SigningMethodES256, oidcClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    oidcIssuer,
			Subject:   user.UserID,
			Audience:  jwt.ClaimStrings{audience},
			ID:        sessionID,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Email:             user.Email,
		EmailVerified:     true,
		Name:              strings.TrimSpace(user.Firstname + " " + user.Lastname),
		PreferredUsername: user.Username,
		Groups:            groups,
	})
	t.Header["kid"] = oidcKeyID
	return t.SignedString(oidcSigningKey)
}

// buildGroups returns OIDC group claims in "org-name" and "org-name/team-name"
// format. Forgejo maps these to org/team membership via its OIDC team sync.
func buildGroups(ctx context.Context, user User) []string {
	if user.OrgID == nil {
		return []string{}
	}
	orgRow, err := (Org{OrgID: *user.OrgID}).Get(ctx)
	if err != nil {
		return []string{}
	}
	org := orgRow.(Org)
	groups := []string{org.OrgName}
	if user.TeamID != nil {
		if teamRow, err := (Team{TeamID: *user.TeamID}).Get(ctx); err == nil {
			groups = append(groups, org.OrgName+"/"+teamRow.(Team).TeamName)
		}
	}
	return groups
}

// extractClientCredentials reads the client_id and client_secret from either
// HTTP Basic auth (client_secret_basic) or form fields (client_secret_post).
func extractClientCredentials(r *http.Request) (clientID, secret string, ok bool) {
	if id, sec, hasBasic := r.BasicAuth(); hasBasic {
		return id, sec, true
	}
	id, sec := r.FormValue("client_id"), r.FormValue("client_secret")
	if id != "" && sec != "" {
		return id, sec, true
	}
	return "", "", false
}

func oauthRedirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, errCode, description string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("error", errCode)
	if description != "" {
		q.Set("error_description", description)
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func oauthRedirectCode(w http.ResponseWriter, r *http.Request, redirectURI, code, state string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func tokenError(w http.ResponseWriter, errCode, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"error":             errCode,
		"error_description": description,
	})
}
