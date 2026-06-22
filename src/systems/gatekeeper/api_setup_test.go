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
