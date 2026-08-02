package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Public repositories (ARCHITECTURE §3 — v1 was private-only). The property under test
// throughout: visibility authorizes READS and nothing else, so a public repo is
// world-clonable while every write still needs a grant.

// seedRepoVisible is seedRepo with an explicit visibility, since visibility is the
// whole subject here.
func seedRepoVisible(t *testing.T, id, owner, ns, name, visibility string) Repo {
	t.Helper()
	re := Repo{ID: id, Owner: owner, Namespace: ns, Name: name, Visibility: visibility}
	if err := re.Add(context.Background()); err != nil {
		t.Fatalf("seed repo %s: %v", id, err)
	}
	return re
}

func TestAllowedByVisibility(t *testing.T) {
	cases := []struct {
		visibility string
		action     string
		want       bool
		why        string
	}{
		{visibilityPublic, "readRepo", true, "anonymous clone is the point of a public repo"},
		{visibilityPublic, "getRepo", true, "reading metadata is a read"},
		{visibilityPublic, "getBlob", true, "browsing code is a read"},
		{visibilityPublic, "writeRepo", false, "push is never public"},
		{visibilityPublic, "deleteRepo", false, "destructive actions are never public"},
		{visibilityPublic, "shareRepo", false, "managing collaborators is a write"},
		{visibilityPublic, "createPull", false, "opening a PR writes to the repo"},
		{visibilityPublic, "setProtection", false, "branch rules are a write"},
		{visibilityPublic, "listProtection", false, "a repo's rules are not published with its code"},
		{visibilityPrivate, "readRepo", false, "private is private"},
		{visibilityPrivate, "getRepo", false, "private is private"},
		{"", "readRepo", false, "an unset column (an old row) must read as private"},
		{"publik", "readRepo", false, "anything unrecognised is private, never public"},
	}
	for _, tc := range cases {
		got := allowedByVisibility(Repo{Visibility: tc.visibility}, tc.action)
		if got != tc.want {
			t.Errorf("allowedByVisibility(%q, %q) = %v, want %v — %s", tc.visibility, tc.action, got, tc.want, tc.why)
		}
	}
}

// The headline case: no credential at all, and the fetch is served.
func TestInfoRefs_PublicRepoServesAnonymousFetch(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")
	seedRepoVisible(t, uuid.New().String(), "user-1", "admin", "open-repo", visibilityPublic)

	rec := infoRefs(t, "/admin/open-repo.git", string(svcUploadPack), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an anonymous fetch of a public repo must be served (%s)", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "application/x-"+string(svcUploadPack)+"-advertisement"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
}

// Read-only means read-only: receive-pack is challenged whatever the visibility, so
// "public" can never be mistaken for "world-writable".
func TestInfoRefs_PublicRepoStillChallengesPush(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")
	seedRepoVisible(t, uuid.New().String(), "user-1", "admin", "open-repo", visibilityPublic)

	rec := infoRefs(t, "/admin/open-repo.git", string(svcReceivePack), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a public repo must still authenticate pushes", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(strings.ToLower(got), "basic") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
	}
}

func TestInfoRefs_PrivateRepoStillChallengesFetch(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")
	seedRepoVisible(t, uuid.New().String(), "user-1", "admin", "closed-repo", visibilityPrivate)

	rec := infoRefs(t, "/admin/closed-repo.git", string(svcUploadPack), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// The repo is now loaded BEFORE the challenge (visibility is a property of the record),
// so this pins the property that reordering could have cost: an anonymous caller gets
// the same 401 for a repo that does not exist as for one that is private. A 404 here
// would turn the wire into a repo-enumeration oracle.
func TestInfoRefs_MissingRepoIsIndistinguishableFromPrivate(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")
	seedRepoVisible(t, uuid.New().String(), "user-1", "admin", "closed-repo", visibilityPrivate)

	missing := infoRefs(t, "/admin/no-such-repo.git", string(svcUploadPack), "")
	private := infoRefs(t, "/admin/closed-repo.git", string(svcUploadPack), "")
	if missing.Code != private.Code {
		t.Fatalf("missing repo = %d, private repo = %d — they must be indistinguishable", missing.Code, private.Code)
	}
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", missing.Code)
	}
}

// A caller holding no grant over the repo — gatekeeper denies, since the resource lives
// in the owner's namespace — still reads it when it is public, and still cannot see a
// private one.
func TestManagementRead_PublicRepoSurvivesDenial(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStubForNamespace(t, "user-1", "alice")

	pub := seedRepoVisible(t, uuid.New().String(), "user-2", "bob", "open-repo", visibilityPublic)
	priv := seedRepoVisible(t, uuid.New().String(), "user-2", "bob", "closed-repo", visibilityPrivate)

	if rec := doGet(t, pub.ID, true); rec.Code != http.StatusOK {
		t.Errorf("public repo: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if rec := doGet(t, priv.ID, true); rec.Code != http.StatusNotFound {
		t.Errorf("private repo: status = %d, want 404 — a denial must still read as not-found", rec.Code)
	}
}

// Visibility is chosen at creation and defaults to private, so a repo is never
// published by omission.
func TestCreateRepo_Visibility(t *testing.T) {
	t.Run("defaults to private", func(t *testing.T) {
		setupTestDB(t)
		initMetrics()
		newGatekeeperStub(t, "user-1")

		rec := postCreateRepo(t, map[string]any{"name": "quiet"}, true)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got["visibility"] != visibilityPrivate {
			t.Errorf("visibility = %v, want %q", got["visibility"], visibilityPrivate)
		}
	})

	t.Run("public when asked for", func(t *testing.T) {
		setupTestDB(t)
		initMetrics()
		newGatekeeperStub(t, "user-1")

		rec := postCreateRepo(t, map[string]any{"name": "loud", "visibility": visibilityPublic}, true)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got["visibility"] != visibilityPublic {
			t.Errorf("visibility = %v, want %q", got["visibility"], visibilityPublic)
		}
	})

	// Refused, not coerced: a caller who meant to publish should hear about the typo
	// rather than quietly get the opposite of what they asked for.
	t.Run("rejects an unrecognised value", func(t *testing.T) {
		setupTestDB(t)
		initMetrics()
		newGatekeeperStub(t, "user-1")

		rec := postCreateRepo(t, map[string]any{"name": "typo", "visibility": "publik"}, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})
}
