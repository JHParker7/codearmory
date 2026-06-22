package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func getSetupStatus(t *testing.T) setupStatusResponse {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/setup/status", nil)
	w := httptest.NewRecorder()
	handleSetupStatus(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("setup status: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp setupStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode setup status: %v", err)
	}
	return resp
}

func TestHandleSetupStatus_FalseWhenNoUsers(t *testing.T) {
	useIsolatedDB(t)

	if resp := getSetupStatus(t); resp.Initialized {
		t.Fatal("expected initialized=false on an empty instance")
	}
}

func TestHandleSetupStatus_TrueAfterFirstSignup(t *testing.T) {
	useIsolatedDB(t)

	if resp := getSetupStatus(t); resp.Initialized {
		t.Fatal("precondition: expected initialized=false before any signup")
	}

	w := doSignup(t, signupRequest{Email: "admin@test.com", Username: "admin-user", Password: "password123"})
	if w.Code != http.StatusCreated {
		t.Fatalf("signup: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	if resp := getSetupStatus(t); !resp.Initialized {
		t.Fatal("expected initialized=true once a user exists")
	}
}

func TestHandleSetupStatus_TrueWhenOnlyInactiveUsers(t *testing.T) {
	useIsolatedDB(t)

	w := doSignup(t, signupRequest{Email: "admin@test.com", Username: "admin-user", Password: "password123"})
	if w.Code != http.StatusCreated {
		t.Fatalf("signup: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	// Deactivate every account. The instance is still bootstrapped, so setup status
	// must stay true — otherwise it would re-route to first-run setup and the next
	// signup would be granted bootstrap admin.
	if err := connect().Model(&User{}).Where("1 = 1").Update("active", false).Error; err != nil {
		t.Fatalf("deactivate users: %v", err)
	}
	if resp := getSetupStatus(t); !resp.Initialized {
		t.Fatal("expected initialized=true with only inactive users (instance already bootstrapped)")
	}
}
