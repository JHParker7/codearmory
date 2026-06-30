package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// registryNamePattern constrains registry names to path/query-safe characters,
// since names are used in /registries/{name} and the ?registry=<name> selector.
var registryNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// registryInput is the request body for create/update. Pointer fields on update
// distinguish "omitted" (leave unchanged) from "set to empty".
type registryInput struct {
	Name      *string `json:"name"`
	URL       *string `json:"url"`
	Username  *string `json:"username"`
	Password  *string `json:"password"`
	IsDefault *bool   `json:"is_default"`
}

func validRegistryURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// registryForRead resolves the registry a read request targets — the one named
// by ?registry=<name>, or the default when omitted — writing the appropriate
// HTTP error and returning false when it cannot be resolved.
func registryForRead(ctx context.Context, w http.ResponseWriter, r *http.Request) (*registryClient, bool) {
	reg, err := resolveRegistry(ctx, r.URL.Query().Get("registry"))
	if err == nil {
		return reg, true
	}
	switch {
	case errors.Is(err, errNoRegistry):
		http.Error(w, "no container registry configured", http.StatusServiceUnavailable)
	case errors.Is(err, errRegistryNotFound):
		http.Error(w, "registry not found", http.StatusNotFound)
	default:
		slog.ErrorContext(ctx, "resolve registry: db error", "error", err)
		http.Error(w, "failed to resolve registry", http.StatusInternalServerError)
	}
	return nil, false
}

func handleListRegistries(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("containers").Start(r.Context(), "handleListRegistries")
	defer span.End()

	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listRegistry", "containers/registries"); !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	rgs, err := listRegistries(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list error")
		slog.ErrorContext(ctx, "list registries: db error", "error", err)
		http.Error(w, "failed to list registries", http.StatusInternalServerError)
		return
	}
	if rgs == nil {
		rgs = []Registry{}
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, rgs)
}

func handleGetRegistry(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("containers").Start(r.Context(), "handleGetRegistry")
	defer span.End()

	name := r.PathValue("name")
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getRegistry", "containers/registries/"+name); !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	rg, err := getRegistryByName(ctx, name)
	if err != nil {
		if errors.Is(err, errRegistryNotFound) {
			http.Error(w, "registry not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		http.Error(w, "failed to get registry", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, rg)
}

func handleCreateRegistry(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("containers").Start(r.Context(), "handleCreateRegistry")
	defer span.End()

	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createRegistry", "containers/registries")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	var in registryInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if in.Name == nil || !registryNamePattern.MatchString(*in.Name) {
		http.Error(w, "name is required and must match ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$", http.StatusBadRequest)
		return
	}
	if in.URL == nil || !validRegistryURL(*in.URL) {
		http.Error(w, "url is required and must be a valid http(s) URL", http.StatusBadRequest)
		return
	}

	if _, err := getRegistryByName(ctx, *in.Name); err == nil {
		http.Error(w, "a registry with that name already exists", http.StatusConflict)
		return
	} else if !errors.Is(err, errRegistryNotFound) {
		span.RecordError(err)
		http.Error(w, "failed to create registry", http.StatusInternalServerError)
		return
	}

	// The first registry is always the default regardless of the request, so
	// the service is never left with registries but no default.
	makeDefault := in.IsDefault != nil && *in.IsDefault
	if n, err := countRegistries(ctx); err == nil && n == 0 {
		makeDefault = true
	}

	now := time.Now().UTC()
	rg := Registry{
		ID:        newID(),
		Name:      *in.Name,
		URL:       strings.TrimRight(*in.URL, "/"),
		IsDefault: makeDefault,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if in.Username != nil {
		rg.Username = *in.Username
	}
	if in.Password != nil {
		rg.Password = *in.Password
	}

	if err := addRegistryTx(ctx, rg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "create registry: db error", "name", rg.Name, "error", err)
		http.Error(w, "failed to create registry", http.StatusInternalServerError)
		return
	}
	invalidateRegistryCache()
	span.SetAttributes(attribute.String("registry.name", rg.Name))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "registry created", "user_id", userID, "name", rg.Name, "url", rg.URL, "is_default", rg.IsDefault)
	writeJSON(w, http.StatusCreated, rg)
}

func handleUpdateRegistry(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("containers").Start(r.Context(), "handleUpdateRegistry")
	defer span.End()

	name := r.PathValue("name")
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateRegistry", "containers/registries/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	var in registryInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	rg, err := getRegistryByName(ctx, name)
	if err != nil {
		if errors.Is(err, errRegistryNotFound) {
			http.Error(w, "registry not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		http.Error(w, "failed to update registry", http.StatusInternalServerError)
		return
	}

	if in.Name != nil && *in.Name != rg.Name {
		if !registryNamePattern.MatchString(*in.Name) {
			http.Error(w, "name must match ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$", http.StatusBadRequest)
			return
		}
		if _, err := getRegistryByName(ctx, *in.Name); err == nil {
			http.Error(w, "a registry with that name already exists", http.StatusConflict)
			return
		} else if !errors.Is(err, errRegistryNotFound) {
			http.Error(w, "failed to update registry", http.StatusInternalServerError)
			return
		}
		rg.Name = *in.Name
	}
	if in.URL != nil {
		if !validRegistryURL(*in.URL) {
			http.Error(w, "url must be a valid http(s) URL", http.StatusBadRequest)
			return
		}
		rg.URL = strings.TrimRight(*in.URL, "/")
	}
	if in.Username != nil {
		rg.Username = *in.Username
	}
	if in.Password != nil {
		rg.Password = *in.Password
	}

	// Demoting the only default to non-default is rejected — there must always
	// be a default while any registry exists. Promotion goes through the
	// transactional setDefaultRegistry so exactly one row stays default.
	promote := in.IsDefault != nil && *in.IsDefault && !rg.IsDefault
	demote := in.IsDefault != nil && !*in.IsDefault && rg.IsDefault
	if demote {
		http.Error(w, "cannot unset the default; set another registry as default instead", http.StatusBadRequest)
		return
	}

	rg.UpdatedAt = time.Now().UTC()
	if err := rg.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "update registry: db error", "name", name, "error", err)
		http.Error(w, "failed to update registry", http.StatusInternalServerError)
		return
	}
	if promote {
		if err := setDefaultRegistry(ctx, rg.Name); err != nil {
			span.RecordError(err)
			http.Error(w, "failed to set default", http.StatusInternalServerError)
			return
		}
		rg.IsDefault = true
	}
	invalidateRegistryCache()
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "registry updated", "user_id", userID, "name", rg.Name)
	writeJSON(w, http.StatusOK, rg)
}

func handleDeleteRegistry(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("containers").Start(r.Context(), "handleDeleteRegistry")
	defer span.End()

	name := r.PathValue("name")
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteRegistry", "containers/registries/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	rg, err := getRegistryByName(ctx, name)
	if err != nil {
		if errors.Is(err, errRegistryNotFound) {
			http.Error(w, "registry not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		http.Error(w, "failed to delete registry", http.StatusInternalServerError)
		return
	}

	if err := rg.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "delete registry: db error", "name", name, "error", err)
		http.Error(w, "failed to delete registry", http.StatusInternalServerError)
		return
	}
	// If the deleted registry was the default, promote another so push/pull
	// keeps working as long as any registry remains.
	if rg.IsDefault {
		if err := promoteAnyDefault(ctx); err != nil {
			slog.ErrorContext(ctx, "delete registry: promote default failed", "error", err)
		}
	}
	invalidateRegistryCache()
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "registry deleted", "user_id", userID, "name", name)
	w.WriteHeader(http.StatusNoContent)
}
