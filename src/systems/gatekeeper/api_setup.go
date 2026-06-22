package main

import (
	"encoding/json"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// setupStatusResponse reports whether the instance has completed first-run setup.
// Initialized is true once at least one active user exists.
type setupStatusResponse struct {
	Initialized bool `json:"initialized"`
}

// handleSetupStatus is a public (unauthenticated) endpoint reporting whether the
// instance has been bootstrapped yet — i.e. whether any active user account exists.
// The portal calls it on load with no session: when initialized is false it routes
// to the first-run setup page, which creates the very first account (made the
// bootstrap admin by createUserWithBootstrapAdmin). It deliberately returns only a
// boolean — never a user count — so it leaks nothing beyond "has setup happened".
//
// Note: when GATEKEEPER_ADMIN_EMAIL/PASSWORD seed an admin at startup, that account
// already exists, so this reports initialized=true and the portal skips setup.
func handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleSetupStatus")
	defer span.End()

	var count int64
	if err := connectRead().WithContext(ctx).Model(&User{}).Where("active = ?", true).Count(&count).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "failed to read setup status", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(setupStatusResponse{Initialized: count > 0}); err != nil {
		span.RecordError(err)
	}
}
