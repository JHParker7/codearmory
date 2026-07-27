package main

import "testing"

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
