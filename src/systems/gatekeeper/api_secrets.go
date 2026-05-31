package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/google/uuid"
)

var adapterClient = &http.Client{Timeout: 10 * time.Second}

// ── Secrets CRUD ──────────────────────────────────────────────────────────────

type secretRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type secretResponse struct {
	SecretID  string    `json:"secret_id"`
	OrgID     string    `json:"org_id"`
	Name      string    `json:"name"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toSecretResponse(s Secret) secretResponse {
	return secretResponse{
		SecretID:  s.SecretID,
		OrgID:     s.OrgID,
		Name:      s.Name,
		CreatedBy: s.CreatedBy,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
}

func secretsEnabledOrError(w http.ResponseWriter) bool {
	secretsEncMu.RLock()
	ok := secretsEnabled
	secretsEncMu.RUnlock()
	if !ok {
		http.Error(w, "secrets not available: GATEKEEPER_SECRETS_KEY not configured", http.StatusServiceUnavailable)
	}
	return ok
}

func handleCreateSecret(w http.ResponseWriter, r *http.Request) {
	if !secretsEnabledOrError(w) {
		return
	}
	callerID, _ := r.Context().Value(userIDKey).(string)
	if !requirePermission(w, r, "createSecret", "gatekeeper/secrets") {
		return
	}

	callerRow, err := (User{UserID: callerID}).Get(r.Context())
	if err != nil {
		http.Error(w, "caller not found", http.StatusUnauthorized)
		return
	}
	orgID := ""
	if oid := callerRow.(User).OrgID; oid != nil {
		orgID = *oid
	}
	if orgID == "" {
		http.Error(w, "caller must belong to an org to create secrets", http.StatusBadRequest)
		return
	}

	var req secretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Value == "" {
		http.Error(w, "name and value are required", http.StatusBadRequest)
		return
	}

	var existing Secret
	if connectRead().WithContext(r.Context()).
		Where("org_id = ? AND name = ? AND active = true", orgID, req.Name).
		First(&existing).Error == nil {
		http.Error(w, "secret with that name already exists", http.StatusConflict)
		return
	}

	ct, err := encryptSecret(req.Value)
	if err != nil {
		slog.Error("create secret: encrypt", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	s := Secret{
		SecretID:   uuid.New().String(),
		OrgID:      orgID,
		Name:       req.Name,
		Ciphertext: ct,
		CreatedBy:  callerID,
	}
	if err := connect().WithContext(r.Context()).Create(&s).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			http.Error(w, "secret with that name already exists", http.StatusConflict)
			return
		}
		slog.Error("create secret: db", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("secret created", "secret_id", s.SecretID, "org_id", orgID, "caller_id", callerID)
	writeAudit(r.Context(), callerID, "user", "secret.create", s.SecretID, req.Name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(toSecretResponse(s)) //nolint:errcheck
}

func handleListSecrets(w http.ResponseWriter, r *http.Request) {
	if !secretsEnabledOrError(w) {
		return
	}
	callerID, _ := r.Context().Value(userIDKey).(string)
	if !requirePermission(w, r, "listSecret", "gatekeeper/secrets") {
		return
	}

	var callerOrgID string
	if callerRow, err := (User{UserID: callerID}).Get(r.Context()); err == nil {
		if oid := callerRow.(User).OrgID; oid != nil {
			callerOrgID = *oid
		}
	}

	var secrets []Secret
	if err := connectRead().WithContext(r.Context()).
		Where("org_id = ? AND active = true", callerOrgID).
		Find(&secrets).Error; err != nil {
		slog.Error("list secrets: db", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	resp := make([]secretResponse, len(secrets))
	for i, s := range secrets {
		resp[i] = toSecretResponse(s)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

func handleUpdateSecret(w http.ResponseWriter, r *http.Request) {
	if !secretsEnabledOrError(w) {
		return
	}
	callerID, _ := r.Context().Value(userIDKey).(string)
	id := r.PathValue("id")
	if !requirePermission(w, r, "updateSecret", "gatekeeper/secrets/"+id) {
		return
	}

	var s Secret
	if err := connect().WithContext(r.Context()).
		Where("secret_id = ? AND active = true", id).First(&s).Error; err != nil {
		http.Error(w, "secret not found", http.StatusNotFound)
		return
	}

	var callerOrgID string
	if callerRow, err := (User{UserID: callerID}).Get(r.Context()); err == nil {
		if oid := callerRow.(User).OrgID; oid != nil {
			callerOrgID = *oid
		}
	}
	if s.OrgID != callerOrgID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req secretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Value == "" {
		http.Error(w, "value is required", http.StatusBadRequest)
		return
	}

	ct, err := encryptSecret(req.Value)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.Ciphertext = ct
	s.UpdatedAt = time.Now()
	if req.Name != "" {
		s.Name = req.Name
	}
	if err := connect().WithContext(r.Context()).Save(&s).Error; err != nil {
		slog.Error("update secret: db", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("secret updated", "secret_id", id, "caller_id", callerID)
	writeAudit(r.Context(), callerID, "user", "secret.update", id, s.Name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toSecretResponse(s)) //nolint:errcheck
}

func handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	if !secretsEnabledOrError(w) {
		return
	}
	callerID, _ := r.Context().Value(userIDKey).(string)
	id := r.PathValue("id")
	if !requirePermission(w, r, "deleteSecret", "gatekeeper/secrets/"+id) {
		return
	}

	var s Secret
	if err := connect().WithContext(r.Context()).
		Where("secret_id = ? AND active = true", id).First(&s).Error; err != nil {
		http.Error(w, "secret not found", http.StatusNotFound)
		return
	}

	var callerOrgID string
	if callerRow, err := (User{UserID: callerID}).Get(r.Context()); err == nil {
		if oid := callerRow.(User).OrgID; oid != nil {
			callerOrgID = *oid
		}
	}
	if s.OrgID != callerOrgID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	connect().WithContext(r.Context()).Model(&s).Update("active", false) //nolint:errcheck
	slog.Info("secret deleted", "secret_id", id, "caller_id", callerID)
	writeAudit(r.Context(), callerID, "user", "secret.delete", id, s.Name)
	w.WriteHeader(http.StatusNoContent)
}

// ── Provider configuration ────────────────────────────────────────────────────

type providerRequest struct {
	Provider string          `json:"provider"` // builtin, doppler, vault, aws_sm
	Config   json.RawMessage `json:"config"`
}

type providerResponse struct {
	OrgID     string    `json:"org_id"`
	Provider  string    `json:"provider"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func handleGetSecretProvider(w http.ResponseWriter, r *http.Request) {
	if !secretsEnabledOrError(w) {
		return
	}
	orgID := r.PathValue("id")
	callerID, _ := r.Context().Value(userIDKey).(string)
	if !requirePermission(w, r, "getSecretProvider", "gatekeeper/orgs/"+orgID) {
		return
	}

	var p OrgSecretProvider
	if err := connectRead().WithContext(r.Context()).First(&p, "org_id = ?", orgID).Error; err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_ = callerID
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(providerResponse{ //nolint:errcheck
		OrgID: p.OrgID, Provider: p.Provider, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	})
}

func handleSetSecretProvider(w http.ResponseWriter, r *http.Request) {
	if !secretsEnabledOrError(w) {
		return
	}
	orgID := r.PathValue("id")
	callerID, _ := r.Context().Value(userIDKey).(string)
	if !requirePermission(w, r, "updateSecretProvider", "gatekeeper/orgs/"+orgID) {
		return
	}

	var req providerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	validProviders := map[string]bool{"builtin": true, "doppler": true, "vault": true, "aws_sm": true}
	if !validProviders[req.Provider] {
		http.Error(w, "provider must be one of: builtin, doppler, vault, aws_sm", http.StatusBadRequest)
		return
	}

	var configCT []byte
	if len(req.Config) > 0 && string(req.Config) != "null" {
		ct, err := encryptSecret(string(req.Config))
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		configCT = ct
	}

	p := OrgSecretProvider{OrgID: orgID, Provider: req.Provider, Config: configCT}
	if err := connect().WithContext(r.Context()).
		Save(&p).Error; err != nil {
		slog.Error("set secret provider: db", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("secret provider configured", "org_id", orgID, "provider", req.Provider, "caller_id", callerID)
	writeAudit(r.Context(), callerID, "user", "secret_provider.set", orgID, req.Provider)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(providerResponse{ //nolint:errcheck
		OrgID: p.OrgID, Provider: p.Provider, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	})
}

func handleDeleteSecretProvider(w http.ResponseWriter, r *http.Request) {
	if !secretsEnabledOrError(w) {
		return
	}
	orgID := r.PathValue("id")
	callerID, _ := r.Context().Value(userIDKey).(string)
	if !requirePermission(w, r, "deleteSecretProvider", "gatekeeper/orgs/"+orgID) {
		return
	}

	connect().WithContext(r.Context()).Delete(&OrgSecretProvider{}, "org_id = ?", orgID) //nolint:errcheck
	slog.Info("secret provider removed", "org_id", orgID, "caller_id", callerID)
	writeAudit(r.Context(), callerID, "user", "secret_provider.delete", orgID, "")
	w.WriteHeader(http.StatusNoContent)
}

// ── Internal resolve endpoint ─────────────────────────────────────────────────

type resolveRequest struct {
	OrgID string   `json:"org_id"`
	Names []string `json:"names"`
}

// handleResolveSecrets is called by the workflow worker to decrypt secrets for a run.
// Auth: service account key (the calling service must be in GATEKEEPER_SERVICES).
func handleResolveSecrets(w http.ResponseWriter, r *http.Request) {
	if !secretsEnabledOrError(w) {
		return
	}
	svc, ok := requireServiceAuth(w, r)
	if !ok {
		return
	}

	var req resolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.OrgID == "" || len(req.Names) == 0 {
		http.Error(w, "org_id and names are required", http.StatusBadRequest)
		return
	}

	values, err := resolveSecrets(r.Context(), req.OrgID, req.Names)
	if err != nil {
		slog.Warn("resolve secrets: failed", "org_id", req.OrgID, "service", svc.ServiceName, "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	slog.Info("secrets resolved", "org_id", req.OrgID, "count", len(values), "service", svc.ServiceName)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(values) //nolint:errcheck
}

// resolveSecrets dispatches to the org's configured provider (or builtin if none set).
func resolveSecrets(ctx context.Context, orgID string, names []string) (map[string]string, error) {
	var p OrgSecretProvider
	if err := connectRead().WithContext(ctx).First(&p, "org_id = ?", orgID).Error; err != nil {
		p = OrgSecretProvider{OrgID: orgID, Provider: "builtin"}
	}

	switch p.Provider {
	case "builtin", "":
		return resolveBuiltin(ctx, orgID, names)
	case "doppler":
		return resolveDoppler(ctx, p.Config, names)
	case "vault":
		return resolveVault(ctx, p.Config, names)
	case "aws_sm":
		return resolveAWSSM(ctx, p.Config, names)
	default:
		return nil, fmt.Errorf("unknown provider %q", p.Provider)
	}
}

// ── Built-in adapter ──────────────────────────────────────────────────────────

func resolveBuiltin(ctx context.Context, orgID string, names []string) (map[string]string, error) {
	var secrets []Secret
	if err := connectRead().WithContext(ctx).
		Where("org_id = ? AND active = true AND name IN ?", orgID, names).
		Find(&secrets).Error; err != nil {
		return nil, fmt.Errorf("db query: %w", err)
	}

	found := make(map[string]string, len(secrets))
	for _, s := range secrets {
		pt, err := decryptSecret(s.Ciphertext)
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %q: %w", s.Name, err)
		}
		found[s.Name] = pt
	}

	for _, name := range names {
		if _, ok := found[name]; !ok {
			return nil, fmt.Errorf("secret %q not found", name)
		}
	}
	return found, nil
}

// ── Doppler adapter ───────────────────────────────────────────────────────────

type dopplerConfig struct {
	ServiceToken string `json:"service_token"`
	Project      string `json:"project"`
	Config       string `json:"config"`
}

func resolveDoppler(ctx context.Context, encConfig []byte, names []string) (map[string]string, error) {
	cfg, err := decodeDopplerConfig(encConfig)
	if err != nil {
		return nil, fmt.Errorf("doppler config: %w", err)
	}

	result := make(map[string]string, len(names))
	for _, name := range names {
		url := fmt.Sprintf("https://api.doppler.com/v3/configs/config/secret?name=%s", name)
		if cfg.Project != "" {
			url += "&project=" + cfg.Project
		}
		if cfg.Config != "" {
			url += "&config=" + cfg.Config
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+cfg.ServiceToken)

		resp, err := adapterClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("doppler request: %w", err)
		}
		val, err := func() (string, error) {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				return "", fmt.Errorf("secret %q not found in Doppler", name)
			}
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				return "", fmt.Errorf("doppler HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			}
			var payload struct {
				Secret struct {
					Raw struct {
						Raw string `json:"raw"`
					} `json:"raw"`
				} `json:"secret"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				return "", fmt.Errorf("decode doppler response: %w", err)
			}
			return payload.Secret.Raw.Raw, nil
		}()
		if err != nil {
			return nil, err
		}
		result[name] = val
	}
	return result, nil
}

func decodeDopplerConfig(encConfig []byte) (dopplerConfig, error) {
	raw, err := decryptSecret(encConfig)
	if err != nil {
		return dopplerConfig{}, err
	}
	var cfg dopplerConfig
	return cfg, json.Unmarshal([]byte(raw), &cfg)
}

// ── HashiCorp Vault adapter ───────────────────────────────────────────────────

type vaultConfig struct {
	Address   string `json:"address"`
	Token     string `json:"token"`
	Namespace string `json:"namespace"`
	Mount     string `json:"mount"` // defaults to "secret"
}

func resolveVault(ctx context.Context, encConfig []byte, names []string) (map[string]string, error) {
	raw, err := decryptSecret(encConfig)
	if err != nil {
		return nil, fmt.Errorf("vault config: %w", err)
	}
	var cfg vaultConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("parse vault config: %w", err)
	}
	if cfg.Mount == "" {
		cfg.Mount = "secret"
	}

	result := make(map[string]string, len(names))
	for _, name := range names {
		url := strings.TrimRight(cfg.Address, "/") + "/v1/" + cfg.Mount + "/data/" + name

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("X-Vault-Token", cfg.Token)
		if cfg.Namespace != "" {
			req.Header.Set("X-Vault-Namespace", cfg.Namespace)
		}

		resp, err := adapterClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("vault request: %w", err)
		}
		val, err := func() (string, error) {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				return "", fmt.Errorf("secret %q not found in Vault", name)
			}
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				return "", fmt.Errorf("vault HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			}
			var payload struct {
				Data struct {
					Data map[string]string `json:"data"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				return "", fmt.Errorf("decode vault response: %w", err)
			}
			v, ok := payload.Data.Data["value"]
			if !ok {
				for _, v2 := range payload.Data.Data {
					return v2, nil
				}
				return "", fmt.Errorf("secret %q has no value key in Vault", name)
			}
			return v, nil
		}()
		if err != nil {
			return nil, err
		}
		result[name] = val
	}
	return result, nil
}

// ── AWS Secrets Manager adapter ───────────────────────────────────────────────

type awssmConfig struct {
	Region string `json:"region"`
}

func resolveAWSSM(ctx context.Context, encConfig []byte, names []string) (map[string]string, error) {
	var cfg awssmConfig
	if len(encConfig) > 0 {
		raw, err := decryptSecret(encConfig)
		if err != nil {
			return nil, fmt.Errorf("aws_sm config: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return nil, fmt.Errorf("parse aws_sm config: %w", err)
		}
	}

	var opts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	client := secretsmanager.NewFromConfig(awsCfg)
	result := make(map[string]string, len(names))
	for _, name := range names {
		out, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
			SecretId: &name,
		})
		if err != nil {
			return nil, fmt.Errorf("secret %q: %w", name, err)
		}
		if out.SecretString != nil {
			result[name] = *out.SecretString
		} else {
			return nil, fmt.Errorf("secret %q is a binary secret; only string secrets are supported", name)
		}
	}
	return result, nil
}

