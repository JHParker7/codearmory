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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/crypto/bcrypt"
)

// handleTOTPEnroll generates a new TOTP secret for the authenticated user and
// stores it as unconfirmed. The caller must follow up with POST /mfa/totp/confirm
// to activate it. Re-enrolling while TOTP is confirmed requires disabling first.
func handleTOTPEnroll(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleTOTPEnroll")
	defer span.End()
	r = r.WithContext(ctx)

	userID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", userID))

	var confirmed TOTPCredential
	if connectRead().WithContext(ctx).
		Where("user_id = ? AND confirmed = ? AND active = ?", userID, true, true).
		First(&confirmed).Error == nil {
		http.Error(w, "TOTP already enabled; disable it before re-enrolling", http.StatusConflict)
		return
	}

	userRow, err := (User{UserID: userID}).Get(ctx)
	if err != nil {
		http.Error(w, "user not found", http.StatusInternalServerError)
		return
	}
	user := userRow.(User)

	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "Codearmory",
		AccountName: user.Email,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "key generation failed")
		slog.ErrorContext(ctx, "totp enroll: key generation failed", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	encSecret, err := encryptSecret(key.Secret())
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "secret encryption failed")
		slog.ErrorContext(ctx, "totp enroll: secret encryption failed", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Remove any previous unconfirmed enrollment before inserting a fresh one.
	deleteUnconfirmedTOTP(ctx, userID) //nolint:errcheck

	cred := TOTPCredential{
		CredentialID: uuid.New().String(),
		UserID:       userID,
		EncSecret:    encSecret,
		Confirmed:    false,
		Active:       true,
	}
	if err := cred.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "totp enroll: db insert failed", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "totp enroll: secret generated", "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"uri":    key.URL(),
		"secret": key.Secret(),
	})
}

// handleTOTPConfirm activates a pending TOTP enrollment by verifying the first code.
func handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleTOTPConfirm")
	defer span.End()
	r = r.WithContext(ctx)

	userID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", userID))

	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}

	var cred TOTPCredential
	if err := connectRead().WithContext(ctx).
		Where("user_id = ? AND confirmed = ? AND active = ?", userID, false, true).
		First(&cred).Error; err != nil {
		http.Error(w, "no pending TOTP enrollment found", http.StatusNotFound)
		return
	}

	// Validate the code without recording replay state — enrollment confirmation
	// is not an authentication event, and consuming the current time step here
	// would block an immediate MFA login with the same code.
	enrollSecret, err := decryptSecret(cred.EncSecret)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "decrypt failed")
		slog.ErrorContext(ctx, "totp confirm: decrypt failed", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !totp.Validate(req.Code, enrollSecret) {
		span.SetStatus(codes.Error, "invalid TOTP code")
		http.Error(w, "invalid TOTP code", http.StatusUnauthorized)
		return
	}

	cred.Confirmed = true
	if err := cred.Update(ctx); err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "totp confirm: db update failed", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "totp confirm: TOTP enabled", "user_id", userID)
	writeAudit(ctx, userID, "user", "mfa.totp.enable", userID, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleTOTPStatus returns whether the authenticated user has confirmed TOTP enabled.
func handleTOTPStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, _ := ctx.Value(userIDKey).(string)

	var cred TOTPCredential
	enabled := connectRead().WithContext(ctx).
		Where("user_id = ? AND confirmed = ? AND active = ?", userID, true, true).
		First(&cred).Error == nil

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"enabled": enabled}) //nolint:errcheck
}

// handleTOTPDisable removes TOTP for the authenticated user. The user's current
// password is required to prevent accidental or unauthorized disable.
func handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleTOTPDisable")
	defer span.End()
	r = r.WithContext(ctx)

	userID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", userID))

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		http.Error(w, "password is required", http.StatusBadRequest)
		return
	}

	userRow, err := (User{UserID: userID}).Get(ctx)
	if err != nil {
		http.Error(w, "user not found", http.StatusInternalServerError)
		return
	}
	user := userRow.(User)

	if err := bcrypt.CompareHashAndPassword([]byte(user.HashedPassword), []byte(req.Password)); err != nil {
		span.SetStatus(codes.Error, "password mismatch")
		slog.WarnContext(ctx, "totp disable: bad password", "user_id", userID)
		http.Error(w, "invalid password", http.StatusUnauthorized)
		return
	}

	n, dbErr := deactivateTOTP(ctx, userID)
	if dbErr != nil {
		span.RecordError(dbErr)
		span.SetStatus(codes.Error, dbErr.Error())
		slog.ErrorContext(ctx, "totp disable: db error", "user_id", userID, "error", dbErr)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "TOTP not enabled", http.StatusNotFound)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "totp disable: TOTP disabled", "user_id", userID)
	writeAudit(ctx, userID, "user", "mfa.totp.disable", userID, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleMFAVerify completes a pending MFA challenge for the direct API login flow.
// It accepts the mfa_token from POST /login and a TOTP code; on success it issues
// a full session and returns the JWT.
func handleMFAVerify(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleMFAVerify")
	defer span.End()
	r = r.WithContext(ctx)

	var req struct {
		MFAToken string `json:"mfa_token"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MFAToken == "" || req.Code == "" {
		http.Error(w, "mfa_token and code are required", http.StatusBadRequest)
		return
	}

	pending, ok := consumeMFAPending(ctx, w, req.MFAToken)
	if !ok {
		span.SetStatus(codes.Error, "invalid mfa token")
		return
	}
	if pending.OAuthClientID != "" {
		http.Error(w, "use POST /oauth/mfa for OAuth flows", http.StatusBadRequest)
		return
	}

	valid, status, msg := totpValidateForUser(ctx, pending.UserID, req.Code)
	if !valid {
		span.SetStatus(codes.Error, msg)
		meterLogins.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "mfa_failure")))
		slog.WarnContext(ctx, "mfa verify: invalid code", "user_id", pending.UserID)
		http.Error(w, msg, status)
		return
	}

	tokenString, sessionID, expiresAt, err := issueSession(ctx, pending.UserID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "session creation failed")
		slog.ErrorContext(ctx, "mfa verify: session creation failed", "user_id", pending.UserID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	meterLogins.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "mfa_success")))
	slog.InfoContext(ctx, "mfa verify: login complete", "user_id", pending.UserID, "session_id", sessionID)

	if userRow, err := (User{UserID: pending.UserID}).Get(ctx); err == nil {
		writeAudit(ctx, pending.UserID, "user", "session.create", sessionID, userRow.(User).Username)
	}

	setSessionCookie(w, tokenString, expiresAt)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": tokenString}) //nolint:errcheck
}

// ── Shared helpers ────────────────────────────────────────────────────────────

// issueSession creates a new gatekeeper session for userID and returns the signed
// JWT, sessionID, and expiry time. It respects SESSION_TTL_HOURS (default 24h, max 720h).
func issueSession(ctx context.Context, userID string) (token, sessionID string, expiresAt time.Time, err error) {
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
			Subject:   userID,
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
		UserID:    userID,
		ExpiresAt: expiresAt,
		PubKey:    string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubKeyBytes})),
	}
	if err := session.Add(ctx); err != nil {
		return "", "", time.Time{}, err
	}
	return tokenString, sessionID, expiresAt, nil
}

// consumeMFAPending atomically marks a pending MFA token as used and returns it.
// Writes an HTTP error and returns false if the token is invalid, expired, or already used.
func consumeMFAPending(ctx context.Context, w http.ResponseWriter, token string) (MFAPending, bool) {
	var pending MFAPending
	if err := connectRead().WithContext(ctx).
		Where("token = ? AND used = ?", token, false).
		First(&pending).Error; err != nil {
		http.Error(w, "invalid or expired MFA token", http.StatusUnauthorized)
		return MFAPending{}, false
	}
	if time.Now().After(pending.ExpiresAt) {
		http.Error(w, "MFA token expired", http.StatusUnauthorized)
		return MFAPending{}, false
	}
	n, redeemErr := redeemMFAPending(ctx, token)
	if redeemErr != nil || n == 0 {
		http.Error(w, "invalid or expired MFA token", http.StatusUnauthorized)
		return MFAPending{}, false
	}
	return pending, true
}

// totpValidateForUser looks up the user's confirmed TOTP credential by userID
// and validates code against it.
func totpValidateForUser(ctx context.Context, userID, code string) (bool, int, string) {
	var cred TOTPCredential
	if err := connectRead().WithContext(ctx).
		Where("user_id = ? AND confirmed = ? AND active = ?", userID, true, true).
		First(&cred).Error; err != nil {
		return false, http.StatusInternalServerError, "TOTP credential not found"
	}
	return totpValidate(ctx, userID, code, &cred)
}

// totpValidate decrypts cred's secret and validates code against it.
// It also enforces per-step replay protection: the same 6-digit code is rejected
// if it was already accepted within the current 30-second time step.
func totpValidate(ctx context.Context, userID, code string, cred *TOTPCredential) (bool, int, string) {
	secret, err := decryptSecret(cred.EncSecret)
	if err != nil {
		slog.ErrorContext(ctx, "totp validate: decrypt failed", "user_id", userID, "error", err)
		return false, http.StatusInternalServerError, "internal server error"
	}
	if !totp.Validate(code, secret) {
		return false, http.StatusUnauthorized, "invalid TOTP code"
	}
	// Reject replay: same code already accepted in this 30-second step.
	if cred.LastUsedCode == code && time.Now().Unix()/30 == cred.LastUsedAt.Unix()/30 {
		return false, http.StatusUnauthorized, "invalid TOTP code"
	}
	// Record the accepted code to block replay within this time step.
	cred.LastUsedCode = code
	cred.LastUsedAt = time.Now().UTC()
	if err := cred.Update(ctx); err != nil {
		slog.ErrorContext(ctx, "totp validate: failed to record used code", "user_id", userID, "error", err)
	}
	return true, http.StatusOK, ""
}

// oauthPending carries the OAuth2 parameters an MFA detour has to preserve, so the
// code minted after verification is identical to the one the non-MFA path would have
// issued. Zero-valued for a direct (non-OAuth) login.
//
// A struct rather than positional arguments: these are six interchangeable strings,
// and the failure mode of getting two of them the wrong way round is a silently
// weakened flow rather than a compile error.
type oauthPending struct {
	ClientID            string
	RedirectURI         string
	State               string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// newMFAPending builds and persists a fresh pending MFA token for userID with a
// 2-minute TTL. oa is zero-valued for direct login flows.
func newMFAPending(ctx context.Context, userID string, oa oauthPending) (MFAPending, error) {
	pending := MFAPending{
		Token:                    uuid.New().String(),
		UserID:                   userID,
		ExpiresAt:                time.Now().Add(2 * time.Minute).UTC(),
		CreatedAt:                time.Now().UTC(),
		OAuthClientID:            oa.ClientID,
		OAuthRedirectURI:         oa.RedirectURI,
		OAuthState:               oa.State,
		OAuthScope:               oa.Scope,
		OAuthCodeChallenge:       oa.CodeChallenge,
		OAuthCodeChallengeMethod: oa.CodeChallengeMethod,
	}
	if err := pending.Add(ctx); err != nil {
		return MFAPending{}, err
	}
	return pending, nil
}

// userHasTOTP returns true if the user has a confirmed, active TOTP credential.
func userHasTOTP(ctx context.Context, userID string) bool {
	var cred TOTPCredential
	return connectRead().WithContext(ctx).
		Where("user_id = ? AND confirmed = ? AND active = ?", userID, true, true).
		First(&cred).Error == nil
}

// mfaPendingOAuthScopes splits a space-separated scope string (as stored in
// MFAPending.OAuthScope) into a slice, matching how OAuthCode.Scopes is set.
func mfaPendingOAuthScopes(scope string) []string {
	return strings.Fields(scope)
}
