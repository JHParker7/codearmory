package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// serviceWithEndpoints is the shape returned by GET /services.
// Embedding endpoints lets Conductor build its routing table in one call.
type serviceWithEndpoints struct {
	Service
	Endpoints []ServiceEndpoint `json:"endpoints"`
}

// handleListServices returns all active registered services with their endpoints.
// Any authenticated user may call this; Conductor uses its service account token
// to refresh its cache.
func handleListServices(w http.ResponseWriter, r *http.Request) {
	var svcs []Service
	if err := connectRead().WithContext(r.Context()).Where("active = ?", true).Find(&svcs).Error; err != nil {
		slog.Error("list services: db error", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	ids := make([]string, len(svcs))
	for i, s := range svcs {
		ids[i] = s.ServiceID
	}
	var endpoints []ServiceEndpoint
	if len(ids) > 0 {
		connectRead().WithContext(r.Context()).
			Where("service_id IN ? AND active = ?", ids, true).
			Find(&endpoints)
	}

	epMap := make(map[string][]ServiceEndpoint, len(svcs))
	for _, ep := range endpoints {
		epMap[ep.ServiceID] = append(epMap[ep.ServiceID], ep)
	}

	result := make([]serviceWithEndpoints, len(svcs))
	for i, s := range svcs {
		result[i] = serviceWithEndpoints{Service: s, Endpoints: epMap[s.ServiceID]}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleRegisterService creates a new service entry.
// Requires gatekeeper:createService:services permission.
// An optional service_key may be supplied; it is hashed and stored so the
// service can authenticate its own self-registration calls later.
func handleRegisterService(w http.ResponseWriter, r *http.Request) {
	if !requirePermission(w, r, "createService", "services") {
		return
	}
	var req struct {
		Name       string `json:"name"`
		URL        string `json:"url"`
		ServiceKey string `json:"service_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.URL == "" {
		http.Error(w, "name and url are required", http.StatusBadRequest)
		return
	}

	svc := Service{ServiceID: uuid.New().String(), Name: req.Name, URL: req.URL, Active: true}
	if req.ServiceKey != "" {
		hashed, err := bcrypt.GenerateFromPassword([]byte(req.ServiceKey), bcrypt.DefaultCost)
		if err != nil {
			slog.Error("register service: hash key", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		svc.ServiceKeyHash = string(hashed)
	}

	if err := connect().WithContext(r.Context()).Create(&svc).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			http.Error(w, "service already registered", http.StatusConflict)
			return
		}
		slog.Error("register service: db error", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("service registered", "name", svc.Name, "url", svc.URL)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(svc)
}

// handleDeleteService soft-deletes a service.
// Requires gatekeeper:deleteService:services permission.
func handleDeleteService(w http.ResponseWriter, r *http.Request) {
	if !requirePermission(w, r, "deleteService", "services") {
		return
	}
	serviceID := r.PathValue("id")
	result := connect().WithContext(r.Context()).
		Model(&Service{}).
		Where("service_id = ? AND active = ?", serviceID, true).
		Update("active", false)
	if result.Error != nil {
		slog.Error("delete service: db error", "error", result.Error)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result.RowsAffected == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleServiceSelfRegister is called by backend services at startup to confirm
// their URL and declare the endpoint permissions Conductor should enforce.
// Authentication uses the pre-shared service key (no user JWT required).
func handleServiceSelfRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		ServiceKey string `json:"service_key"`
		URL        string `json:"url"`
		Endpoints  []struct {
			Method   string `json:"method"`
			Path     string `json:"path"`
			Action   string `json:"action"`
			Resource string `json:"resource"`
		} `json:"endpoints"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.ServiceKey == "" {
		http.Error(w, "name and service_key are required", http.StatusBadRequest)
		return
	}

	var svc Service
	if err := connectRead().Where("name = ? AND active = ?", req.Name, true).First(&svc).Error; err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if svc.ServiceKeyHash == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(svc.ServiceKeyHash), []byte(req.ServiceKey)); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	db := connect().WithContext(r.Context())

	updates := map[string]any{"active": true}
	if req.URL != "" {
		updates["url"] = req.URL
	}
	db.Model(&svc).Updates(updates)

	// Replace the endpoint list atomically so stale entries don't linger.
	db.Where("service_id = ?", svc.ServiceID).Delete(&ServiceEndpoint{})
	for _, ep := range req.Endpoints {
		if ep.Method == "" || ep.Path == "" || ep.Action == "" || ep.Resource == "" {
			continue
		}
		db.Create(&ServiceEndpoint{
			EndpointID: uuid.New().String(),
			ServiceID:  svc.ServiceID,
			Method:     ep.Method,
			Path:       ep.Path,
			Action:     ep.Action,
			Resource:   ep.Resource,
			Active:     true,
		})
	}

	slog.Info("service self-registered", "name", svc.Name, "endpoints", len(req.Endpoints))
	w.WriteHeader(http.StatusNoContent)
}

// ensureConductorUser creates the Conductor service account at cold start if it
// does not yet exist. The account receives a role with a single read permission
// on the service registry so Conductor can call GET /services with its own token.
func ensureConductorUser(db *gorm.DB, email, password string) {
	var existing User
	if db.Where("email = ? AND active = ?", email, true).First(&existing).Error == nil {
		slog.Info("conductor service user already exists", "email", email)
		return
	}

	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Name:          "conductor-list-services",
		Service:       "gatekeeper",
		Actions:       []string{"listService"},
		Resources:     []string{"services"},
		OwnerID:       "system",
		Active:        true,
	}
	if err := db.Create(&perm).Error; err != nil {
		slog.Error("conductor setup: create permission", "error", err)
		return
	}

	roleID := uuid.New().String()
	if err := db.Create(&Role{
		RoleID:         roleID,
		PermissionsIDs: []string{perm.PermissionsID},
		OwnerID:        "system",
		Active:         true,
	}).Error; err != nil {
		slog.Error("conductor setup: create role", "error", err)
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		slog.Error("conductor setup: hash password", "error", err)
		return
	}
	user := User{
		UserID:         uuid.New().String(),
		Email:          email,
		HashedPassword: string(hashed),
		Username:       "conductor",
		Firstname:      "Conductor",
		Lastname:       "Service",
		RoleID:         &roleID,
		Active:         true,
	}
	if err := db.Create(&user).Error; err != nil {
		slog.Error("conductor setup: create user", "email", email, "error", err)
		return
	}
	slog.Info("conductor service user created", "email", email, "user_id", user.UserID)
}
