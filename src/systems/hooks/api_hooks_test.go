package main

import "testing"

// repo_filter reuses matchesRefFilter against the payload repo path; lock the
// semantics the cross-trigger fix depends on: exact match, "/*" prefix, and that an
// empty filter matches every repo (back-compat).
func TestRepoFilterSemantics(t *testing.T) {
	cases := []struct {
		filter, repo string
		want         bool
	}{
		{"", "admin/codearmory", true},                        // empty = match all
		{"admin/codearmory", "admin/codearmory", true},        // exact
		{"admin/codearmory", "admin/codearmory-git-factory", false}, // the bug: no longer matches
		{"admin/*", "admin/codearmory", true},                 // "/*" prefix
		{"admin/*", "other/codearmory", false},
	}
	for _, c := range cases {
		if got := matchesRefFilter(c.filter, c.repo); got != c.want {
			t.Errorf("matchesRefFilter(%q, %q) = %v, want %v", c.filter, c.repo, got, c.want)
		}
	}
}

func TestHookVarName(t *testing.T) {
	cases := map[string]string{
		"clone_url":      "HOOK_CLONE_URL",
		"default_branch": "HOOK_DEFAULT_BRANCH",
		"repo_id":        "HOOK_REPO_ID",
		"ref":            "HOOK_REF",
		"commit":         "HOOK_COMMIT",
		"default.branch": "HOOK_DEFAULT_BRANCH", // dotted keys collapse to one underscore
		"a--b":           "HOOK_A_B",            // runs of separators collapse
		"_leading":       "HOOK_LEADING",        // no bare underscore right after the prefix
		"trailing_":      "HOOK_TRAILING",       // no trailing underscore
		"MixedCase":      "HOOK_MIXEDCASE",
		"":               "",   // empty key -> no var
		"___":            "",   // all-separator key -> no var, never a bare "HOOK_"
	}
	for in, want := range cases {
		if got := hookVarName(in); got != want {
			t.Errorf("hookVarName(%q) = %q, want %q", in, got, want)
		}
	}
}
