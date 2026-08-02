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

// countOnly is the pre-age-rule policy: keep the newest n, nothing else prunes.
func countOnly(n int, protected map[string]bool) retention {
	return retention{keep: n, keepMin: 1, protected: protected}
}

func TestPrunable_KeepsNewestNPerImage(t *testing.T) {
	versions := []packageVersion{
		{Name: "portal", Version: "v1", CreatedAt: at("2026-07-01T00:00:00Z")},
		{Name: "portal", Version: "v2", CreatedAt: at("2026-07-02T00:00:00Z")},
		{Name: "portal", Version: "v3", CreatedAt: at("2026-07-03T00:00:00Z")},
		{Name: "portal", Version: "v4", CreatedAt: at("2026-07-04T00:00:00Z")},
		{Name: "forge", Version: "f1", CreatedAt: at("2026-07-01T00:00:00Z")},
	}
	got := prunable(versions, countOnly(2, map[string]bool{}))

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
	got := prunable(versions, countOnly(1, map[string]bool{"latest": true, "deadbee": true}))
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
	first := prunable(versions, countOnly(1, map[string]bool{}))["portal"]
	for i := 0; i < 20; i++ {
		again := prunable(versions, countOnly(1, map[string]bool{}))["portal"]
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

// The count rule alone leaves a service that stopped being rebuilt sitting on a full
// keep-window of stale images forever. The age rule is what reclaims those.
func TestPrunable_AgeRuleDeletesInsideTheKeepWindow(t *testing.T) {
	now := at("2026-07-31T00:00:00Z")
	versions := []packageVersion{
		{Name: "hooks", Version: "recent", CreatedAt: at("2026-07-30T00:00:00Z")},
		{Name: "hooks", Version: "stale1", CreatedAt: at("2026-05-01T00:00:00Z")},
		{Name: "hooks", Version: "stale2", CreatedAt: at("2026-04-01T00:00:00Z")},
		{Name: "hooks", Version: "stale3", CreatedAt: at("2026-03-01T00:00:00Z")},
	}
	// keep 10 means the count rule prunes nothing at all here.
	got := prunable(versions, retention{keep: 10, keepMin: 1, maxAge: 168 * time.Hour, now: now})

	var pruned []string
	for _, v := range got["hooks"] {
		pruned = append(pruned, v.Version)
	}
	sort.Strings(pruned)
	if strings.Join(pruned, ",") != "stale1,stale2,stale3" {
		t.Errorf("pruned %v, want all three stale tags — the count rule alone would free nothing", pruned)
	}
}

// keepMin is the guard that stops the age rule deleting the tag the running
// deployment references when a service has not been rebuilt in a long time.
func TestPrunable_AgeRuleStopsAtKeepMin(t *testing.T) {
	now := at("2026-07-31T00:00:00Z")
	versions := []packageVersion{
		{Name: "forge", Version: "n1", CreatedAt: at("2026-01-03T00:00:00Z")},
		{Name: "forge", Version: "n2", CreatedAt: at("2026-01-02T00:00:00Z")},
		{Name: "forge", Version: "n3", CreatedAt: at("2026-01-01T00:00:00Z")},
	}
	// Every tag is months past max-age; only the oldest may go.
	got := prunable(versions, retention{keep: 10, keepMin: 2, maxAge: time.Hour, now: now})

	if len(got["forge"]) != 1 || got["forge"][0].Version != "n3" {
		t.Fatalf("pruned %v, want only n3 — the newest %d must survive the age rule", got["forge"], 2)
	}
}

// The age rule must never override an explicit protection: the SHA this run just
// deployed is protected even though a skewed registry clock could date it old.
func TestPrunable_AgeRuleRespectsProtectedTags(t *testing.T) {
	now := at("2026-07-31T00:00:00Z")
	versions := []packageVersion{
		{Name: "ci-e2e", Version: "latest", CreatedAt: at("2025-01-01T00:00:00Z")},
		{Name: "ci-e2e", Version: "deadbee", CreatedAt: at("2025-01-01T00:00:00Z")},
		{Name: "ci-e2e", Version: "old", CreatedAt: at("2025-01-01T00:00:00Z")},
	}
	got := prunable(versions, retention{
		keep: 10, keepMin: 1, maxAge: time.Hour, now: now,
		protected: map[string]bool{"latest": true, "deadbee": true},
	})
	for _, v := range got["ci-e2e"] {
		if v.Version != "old" {
			t.Errorf("protected tag %q was selected for deletion by the age rule", v.Version)
		}
	}
}

// A zero maxAge must behave exactly like the count-only policy, so the flag being
// unset cannot start deleting things.
func TestPrunable_ZeroMaxAgeDisablesTheAgeRule(t *testing.T) {
	versions := []packageVersion{
		{Name: "git", Version: "ancient1", CreatedAt: at("2020-01-01T00:00:00Z")},
		{Name: "git", Version: "ancient2", CreatedAt: at("2020-01-02T00:00:00Z")},
	}
	got := prunable(versions, retention{keep: 10, keepMin: 1, maxAge: 0, now: at("2026-07-31T00:00:00Z")})
	if len(got) != 0 {
		t.Errorf("max-age 0 must disable age pruning entirely, got %v", got)
	}
}

// A version must not be reported twice when both rules select it.
func TestPrunable_RulesDoNotDoubleCount(t *testing.T) {
	now := at("2026-07-31T00:00:00Z")
	versions := []packageVersion{
		{Name: "portal", Version: "a", CreatedAt: at("2026-07-30T00:00:00Z")},
		{Name: "portal", Version: "b", CreatedAt: at("2020-01-02T00:00:00Z")},
		{Name: "portal", Version: "c", CreatedAt: at("2020-01-01T00:00:00Z")},
	}
	got := prunable(versions, retention{keep: 1, keepMin: 1, maxAge: time.Hour, now: now})
	if len(got["portal"]) != 2 {
		t.Errorf("got %d doomed versions, want 2 — b and c each qualify under BOTH rules but are one version each", len(got["portal"]))
	}
}

// reportDisk is the pipeline's only window onto node disk pressure, so it must
// return a usable percentage for a real path and degrade quietly for a bad one.
func TestReportDisk(t *testing.T) {
	if pct := reportDisk([]string{"/"}, 85); pct < 0 || pct > 100 {
		t.Errorf("got %d%% for /, want a percentage in 0..100", pct)
	}
	if pct := reportDisk([]string{"/definitely/not/a/path"}, 85); pct != -1 {
		t.Errorf("got %d for an unmeasurable path, want -1 (nothing measured)", pct)
	}
	if pct := reportDisk([]string{"", "  "}, 85); pct != -1 {
		t.Errorf("got %d for empty paths, want -1", pct)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want string
	}{{512, "512B"}, {2048, "2.0KB"}, {5 * 1024 * 1024 * 1024, "5.0GB"}} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
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
	for name, vs := range prunable(got, countOnly(2, map[string]bool{"latest": true})) {
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
