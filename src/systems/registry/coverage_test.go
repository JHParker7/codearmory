package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Pure helpers — no DB needed.
// ---------------------------------------------------------------------------

func TestJSONBOrNil(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantNil bool
	}{
		{"empty", "", true},
		{"null literal", "null", true},
		{"object", `{"a":1}`, false},
		{"array", `[1,2,3]`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jsonbOrNil(json.RawMessage(tt.in))
			if tt.wantNil {
				if got != nil {
					t.Fatalf("jsonbOrNil(%q) = %v, want nil", tt.in, got)
				}
				return
			}
			b, ok := got.([]byte)
			if !ok {
				t.Fatalf("jsonbOrNil(%q) returned %T, want []byte", tt.in, got)
			}
			if string(b) != tt.in {
				t.Fatalf("jsonbOrNil(%q) = %q, want %q", tt.in, b, tt.in)
			}
		})
	}
}

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("COV_TEST_ENV", "set-value")
	if got := envOrDefault("COV_TEST_ENV", "fallback"); got != "set-value" {
		t.Fatalf("envOrDefault with set var = %q, want set-value", got)
	}
	os.Unsetenv("COV_TEST_ENV")
	if got := envOrDefault("COV_TEST_ENV", "fallback"); got != "fallback" {
		t.Fatalf("envOrDefault with unset var = %q, want fallback", got)
	}
}

func TestSecret_EnvVar(t *testing.T) {
	t.Setenv("COV_SECRET", "plain-secret")
	os.Unsetenv("COV_SECRET_FILE")
	if got := secret("COV_SECRET"); got != "plain-secret" {
		t.Fatalf("secret from env = %q, want plain-secret", got)
	}
}

func TestSecret_FileTrimsTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sk")
	if err := os.WriteFile(p, []byte("file-secret\n\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	t.Setenv("COV_SECRET2_FILE", p)
	if got := secret("COV_SECRET2"); got != "file-secret" {
		t.Fatalf("secret from file = %q, want file-secret", got)
	}
}

func TestHashServiceKey_Verifiable(t *testing.T) {
	h, err := hashServiceKey("rotate-me")
	if err != nil {
		t.Fatalf("hashServiceKey: %v", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(h), []byte("rotate-me")) != nil {
		t.Fatal("hash does not verify against plaintext")
	}
	if bcrypt.CompareHashAndPassword([]byte(h), []byte("wrong")) == nil {
		t.Fatal("hash incorrectly verified a wrong plaintext")
	}
}

// ---------------------------------------------------------------------------
// validateServiceURL — table of SSRF / scheme / resolution outcomes.
// resolveHost is stubbed per-case; restored in cleanup.
// ---------------------------------------------------------------------------

func TestValidateServiceURL(t *testing.T) {
	orig := resolveHost
	t.Cleanup(func() { resolveHost = orig })

	tests := []struct {
		name         string
		url          string
		resolve      func(string) ([]string, error)
		allowPrivate bool
		wantErr      bool
	}{
		{"public ip ok", "http://93.184.216.34:80", nil, denyPrivate, false},
		{"https public host ok", "https://example.com", func(string) ([]string, error) { return []string{"93.184.216.34"}, nil }, denyPrivate, false},
		{"bad scheme ftp", "ftp://example.com", nil, denyPrivate, true},
		{"no scheme", "example.com:80", nil, denyPrivate, true},
		{"loopback literal", "http://127.0.0.1", nil, denyPrivate, true},
		{"private 10.x literal", "http://10.0.0.5:8080", nil, denyPrivate, true},
		{"private 192.168 literal", "http://192.168.1.1", nil, denyPrivate, true},
		{"link-local literal", "http://169.254.169.254", nil, denyPrivate, true},
		{"ipv6 ula literal", "http://[fc00::1]", nil, denyPrivate, true},
		{"ipv6 loopback literal", "http://[::1]", nil, denyPrivate, true},
		{"unresolvable host fails closed", "http://nope.invalid", func(string) ([]string, error) { return nil, errAlwaysFail }, denyPrivate, true},
		{"host resolves to private", "http://sneaky.example", func(string) ([]string, error) { return []string{"10.1.2.3"}, nil }, denyPrivate, true},
		{"host resolves to link-local metadata", "http://meta.example", func(string) ([]string, error) { return []string{"169.254.169.254"}, nil }, denyPrivate, true},
		{"host resolves public ok", "http://good.example", func(string) ([]string, error) { return []string{"93.184.216.34"}, nil }, denyPrivate, false},
		{"malformed url", "http://%zz", nil, denyPrivate, true},

		// An admin service key (builder) may register an in-cluster ClusterIP, which is
		// always RFC-1918 — but never loopback or cloud metadata, which are not service
		// addresses under any caller.
		{"trusted caller: cluster ip ok", "http://10.111.42.43:9002", nil, allowPrivate, false},
		{"trusted caller: cluster dns name ok", "http://ca-codearmory-git-factory:9002", func(string) ([]string, error) { return []string{"10.111.42.43"}, nil }, allowPrivate, false},
		{"trusted caller: ipv6 ula ok", "http://[fc00::1]", nil, allowPrivate, false},
		{"trusted caller: loopback still blocked", "http://127.0.0.1", nil, allowPrivate, true},
		{"trusted caller: metadata still blocked", "http://169.254.169.254", nil, allowPrivate, true},
		{"trusted caller: resolved metadata still blocked", "http://meta.example", func(string) ([]string, error) { return []string{"169.254.169.254"}, nil }, allowPrivate, true},
		{"trusted caller: unresolvable still fails closed", "http://nope.invalid", func(string) ([]string, error) { return nil, errAlwaysFail }, allowPrivate, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.resolve != nil {
				resolveHost = tt.resolve
			} else {
				resolveHost = orig
			}
			err := validateServiceURL(tt.url, tt.allowPrivate)
			if tt.wantErr && err == nil {
				t.Fatalf("validateServiceURL(%q) = nil, want error", tt.url)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateServiceURL(%q) = %v, want nil", tt.url, err)
			}
		})
	}
}

var errAlwaysFail = &resolveErr{}

type resolveErr struct{}

func (*resolveErr) Error() string { return "no such host" }

// ---------------------------------------------------------------------------
// Stub batch-entity CRUD methods — every one returns "not implemented".
// ---------------------------------------------------------------------------

func TestStubModelMethods_NotImplemented(t *testing.T) {
	ctx := context.Background()
	type stub interface {
		Add(context.Context) error
		Update(context.Context) error
		Remove(context.Context) error
		Get(context.Context) (db, error)
		List(context.Context, int, int) ([]db, error)
	}
	stubs := []stub{
		ServiceRoleModel{},
		ServiceEndpointModel{},
		ServiceActionModel{},
		ServiceDefaultGrantModel{},
	}
	for _, s := range stubs {
		if err := s.Add(ctx); err == nil {
			t.Errorf("%T.Add: expected not-implemented error", s)
		}
		if err := s.Update(ctx); err == nil {
			t.Errorf("%T.Update: expected not-implemented error", s)
		}
		if err := s.Remove(ctx); err == nil {
			t.Errorf("%T.Remove: expected not-implemented error", s)
		}
		if _, err := s.Get(ctx); err == nil {
			t.Errorf("%T.Get: expected not-implemented error", s)
		}
		if _, err := s.List(ctx, 0, 0); err == nil {
			t.Errorf("%T.List: expected not-implemented error", s)
		}
	}
}

// ---------------------------------------------------------------------------
// authenticateServiceKey — bootstrap-key fallback exercised against the DB.
// ---------------------------------------------------------------------------

func TestAuthenticateServiceKey_RotatedHashAndBootstrapFallback(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	name := "auth-" + uuid.New().String()
	// Seed a real account whose stored hash is the *rotated* key, while the
	// bootstrap key is remembered in seedServiceKeys (the recovery credential).
	rotatedHash, _ := bcrypt.GenerateFromPassword([]byte("rotated-key"), 4)
	if err := connect().Exec(
		`INSERT INTO registry_service_accounts (account_id, name, hashed_key, role) VALUES (?,?,?,?)`,
		uuid.New().String(), name, string(rotatedHash), "read").Error; err != nil {
		t.Fatalf("seed account: %v", err)
	}
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM registry_service_accounts WHERE name = ?`, name) //nolint:errcheck
	})

	prevSeed := seedServiceKeys
	seedServiceKeys = map[string]seedAccount{name: {key: "boot-key", role: "read"}}
	t.Cleanup(func() { seedServiceKeys = prevSeed })

	// 1. current rotated key authenticates via the DB hash.
	if role, ok := authenticateServiceKey(ctx, name, "rotated-key"); !ok || role != "read" {
		t.Fatalf("rotated key: ok=%v role=%q, want true read", ok, role)
	}
	// 2. bootstrap key authenticates via the seed fallback (recovery path).
	if role, ok := authenticateServiceKey(ctx, name, "boot-key"); !ok || role != "read" {
		t.Fatalf("bootstrap key: ok=%v role=%q, want true read", ok, role)
	}
	// 3. a key matching neither is rejected.
	if _, ok := authenticateServiceKey(ctx, name, "garbage"); ok {
		t.Fatal("garbage key should not authenticate")
	}
	// 4. an unknown account with no seed entry is rejected.
	if _, ok := authenticateServiceKey(ctx, "ghost-"+uuid.New().String(), "x"); ok {
		t.Fatal("unknown account should not authenticate")
	}
}

// ---------------------------------------------------------------------------
// requireAuthWithRole — role enforcement against real DB accounts.
// ---------------------------------------------------------------------------

func TestRequireAuthWithRole_ReadAccountForbiddenForAdmin(t *testing.T) {
	requireDB(t)
	// test-reader is a read account; requiring "admin" must 403.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Service-Key", testReadKey)
	w := httptest.NewRecorder()
	if _, ok := requireAuthWithRole(w, r, "admin"); ok {
		t.Fatal("read account should be forbidden for admin-required route")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestRequireAuthWithRole_BadKey401(t *testing.T) {
	requireDB(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Service-Key", "test-reader:wrong-password")
	w := httptest.NewRecorder()
	if _, ok := requireAuthWithRole(w, r, ""); ok {
		t.Fatal("wrong key should not authenticate")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestRequireAdminAuth_Success(t *testing.T) {
	requireDB(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	name, ok := requireAdminAuth(w, r)
	if !ok || name != "test-admin" {
		t.Fatalf("admin auth: ok=%v name=%q, want true test-admin", ok, name)
	}
}

// ---------------------------------------------------------------------------
// ServiceModel CRUD — Add / Get / Update / List / Remove against the DB.
// ---------------------------------------------------------------------------

func TestServiceModel_CRUD(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	id := uuid.New().String()
	name := "crud-" + uuid.New().String()
	m := ServiceModel{ServiceID: id, Name: name, URL: "http://svc:9000", Description: "d"}
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM services WHERE service_id = ?`, id) //nolint:errcheck
	})

	if err := m.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Get by ID.
	got, err := (ServiceModel{ServiceID: id}).Get(ctx)
	if err != nil {
		t.Fatalf("Get by id: %v", err)
	}
	if got.(ServiceModel).Name != name {
		t.Fatalf("Get name = %q, want %q", got.(ServiceModel).Name, name)
	}

	// Get by Name.
	gotByName, err := (ServiceModel{Name: name}).Get(ctx)
	if err != nil {
		t.Fatalf("Get by name: %v", err)
	}
	if gotByName.(ServiceModel).ServiceID != id {
		t.Fatalf("Get by name id = %q, want %q", gotByName.(ServiceModel).ServiceID, id)
	}

	// Update.
	upd := got.(ServiceModel)
	upd.Description = "updated"
	upd.URL = "http://svc:9001"
	if err := upd.Update(ctx); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reread, _ := (ServiceModel{ServiceID: id}).Get(ctx)
	if reread.(ServiceModel).Description != "updated" {
		t.Fatalf("after Update description = %q, want updated", reread.(ServiceModel).Description)
	}

	// List active services includes ours.
	rows, err := (ServiceModel{}).List(ctx, 0, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.(ServiceModel).ServiceID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("List did not include the active service")
	}

	// List with limit/offset paths execute.
	if _, err := (ServiceModel{}).List(ctx, 1, 1); err != nil {
		t.Fatalf("List with limit/offset: %v", err)
	}

	// Remove soft-deletes.
	if err := m.Remove(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Removing again returns not found (already inactive).
	if err := m.Remove(ctx); !isDbNotFound(err) {
		t.Fatalf("second Remove: %v, want not found", err)
	}
	// Get not found for a nonexistent id.
	if _, err := (ServiceModel{ServiceID: "no-such"}).Get(ctx); !isDbNotFound(err) {
		t.Fatalf("Get missing: %v, want not found", err)
	}
}

// ---------------------------------------------------------------------------
// ServiceAccountModel CRUD + lookupServiceAccount + rotateServiceKeyDB.
// ---------------------------------------------------------------------------

func TestServiceAccountModel_CRUD(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	name := "acct-" + uuid.New().String()
	hash, _ := bcrypt.GenerateFromPassword([]byte("key1"), 4)
	a := ServiceAccountModel{AccountID: uuid.New().String(), Name: name, HashedKey: string(hash), Role: "read"}
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM registry_service_accounts WHERE name = ?`, name) //nolint:errcheck
	})

	if err := a.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := (ServiceAccountModel{Name: name}).Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.(ServiceAccountModel).Role != "read" {
		t.Fatalf("role = %q, want read", got.(ServiceAccountModel).Role)
	}

	// lookupServiceAccount helper.
	la, err := lookupServiceAccount(ctx, name)
	if err != nil || la.Name != name {
		t.Fatalf("lookupServiceAccount: %v / %q", err, la.Name)
	}
	if _, err := lookupServiceAccount(ctx, "missing-"+uuid.New().String()); err == nil {
		t.Fatal("lookupServiceAccount for missing should error")
	}

	// Update role.
	upd := got.(ServiceAccountModel)
	upd.Role = "admin"
	if err := upd.Update(ctx); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// rotateServiceKeyDB swaps the stored hash.
	newHash, _ := bcrypt.GenerateFromPassword([]byte("key2"), 4)
	if err := rotateServiceKeyDB(ctx, name, string(newHash)); err != nil {
		t.Fatalf("rotateServiceKeyDB: %v", err)
	}
	after, _ := lookupServiceAccount(ctx, name)
	if bcrypt.CompareHashAndPassword([]byte(after.HashedKey), []byte("key2")) != nil {
		t.Fatal("rotated hash does not verify against new key")
	}

	// List includes our account.
	rows, err := (ServiceAccountModel{}).List(ctx, 0, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("List returned no accounts")
	}
	if _, err := (ServiceAccountModel{}).List(ctx, 1, 0); err != nil {
		t.Fatalf("List with limit: %v", err)
	}

	// Remove.
	if err := upd.Remove(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := (ServiceAccountModel{AccountID: upd.AccountID}).Remove(ctx); !isDbNotFound(err) {
		t.Fatalf("second Remove: %v, want not found", err)
	}
	if _, err := (ServiceAccountModel{Name: name}).Get(ctx); !isDbNotFound(err) {
		t.Fatalf("Get after remove: %v, want not found", err)
	}
}

// ---------------------------------------------------------------------------
// upsertServiceModelByName — insert then conflict-update path.
// ---------------------------------------------------------------------------

func TestUpsertServiceModelByName(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	name := "seed-" + uuid.New().String()
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM services WHERE name = ?`, name) //nolint:errcheck
	})

	// First call inserts.
	if err := upsertServiceModelByName(ctx, ServiceModel{ServiceID: uuid.New().String(), Name: name, URL: "http://a:1"}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	row, _ := (ServiceModel{Name: name}).Get(ctx)
	firstID := row.(ServiceModel).ServiceID

	// Soft-delete then upsert again: conflict path updates url + active=true.
	connect().Exec(`UPDATE services SET active = false WHERE name = ?`, name) //nolint:errcheck
	if err := upsertServiceModelByName(ctx, ServiceModel{ServiceID: uuid.New().String(), Name: name, URL: "http://b:2"}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	row2, _ := (ServiceModel{Name: name}).Get(ctx)
	got := row2.(ServiceModel)
	if got.ServiceID != firstID {
		t.Fatalf("conflict upsert changed id: %q -> %q", firstID, got.ServiceID)
	}
	if got.URL != "http://b:2" {
		t.Fatalf("conflict upsert url = %q, want http://b:2", got.URL)
	}
	if !got.Active {
		t.Fatal("conflict upsert should reactivate (active=true)")
	}
}

// ---------------------------------------------------------------------------
// listServicesWithEndpoints — full row + roles + endpoints population.
// ---------------------------------------------------------------------------

func TestListServicesWithEndpoints_Populated(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	name := "lswe-" + uuid.New().String()
	id := insertTestService(t, name)
	connect().Exec(`INSERT INTO service_roles (role_id, service_id, name, description) VALUES (?,?,?,?)`,
		uuid.New().String(), id, "viewer", "read-only") //nolint:errcheck
	connect().Exec(`INSERT INTO service_endpoints (endpoint_id, service_id, method, path, action, resource, public, active) VALUES (?,?,?,?,?,?,?,?)`,
		uuid.New().String(), id, "GET", "/x", "getX", "svc/x", false, true) //nolint:errcheck
	// An inactive endpoint must be excluded.
	connect().Exec(`INSERT INTO service_endpoints (endpoint_id, service_id, method, path, action, resource, public, active) VALUES (?,?,?,?,?,?,?,?)`,
		uuid.New().String(), id, "GET", "/old", "getOld", "svc/old", false, false) //nolint:errcheck

	result, err := listServicesWithEndpoints(ctx)
	if err != nil {
		t.Fatalf("listServicesWithEndpoints: %v", err)
	}
	var found *serviceWithEndpoints
	for i := range result {
		if result[i].ServiceID == id {
			found = &result[i]
		}
	}
	if found == nil {
		t.Fatal("service not present in result")
	}
	if len(found.Roles) != 1 || found.Roles[0].Name != "viewer" {
		t.Fatalf("roles = %+v, want one viewer", found.Roles)
	}
	if len(found.Endpoints) != 1 || found.Endpoints[0].Path != "/x" {
		t.Fatalf("endpoints = %+v, want one active /x", found.Endpoints)
	}
}

// ---------------------------------------------------------------------------
// listAllActions + listAllDefaultGrants + queryActiveServiceURLs.
// ---------------------------------------------------------------------------

func TestListAllActionsAndGrants(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	name := "act-" + uuid.New().String()
	id := insertTestService(t, name)
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM service_actions WHERE service_id = ?`, id)        //nolint:errcheck
		connect().Exec(`DELETE FROM service_default_grants WHERE service_id = ?`, id) //nolint:errcheck
	})

	// An action whose method/path matches a registered endpoint → gk_action populated.
	connect().Exec(`INSERT INTO service_endpoints (endpoint_id, service_id, method, path, action, resource, public, active) VALUES (?,?,?,?,?,?,?,?)`,
		uuid.New().String(), id, "POST", "/run", "runAction", "svc/run", false, true) //nolint:errcheck
	connect().Exec(`INSERT INTO service_actions (action_id, service_id, name, method, path, body_transforms, async_config) VALUES (?,?,?,?,?,?,?)`,
		uuid.New().String(), id, name+"/run", "POST", "/run", []byte(`[{"x":1}]`), []byte(`{"y":2}`)) //nolint:errcheck

	actions, err := listAllActions(ctx)
	if err != nil {
		t.Fatalf("listAllActions: %v", err)
	}
	var act *ServiceAction
	for i := range actions {
		if actions[i].ServiceID == id {
			act = &actions[i]
		}
	}
	if act == nil {
		t.Fatal("action not returned")
	}
	if act.GkAction != "runAction" || act.GkService != name {
		t.Fatalf("gk join: action=%q service=%q, want runAction %s", act.GkAction, act.GkService, name)
	}
	if len(act.BodyTransforms) == 0 || len(act.AsyncConfig) == 0 {
		t.Fatalf("jsonb blobs not scanned: bt=%q async=%q", act.BodyTransforms, act.AsyncConfig)
	}

	// Default grant.
	connect().Exec(`INSERT INTO service_default_grants (grant_id, service_id, grant_on, actions, resources) VALUES (?,?,?,?,?)`,
		uuid.New().String(), id, "user", []byte(`["a","b"]`), []byte(`["r1"]`)) //nolint:errcheck
	grants, err := listAllDefaultGrants(ctx)
	if err != nil {
		t.Fatalf("listAllDefaultGrants: %v", err)
	}
	var g *ServiceDefaultGrant
	for i := range grants {
		if grants[i].ServiceID == id {
			g = &grants[i]
		}
	}
	if g == nil {
		t.Fatal("default grant not returned")
	}
	if g.ServiceName != name || g.GrantOn != "user" {
		t.Fatalf("grant join: name=%q on=%q", g.ServiceName, g.GrantOn)
	}
	if len(g.Actions) != 2 || len(g.Resources) != 1 {
		t.Fatalf("grant actions/resources = %v / %v", g.Actions, g.Resources)
	}

	// queryActiveServiceURLs includes our service.
	urls, err := queryActiveServiceURLs(ctx)
	if err != nil {
		t.Fatalf("queryActiveServiceURLs: %v", err)
	}
	hit := false
	for _, u := range urls {
		if u.Name == name {
			hit = true
		}
	}
	if !hit {
		t.Fatal("queryActiveServiceURLs missing the active service")
	}
}

// ---------------------------------------------------------------------------
// replaceServiceManifest — atomic replace, 404 for missing/inactive, filtering.
// ---------------------------------------------------------------------------

func TestReplaceServiceManifest(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	name := "rsm-" + uuid.New().String()
	id := insertTestService(t, name)
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM service_actions WHERE service_id = ?`, id)        //nolint:errcheck
		connect().Exec(`DELETE FROM service_default_grants WHERE service_id = ?`, id) //nolint:errcheck
	})

	roles := []manifestRoleSpec{
		{Name: "admin", Description: "all"},
		{Name: "", Description: "skipped"}, // empty name → skipped
	}
	endpoints := []manifestEndpointSpec{
		{Method: "GET", Path: "/a", Action: "getA", Resource: "svc/a", Public: true},
		{Method: "", Path: "/bad"}, // incomplete → skipped
	}
	actions := []manifestActionEntry{
		{Name: name + "/run", Method: "POST", Path: "/run", BodyTransforms: json.RawMessage(`[{"k":1}]`)},
		{Name: "", Method: "GET", Path: "/skip"}, // incomplete → skipped
	}
	grants := []manifestDefaultGrant{
		{GrantOn: "user", Actions: []string{"getA"}, Resources: []string{"svc/a"}},
		{GrantOn: "", Actions: nil, Resources: nil}, // incomplete → skipped
	}

	if err := replaceServiceManifest(ctx, id, "http://updated:9000", "new-desc", roles, endpoints, actions, grants); err != nil {
		t.Fatalf("replaceServiceManifest: %v", err)
	}

	var roleCount, epCount, actCount, grantCount int64
	connect().Raw(`SELECT count(*) FROM service_roles WHERE service_id = ?`, id).Scan(&roleCount)              //nolint:errcheck
	connect().Raw(`SELECT count(*) FROM service_endpoints WHERE service_id = ?`, id).Scan(&epCount)            //nolint:errcheck
	connect().Raw(`SELECT count(*) FROM service_actions WHERE service_id = ?`, id).Scan(&actCount)             //nolint:errcheck
	connect().Raw(`SELECT count(*) FROM service_default_grants WHERE service_id = ?`, id).Scan(&grantCount)    //nolint:errcheck
	if roleCount != 1 || epCount != 1 || actCount != 1 || grantCount != 1 {
		t.Fatalf("counts roles=%d eps=%d actions=%d grants=%d, want 1 each", roleCount, epCount, actCount, grantCount)
	}

	var url, desc string
	connect().Raw(`SELECT url, description FROM services WHERE service_id = ?`, id).Row().Scan(&url, &desc) //nolint:errcheck
	if url != "http://updated:9000" || desc != "new-desc" {
		t.Fatalf("service not updated: url=%q desc=%q", url, desc)
	}

	// Calling again with empty url/desc and no grants leaves grants untouched (len(grants)==0 path).
	if err := replaceServiceManifest(ctx, id, "", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("replace (clear) : %v", err)
	}
	connect().Raw(`SELECT count(*) FROM service_roles WHERE service_id = ?`, id).Scan(&roleCount) //nolint:errcheck
	if roleCount != 0 {
		t.Fatalf("roles not cleared: %d", roleCount)
	}
	// grants are NOT cleared when grants arg is empty.
	connect().Raw(`SELECT count(*) FROM service_default_grants WHERE service_id = ?`, id).Scan(&grantCount) //nolint:errcheck
	if grantCount != 1 {
		t.Fatalf("grants should be preserved when grants arg empty, got %d", grantCount)
	}

	// 404 for a nonexistent service id.
	if err := replaceServiceManifest(ctx, uuid.New().String(), "", "", nil, nil, nil, nil); !isDbNotFound(err) {
		t.Fatalf("replace missing: %v, want not found", err)
	}
}

// ---------------------------------------------------------------------------
// loadManifestEntry + loadManifest — DB upsert of full entries.
// ---------------------------------------------------------------------------

func TestLoadManifestEntry_CreateThenUpdate(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	name := "lme-" + uuid.New().String()
	t.Cleanup(func() {
		var sid string
		connect().Raw(`SELECT service_id FROM services WHERE name = ?`, name).Row().Scan(&sid) //nolint:errcheck
		if sid != "" {
			connect().Exec(`DELETE FROM service_endpoints WHERE service_id = ?`, sid)      //nolint:errcheck
			connect().Exec(`DELETE FROM service_actions WHERE service_id = ?`, sid)        //nolint:errcheck
			connect().Exec(`DELETE FROM service_default_grants WHERE service_id = ?`, sid) //nolint:errcheck
			connect().Exec(`DELETE FROM service_roles WHERE service_id = ?`, sid)          //nolint:errcheck
		}
		connect().Exec(`DELETE FROM services WHERE name = ?`, name) //nolint:errcheck
	})

	entry := manifestEntry{
		Name:        name,
		URL:         "http://lme:1",
		Description: "first",
		ForwardAuth: true,
		ServiceKey:  "sk",
	}
	entry.Endpoints = append(entry.Endpoints,
		manifestEndpointSpec{Method: "GET", Path: "/p", Action: "getP", Resource: "svc/p"})
	entry.Actions = []manifestActionEntry{
		{Name: name + "/a", Method: "POST", Path: "/run", Async: json.RawMessage(`{"id_field":"x"}`)},
		{Name: "", Method: "GET", Path: "/skip"}, // skipped
	}
	entry.DefaultGrants = []manifestDefaultGrant{
		{GrantOn: "user", Actions: []string{"getP"}, Resources: []string{"svc/p"}},
		{GrantOn: "", Actions: nil, Resources: nil}, // skipped
	}

	// First load → create.
	loadManifestEntry(ctx, entry)
	var sid string
	if err := connect().Raw(`SELECT service_id FROM services WHERE name = ? AND active = true`, name).Row().Scan(&sid); err != nil {
		t.Fatalf("service not created: %v", err)
	}
	var epCount, actCount, grantCount int64
	connect().Raw(`SELECT count(*) FROM service_endpoints WHERE service_id = ?`, sid).Scan(&epCount)         //nolint:errcheck
	connect().Raw(`SELECT count(*) FROM service_actions WHERE service_id = ?`, sid).Scan(&actCount)          //nolint:errcheck
	connect().Raw(`SELECT count(*) FROM service_default_grants WHERE service_id = ?`, sid).Scan(&grantCount) //nolint:errcheck
	if epCount != 1 || actCount != 1 || grantCount != 1 {
		t.Fatalf("after create eps=%d actions=%d grants=%d, want 1 each", epCount, actCount, grantCount)
	}

	// Second load with same name → update path, preserves service_id, replaces children.
	entry.Description = "second"
	entry.Endpoints = nil
	loadManifestEntry(ctx, entry)
	var sid2, desc string
	connect().Raw(`SELECT service_id, description FROM services WHERE name = ?`, name).Row().Scan(&sid2, &desc) //nolint:errcheck
	if sid2 != sid {
		t.Fatalf("update changed service_id %q -> %q", sid, sid2)
	}
	if desc != "second" {
		t.Fatalf("description not updated: %q", desc)
	}
	connect().Raw(`SELECT count(*) FROM service_endpoints WHERE service_id = ?`, sid).Scan(&epCount) //nolint:errcheck
	if epCount != 0 {
		t.Fatalf("endpoints not replaced on update: %d", epCount)
	}
}

func TestLoadManifest_FileAndErrors(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	// Missing file → logged, no panic.
	loadManifest(ctx, filepath.Join(t.TempDir(), "nope.json"))

	// Invalid JSON → logged, no panic.
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{not json`), 0o600) //nolint:errcheck
	loadManifest(ctx, bad)

	// Valid manifest → entries loaded.
	name := "lm-" + uuid.New().String()
	t.Cleanup(func() {
		var sid string
		connect().Raw(`SELECT service_id FROM services WHERE name = ?`, name).Row().Scan(&sid) //nolint:errcheck
		if sid != "" {
			connect().Exec(`DELETE FROM service_endpoints WHERE service_id = ?`, sid) //nolint:errcheck
		}
		connect().Exec(`DELETE FROM services WHERE name = ?`, name) //nolint:errcheck
	})
	good := filepath.Join(t.TempDir(), "good.json")
	os.WriteFile(good, []byte(`[{"name":"`+name+`","url":"http://lm:1","endpoints":[{"method":"GET","path":"/h","action":"h","resource":"svc/h","public":true}]}]`), 0o600) //nolint:errcheck
	loadManifest(ctx, good)
	var cnt int64
	connect().Raw(`SELECT count(*) FROM services WHERE name = ? AND active = true`, name).Scan(&cnt) //nolint:errcheck
	if cnt != 1 {
		t.Fatalf("manifest service not loaded, count=%d", cnt)
	}
}

// ---------------------------------------------------------------------------
// handleRotateServiceKey — full flow incl. bootstrap fallback + auth failures.
// ---------------------------------------------------------------------------

func TestHandleRotateServiceKey(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	name := "rot-" + uuid.New().String()
	curHash, _ := bcrypt.GenerateFromPassword([]byte("current-key"), 4)
	connect().Exec(`INSERT INTO registry_service_accounts (account_id, name, hashed_key, role) VALUES (?,?,?,?)`,
		uuid.New().String(), name, string(curHash), "read") //nolint:errcheck
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM registry_service_accounts WHERE name = ?`, name) //nolint:errcheck
	})

	// Missing header → 401.
	w := httptest.NewRecorder()
	handleRotateServiceKey(w, httptest.NewRequest(http.MethodPost, "/service-accounts/rotate-key", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no header: got %d, want 401", w.Code)
	}

	// Malformed header (no colon) → 401.
	r := httptest.NewRequest(http.MethodPost, "/service-accounts/rotate-key", nil)
	r.Header.Set("X-Service-Key", "noколон")
	w = httptest.NewRecorder()
	handleRotateServiceKey(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("malformed header: got %d, want 401", w.Code)
	}

	// Wrong key → 401.
	r = httptest.NewRequest(http.MethodPost, "/service-accounts/rotate-key", nil)
	r.Header.Set("X-Service-Key", name+":bad")
	w = httptest.NewRecorder()
	handleRotateServiceKey(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad key: got %d, want 401", w.Code)
	}

	// Success with current key → 200, returns a new key that authenticates.
	r = httptest.NewRequest(http.MethodPost, "/service-accounts/rotate-key", nil)
	r.Header.Set("X-Service-Key", name+":current-key")
	w = httptest.NewRecorder()
	handleRotateServiceKey(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate: got %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	newKey := resp["key"]
	if newKey == "" {
		t.Fatal("rotate returned empty key")
	}
	if _, ok := authenticateServiceKey(ctx, name, newKey); !ok {
		t.Fatal("rotated key does not authenticate")
	}

	// Bootstrap fallback: the original current-key no longer matches the stored hash,
	// but a seeded bootstrap key still lets the account rotate (recovery path).
	prevSeed := seedServiceKeys
	seedServiceKeys = map[string]seedAccount{name: {key: "boot-recover", role: "read"}}
	t.Cleanup(func() { seedServiceKeys = prevSeed })
	r = httptest.NewRequest(http.MethodPost, "/service-accounts/rotate-key", nil)
	r.Header.Set("X-Service-Key", name+":boot-recover")
	w = httptest.NewRecorder()
	handleRotateServiceKey(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("bootstrap rotate: got %d, want 200: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleListActions / handleListDefaultGrants — authorized DB-backed success.
// ---------------------------------------------------------------------------

func TestHandleListActions_Success(t *testing.T) {
	requireDB(t)
	r := httptest.NewRequest(http.MethodGet, "/actions", nil)
	r.Header.Set("X-Service-Key", testReadKey)
	w := httptest.NewRecorder()
	handleListActions(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var out []ServiceAction
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestHandleListDefaultGrants_Success(t *testing.T) {
	requireDB(t)
	r := httptest.NewRequest(http.MethodGet, "/default-grants", nil)
	r.Header.Set("X-Service-Key", testReadKey)
	w := httptest.NewRecorder()
	handleListDefaultGrants(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var out []ServiceDefaultGrant
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// ---------------------------------------------------------------------------
// handleUpdateServiceEndpoints — bad JSON, invalid URL, 404, and success.
// ---------------------------------------------------------------------------

func TestHandleUpdateServiceEndpoints_BadJSON(t *testing.T) {
	requireDB(t)
	r := httptest.NewRequest(http.MethodPut, "/services/x/endpoints", bytes.NewBufferString(`{bad`))
	r.SetPathValue("id", "x")
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	handleUpdateServiceEndpoints(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleUpdateServiceEndpoints_InvalidURL(t *testing.T) {
	requireDB(t)
	r := httptest.NewRequest(http.MethodPut, "/services/x/endpoints", bytes.NewBufferString(`{"url":"http://127.0.0.1:9"}`))
	r.SetPathValue("id", "x")
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	handleUpdateServiceEndpoints(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for SSRF url", w.Code)
	}
}

func TestHandleUpdateServiceEndpoints_NotFound(t *testing.T) {
	requireDB(t)
	r := httptest.NewRequest(http.MethodPut, "/services/missing/endpoints", bytes.NewBufferString(`{"roles":[]}`))
	r.SetPathValue("id", "no-such-"+uuid.New().String())
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	handleUpdateServiceEndpoints(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestHandleUpdateServiceEndpoints_Success(t *testing.T) {
	requireDB(t)

	name := "huse-" + uuid.New().String()
	id := insertTestService(t, name)

	var notified int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&notified, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("CONDUCTOR_URL", srv.URL)
	t.Setenv("CONDUCTOR_NOTIFY_KEY", "k")

	body := `{"url":"http://newurl:9000","description":"d","roles":[{"Name":"r","Description":"x"}],` +
		`"endpoints":[{"Method":"GET","Path":"/p","Action":"getP","Resource":"svc/p","Public":false}]}`
	r := httptest.NewRequest(http.MethodPut, "/services/"+id+"/endpoints", bytes.NewBufferString(body))
	r.SetPathValue("id", id)
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	handleUpdateServiceEndpoints(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&notified) == 0 {
		t.Error("expected conductor to be notified")
	}
}

// ---------------------------------------------------------------------------
// handleUpsertServiceAccount — bad body, invalid role, default role.
// ---------------------------------------------------------------------------

func TestHandleUpsertServiceAccount_BadBody(t *testing.T) {
	requireDB(t)
	r := httptest.NewRequest(http.MethodPost, "/service-accounts", bytes.NewBufferString(`{"name":""}`))
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	handleUpsertServiceAccount(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleUpsertServiceAccount_InvalidRole(t *testing.T) {
	requireDB(t)
	r := httptest.NewRequest(http.MethodPost, "/service-accounts", bytes.NewBufferString(`{"name":"n","key":"k","role":"superuser"}`))
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	handleUpsertServiceAccount(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for invalid role", w.Code)
	}
}

func TestHandleUpsertServiceAccount_DefaultRoleRead(t *testing.T) {
	requireDB(t)
	name := "ua-" + uuid.New().String()
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM registry_service_accounts WHERE name = ?`, name) //nolint:errcheck
	})
	// No role provided → defaults to "read".
	r := httptest.NewRequest(http.MethodPost, "/service-accounts", bytes.NewBufferString(`{"name":"`+name+`","key":"k"}`))
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	handleUpsertServiceAccount(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204", w.Code)
	}
	acct, err := lookupServiceAccount(context.Background(), name)
	if err != nil || acct.Role != "read" {
		t.Fatalf("default role: %v / %q, want read", err, acct.Role)
	}
}

// requireAdminAuth-protected handlers reject a read account (403).
func TestAdminHandlers_ForbiddenForReadAccount(t *testing.T) {
	requireDB(t)
	cases := []struct {
		name string
		fn   http.HandlerFunc
		req  *http.Request
	}{
		{"create", handleCreateService, httptest.NewRequest(http.MethodPost, "/services", bytes.NewBufferString(`{"name":"n","url":"http://x"}`))},
		{"delete", handleDeleteService, httptest.NewRequest(http.MethodDelete, "/services/x", nil)},
		{"upsert-account", handleUpsertServiceAccount, httptest.NewRequest(http.MethodPost, "/service-accounts", bytes.NewBufferString(`{"name":"n","key":"k"}`))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.req.Header.Set("X-Service-Key", testReadKey)
			w := httptest.NewRecorder()
			c.fn(w, c.req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s: got %d, want 403", c.name, w.Code)
			}
		})
	}
}

// handleCreateService malformed JSON → 400.
func TestHandleCreateService_MalformedJSON(t *testing.T) {
	requireDB(t)
	r := httptest.NewRequest(http.MethodPost, "/services", bytes.NewBufferString(`{not json`))
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	handleCreateService(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

// ---------------------------------------------------------------------------
// handleSystemHealth — authorized, with a populated cache (degraded + healthy).
// ---------------------------------------------------------------------------

func TestHandleSystemHealth_Authorized(t *testing.T) {
	requireDB(t)

	// Seed the cache directly to exercise the overall-status aggregation.
	healthMu.Lock()
	healthCache = map[string]serviceHealth{
		"svc-a": {Status: "healthy", CheckedAt: time.Now()},
		"svc-b": {Status: "unhealthy", Error: "boom", CheckedAt: time.Now()},
	}
	healthMu.Unlock()
	t.Cleanup(func() {
		healthMu.Lock()
		healthCache = nil
		healthMu.Unlock()
	})

	r := httptest.NewRequest(http.MethodGet, "/system_health", nil)
	r.Header.Set("X-Service-Key", testReadKey)
	w := httptest.NewRecorder()
	handleSystemHealth(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var resp struct {
		Status   string                   `json:"status"`
		Services map[string]serviceHealth `json:"services"`
	}
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Status != "degraded" {
		t.Fatalf("status = %q, want degraded (one service unhealthy)", resp.Status)
	}
	if len(resp.Services) != 2 {
		t.Fatalf("services = %d, want 2", len(resp.Services))
	}
}

func TestHandleSystemHealth_AllHealthy(t *testing.T) {
	requireDB(t)
	healthMu.Lock()
	healthCache = map[string]serviceHealth{"svc": {Status: "healthy", CheckedAt: time.Now()}}
	healthMu.Unlock()
	t.Cleanup(func() {
		healthMu.Lock()
		healthCache = nil
		healthMu.Unlock()
	})

	r := httptest.NewRequest(http.MethodGet, "/system_health", nil)
	r.Header.Set("X-Service-Key", testReadKey)
	w := httptest.NewRecorder()
	handleSystemHealth(w, r)
	var resp struct {
		Status string `json:"status"`
	}
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Status != "healthy" {
		t.Fatalf("status = %q, want healthy", resp.Status)
	}
}

// ---------------------------------------------------------------------------
// startHealthCollector — drive one collection pass against test backends.
// ---------------------------------------------------------------------------

func TestStartHealthCollector_CollectsHealthyAndUnhealthy(t *testing.T) {
	requireDB(t)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer unhealthy.Close()

	hName := "hc-healthy-" + uuid.New().String()
	uName := "hc-unhealthy-" + uuid.New().String()
	for _, s := range []struct{ n, u string }{{hName, healthy.URL}, {uName, unhealthy.URL}} {
		connect().Exec(`INSERT INTO services (service_id, name, url) VALUES (?,?,?)`, uuid.New().String(), s.n, s.u) //nolint:errcheck
	}
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM services WHERE name IN (?, ?)`, hName, uName) //nolint:errcheck
	})

	// Drive a single collection then cancel so the ticker goroutine exits.
	ctx, cancel := context.WithCancel(context.Background())
	startHealthCollector(ctx)
	// Poll the cache until both services appear (collect() runs once immediately).
	deadline := time.Now().Add(5 * time.Second)
	var gotHealthy, gotUnhealthy bool
	for time.Now().Before(deadline) {
		healthMu.RLock()
		c := healthCache
		healthMu.RUnlock()
		if h, ok := c[hName]; ok && h.Status == "healthy" {
			gotHealthy = true
		}
		if u, ok := c[uName]; ok && u.Status == "unhealthy" {
			gotUnhealthy = true
		}
		if gotHealthy && gotUnhealthy {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if !gotHealthy {
		t.Error("healthy service not recorded as healthy")
	}
	if !gotUnhealthy {
		t.Error("unhealthy (500) service not recorded as unhealthy")
	}
	t.Cleanup(func() {
		healthMu.Lock()
		healthCache = nil
		healthMu.Unlock()
	})
}

// ---------------------------------------------------------------------------
// notifyWorkflows + startNotificationTicker.
// ---------------------------------------------------------------------------

func TestNotifyWorkflows_PostsCatalogRefresh(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Service-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("WORKFLOWS_URL", srv.URL)
	t.Setenv("WORKFLOWS_NOTIFY_KEY", "wfkey")
	notifyWorkflows(context.Background())
	if gotPath != "/internal/catalog/refresh" {
		t.Errorf("path = %q, want /internal/catalog/refresh", gotPath)
	}
	if gotKey != "registry:wfkey" {
		t.Errorf("key = %q, want registry:wfkey", gotKey)
	}
}

func TestNotifyWorkflows_NoopWithoutEnv(t *testing.T) {
	t.Setenv("WORKFLOWS_URL", "")
	t.Setenv("WORKFLOWS_NOTIFY_KEY", "")
	notifyWorkflows(context.Background()) // must be a no-op, no panic.
}

func TestNotifyService_RequestCreationFailure(t *testing.T) {
	// An invalid target URL makes http.NewRequestWithContext fail; the helper
	// must log and return without panicking.
	notifyService(context.Background(), "x", "http://%zz/bad", "key")
}

func TestStartNotificationTicker_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	startNotificationTicker(ctx, "")
	cancel() // The goroutine selects on ctx.Done() and returns; no assertion needed beyond no panic.
	time.Sleep(20 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// handleOpenAPIYAML + logger.ServeHTTP.
// ---------------------------------------------------------------------------

func TestHandleOpenAPIYAML(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	w := httptest.NewRecorder()
	handleOpenAPIYAML(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Fatalf("content-type = %q, want application/yaml", ct)
	}
	if w.Body.Len() == 0 {
		t.Fatal("expected non-empty openapi body")
	}
}

func TestLoggerServeHTTP(t *testing.T) {
	called := false
	l := &logger{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	})}

	// Regular path → logged at info.
	w := httptest.NewRecorder()
	l.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/services", nil))
	if !called || w.Code != http.StatusCreated {
		t.Fatalf("handler not invoked or wrong code: called=%v code=%d", called, w.Code)
	}

	// Probe path with 500 → warn branch.
	w = httptest.NewRecorder()
	l500 := &logger{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})}
	l500.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("probe code = %d, want 500", w.Code)
	}

	// Probe path with 200 → debug branch.
	w = httptest.NewRecorder()
	lok := &logger{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	lok.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("probe ok code = %d, want 200", w.Code)
	}
}

// ---------------------------------------------------------------------------
// seedServiceAccounts — invalid-entry skipping.
// ---------------------------------------------------------------------------

func TestSeedServiceAccounts_SkipsInvalidEntries(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	prev := seedServiceKeys
	seedServiceKeys = map[string]seedAccount{}
	t.Cleanup(func() { seedServiceKeys = prev })

	good := "seed-" + uuid.New().String()
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM registry_service_accounts WHERE name = ?`, good) //nolint:errcheck
	})

	// Entries: blank, no '=', trailing '=' (empty key), and a valid one.
	raw := "  ," + "noequals," + "trailing=," + good + "=secret"
	seedServiceAccounts(ctx, raw, "read")

	if _, ok := seedServiceKeys[good]; !ok {
		t.Fatal("valid entry not seeded into seedServiceKeys")
	}
	if _, ok := seedServiceKeys["noequals"]; ok {
		t.Error("entry without '=' should be skipped")
	}
	if _, ok := seedServiceKeys["trailing"]; ok {
		t.Error("entry with empty key should be skipped")
	}
	if acct, err := lookupServiceAccount(ctx, good); err != nil || acct.Role != "read" {
		t.Fatalf("valid account not created: %v / %q", err, acct.Role)
	}

	// Empty raw → no-op.
	seedServiceAccounts(ctx, "", "read")
}

// Guard: a hashServiceKey on a too-long key surfaces the bcrypt error path in
// handleUpsertServiceAccount (>72 bytes makes bcrypt fail) → 500.
func TestHandleUpsertServiceAccount_HashFailure(t *testing.T) {
	requireDB(t)
	longKey := strings.Repeat("x", 100)
	body := `{"name":"hf-` + uuid.New().String() + `","key":"` + longKey + `","role":"read"}`
	r := httptest.NewRequest(http.MethodPost, "/service-accounts", bytes.NewBufferString(body))
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	handleUpsertServiceAccount(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500 for over-long key", w.Code)
	}
}

// And the same over-long-key path through handleCreateService → 500.
func TestHandleCreateService_HashFailure(t *testing.T) {
	requireDB(t)
	longKey := strings.Repeat("y", 100)
	body := `{"name":"hcf-` + uuid.New().String() + `","url":"http://93.184.216.34","service_key":"` + longKey + `"}`
	r := httptest.NewRequest(http.MethodPost, "/services", bytes.NewBufferString(body))
	r.Header.Set("X-Service-Key", testAdminKey)
	w := httptest.NewRecorder()
	handleCreateService(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500 for over-long service_key", w.Code)
	}
}

var _ = gorm.ErrRecordNotFound
