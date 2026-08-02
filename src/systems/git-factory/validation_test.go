package main

// Unit spec for the two pure helpers the storage/URL design depends on:
//
//   validRepoName   the ARCHITECTURE §6 allowlist — the single guard standing
//                   between a URL path segment and the filesystem.
//   gitHTTPBaseURL  where clone URLs get their host, so the "<host>" placeholder
//                   can never come back.
//
// Both are exercised indirectly through handleCreateRepo elsewhere; these pin the
// behavior directly so a regression names the function rather than a status code.

import (
	"context"
	"testing"
)

func TestValidRepoName(t *testing.T) {
	cases := []struct {
		name string
		want bool
		why  string
	}{
		// Allowed: the [A-Za-z0-9._-]+ allowlist, dots included.
		{"widgets", true, "plain lowercase"},
		{"Widgets", true, "uppercase is allowed"},
		{"a", true, "single character"},
		{"123", true, "digits only"},
		{"my.repo", true, "dots are legal — only a .git suffix is not"},
		{"v1.2.3", true, "multiple dots"},
		{"with-dash", true, "hyphen"},
		{"with_underscore", true, "underscore"},
		{"a.b-c_d", true, "all permitted classes together"},

		// Rejected by the allowlist itself.
		{"", false, "empty"},
		{" ", false, "space only"},
		{"with space", false, "embedded space"},
		{"a/b", false, "path separator"},
		{"a\\b", false, "windows separator"},
		{"../escape", false, "traversal"},
		{"a:b", false, "colon"},
		{"a*b", false, "glob metacharacter"},
		{"a?b", false, "glob metacharacter"},
		{"tab\there", false, "control character"},
		{"emoji🙂", false, "non-ascii"},

		// Rejected explicitly — these match the pattern but are still unsafe.
		{".", false, "current-directory name"},
		{"..", false, "parent-directory name — matches the pattern, must be rejected"},
		{".git", false, "the git directory name itself"},
		{"foo.git", false, ".git suffix would yield a doubled /foo.git.git clone URL"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validRepoName(tc.name); got != tc.want {
				t.Errorf("validRepoName(%q) = %v, want %v — %s", tc.name, got, tc.want, tc.why)
			}
		})
	}
}

func TestGitHTTPBaseURL(t *testing.T) {
	t.Run("uses GIT_HTTP_BASE_URL", func(t *testing.T) {
		t.Setenv("GIT_HTTP_BASE_URL", "https://git.example.com")
		if got, want := gitHTTPBaseURL(), "https://git.example.com"; got != want {
			t.Errorf("gitHTTPBaseURL() = %q, want %q", got, want)
		}
	})

	t.Run("trims a trailing slash so callers join with one /", func(t *testing.T) {
		t.Setenv("GIT_HTTP_BASE_URL", "https://git.example.com/")
		if got, want := gitHTTPBaseURL(), "https://git.example.com"; got != want {
			t.Errorf("gitHTTPBaseURL() = %q, want %q (a doubled slash breaks the clone path)", got, want)
		}
	})

	t.Run("falls back to the service port", func(t *testing.T) {
		t.Setenv("GIT_HTTP_BASE_URL", "")
		t.Setenv("PORT", "9999")
		if got, want := gitHTTPBaseURL(), "http://localhost:9999"; got != want {
			t.Errorf("gitHTTPBaseURL() = %q, want %q", got, want)
		}
	})

	t.Run("never emits the placeholder host", func(t *testing.T) {
		t.Setenv("GIT_HTTP_BASE_URL", "")
		if got := gitHTTPBaseURL(); got == "http://<host>" {
			t.Error("gitHTTPBaseURL() returned the <host> placeholder")
		}
	})
}

// The clone URL must be built from the configured base, not a hardcoded host.
// Asserts the whole URL rather than just "no placeholder", so a wrong base is
// caught too.
func TestRepoAdd_HTTPURLUsesConfiguredBase(t *testing.T) {
	setupTestDB(t)
	t.Setenv("GIT_HTTP_BASE_URL", "https://git.example.com")

	re := Repo{ID: "ba100000-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}
	if err := re.Add(context.Background()); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := getRepo(context.Background(), "user-1", re.ID)
	if err != nil {
		t.Fatalf("getRepo: %v", err)
	}
	if want := "https://git.example.com/alice/widgets.git"; got.HttpUrl != want {
		t.Errorf("http_url = %q, want %q", got.HttpUrl, want)
	}
}

// http_url is derived, not stored (ARCHITECTURE §4). Pointing the deployment at its
// real external host is a config change — GIT_HTTP_BASE_URL on the builder-managed
// service — and it has to take effect for the repos that already exist, not just the
// ones created afterwards. When it was a column, every pre-existing repo kept handing
// out the http://localhost:<port> fallback until someone rewrote the table by hand.
func TestRepoHTTPURL_ReflectsTheCurrentBase(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	t.Setenv("GIT_HTTP_BASE_URL", "")
	re := Repo{ID: "ba200000-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}
	if err := re.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}

	t.Setenv("GIT_HTTP_BASE_URL", "https://git.example.com")

	got, err := getRepo(ctx, "user-1", re.ID)
	if err != nil {
		t.Fatalf("getRepo: %v", err)
	}
	if want := "https://git.example.com/alice/widgets.git"; got.HttpUrl != want {
		t.Errorf("getRepo http_url = %q, want %q — the base is read at creation, not at read", got.HttpUrl, want)
	}

	list, err := listRepos(ctx, "user-1", nil)
	if err != nil {
		t.Fatalf("listRepos: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("listRepos returned %d repos, want 1", len(list))
	}
	if want := "https://git.example.com/alice/widgets.git"; list[0].HttpUrl != want {
		t.Errorf("listRepos http_url = %q, want %q", list[0].HttpUrl, want)
	}

	// The wire path loads by clone path and puts HttpUrl on the push event as
	// clone_url, so it needs the same treatment.
	byPath, err := getRepoByPath(ctx, "alice", "widgets")
	if err != nil {
		t.Fatalf("getRepoByPath: %v", err)
	}
	if want := "https://git.example.com/alice/widgets.git"; byPath.HttpUrl != want {
		t.Errorf("getRepoByPath http_url = %q, want %q", byPath.HttpUrl, want)
	}
}

// A rename moves the clone URL: the bytes stay put (the on-disk path is keyed by the
// stable id) but /{namespace}/{name}.git changes, so a stored URL would point at the
// old name forever.
func TestRepoHTTPURL_FollowsARename(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	t.Setenv("GIT_HTTP_BASE_URL", "https://git.example.com")

	re := Repo{ID: "ba300000-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}
	if err := re.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}

	re.Name = "gadgets"
	if err := re.Update(ctx); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := getRepo(ctx, "user-1", re.ID)
	if err != nil {
		t.Fatalf("getRepo: %v", err)
	}
	if want := "https://git.example.com/alice/gadgets.git"; got.HttpUrl != want {
		t.Errorf("http_url after rename = %q, want %q", got.HttpUrl, want)
	}
}

// Regression: Add used to derive Shard from the first three characters of the NAME,
// which panicked on names shorter than that ("go", "ui", "k8"). Shard is now keyed by
// the stable id, so short names are structurally incapable of reaching that slice —
// this keeps the case covered anyway, since the panic reached the handler mid-request.
func TestRepoAdd_ShortNameDoesNotPanic(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	for i, name := range []string{"g", "go", "k8s"} {
		re := Repo{
			ID:        "bb" + string(rune('1'+i)) + "00000-0000-0000-0000-000000000000",
			Owner:     "user-1",
			Namespace: "alice",
			Name:      name,
		}
		if err := re.Add(ctx); err != nil {
			t.Fatalf("Add(name=%q): %v", name, err)
		}
	}
}
