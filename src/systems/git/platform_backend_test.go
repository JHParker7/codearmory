package main

import (
	"context"
	"net/http/httptest"
	"testing"
)

// clearPlatformBackends drops every platform-owned row so each test starts from a known
// state — the rows are keyed by (owner, host) AND unique by (owner, name), so a row left
// behind by one test would collide with the next.
func clearPlatformBackends(t *testing.T) {
	t.Helper()
	del := func() {
		if err := gormDB.Where("owner = ?", platformOwner).Delete(&GitBackend{}).Error; err != nil {
			t.Fatalf("clear platform backends: %v", err)
		}
	}
	del()
	t.Cleanup(del)
}

// platformBackends returns every platform-owned backend row.
func platformBackends(t *testing.T) []GitBackend {
	t.Helper()
	var rows []GitBackend
	if err := gormDBRead.Where("owner = ?", platformOwner).Find(&rows).Error; err != nil {
		t.Fatalf("list platform backends: %v", err)
	}
	return rows
}

// Without GIT_FACTORY_URL there is nothing to link: seeding must be inert, not a
// half-formed row and not an error.
func TestSeedPlatformBackend_NoURLIsNoop(t *testing.T) {
	clearPlatformBackends(t)
	withGitFactory(t, "", "")

	if err := seedPlatformGitFactoryBackend(context.Background()); err != nil {
		t.Fatalf("seed with no GIT_FACTORY_URL: %v", err)
	}
	if rows := platformBackends(t); len(rows) != 0 {
		t.Fatalf("seeded %d platform backends with GIT_FACTORY_URL unset, want 0", len(rows))
	}
}

// The happy path: GIT_FACTORY_URL alone (no internal key) is enough to create the
// credential-free platform row builder used to POST in.
func TestSeedPlatformBackend_CreatesRow(t *testing.T) {
	clearPlatformBackends(t)
	// Deliberately no git-factory key: seeding must not depend on GIT_FACTORY_INTERNAL_KEY.
	withGitFactory(t, "http://rel-git-factory:9002", "")

	if err := seedPlatformGitFactoryBackend(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows := platformBackends(t)
	if len(rows) != 1 {
		t.Fatalf("got %d platform backends, want 1", len(rows))
	}
	b := rows[0]
	// The name must match what builder registered (gitConnectorBackend.name), or an
	// existing install would end up with a second, competing backend.
	if b.Name != "git-factory" {
		t.Errorf("name=%q, want git-factory", b.Name)
	}
	if b.Type != backendGitFactory || b.AuthMode != modeService {
		t.Errorf("type=%q auth_mode=%q, want %q/%q", b.Type, b.AuthMode, backendGitFactory, modeService)
	}
	if b.Host != "rel-git-factory" || b.BaseURL != "http://rel-git-factory:9002" {
		t.Errorf("host=%q base_url=%q", b.Host, b.BaseURL)
	}
	// The row is stored in the same shape as every other backend, so openAuth works.
	auth, err := openAuth(b.AuthEnc)
	if err != nil {
		t.Fatalf("openAuth: %v", err)
	}
	if auth.Mode != modeService || auth.Token != "" || auth.Password != "" {
		t.Errorf("platform auth carries credential material: %+v", auth)
	}
	// It must resolve for an arbitrary user with no link of their own.
	got, err := getBackendByHost(context.Background(), "some-user", "rel-git-factory")
	if err != nil {
		t.Fatalf("getBackendByHost: %v", err)
	}
	if got.ID != b.ID {
		t.Errorf("resolved backend %q, want %q", got.ID, b.ID)
	}
}

// Every pod start re-runs seeding, and the seeder re-runs it on a timer: repeats must
// converge on the one row rather than duplicating or churning its identity.
func TestSeedPlatformBackend_Idempotent(t *testing.T) {
	clearPlatformBackends(t)
	withGitFactory(t, "http://rel-git-factory:9002/", "")

	if err := seedPlatformGitFactoryBackend(context.Background()); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	first := platformBackends(t)
	if len(first) != 1 {
		t.Fatalf("after first seed: %d rows, want 1", len(first))
	}
	for i := 0; i < 3; i++ {
		if err := seedPlatformGitFactoryBackend(context.Background()); err != nil {
			t.Fatalf("re-seed %d: %v", i, err)
		}
	}
	after := platformBackends(t)
	if len(after) != 1 {
		t.Fatalf("after re-seeding: %d rows, want 1", len(after))
	}
	if after[0].ID != first[0].ID {
		t.Errorf("row id churned: %q → %q", first[0].ID, after[0].ID)
	}
	if after[0].CreatedAt.UTC() != first[0].CreatedAt.UTC() {
		t.Errorf("created_at rewritten: %v → %v", first[0].CreatedAt, after[0].CreatedAt)
	}
}

// An install upgraded from the builder-registered world already has a platform row.
// Seeding must adopt it — same id, no duplicate — not write a second backend beside it.
func TestSeedPlatformBackend_AdoptsExistingRow(t *testing.T) {
	clearPlatformBackends(t)
	withGitFactory(t, "http://rel-git-factory:9002", "")

	// The row an older builder would have POSTed in, with its own generated id.
	enc, err := sealAuth(authConfig{Mode: modeService})
	if err != nil {
		t.Fatal(err)
	}
	existing := GitBackend{
		ID: "builder-registered-id", Owner: platformOwner, Name: "git-factory",
		Type: backendGitFactory, BaseURL: "http://rel-git-factory:9002", Host: "rel-git-factory",
		AuthMode: modeService, AuthEnc: enc,
	}
	if err := existing.Add(context.Background()); err != nil {
		t.Fatalf("seed pre-existing row: %v", err)
	}

	if err := seedPlatformGitFactoryBackend(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows := platformBackends(t)
	if len(rows) != 1 {
		t.Fatalf("got %d platform backends, want 1 (the pre-existing row, updated in place)", len(rows))
	}
	if rows[0].ID != existing.ID {
		t.Errorf("id=%q, want the pre-existing %q — seeding replaced the row instead of upserting it", rows[0].ID, existing.ID)
	}
	if rows[0].Name != existing.Name || rows[0].Host != existing.Host {
		t.Errorf("identity changed: name=%q host=%q", rows[0].Name, rows[0].Host)
	}
}

// The HTTP endpoint must keep behaving exactly as before the refactor: same auth, same
// validation, and the same upsert the startup path performs.
func TestInternalPlatformBackendEndpointStillUpserts(t *testing.T) {
	clearPlatformBackends(t)
	prev := internalKey
	internalKey = "ik-platform"
	t.Cleanup(func() { internalKey = prev })

	post := func(body any, key string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := req("POST", "/internal/backends/platform", "", body)
		r.Header.Set("X-Internal-Key", key)
		handleInternalPlatformBackend(rec, r)
		return rec
	}

	if rec := post(map[string]string{"name": "git-factory", "base_url": "http://ep-git-factory:9002"}, "wrong"); rec.Code != 401 {
		t.Fatalf("wrong key: code=%d, want 401", rec.Code)
	}
	if rec := post(map[string]string{"name": "", "base_url": "http://ep-git-factory:9002"}, "ik-platform"); rec.Code != 400 {
		t.Fatalf("missing name: code=%d, want 400", rec.Code)
	} else if got := rec.Body.String(); got != "name and base_url are required\n" {
		t.Errorf("missing name body=%q", got)
	}
	if rec := post(map[string]string{"name": "git-factory", "base_url": "ftp://nope"}, "ik-platform"); rec.Code != 400 {
		t.Fatalf("bad scheme: code=%d, want 400", rec.Code)
	} else if got := rec.Body.String(); got != "base_url: url must be http(s)\n" {
		t.Errorf("bad scheme body=%q", got)
	}

	rec := post(map[string]string{"name": "git-factory", "base_url": "http://ep-git-factory:9002"}, "ik-platform")
	if rec.Code != 200 {
		t.Fatalf("register: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// A second call (an older builder's next reconcile pass) still leaves one row.
	rec = post(map[string]string{"name": "git-factory", "base_url": "http://ep-git-factory:9002"}, "ik-platform")
	if rec.Code != 200 {
		t.Fatalf("re-register: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rows := platformBackends(t)
	if len(rows) != 1 {
		t.Fatalf("got %d platform backends, want 1", len(rows))
	}
	if rows[0].Host != "ep-git-factory" || rows[0].Type != backendGitFactory {
		t.Errorf("unexpected row: %+v", rows[0])
	}
}
