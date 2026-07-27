package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// resetSignupPolicy deletes the singleton policy row so the shared in-memory DB
// returns to the default (open registration) state after a test mutates it.
func resetSignupPolicy(t *testing.T) {
	t.Helper()
	if err := connect().Exec("DELETE FROM signup_policy").Error; err != nil {
		t.Fatalf("resetSignupPolicy: %v", err)
	}
}

// --- normalizeAllowlistValue ---

func TestNormalizeAllowlistValue(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"Alice@Example.com", "alice@example.com", false},
		{"  bob@example.com  ", "bob@example.com", false},
		{"@Example.com", "@example.com", false},
		{"@corp.internal.io", "@corp.internal.io", false},
		{"", "", true},
		{"not-an-email", "", true},
		{"@", "", true},
		{"@nodot", "", true},
		{"@bad domain.com", "", true},
	}
	for _, c := range cases {
		got, err := normalizeAllowlistValue(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("normalizeAllowlistValue(%q): expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeAllowlistValue(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeAllowlistValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- isSignupEmailAllowed ---

func TestIsSignupEmailAllowed(t *testing.T) {
	ctx := context.Background()
	exact := SignupAllowlistEntry{EntryID: uuid.New().String(), Email: "carol@allowed.com"}
	domain := SignupAllowlistEntry{EntryID: uuid.New().String(), Email: "@company.com"}
	if err := exact.Add(ctx); err != nil {
		t.Fatal(err)
	}
	if err := domain.Add(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exact.Remove(ctx); domain.Remove(ctx) })

	cases := []struct {
		email string
		want  bool
	}{
		{"carol@allowed.com", true},
		{"CAROL@allowed.com", true}, // case-insensitive exact
		{"dave@company.com", true},  // domain rule
		{"eve@COMPANY.com", true},   // case-insensitive domain
		{"frank@notallowed.com", false},
		{"carol@company.org", false}, // wrong domain
		{"noatsign", false},
	}
	for _, c := range cases {
		got, err := isSignupEmailAllowed(ctx, c.email)
		if err != nil {
			t.Fatalf("isSignupEmailAllowed(%q): %v", c.email, err)
		}
		if got != c.want {
			t.Errorf("isSignupEmailAllowed(%q) = %v, want %v", c.email, got, c.want)
		}
	}
}

// --- getSignupPolicy / setSignupPolicy ---

func TestSignupPolicyRoundTrip(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { resetSignupPolicy(t) })

	// Default (no row) is open registration.
	p, err := getSignupPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.InviteOnly {
		t.Fatalf("default policy should be open, got invite_only=true")
	}

	if err := setSignupPolicy(ctx, true, "admin-1"); err != nil {
		t.Fatal(err)
	}
	p, err = getSignupPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !p.InviteOnly {
		t.Fatalf("expected invite_only=true after set")
	}
	if p.UpdatedBy != "admin-1" {
		t.Fatalf("expected updated_by=admin-1, got %q", p.UpdatedBy)
	}

	// Flipping back to false must persist (no default tag drops the zero value).
	if err := setSignupPolicy(ctx, false, "admin-2"); err != nil {
		t.Fatal(err)
	}
	p, err = getSignupPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.InviteOnly {
		t.Fatalf("expected invite_only=false after flipping back")
	}
}

// --- handleCreateSignupAllowlist ---

func TestCreateSignupAllowlist_Success(t *testing.T) {
	actor := createAuthorizedUser(t, "createSignupAllowlist", "gatekeeper/signup-allowlist")
	b, _ := json.Marshal(signupAllowlistRequest{Email: "New@Example.com", Note: "hello"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/signup-allowlist", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateSignupAllowlist(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp SignupAllowlistEntry
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Cleanup(func() { resp.Remove(context.Background()) })
	if resp.Email != "new@example.com" {
		t.Fatalf("email not normalised: got %q", resp.Email)
	}
	if resp.CreatedBy != actor.UserID {
		t.Fatalf("created_by = %q, want %q", resp.CreatedBy, actor.UserID)
	}
}

func TestCreateSignupAllowlist_Duplicate(t *testing.T) {
	actor := createAuthorizedUser(t, "createSignupAllowlist", "gatekeeper/signup-allowlist")
	existing := SignupAllowlistEntry{EntryID: uuid.New().String(), Email: "dup@example.com"}
	if err := existing.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { existing.Remove(context.Background()) })

	b, _ := json.Marshal(signupAllowlistRequest{Email: "DUP@example.com"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/signup-allowlist", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateSignupAllowlist(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateSignupAllowlist_InvalidEmail(t *testing.T) {
	actor := createAuthorizedUser(t, "createSignupAllowlist", "gatekeeper/signup-allowlist")
	b, _ := json.Marshal(signupAllowlistRequest{Email: "garbage"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/signup-allowlist", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateSignupAllowlist(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateSignupAllowlist_Forbidden(t *testing.T) {
	actor := createTestUser(t)
	b, _ := json.Marshal(signupAllowlistRequest{Email: "x@example.com"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/signup-allowlist", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateSignupAllowlist(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

// --- handleListSignupAllowlist ---

func TestListSignupAllowlist_Success(t *testing.T) {
	entry := SignupAllowlistEntry{EntryID: uuid.New().String(), Email: "listed@example.com"}
	if err := entry.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { entry.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "listSignupAllowlist", "gatekeeper/signup-allowlist")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/signup-allowlist", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListSignupAllowlist(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []SignupAllowlistEntry
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, e := range resp {
		if e.EntryID == entry.EntryID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created entry not present in list of %d", len(resp))
	}
}

// --- handleDeleteSignupAllowlist ---

func TestDeleteSignupAllowlist_Success(t *testing.T) {
	entry := SignupAllowlistEntry{EntryID: uuid.New().String(), Email: "todelete@example.com"}
	if err := entry.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { entry.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "deleteSignupAllowlist", "gatekeeper/signup-allowlist/*")
	r := withUserID(httptest.NewRequest(http.MethodDelete, "/signup-allowlist/"+entry.EntryID, nil), actor.UserID)
	r.SetPathValue("id", entry.EntryID)
	w := httptest.NewRecorder()
	handleDeleteSignupAllowlist(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := (SignupAllowlistEntry{EntryID: entry.EntryID}).Get(context.Background()); err == nil {
		t.Fatalf("entry should be soft-deleted")
	}
}

func TestDeleteSignupAllowlist_NotFound(t *testing.T) {
	actor := createAuthorizedUser(t, "deleteSignupAllowlist", "gatekeeper/signup-allowlist/*")
	id := uuid.New().String()
	r := withUserID(httptest.NewRequest(http.MethodDelete, "/signup-allowlist/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeleteSignupAllowlist(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- handleGetSignupPolicy / handleUpdateSignupPolicy ---

func TestSignupPolicyHandlers(t *testing.T) {
	t.Cleanup(func() { resetSignupPolicy(t) })
	updater := createAuthorizedUser(t, "updateSignupPolicy", "gatekeeper/signup-policy")
	getter := createAuthorizedUser(t, "getSignupPolicy", "gatekeeper/signup-policy")

	// Turn invite-only on.
	b, _ := json.Marshal(signupPolicyRequest{InviteOnly: true})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/signup-policy", bytes.NewReader(b)), updater.UserID)
	w := httptest.NewRecorder()
	handleUpdateSignupPolicy(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("update expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Read it back.
	r = withUserID(httptest.NewRequest(http.MethodGet, "/signup-policy", nil), getter.UserID)
	w = httptest.NewRecorder()
	handleGetSignupPolicy(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("get expected 200, got %d", w.Code)
	}
	var resp SignupPolicy
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.InviteOnly {
		t.Fatalf("expected invite_only=true")
	}
}

func TestSignupPolicy_Forbidden(t *testing.T) {
	actor := createTestUser(t)
	b, _ := json.Marshal(signupPolicyRequest{InviteOnly: true})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/signup-policy", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleUpdateSignupPolicy(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

// --- signup gate enforcement ---

func TestHandleSignup_InviteOnly_Denied(t *testing.T) {
	// Ensure the instance is not empty so the bootstrap exemption never applies.
	createTestUser(t)
	if err := setSignupPolicy(context.Background(), true, "admin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetSignupPolicy(t) })

	req := signupRequest{Email: uuid.New().String() + "@blocked.com", Username: "u-" + uuid.New().String(), Password: "password123"}
	b, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleSignup(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-allowlisted email under invite-only, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleSignup_InviteOnly_AllowedExact(t *testing.T) {
	createTestUser(t)
	email := uuid.New().String() + "@allowed.com"
	entry := SignupAllowlistEntry{EntryID: uuid.New().String(), Email: normalizeSignupEmail(email)}
	if err := entry.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { entry.Remove(context.Background()) })
	if err := setSignupPolicy(context.Background(), true, "admin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetSignupPolicy(t) })

	req := signupRequest{Email: email, Username: "u-" + uuid.New().String(), Password: "password123"}
	b, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleSignup(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for allowlisted email, got %d: %s", w.Code, w.Body.String())
	}
	var resp signupResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	t.Cleanup(func() { cleanupSignup(t, resp.UserID) })
}

func TestHandleSignup_InviteOnly_AllowedDomain(t *testing.T) {
	createTestUser(t)
	entry := SignupAllowlistEntry{EntryID: uuid.New().String(), Email: "@allowed-domain.com"}
	if err := entry.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { entry.Remove(context.Background()) })
	if err := setSignupPolicy(context.Background(), true, "admin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetSignupPolicy(t) })

	req := signupRequest{Email: "someone-" + uuid.New().String() + "@allowed-domain.com", Username: "u-" + uuid.New().String(), Password: "password123"}
	b, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleSignup(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for email matching domain rule, got %d: %s", w.Code, w.Body.String())
	}
	var resp signupResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	t.Cleanup(func() { cleanupSignup(t, resp.UserID) })
}

// --- setup status exposes invite_only ---

func TestSetupStatus_ExposesInviteOnly(t *testing.T) {
	if err := setSignupPolicy(context.Background(), true, "admin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetSignupPolicy(t) })

	r := httptest.NewRequest(http.MethodGet, "/setup/status", nil)
	w := httptest.NewRecorder()
	handleSetupStatus(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp setupStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.InviteOnly {
		t.Fatalf("expected invite_only=true in setup status")
	}
}
