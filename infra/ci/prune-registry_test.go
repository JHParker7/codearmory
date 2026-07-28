package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return ts
}

func TestCredentials_FromBase64Auth(t *testing.T) {
	raw := `{"auths":{"192.168.53.171:3000":{"auth":"` +
		base64.StdEncoding.EncodeToString([]byte("ci:s3cret")) + `"}}}`
	user, pass, err := credentials(raw, "192.168.53.171:3000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user != "ci" || pass != "s3cret" {
		t.Errorf("got %q/%q, want ci/s3cret", user, pass)
	}
}

func TestCredentials_PrefersExplicitUsernamePassword(t *testing.T) {
	raw := `{"auths":{"reg:3000":{"username":"bob","password":"pw"}}}`
	user, pass, err := credentials(raw, "reg:3000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user != "bob" || pass != "pw" {
		t.Errorf("got %q/%q, want bob/pw", user, pass)
	}
}

// A config.json written by `docker login` is often keyed by a URL or bare host
// rather than the host:port the pipeline pushes to.
func TestCredentials_FallsBackWhenHostKeyDiffers(t *testing.T) {
	raw := `{"auths":{"https://192.168.53.171:3000/v2/":{"auth":"` +
		base64.StdEncoding.EncodeToString([]byte("ci:pw")) + `"}}}`
	user, _, err := credentials(raw, "192.168.53.171:3000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user != "ci" {
		t.Errorf("got %q, want ci", user)
	}
}

func TestCredentials_RejectsGarbage(t *testing.T) {
	if _, _, err := credentials("not json", "h"); err == nil {
		t.Error("want an error for a non-JSON REGISTRY_AUTH")
	}
	if _, _, err := credentials(`{"auths":{}}`, "h"); err == nil {
		t.Error("want an error when there are no auths entries")
	}
}

func TestPrunable_KeepsNewestNPerImage(t *testing.T) {
	versions := []packageVersion{
		{Name: "portal", Version: "v1", CreatedAt: at("2026-07-01T00:00:00Z")},
		{Name: "portal", Version: "v2", CreatedAt: at("2026-07-02T00:00:00Z")},
		{Name: "portal", Version: "v3", CreatedAt: at("2026-07-03T00:00:00Z")},
		{Name: "portal", Version: "v4", CreatedAt: at("2026-07-04T00:00:00Z")},
		{Name: "forge", Version: "f1", CreatedAt: at("2026-07-01T00:00:00Z")},
	}
	got := prunable(versions, 2, map[string]bool{})

	if len(got["forge"]) != 0 {
		t.Errorf("forge has fewer versions than the keep count; want nothing pruned, got %v", got["forge"])
	}
	var pruned []string
	for _, v := range got["portal"] {
		pruned = append(pruned, v.Version)
	}
	sort.Strings(pruned)
	if strings.Join(pruned, ",") != "v1,v2" {
		t.Errorf("pruned %v, want the two OLDEST (v1,v2) — newest 2 must survive", pruned)
	}
}

func TestPrunable_NeverTouchesProtectedTags(t *testing.T) {
	versions := []packageVersion{
		{Name: "ci-e2e", Version: "latest", CreatedAt: at("2026-01-01T00:00:00Z")},
		{Name: "ci-e2e", Version: "a", CreatedAt: at("2026-07-01T00:00:00Z")},
		{Name: "ci-e2e", Version: "b", CreatedAt: at("2026-07-02T00:00:00Z")},
		{Name: "ci-e2e", Version: "deadbee", CreatedAt: at("2026-01-02T00:00:00Z")},
	}
	// 'latest' is the oldest here, and the running SHA older than the keep window —
	// exactly the case where a naive newest-N would delete something still in use.
	got := prunable(versions, 1, map[string]bool{"latest": true, "deadbee": true})
	for _, v := range got["ci-e2e"] {
		if v.Version == "latest" || v.Version == "deadbee" {
			t.Errorf("protected tag %q was selected for deletion", v.Version)
		}
	}
}

// A run pushes several services in the same second, so equal timestamps are normal
// and the selection must not vary between invocations.
func TestPrunable_DeterministicOnEqualTimestamps(t *testing.T) {
	ts := at("2026-07-27T20:41:00Z")
	versions := []packageVersion{
		{Name: "portal", Version: "aaa", CreatedAt: ts},
		{Name: "portal", Version: "bbb", CreatedAt: ts},
		{Name: "portal", Version: "ccc", CreatedAt: ts},
	}
	first := prunable(versions, 1, map[string]bool{})["portal"]
	for i := 0; i < 20; i++ {
		again := prunable(versions, 1, map[string]bool{})["portal"]
		if len(again) != len(first) {
			t.Fatalf("unstable count: %d then %d", len(first), len(again))
		}
		for j := range first {
			if first[j].Version != again[j].Version {
				t.Fatalf("unstable selection: %v then %v", first[j].Version, again[j].Version)
			}
		}
	}
}

// stubRegistry serves the two Forgejo endpoints the pruner uses.
func stubRegistry(t *testing.T, pages [][]packageVersion, deleted *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "ci" || pass != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			*deleted = append(*deleted, strings.TrimPrefix(r.URL.Path, "/api/v1/packages/jp01/container/"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		page := 1
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		var body []packageVersion
		if page-1 < len(pages) {
			body = pages[page-1]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func TestListVersions_PagesUntilShortBatch(t *testing.T) {
	full := make([]packageVersion, 50)
	for i := range full {
		full[i] = packageVersion{Name: "portal", Version: fmt.Sprintf("v%02d", i), CreatedAt: at("2026-07-01T00:00:00Z")}
	}
	second := []packageVersion{{Name: "portal", Version: "tail", CreatedAt: at("2026-07-02T00:00:00Z")}}

	var deleted []string
	srv := stubRegistry(t, [][]packageVersion{full, second}, &deleted)
	defer srv.Close()

	c := &client{base: srv.URL, owner: "jp01", user: "ci", pass: "pw", http: srv.Client()}
	got, err := c.listVersions()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 51 {
		t.Errorf("got %d versions, want 51 (a full page then a short one)", len(got))
	}
}

func TestDeleteVersion_ToleratesAlreadyGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := &client{base: srv.URL, owner: "jp01", user: "ci", pass: "pw", http: srv.Client()}
	if err := c.deleteVersion("portal", "gone"); err != nil {
		t.Errorf("404 means the version is already absent — the desired state; got %v", err)
	}
}

func TestDeleteVersion_ReportsRealFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	c := &client{base: srv.URL, owner: "jp01", user: "ci", pass: "pw", http: srv.Client()}
	if err := c.deleteVersion("portal", "v1"); err == nil {
		t.Error("want an error for a 500")
	}
}

// End to end over the stub: only the oldest beyond the keep window are deleted.
func TestPrune_EndToEndDeletesOnlyTheOldest(t *testing.T) {
	versions := []packageVersion{
		{Name: "portal", Version: "old1", CreatedAt: at("2026-07-01T00:00:00Z")},
		{Name: "portal", Version: "old2", CreatedAt: at("2026-07-02T00:00:00Z")},
		{Name: "portal", Version: "new1", CreatedAt: at("2026-07-20T00:00:00Z")},
		{Name: "portal", Version: "new2", CreatedAt: at("2026-07-21T00:00:00Z")},
	}
	var deleted []string
	srv := stubRegistry(t, [][]packageVersion{versions}, &deleted)
	defer srv.Close()

	c := &client{base: srv.URL, owner: "jp01", user: "ci", pass: "pw", http: srv.Client()}
	got, err := c.listVersions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for name, vs := range prunable(got, 2, map[string]bool{"latest": true}) {
		for _, v := range vs {
			if err := c.deleteVersion(name, v.Version); err != nil {
				t.Fatalf("delete: %v", err)
			}
		}
	}
	sort.Strings(deleted)
	if strings.Join(deleted, ",") != "portal/old1,portal/old2" {
		t.Errorf("deleted %v, want only portal/old1 and portal/old2", deleted)
	}
}
