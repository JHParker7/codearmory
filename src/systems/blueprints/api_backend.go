package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultBackendTTL = 4 * time.Hour
	maxBackendTTL     = 24 * time.Hour
)

var (
	clientCA        *x509.Certificate
	clientCAKey     *ecdsa.PrivateKey
	clientCACertPEM []byte
)

// initClientCA loads the client CA from BLUEPRINTS_CLIENT_CA_CERT / BLUEPRINTS_CLIENT_CA_KEY,
// or generates an ephemeral self-signed CA when neither is set.
// The ephemeral CA is not suitable for HA deployments; configure static keys in production.
func initClientCA() error {
	certPEM := secret("BLUEPRINTS_CLIENT_CA_CERT")
	keyPEM := secret("BLUEPRINTS_CLIENT_CA_KEY")

	if certPEM != "" && keyPEM != "" {
		certBlock, _ := pem.Decode([]byte(certPEM))
		if certBlock == nil {
			return fmt.Errorf("BLUEPRINTS_CLIENT_CA_CERT: invalid PEM")
		}
		cert, err := x509.ParseCertificate(certBlock.Bytes)
		if err != nil {
			return fmt.Errorf("BLUEPRINTS_CLIENT_CA_CERT: %w", err)
		}
		keyBlock, _ := pem.Decode([]byte(keyPEM))
		if keyBlock == nil {
			return fmt.Errorf("BLUEPRINTS_CLIENT_CA_KEY: invalid PEM")
		}
		key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
		if err != nil {
			return fmt.Errorf("BLUEPRINTS_CLIENT_CA_KEY: %w", err)
		}
		clientCA = cert
		clientCAKey = key
		clientCACertPEM = []byte(certPEM)
		slog.Info("blueprints: client CA loaded from environment")
		return nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate CA serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "blueprints-client-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return fmt.Errorf("parse CA cert: %w", err)
	}
	clientCA = cert
	clientCAKey = key
	clientCACertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	slog.Info("blueprints: ephemeral client CA generated")
	return nil
}

func generateClientCert(ttl time.Duration) (certPEM, keyPEM, fingerprint string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", "", fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", "", fmt.Errorf("generate serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "blueprints-backend-" + uuid.New().String()},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, clientCA, &key.PublicKey, clientCAKey)
	if err != nil {
		return "", "", "", fmt.Errorf("sign cert: %w", err)
	}
	fp := sha256.Sum256(certDER)
	fingerprint = hex.EncodeToString(fp[:])
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", "", fmt.Errorf("marshal key: %w", err)
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM, fingerprint, nil
}

func rawFingerprint(raw []byte) string {
	fp := sha256.Sum256(raw)
	return hex.EncodeToString(fp[:])
}

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// checkCertCredential returns true when the TLS connection presents a valid
// client cert whose fingerprint is registered for workspaceKey.
func checkCertCredential(ctx context.Context, cs *tls.ConnectionState, workspaceKey string) bool {
	if cs == nil || len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
		return false
	}
	fp := rawFingerprint(cs.VerifiedChains[0][0].Raw)
	return (BackendCredential{CertFP: fp, Workspace: workspaceKey}).CheckCert(ctx)
}

// checkTokenCredential returns true when the bp_ bearer token is registered
// and still valid for workspaceKey.
func checkTokenCredential(ctx context.Context, token, workspaceKey string) bool {
	h := tokenHash(token)
	return (BackendCredential{TokenHash: h, Workspace: workspaceKey}).CheckToken(ctx)
}

// requireWorkspaceAuth is the unified auth gate for state operations. It accepts,
// in priority order: mTLS client cert, ephemeral bp_ bearer/basic token, JWT via gatekeeper.
func requireWorkspaceAuth(ctx context.Context, w http.ResponseWriter, r *http.Request, workspaceKey, action string) bool {
	if checkCertCredential(ctx, r.TLS, workspaceKey) {
		return true
	}

	authHeader := r.Header.Get("Authorization")

	if bearer, ok := strings.CutPrefix(authHeader, "Bearer bp_"); ok {
		if checkTokenCredential(ctx, "bp_"+bearer, workspaceKey) {
			return true
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}

	if encoded, ok := strings.CutPrefix(authHeader, "Basic "); ok {
		if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			if _, pass, cut := strings.Cut(string(decoded), ":"); cut && strings.HasPrefix(pass, "bp_") {
				if checkTokenCredential(ctx, pass, workspaceKey) {
					return true
				}
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return false
			}
		}
	}

	token, ok := extractToken(ctx, r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="blueprints"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if !checkPermissions(ctx, token, "states/"+workspaceKey, action) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// parseJWTSubject extracts the subject (user ID) from a JWT without verifying
// the signature — gatekeeper has already verified it via checkPermissions.
func parseJWTSubject(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Sub == "" {
		return "", fmt.Errorf("JWT missing sub claim")
	}
	return claims.Sub, nil
}

type callerContext struct {
	UserID string
	OrgID  string
	TeamID string // empty when the user has no team
}

// getCallerContext resolves the org and team for the authenticated user by
// parsing the JWT subject and fetching the user record from gatekeeper.
func getCallerContext(ctx context.Context, token string) (callerContext, error) {
	userID, err := parseJWTSubject(token)
	if err != nil {
		return callerContext{}, fmt.Errorf("parse token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatekeeperURL+"/users/"+userID, nil)
	if err != nil {
		return callerContext{}, fmt.Errorf("build user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return callerContext{}, fmt.Errorf("get user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return callerContext{}, fmt.Errorf("get user %s: status %d", userID, resp.StatusCode)
	}
	var user struct {
		OrgID  *string `json:"org_id"`
		TeamID *string `json:"team_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return callerContext{}, fmt.Errorf("decode user: %w", err)
	}
	cc := callerContext{UserID: userID}
	if user.OrgID != nil {
		cc.OrgID = *user.OrgID
	}
	if user.TeamID != nil {
		cc.TeamID = *user.TeamID
	}
	return cc, nil
}

// backendUsername builds the workspace owner segment:
//   - org member with team:  {org_id}_{team_id}_{uuid}
//   - org member, no team:   {org_id}_{uuid}
//   - no org:                {user_id}_{uuid}
func backendUsername(cc callerContext) string {
	parts := make([]string, 0, 3)
	if cc.OrgID != "" {
		parts = append(parts, cc.OrgID)
		if cc.TeamID != "" {
			parts = append(parts, cc.TeamID)
		}
	} else {
		parts = append(parts, cc.UserID)
	}
	return strings.Join(parts, "_")
}

type backendRequest struct {
	Workspace string `json:"workspace"`
	TTLSecs   int    `json:"ttl_secs"`
}

type backendResponse struct {
	CredentialID string            `json:"credential_id"`
	Workspace    string            `json:"workspace"`
	// BackendTF contains only the address/lock/unlock block — no credentials.
	// It is optional: omit it and write your own backend.tf if the repo already has one.
	BackendTF string            `json:"backend_tf"`
	// Env holds all credential values as TF_HTTP_* env vars ready to inject into a forge step.
	Env       map[string]string `json:"env"`
	ExpiresAt string            `json:"expires_at"`
}

func handleCreateBackend(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("blueprints").Start(r.Context(), "handleCreateBackend")
	defer span.End()
	r = r.WithContext(ctx)

	token, ok := requireAuth(ctx, w, r)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	if !checkPermissions(ctx, token, "states", "createBackend") {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	span.AddEvent("permission.granted")

	cc, err := getCallerContext(ctx, token)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "resolve caller context failed")
		slog.ErrorContext(ctx, "backend: resolve caller context", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(attribute.String("caller.id", cc.UserID))
	slog.InfoContext(ctx, "create backend request", "user_id", cc.UserID)

	var req backendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.SetStatus(codes.Error, "invalid request body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	workspace := req.Workspace
	if workspace == "" {
		workspace = uuid.New().String()
	}

	workspaceKey := backendUsername(cc) + "/" + workspace
	span.SetAttributes(attribute.String("workspace", workspaceKey))

	ttl := defaultBackendTTL
	if req.TTLSecs > 0 {
		ttl = time.Duration(req.TTLSecs) * time.Second
		if ttl > maxBackendTTL {
			ttl = maxBackendTTL
		}
	}

	certPEM, keyPEM, certFP, err := generateClientCert(ttl)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "cert generation failed")
		slog.ErrorContext(ctx, "backend: generate client cert", "user_id", cc.UserID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	bearerToken := "bp_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	th := tokenHash(bearerToken)
	credID := uuid.New().String()
	expiresAt := time.Now().UTC().Add(ttl)

	cred := BackendCredential{
		CredentialID: credID,
		Workspace:    workspaceKey,
		CertFP:       certFP,
		TokenHash:    th,
		CreatedBy:    cc.UserID,
		ExpiresAt:    expiresAt,
	}
	if err := cred.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "backend: insert credential", "user_id", cc.UserID, "workspace", workspaceKey, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("credential.id", credID),
		attribute.String("workspace", workspaceKey),
	))

	blueprintsURL := envOrDefault("BLUEPRINTS_EXTERNAL_URL", "http://blueprints:8093")
	addr := fmt.Sprintf("%s/state/%s", blueprintsURL, workspaceKey)

	resp := backendResponse{
		CredentialID: credID,
		Workspace:    workspaceKey,
		BackendTF:    buildBackendTF(addr),
		Env: map[string]string{
			"TF_HTTP_USERNAME":                  "_",
			"TF_HTTP_PASSWORD":                  bearerToken,
			"TF_HTTP_CLIENT_CERTIFICATE_PEM":    certPEM,
			"TF_HTTP_CLIENT_PRIVATE_KEY_PEM":    keyPEM,
			"TF_HTTP_CLIENT_CA_CERTIFICATE_PEM": string(clientCACertPEM),
		},
		ExpiresAt: expiresAt.Format(time.RFC3339),
	}

	span.SetAttributes(attribute.String("credential.id", credID))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "backend credential created", "user_id", cc.UserID, "credential_id", credID, "workspace", workspaceKey, "ttl_secs", int(ttl.Seconds()))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

// buildBackendTF generates the address-only backend.tf block. Credentials are
// returned separately in Env as TF_HTTP_* vars and must not appear in this file.
func buildBackendTF(address string) string {
	return fmt.Sprintf(`terraform {
  backend "http" {
    address        = %q
    lock_address   = %q
    unlock_address = %q
    lock_method    = "LOCK"
    unlock_method  = "UNLOCK"
  }
}
`, address, address, address)
}
