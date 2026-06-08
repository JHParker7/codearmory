package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"golang.org/x/crypto/bcrypt"
)

// Service is a registered backend service known to the registry.
// ForwardAuth, when true, signals that conductor should forward the user's
// Authorization header when proxying requests to this service.
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

// ServiceRole is a named role defined by a service and published in the registry.
// Gatekeeper resolves these names when evaluating permission requests.
type ServiceRole struct {
	RoleID      string    `json:"role_id"`
	ServiceID   string    `json:"service_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// ServiceEndpoint maps an HTTP method + path on a service to a gatekeeper
// action/resource pair. Public endpoints bypass permission checks entirely.
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
	Roles         []ServiceRole         `json:"roles"`
	Endpoints     []ServiceEndpoint     `json:"endpoints"`
	DefaultGrants []ServiceDefaultGrant `json:"default_grants,omitempty"`
}

// ServiceDefaultGrant is a permission template a service declares for new
// users, orgs, or teams. Gatekeeper reads these at startup and applies them
// when creating new principals.
type ServiceDefaultGrant struct {
	GrantID     string    `json:"grant_id"`
	ServiceID   string    `json:"service_id"`
	ServiceName string    `json:"service_name"`
	GrantOn     string    `json:"grant_on"` // "user", "org", or "team"
	Actions     []string  `json:"actions"`
	Resources   []string  `json:"resources"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ServiceAction is a callable workflow action registered by a service.
// BodyTransforms and AsyncConfig are stored as raw JSONB and passed through.
// GkService/GkAction/GkResource are the gatekeeper permission triple required
// to call this action, derived from the matching service_endpoint record. Empty
// when the endpoint has no registered service_endpoint entry.
type ServiceAction struct {
	ActionID       string          `json:"action_id"`
	ServiceID      string          `json:"service_id"`
	ServiceName    string          `json:"service_name"`
	ServiceURL     string          `json:"service_url"`
	Name           string          `json:"name"`
	Method         string          `json:"method"`
	Path           string          `json:"path"`
	BodyTransforms json.RawMessage `json:"body_transforms,omitempty"`
	AsyncConfig    json.RawMessage `json:"async,omitempty"`
	Active         bool            `json:"active"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	GkService      string          `json:"gk_service,omitempty"`
	GkAction       string          `json:"gk_action,omitempty"`
	GkResource     string          `json:"gk_resource,omitempty"`
}

// resolveHost is the DNS lookup used by validateServiceURL. Tests can replace it.
var resolveHost = net.LookupHost

// jsonbOrNil returns nil when raw is empty or the JSON null literal,
// so that pgx inserts a SQL NULL into a JSONB column instead of the string "null".
func jsonbOrNil(raw json.RawMessage) interface{} {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return []byte(raw)
}

// jsonbBytes returns nil when raw is empty or the JSON null literal, else the
// raw bytes. Used where a []byte field is required instead of interface{}.
func jsonbBytes(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return []byte(raw)
}

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

// requireReadAuth verifies X-Service-Key against registry_service_accounts.
// Any valid service account (any role) is accepted.
// Returns the caller's service name and true on success.
func requireReadAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	return requireAuthWithRole(w, r, "")
}

// requireAdminAuth verifies X-Service-Key and requires role=admin.
// Returns the caller's service name and true on success.
func requireAdminAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	return requireAuthWithRole(w, r, "admin")
}

func requireAuthWithRole(w http.ResponseWriter, r *http.Request, requiredRole string) (string, bool) {
	header := r.Header.Get("X-Service-Key")
	if header == "" {
		http.Error(w, "missing X-Service-Key header", http.StatusUnauthorized)
		return "", false
	}
	idx := strings.Index(header, ":")
	if idx < 1 {
		http.Error(w, "invalid X-Service-Key format, expected name:key", http.StatusUnauthorized)
		return "", false
	}
	name, key := header[:idx], header[idx+1:]

	var acct ServiceAccountModel
	if err := connect().WithContext(r.Context()).Where("name = ?", name).First(&acct).Error; err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	if bcrypt.CompareHashAndPassword([]byte(acct.HashedKey), []byte(key)) != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	if requiredRole != "" && acct.Role != requiredRole {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}
	return name, true
}

// validateServiceURL rejects URLs that target loopback, link-local, or any
// private/reserved address (IPv4 RFC-1918, IPv6 ULA fc00::/7, etc.) to prevent
// SSRF via the service registry.
func validateServiceURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	host := u.Hostname()

	// checkIP rejects any address that is loopback, link-local, or private.
	// net.IP.IsPrivate covers IPv4 RFC-1918 and IPv6 ULA (fc00::/7).
	checkIP := func(ip net.IP) error {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
			return fmt.Errorf("URL must not target a private or reserved address")
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

// handleListActions returns all active workflow actions joined with their service URLs.
func handleListActions(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleListActions")
	defer span.End()

	caller, ok := requireReadAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	sqlRows, err := connect().WithContext(ctx).Raw(`
		SELECT sa.action_id, sa.service_id, s.name, s.url,
		       sa.name, sa.method, sa.path,
		       sa.body_transforms, sa.async_config,
		       sa.active, sa.created_at, sa.updated_at,
		       COALESCE(se.action, ''), COALESCE(se.resource, '')
		FROM service_actions sa
		JOIN services s ON sa.service_id = s.service_id
		LEFT JOIN service_endpoints se
		       ON se.service_id = sa.service_id
		      AND se.method     = sa.method
		      AND se.path       = sa.path
		      AND se.active     = true
		WHERE sa.active = true AND s.active = true
		ORDER BY sa.name
	`).Rows()
	if err != nil {
		slog.Error("list actions: query", "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer sqlRows.Close()

	var actions []ServiceAction
	for sqlRows.Next() {
		var a ServiceAction
		var bodyTransforms, asyncConfig []byte
		if err := sqlRows.Scan(
			&a.ActionID, &a.ServiceID, &a.ServiceName, &a.ServiceURL,
			&a.Name, &a.Method, &a.Path,
			&bodyTransforms, &asyncConfig,
			&a.Active, &a.CreatedAt, &a.UpdatedAt,
			&a.GkAction, &a.GkResource,
		); err != nil {
			slog.Error("list actions: scan", "error", err)
			continue
		}
		a.BodyTransforms = json.RawMessage(bodyTransforms)
		a.AsyncConfig = json.RawMessage(asyncConfig)
		if a.GkAction != "" {
			a.GkService = a.ServiceName
		}
		actions = append(actions, a)
	}
	if sqlRows.Err() != nil {
		slog.Error("list actions: rows", "error", sqlRows.Err())
		span.RecordError(sqlRows.Err())
		span.SetStatus(codes.Error, "db rows error")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if actions == nil {
		actions = []ServiceAction{}
	}

	span.SetAttributes(attribute.Int("actions.count", len(actions)))
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(actions) //nolint:errcheck
}

// handleListServices returns all active services with their endpoint manifests.
func handleListServices(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleListServices")
	defer span.End()

	caller, ok := requireReadAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	svcSQLRows, err := connect().WithContext(ctx).Raw(
		`SELECT service_id, name, url, description, forward_auth, active, created_at, updated_at
		 FROM services WHERE active = true ORDER BY name`).Rows()
	if err != nil {
		slog.Error("list services: query", "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer svcSQLRows.Close()

	var svcs []Service
	for svcSQLRows.Next() {
		var s Service
		if err := svcSQLRows.Scan(&s.ServiceID, &s.Name, &s.URL, &s.Description, &s.ForwardAuth, &s.Active, &s.CreatedAt, &s.UpdatedAt); err != nil {
			slog.Error("list services: scan", "error", err)
			continue
		}
		svcs = append(svcs, s)
	}
	if svcSQLRows.Err() != nil {
		slog.Error("list services: rows", "error", svcSQLRows.Err())
		span.RecordError(svcSQLRows.Err())
		span.SetStatus(codes.Error, "db rows error")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Collect service IDs for batch sub-queries — avoids N+1 round-trips.
	svcIndex := make(map[string]int, len(svcs))
	svcIDs := make([]string, len(svcs))
	result := make([]serviceWithEndpoints, len(svcs))
	for i, s := range svcs {
		result[i] = serviceWithEndpoints{Service: s, Roles: []ServiceRole{}, Endpoints: []ServiceEndpoint{}}
		svcIDs[i] = s.ServiceID
		svcIndex[s.ServiceID] = i
	}

	if len(svcIDs) > 0 {
		roleRows, err := connect().WithContext(ctx).Raw(
			`SELECT role_id, service_id, name, description, created_at
			 FROM service_roles WHERE service_id IN (?) ORDER BY service_id, name`,
			svcIDs).Rows()
		if err != nil {
			slog.Error("list services: query roles", "error", err)
		} else {
			for roleRows.Next() {
				var sr ServiceRole
				if err := roleRows.Scan(&sr.RoleID, &sr.ServiceID, &sr.Name, &sr.Description, &sr.CreatedAt); err != nil {
					slog.Error("list services: scan role", "error", err)
					continue
				}
				if i, ok := svcIndex[sr.ServiceID]; ok {
					result[i].Roles = append(result[i].Roles, sr)
				}
			}
			roleRows.Close()
		}

		epRows, err := connect().WithContext(ctx).Raw(
			`SELECT endpoint_id, service_id, method, path, action, resource, public, active, created_at, updated_at
			 FROM service_endpoints WHERE service_id IN (?) AND active = true ORDER BY service_id`,
			svcIDs).Rows()
		if err != nil {
			slog.Error("list services: query endpoints", "error", err)
		} else {
			for epRows.Next() {
				var ep ServiceEndpoint
				if err := epRows.Scan(&ep.EndpointID, &ep.ServiceID, &ep.Method, &ep.Path,
					&ep.Action, &ep.Resource, &ep.Public, &ep.Active, &ep.CreatedAt, &ep.UpdatedAt); err != nil {
					slog.Error("list services: scan endpoint", "error", err)
					continue
				}
				if i, ok := svcIndex[ep.ServiceID]; ok {
					result[i].Endpoints = append(result[i].Endpoints, ep)
				}
			}
			epRows.Close()
		}
	}

	span.SetAttributes(attribute.Int("services.count", len(svcs)))
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleCreateService registers a new service entry (admin key required).
func handleCreateService(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleCreateService")
	defer span.End()

	caller, ok := requireAdminAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	var req struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		Description string `json:"description"`
		ForwardAuth bool   `json:"forward_auth"`
		ServiceKey  string `json:"service_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.URL == "" {
		span.SetStatus(codes.Error, "bad request")
		http.Error(w, "name and url are required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("service.name", req.Name))

	if err := validateServiceURL(req.URL); err != nil {
		span.SetStatus(codes.Error, "invalid url")
		http.Error(w, "invalid url: "+err.Error(), http.StatusBadRequest)
		return
	}

	hashedKey, err := hashServiceKey(req.ServiceKey)
	if err != nil {
		slog.Error("create service: hash key", "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "hash key failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	id := uuid.New().String()
	newSvcModel := ServiceModel{
		ServiceID:   id,
		Name:        req.Name,
		URL:         req.URL,
		Description: req.Description,
		ForwardAuth: req.ForwardAuth,
		ServiceKey:  hashedKey,
	}
	if err := connect().WithContext(ctx).Create(&newSvcModel).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			span.SetStatus(codes.Error, "service already registered")
			http.Error(w, "service already registered", http.StatusConflict)
			return
		}
		slog.Error("create service: db", "error", err, "service_name", req.Name)
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(attribute.String("service.id", id))

	var svc Service
	var fetchedModel ServiceModel
	if err := connect().WithContext(ctx).Where("service_id = ?", id).First(&fetchedModel).Error; err != nil {
		slog.Error("create service: fetch after insert", "error", err, "service_id", id)
		span.RecordError(err)
		span.SetStatus(codes.Error, "fetch after insert failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	svc = Service{
		ServiceID:   fetchedModel.ServiceID,
		Name:        fetchedModel.Name,
		URL:         fetchedModel.URL,
		Description: fetchedModel.Description,
		ForwardAuth: fetchedModel.ForwardAuth,
		Active:      fetchedModel.Active,
		CreatedAt:   fetchedModel.CreatedAt,
		UpdatedAt:   fetchedModel.UpdatedAt,
	}

	slog.Info("service registered", "name", svc.Name)
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(svc)
}

// handleDeleteService soft-deletes a service by ID (admin key required).
func handleDeleteService(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleDeleteService")
	defer span.End()

	caller, ok := requireAdminAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	id := r.PathValue("id")
	span.SetAttributes(attribute.String("service.id", id))

	delResult := connect().WithContext(ctx).Exec(
		`UPDATE services SET active = false, updated_at = now() WHERE service_id = ? AND active = true`, id)
	if delResult.Error != nil {
		slog.Error("delete service: db", "error", delResult.Error, "service_id", id)
		span.RecordError(delResult.Error)
		span.SetStatus(codes.Error, "db update failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if delResult.RowsAffected == 0 {
		span.SetStatus(codes.Error, "not found")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateServiceEndpoints replaces the full endpoint manifest for a service
// (admin key required). Used to seed endpoint definitions without self-registration.
func handleUpdateServiceEndpoints(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleUpdateServiceEndpoints")
	defer span.End()

	caller, ok := requireAdminAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	id := r.PathValue("id")
	span.SetAttributes(attribute.String("service.id", id))

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
		Actions []struct {
			Name           string          `json:"name"`
			Method         string          `json:"method"`
			Path           string          `json:"path"`
			BodyTransforms json.RawMessage `json:"body_transforms,omitempty"`
			Async          json.RawMessage `json:"async,omitempty"`
		} `json:"actions"`
		DefaultGrants []struct {
			GrantOn   string   `json:"grant_on"`
			Actions   []string `json:"actions"`
			Resources []string `json:"resources"`
		} `json:"default_grants"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.SetStatus(codes.Error, "bad request")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.URL != "" {
		if err := validateServiceURL(req.URL); err != nil {
			span.SetStatus(codes.Error, "invalid url")
			http.Error(w, "invalid url: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		span.RecordError(tx.Error)
		span.SetStatus(codes.Error, "begin tx failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	// Verify service exists.
	var exists bool
	existsResult := tx.Raw(`SELECT true FROM services WHERE service_id = ? AND active = true`, id).Scan(&exists)
	if existsResult.Error != nil || existsResult.RowsAffected == 0 {
		span.SetStatus(codes.Error, "not found")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if req.URL != "" {
		if err := tx.Exec(`UPDATE services SET url = ?, updated_at = now() WHERE service_id = ?`, req.URL, id).Error; err != nil {
			slog.Error("update endpoints: set url", "error", err, "service_id", id)
			span.RecordError(err)
			span.SetStatus(codes.Error, "set url failed")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}
	if req.Description != "" {
		if err := tx.Exec(`UPDATE services SET description = ?, updated_at = now() WHERE service_id = ?`, req.Description, id).Error; err != nil {
			slog.Error("update endpoints: set description", "error", err, "service_id", id)
			span.RecordError(err)
			span.SetStatus(codes.Error, "set description failed")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Exec(`DELETE FROM service_roles WHERE service_id = ?`, id).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "delete roles failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	for _, role := range req.Roles {
		if role.Name == "" {
			continue
		}
		if err := tx.Exec(
			`INSERT INTO service_roles (role_id, service_id, name, description) VALUES (?, ?, ?, ?)`,
			uuid.New().String(), id, role.Name, role.Description).Error; err != nil {
			slog.Error("update endpoints: insert role", "error", err, "service_id", id)
			span.RecordError(err)
			span.SetStatus(codes.Error, "insert role failed")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Exec(`DELETE FROM service_endpoints WHERE service_id = ?`, id).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "delete endpoints failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	for _, ep := range req.Endpoints {
		if ep.Method == "" || ep.Path == "" || ep.Action == "" || ep.Resource == "" {
			continue
		}
		if err := tx.Exec(
			`INSERT INTO service_endpoints (endpoint_id, service_id, method, path, action, resource, public) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			uuid.New().String(), id, ep.Method, ep.Path, ep.Action, ep.Resource, ep.Public).Error; err != nil {
			slog.Error("update endpoints: insert endpoint", "error", err, "service_id", id)
			span.RecordError(err)
			span.SetStatus(codes.Error, "insert endpoint failed")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Exec(`DELETE FROM service_actions WHERE service_id = ?`, id).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "delete actions failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	for _, a := range req.Actions {
		if a.Name == "" || a.Method == "" || a.Path == "" {
			continue
		}
		if err := tx.Exec(
			`INSERT INTO service_actions (action_id, service_id, name, method, path, body_transforms, async_config)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			uuid.New().String(), id, a.Name, a.Method, a.Path,
			jsonbBytes(a.BodyTransforms), jsonbBytes(a.Async)).Error; err != nil {
			slog.Error("update endpoints: insert action", "error", err, "service_id", id, "action_name", a.Name)
			span.RecordError(err)
			span.SetStatus(codes.Error, "insert action failed")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	if len(req.DefaultGrants) > 0 {
		if err := tx.Exec(`DELETE FROM service_default_grants WHERE service_id = ?`, id).Error; err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "delete default grants failed")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		for _, g := range req.DefaultGrants {
			if g.GrantOn == "" || len(g.Actions) == 0 || len(g.Resources) == 0 {
				continue
			}
			actionsJSON, _ := json.Marshal(g.Actions)
			resourcesJSON, _ := json.Marshal(g.Resources)
			if err := tx.Exec(
				`INSERT INTO service_default_grants (grant_id, service_id, grant_on, actions, resources)
				 VALUES (?, ?, ?, ?, ?)`,
				uuid.New().String(), id, g.GrantOn, actionsJSON, resourcesJSON).Error; err != nil {
				slog.Error("update endpoints: insert default grant", "error", err, "service_id", id)
				span.RecordError(err)
				span.SetStatus(codes.Error, "insert default grant failed")
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
		}
	}

	if err := tx.Commit().Error; err != nil {
		slog.Error("update endpoints: commit", "error", err, "service_id", id)
		span.RecordError(err)
		span.SetStatus(codes.Error, "commit failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleListDefaultGrants returns all active default grants across all services,
// enriched with the service name so callers don't need a second lookup.
// Requires a valid read service key.
func handleListDefaultGrants(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("registry").Start(r.Context(), "handleListDefaultGrants")
	defer span.End()

	caller, ok := requireReadAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	grantSQLRows, err := connect().WithContext(ctx).Raw(
		`SELECT g.grant_id, g.service_id, s.name, g.grant_on, g.actions, g.resources, g.created_at, g.updated_at
		 FROM service_default_grants g
		 JOIN services s ON s.service_id = g.service_id AND s.active = true
		 ORDER BY s.name, g.grant_on`,
	).Rows()
	if err != nil {
		slog.Error("list default grants: db", "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer grantSQLRows.Close()

	var grants []ServiceDefaultGrant
	for grantSQLRows.Next() {
		var g ServiceDefaultGrant
		var actionsRaw, resourcesRaw []byte
		if err := grantSQLRows.Scan(&g.GrantID, &g.ServiceID, &g.ServiceName, &g.GrantOn,
			&actionsRaw, &resourcesRaw, &g.CreatedAt, &g.UpdatedAt); err != nil {
			slog.Error("list default grants: scan", "error", err)
			span.RecordError(err)
			span.SetStatus(codes.Error, "scan failed")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		json.Unmarshal(actionsRaw, &g.Actions)     //nolint:errcheck
		json.Unmarshal(resourcesRaw, &g.Resources) //nolint:errcheck
		grants = append(grants, g)
	}
	if grants == nil {
		grants = []ServiceDefaultGrant{}
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(grants) //nolint:errcheck
}

// ── System health ─────────────────────────────────────────────────────────────

var registryHTTPClient = &http.Client{
	Transport: otelhttp.NewTransport(http.DefaultTransport),
	Timeout:   5 * time.Second,
}

var (
	healthMu    sync.RWMutex
	healthCache map[string]serviceHealth
)

type serviceHealth struct {
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// startHealthCollector queries active services from the DB every 30 seconds,
// calls /healthz on each concurrently, and caches the results for handleSystemHealth.
func startHealthCollector(ctx context.Context) {
	collect := func() {
		healthRows, err := connect().WithContext(ctx).Raw(`SELECT name, url FROM services WHERE active = true`).Rows()
		if err != nil {
			slog.Warn("health collector: db query failed", "error", err)
			return
		}
		type svc struct{ name, url string }
		var svcs []svc
		for healthRows.Next() {
			var s svc
			if err := healthRows.Scan(&s.name, &s.url); err == nil {
				svcs = append(svcs, s)
			}
		}
		healthRows.Close()

		results := make(map[string]serviceHealth, len(svcs))
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, s := range svcs {
			wg.Add(1)
			go func(name, svcURL string) {
				defer wg.Done()
				hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				now := time.Now().UTC()
				req, err := http.NewRequestWithContext(hctx, http.MethodGet, svcURL+"/healthz", nil)
				if err != nil {
					mu.Lock()
					results[name] = serviceHealth{Status: "unhealthy", Error: err.Error(), CheckedAt: now}
					mu.Unlock()
					return
				}
				resp, err := registryHTTPClient.Do(req)
				if err != nil {
					mu.Lock()
					results[name] = serviceHealth{Status: "unhealthy", Error: err.Error(), CheckedAt: now}
					mu.Unlock()
					return
				}
				resp.Body.Close()
				mu.Lock()
				if resp.StatusCode == http.StatusOK {
					results[name] = serviceHealth{Status: "healthy", CheckedAt: now}
				} else {
					results[name] = serviceHealth{Status: "unhealthy", Error: fmt.Sprintf("status %d", resp.StatusCode), CheckedAt: now}
				}
				mu.Unlock()
			}(s.name, s.url)
		}
		wg.Wait()

		healthMu.Lock()
		healthCache = results
		healthMu.Unlock()
	}

	go func() {
		collect()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				collect()
			}
		}
	}()
}

func handleSystemHealth(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer("registry").Start(r.Context(), "handleSystemHealth")
	defer span.End()

	caller, ok := requireReadAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}
	span.SetAttributes(attribute.String("caller.service", caller))

	healthMu.RLock()
	cache := healthCache
	healthMu.RUnlock()

	overall := "healthy"
	for _, h := range cache {
		if h.Status != "healthy" {
			overall = "degraded"
			break
		}
	}

	span.SetAttributes(attribute.String("health.status", overall))
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct { //nolint:errcheck
		Status   string                   `json:"status"`
		Services map[string]serviceHealth `json:"services"`
	}{Status: overall, Services: cache})
}
