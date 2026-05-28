package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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

// resolveHost is the DNS lookup used by validateServiceURL. Tests can replace it.
var resolveHost = net.LookupHost

// hashServiceKey bcrypt-hashes a plaintext service key for storage.
// Returns an empty string and no error when key is empty (no key configured).
func hashServiceKey(key string) (string, error) {
	if key == "" {
		return "", nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(key), 12)
	if err != nil {
		return "", err
	}
	return string(h), nil
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

// validateServiceURL rejects URLs that target loopback or RFC-1918 addresses
// to prevent SSRF via the service registry.
func validateServiceURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	host := u.Hostname()

	// checkIP validates a single parsed IP against the blocked ranges.
	checkIP := func(ip net.IP) error {
		if ip.IsLoopback() {
			return fmt.Errorf("URL must not target loopback address")
		}
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("URL must not target link-local address")
		}
		privateRanges := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
		for _, cidr := range privateRanges {
			_, network, _ := net.ParseCIDR(cidr)
			if network.Contains(ip) {
				return fmt.Errorf("URL must not target private network address")
			}
		}
		return nil
	}

	if ip := net.ParseIP(host); ip != nil {
		return checkIP(ip)
	}

	// Hostname — resolve and validate every returned address to prevent DNS-based
	// SSRF (e.g. a public domain resolving to 169.254.169.254). Fail closed: if
	// the hostname can't be resolved we reject it rather than allow it, since an
	// unresolvable name today could resolve to a private address tomorrow.
	// Internal service URLs (e.g. Docker service names) should be pre-seeded via
	// SERVICES env var or MANIFEST_FILE, which bypass this validation.
	addrs, err := resolveHost(host)
	if err != nil {
		return fmt.Errorf("hostname %q could not be resolved: %w", host, err)
	}
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil {
			continue
		}
		if err := checkIP(ip); err != nil {
			return fmt.Errorf("hostname %q resolves to blocked address %s: %w", host, addr, err)
		}
	}
	return nil
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
			slog.Error("list services: scan", "error", err)
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
				if err := roleRows.Scan(&sr.RoleID, &sr.ServiceID, &sr.Name, &sr.Description, &sr.CreatedAt); err != nil {
					slog.Error("list services: scan role", "error", err)
					continue
				}
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
			if err := epRows.Scan(&ep.EndpointID, &ep.ServiceID, &ep.Method, &ep.Path,
				&ep.Action, &ep.Resource, &ep.Public, &ep.Active, &ep.CreatedAt, &ep.UpdatedAt); err != nil {
				slog.Error("list services: scan endpoint", "error", err)
				continue
			}
			result[i].Endpoints = append(result[i].Endpoints, ep)
		}
		epRows.Close()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleCreateService registers a new service entry (admin key required).
func handleCreateService(w http.ResponseWriter, r *http.Request) {
	if !requireAdminKey(w, r) {
		return
	}
	var req struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		Description string `json:"description"`
		ForwardAuth bool   `json:"forward_auth"`
		ServiceKey  string `json:"service_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.URL == "" {
		http.Error(w, "name and url are required", http.StatusBadRequest)
		return
	}
	if err := validateServiceURL(req.URL); err != nil {
		http.Error(w, "invalid url: "+err.Error(), http.StatusBadRequest)
		return
	}

	hashedKey, err := hashServiceKey(req.ServiceKey)
	if err != nil {
		slog.Error("create service: hash key", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	id := uuid.New().String()
	_, err = pool.Exec(r.Context(),
		`INSERT INTO services (service_id, name, url, description, forward_auth, service_key)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, req.Name, req.URL, req.Description, req.ForwardAuth, hashedKey)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			http.Error(w, "service already registered", http.StatusConflict)
			return
		}
		slog.Error("create service: db", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var svc Service
	if err := pool.QueryRow(r.Context(),
		`SELECT service_id, name, url, description, forward_auth, active, created_at, updated_at FROM services WHERE service_id = $1`, id).
		Scan(&svc.ServiceID, &svc.Name, &svc.URL, &svc.Description, &svc.ForwardAuth, &svc.Active, &svc.CreatedAt, &svc.UpdatedAt); err != nil {
		slog.Error("create service: fetch after insert", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("service registered", "name", svc.Name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(svc)
}

// handleServiceRegister is called by services at startup to update their URL
// and replace their endpoint manifest. Authenticated by the service's own key
// (set when the service was created via POST /services).
func handleServiceRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		ServiceKey string `json:"service_key"`
		URL        string `json:"url"`
		Endpoints  []struct {
			Method   string `json:"method"`
			Path     string `json:"path"`
			Action   string `json:"action"`
			Resource string `json:"resource"`
			Public   bool   `json:"public"`
		} `json:"endpoints"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.ServiceKey == "" {
		http.Error(w, "name and service_key are required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	var serviceID, storedKey string
	if err := pool.QueryRow(ctx,
		`SELECT service_id, service_key FROM services WHERE name = $1 AND active = true`, req.Name).
		Scan(&serviceID, &storedKey); err != nil {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(storedKey), []byte(req.ServiceKey)); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if req.URL != "" {
		if err := validateServiceURL(req.URL); err != nil {
			http.Error(w, "invalid url: "+err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := tx.Exec(ctx, `UPDATE services SET url = $1, updated_at = now() WHERE service_id = $2`, req.URL, serviceID); err != nil {
			slog.Error("service register: update url", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM service_endpoints WHERE service_id = $1`, serviceID); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	for _, ep := range req.Endpoints {
		if ep.Method == "" || ep.Path == "" || ep.Action == "" || ep.Resource == "" {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO service_endpoints (endpoint_id, service_id, method, path, action, resource, public) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			uuid.New().String(), serviceID, ep.Method, ep.Path, ep.Action, ep.Resource, ep.Public); err != nil {
			slog.Error("service register: insert endpoint", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		slog.Error("service register: commit", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.Info("service self-registered", "name", req.Name, "service_id", serviceID)
	w.WriteHeader(http.StatusNoContent)
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

// handleUpdateServiceEndpoints replaces the full endpoint manifest for a service
// (admin key required). Used to seed endpoint definitions without self-registration.
func handleUpdateServiceEndpoints(w http.ResponseWriter, r *http.Request) {
	if !requireAdminKey(w, r) {
		return
	}
	id := r.PathValue("id")

	var req struct {
		URL         string `json:"url"`
		Description string `json:"description"`
		Roles       []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"roles"`
		Endpoints []struct {
			Method   string `json:"method"`
			Path     string `json:"path"`
			Action   string `json:"action"`
			Resource string `json:"resource"`
			Public   bool   `json:"public"`
		} `json:"endpoints"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.URL != "" {
		if err := validateServiceURL(req.URL); err != nil {
			http.Error(w, "invalid url: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	ctx := r.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Verify service exists.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM services WHERE service_id = $1 AND active = true`, id).Scan(&exists); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if req.URL != "" {
		if _, err := tx.Exec(ctx, `UPDATE services SET url = $1, updated_at = now() WHERE service_id = $2`, req.URL, id); err != nil {
			slog.Error("update endpoints: set url", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}
	if req.Description != "" {
		if _, err := tx.Exec(ctx, `UPDATE services SET description = $1, updated_at = now() WHERE service_id = $2`, req.Description, id); err != nil {
			slog.Error("update endpoints: set description", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM service_roles WHERE service_id = $1`, id); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	for _, role := range req.Roles {
		if role.Name == "" {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO service_roles (role_id, service_id, name, description) VALUES ($1, $2, $3, $4)`,
			uuid.New().String(), id, role.Name, role.Description); err != nil {
			slog.Error("update endpoints: insert role", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM service_endpoints WHERE service_id = $1`, id); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	for _, ep := range req.Endpoints {
		if ep.Method == "" || ep.Path == "" || ep.Action == "" || ep.Resource == "" {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO service_endpoints (endpoint_id, service_id, method, path, action, resource, public) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			uuid.New().String(), id, ep.Method, ep.Path, ep.Action, ep.Resource, ep.Public); err != nil {
			slog.Error("update endpoints: insert endpoint", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		slog.Error("update endpoints: commit", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

