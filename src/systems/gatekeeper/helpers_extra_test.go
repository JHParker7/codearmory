package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestValidateVaultAddress(t *testing.T) {
	bad := []string{"://nope", "ftp://example.com", "http://127.0.0.1", "https://10.0.0.1", "http://169.254.1.1"}
	for _, u := range bad {
		if err := validateVaultAddress(u); err == nil {
			t.Errorf("validateVaultAddress(%q) = nil, want error", u)
		}
	}
	if err := validateVaultAddress("https://1.1.1.1"); err != nil {
		t.Errorf("public IP should be allowed: %v", err)
	}
}

func TestExtractClientCredentials(t *testing.T) {
	// Basic auth.
	r := httptest.NewRequest(http.MethodPost, "/oauth/token", nil)
	r.SetBasicAuth("cid", "csec")
	if id, sec, ok := extractClientCredentials(r); !ok || id != "cid" || sec != "csec" {
		t.Errorf("basic: %q %q %v", id, sec, ok)
	}
	// Form values.
	r2 := httptest.NewRequest(http.MethodPost, "/oauth/token", nil)
	r2.Form = url.Values{"client_id": {"fid"}, "client_secret": {"fsec"}}
	if id, sec, ok := extractClientCredentials(r2); !ok || id != "fid" || sec != "fsec" {
		t.Errorf("form: %q %q %v", id, sec, ok)
	}
	// Neither.
	r3 := httptest.NewRequest(http.MethodPost, "/oauth/token", nil)
	r3.Form = url.Values{}
	if _, _, ok := extractClientCredentials(r3); ok {
		t.Error("no creds should be ok=false")
	}
}

func TestOAuthRedirectError(t *testing.T) {
	w := httptest.NewRecorder()
	oauthRedirectError(w, httptest.NewRequest(http.MethodGet, "/x", nil), "https://app/cb", "st8", "access_denied", "nope")
	if w.Code != http.StatusFound {
		t.Fatalf("got %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	u, _ := url.Parse(loc)
	if u.Query().Get("error") != "access_denied" || u.Query().Get("state") != "st8" {
		t.Errorf("redirect query wrong: %s", loc)
	}
	// Invalid redirect URI → 400.
	w2 := httptest.NewRecorder()
	oauthRedirectError(w2, httptest.NewRequest(http.MethodGet, "/x", nil), "://bad", "", "e", "")
	if w2.Code != http.StatusBadRequest {
		t.Errorf("invalid redirect got %d, want 400", w2.Code)
	}
}

func TestInitTrustedProxies(t *testing.T) {
	orig := trustedProxyNets
	t.Cleanup(func() { trustedProxyNets = orig })
	trustedProxyNets = nil
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 192.168.0.0/16 ,bad-cidr")
	initTrustedProxies()
	if len(trustedProxyNets) != 2 {
		t.Errorf("parsed %d CIDRs, want 2 (bad one skipped)", len(trustedProxyNets))
	}
}

func TestInitAuditPermissionChecks(t *testing.T) {
	orig := auditPermissionChecks
	t.Cleanup(func() { auditPermissionChecks = orig })
	t.Setenv("AUDIT_PERMISSION_CHECKS", "true")
	initAuditPermissionChecks()
	if !auditPermissionChecks {
		t.Error("expected auditPermissionChecks=true")
	}
	t.Setenv("AUDIT_PERMISSION_CHECKS", "false")
	initAuditPermissionChecks()
	if auditPermissionChecks {
		t.Error("expected auditPermissionChecks=false")
	}
}

func TestInitCache_NoRedis(t *testing.T) {
	t.Setenv("REDIS_URL", "")
	initCache() // no REDIS_URL → in-process path, must not panic
}
