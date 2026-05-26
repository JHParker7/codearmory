package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

type Service struct {
	ServiceID   string    `json:"service_id"`
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	Description string    `json:"description"`
	ForwardAuth bool      `json:"forward_auth"`
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ServiceRole struct {
	RoleID      string    `json:"role_id"`
	ServiceID   string    `json:"service_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type ServiceEndpoint struct {
	EndpointID string    `json:"endpoint_id"`
	ServiceID  string    `json:"service_id"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Action     string    `json:"action"`
	Resource   string    `json:"resource"`
	Public     bool      `json:"public"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type serviceWithEndpoints struct {
	Service
	Roles     []ServiceRole     `json:"roles"`
	Endpoints []ServiceEndpoint `json:"endpoints"`
}

type registerRoleReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type registerEndpointReq struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Action      string `json:"action"`
	Resource    string `json:"resource"`
	Public      bool   `json:"public"`
	Description string `json:"description"`
}

// checkKey does a constant-time comparison against a configured API key so the
// check is not vulnerable to timing-based enumeration.
func checkKey(r *http.Request, expected string) bool {
	if expected == "" {
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func requireAdminKey(w http.ResponseWriter, r *http.Request) bool {
	if checkKey(r, os.Getenv("ADMIN_KEY")) {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// requireReadKey accepts either the read key or the admin key.
// Both comparisons are always evaluated to prevent the short-circuit from
// leaking which key was checked first via response timing.
func requireReadKey(w http.ResponseWriter, r *http.Request) bool {
	readOK := checkKey(r, os.Getenv("READ_KEY"))
	adminOK := checkKey(r, os.Getenv("ADMIN_KEY"))
	if readOK || adminOK {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// handleListServices returns all active services with their endpoint manifests.
func handleListServices(w http.ResponseWriter, r *http.Request) {
	if !requireReadKey(w, r) {
		return
	}

	rows, err := pool.Query(r.Context(),
		`SELECT service_id, name, url, description, forward_auth, active, created_at, updated_at
		 FROM services WHERE active = true ORDER BY name`)
	if err != nil {
		slog.Error("list services: query", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var svcs []Service
	for rows.Next() {
		var s Service
		if err := rows.Scan(&s.ServiceID, &s.Name, &s.URL, &s.Description, &s.ForwardAuth, &s.Active, &s.CreatedAt, &s.UpdatedAt); err != nil {
			continue
		}
		svcs = append(svcs, s)
	}
	if rows.Err() != nil {
		slog.Error("list services: rows", "error", rows.Err())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	result := make([]serviceWithEndpoints, len(svcs))
	for i, s := range svcs {
		result[i] = serviceWithEndpoints{Service: s, Roles: []ServiceRole{}, Endpoints: []ServiceEndpoint{}}

		roleRows, err := pool.Query(r.Context(),
			`SELECT role_id, service_id, name, description, created_at
			 FROM service_roles WHERE service_id = $1 ORDER BY name`, s.ServiceID)
		if err == nil {
			for roleRows.Next() {
				var sr ServiceRole
				roleRows.Scan(&sr.RoleID, &sr.ServiceID, &sr.Name, &sr.Description, &sr.CreatedAt)
				result[i].Roles = append(result[i].Roles, sr)
			}
			roleRows.Close()
		}

		epRows, err := pool.Query(r.Context(),
			`SELECT endpoint_id, service_id, method, path, action, resource, public, active, created_at, updated_at
			 FROM service_endpoints WHERE service_id = $1 AND active = true`, s.ServiceID)
		if err != nil {
			continue
		}
		for epRows.Next() {
			var ep ServiceEndpoint
			epRows.Scan(&ep.EndpointID, &ep.ServiceID, &ep.Method, &ep.Path,
				&ep.Action, &ep.Resource, &ep.Public, &ep.Active, &ep.CreatedAt, &ep.UpdatedAt)
			result[i].Endpoints = append(result[i].Endpoints, ep)
		}
		epRows.Close()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleCreateService registers a new service entry (admin key required).
// An optional service_key is hashed and stored to enable self-registration.
func handleCreateService(w http.ResponseWriter, r *http.Request) {
	if !requireAdminKey(w, r) {
		return
	}
	var req struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		Description string `json:"description"`
		ServiceKey  string `json:"service_key"`
		ForwardAuth bool   `json:"forward_auth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.URL == "" {
		http.Error(w, "name and url are required", http.StatusBadRequest)
		return
	}

	var keyHash string
	if req.ServiceKey != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(req.ServiceKey), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		keyHash = string(h)
	}

	id := uuid.New().String()
	_, err := pool.Exec(r.Context(),
		`INSERT INTO services (service_id, name, url, description, service_key_hash, forward_auth)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, req.Name, req.URL, req.Description, keyHash, req.ForwardAuth)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			http.Error(w, "service already registered", http.StatusConflict)
			return
		}
		slog.Error("create service: db", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var svc Service
	pool.QueryRow(r.Context(),
		`SELECT service_id, name, url, description, forward_auth, active, created_at, updated_at FROM services WHERE service_id = $1`, id).
		Scan(&svc.ServiceID, &svc.Name, &svc.URL, &svc.Description, &svc.ForwardAuth, &svc.Active, &svc.CreatedAt, &svc.UpdatedAt)

	slog.Info("service registered", "name", svc.Name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(svc)
}

// handleDeleteService soft-deletes a service by ID (admin key required).
func handleDeleteService(w http.ResponseWriter, r *http.Request) {
	if !requireAdminKey(w, r) {
		return
	}
	id := r.PathValue("id")
	tag, err := pool.Exec(r.Context(),
		`UPDATE services SET active = false, updated_at = now() WHERE service_id = $1 AND active = true`, id)
	if err != nil {
		slog.Error("delete service: db", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleServiceSelfRegister is called by backend services at startup.
//
// Bootstrap (first call): provide name + service_key. The key is verified once,
// marked as used, and the service receives a randomly generated client_id and
// client_secret. The service_key is rejected on any subsequent attempt.
//
// Credential mode (subsequent calls): provide name + client_id + client_secret.
// The service updates its URL and endpoint manifest; returns 204.
func handleServiceSelfRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string                `json:"name"`
		ServiceKey   string                `json:"service_key"`
		ClientID     string                `json:"client_id"`
		ClientSecret string                `json:"client_secret"`
		URL          string                `json:"url"`
		Description  string                `json:"description"`
		Roles        []registerRoleReq     `json:"roles"`
		Endpoints    []registerEndpointReq `json:"endpoints"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Credential mode: client_id + client_secret provided.
	if req.ClientID != "" && req.ClientSecret != "" {
		var svcID, secretHash string
		err := pool.QueryRow(ctx,
			`SELECT service_id, client_secret_hash FROM services WHERE name = $1 AND client_id = $2 AND active = true`,
			req.Name, req.ClientID).Scan(&svcID, &secretHash)
		if err != nil || secretHash == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(req.ClientSecret)) != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			slog.Error("self-register: begin tx", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if err := applyManifest(ctx, tx, svcID, req.Name, req.URL, req.Description, req.Roles, req.Endpoints); err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			slog.Error("self-register: commit tx", "name", req.Name, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		slog.Info("service re-registered", "name", req.Name, "endpoints", len(req.Endpoints))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Bootstrap mode: service_key provided (one-time use).
	if req.ServiceKey == "" {
		http.Error(w, "service_key or (client_id + client_secret) required", http.StatusBadRequest)
		return
	}

	var svcID, keyHash string
	var keyUsed bool
	err := pool.QueryRow(ctx,
		`SELECT service_id, service_key_hash, key_used FROM services WHERE name = $1 AND active = true`,
		req.Name).Scan(&svcID, &keyHash, &keyUsed)
	if err != nil || keyHash == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if keyUsed {
		slog.Warn("bootstrap key reuse attempt", "name", req.Name)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(keyHash), []byte(req.ServiceKey)) != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Generate client credentials.
	clientID := uuid.New().String()
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		slog.Error("self-register: rand", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	clientSecret := base64.RawURLEncoding.EncodeToString(secretBytes)
	secretHash, err := bcrypt.GenerateFromPassword([]byte(clientSecret), bcrypt.DefaultCost)
	if err != nil {
		slog.Error("self-register: bcrypt", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		slog.Error("self-register: begin tx", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx,
		`UPDATE services SET key_used = true, client_id = $1, client_secret_hash = $2, updated_at = now() WHERE service_id = $3`,
		clientID, string(secretHash), svcID); err != nil {
		slog.Error("self-register: store credentials", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := applyManifest(ctx, tx, svcID, req.Name, req.URL, req.Description, req.Roles, req.Endpoints); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("self-register: commit tx", "name", req.Name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("service bootstrap complete — store credentials securely",
		"name", req.Name,
		"client_id", clientID,
		"action", "set CLIENT_ID and CLIENT_SECRET env vars; remove SERVICE_KEY")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
	})
}

// applyManifest updates URL, description, roles, and endpoints atomically
// within the provided transaction.
func applyManifest(ctx context.Context, tx pgx.Tx, svcID, name, serviceURL, description string, roles []registerRoleReq, endpoints []registerEndpointReq) error {
	if serviceURL != "" {
		if _, err := tx.Exec(ctx,
			`UPDATE services SET url = $1, updated_at = now() WHERE service_id = $2`, serviceURL, svcID); err != nil {
			slog.Error("applyManifest: update url", "name", name, "error", err)
			return err
		}
	}
	if description != "" {
		if _, err := tx.Exec(ctx,
			`UPDATE services SET description = $1, updated_at = now() WHERE service_id = $2`, description, svcID); err != nil {
			slog.Error("applyManifest: update description", "name", name, "error", err)
			return err
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM service_roles WHERE service_id = $1`, svcID); err != nil {
		return err
	}
	for _, role := range roles {
		if role.Name == "" {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO service_roles (role_id, service_id, name, description) VALUES ($1, $2, $3, $4)`,
			uuid.New().String(), svcID, role.Name, role.Description); err != nil {
			slog.Error("applyManifest: insert role", "name", name, "error", err)
			return err
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM service_endpoints WHERE service_id = $1`, svcID); err != nil {
		return err
	}
	for _, ep := range endpoints {
		if ep.Method == "" || ep.Path == "" || ep.Action == "" || ep.Resource == "" {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO service_endpoints (endpoint_id, service_id, method, path, action, resource, public) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			uuid.New().String(), svcID, ep.Method, ep.Path, ep.Action, ep.Resource, ep.Public); err != nil {
			slog.Error("applyManifest: insert endpoint", "name", name, "error", err)
			return err
		}
	}
	return nil
}
