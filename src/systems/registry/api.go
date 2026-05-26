package main

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
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

// handleServiceSelfRegister is called by backend services at startup. They
// prove identity with their pre-shared service key, update their URL, and
// replace their endpoint manifest so Conductor always has current metadata.
func handleServiceSelfRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		ServiceKey  string `json:"service_key"`
		URL         string `json:"url"`
		Description string `json:"description"`
		Roles       []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"roles"`
		Endpoints []struct {
			Method      string `json:"method"`
			Path        string `json:"path"`
			Action      string `json:"action"`
			Resource    string `json:"resource"`
			Public      bool   `json:"public"`
			Description string `json:"description"`
		} `json:"endpoints"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.ServiceKey == "" {
		http.Error(w, "name and service_key are required", http.StatusBadRequest)
		return
	}

	var svcID, keyHash string
	err := pool.QueryRow(r.Context(),
		`SELECT service_id, service_key_hash FROM services WHERE name = $1 AND active = true`,
		req.Name).Scan(&svcID, &keyHash)
	if err != nil || keyHash == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(keyHash), []byte(req.ServiceKey)) != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()

	tx, err := pool.Begin(ctx)
	if err != nil {
		slog.Error("self-register: begin tx", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if req.URL != "" {
		tx.Exec(ctx,
			`UPDATE services SET url = $1, updated_at = now() WHERE service_id = $2`, req.URL, svcID)
	}
	if req.Description != "" {
		tx.Exec(ctx,
			`UPDATE services SET description = $1, updated_at = now() WHERE service_id = $2`, req.Description, svcID)
	}

	// Replace roles and endpoints atomically so concurrent GET /services reads
	// never observe a partial state between DELETE and INSERT.
	tx.Exec(ctx, `DELETE FROM service_roles WHERE service_id = $1`, svcID)
	for _, r := range req.Roles {
		if r.Name == "" {
			continue
		}
		tx.Exec(ctx,
			`INSERT INTO service_roles (role_id, service_id, name, description)
			 VALUES ($1, $2, $3, $4)`,
			uuid.New().String(), svcID, r.Name, r.Description)
	}

	tx.Exec(ctx, `DELETE FROM service_endpoints WHERE service_id = $1`, svcID)
	for _, ep := range req.Endpoints {
		if ep.Method == "" || ep.Path == "" || ep.Action == "" || ep.Resource == "" {
			continue
		}
		tx.Exec(ctx,
			`INSERT INTO service_endpoints (endpoint_id, service_id, method, path, action, resource, public)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			uuid.New().String(), svcID, ep.Method, ep.Path, ep.Action, ep.Resource, ep.Public)
	}

	if err := tx.Commit(ctx); err != nil {
		slog.Error("self-register: commit tx", "name", req.Name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("service self-registered", "name", req.Name, "endpoints", len(req.Endpoints))
	w.WriteHeader(http.StatusNoContent)
}
