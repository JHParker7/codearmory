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

// handleListServices returns all active registered services. Any authenticated
// user may call this; Conductor uses its service account token to refresh its cache.
func handleListServices(w http.ResponseWriter, r *http.Request) {
	var svcs []Service
	if err := connectRead().WithContext(r.Context()).Where("active = ?", true).Find(&svcs).Error; err != nil {
		slog.Error("list services: db error", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(svcs)
}

// handleRegisterService creates a new service entry.
// Requires gatekeeper:createService:services permission.
func handleRegisterService(w http.ResponseWriter, r *http.Request) {
	if !requirePermission(w, r, "createService", "services") {
		return
	}
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.URL == "" {
		http.Error(w, "name and url are required", http.StatusBadRequest)
		return
	}

	svc := Service{ServiceID: uuid.New().String(), Name: req.Name, URL: req.URL, Active: true}
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
