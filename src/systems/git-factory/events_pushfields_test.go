package main

import "testing"

// The "before" field is what lets a consumer diff the whole push rather than just its
// last commit. A CI pipeline without it computes its change set from HEAD~1, so a push
// of n commits is read as a push of one and everything the other n-1 touched is never
// rebuilt — while the run still reports success.
func TestPushFields_Before(t *testing.T) {
	repo := Repo{ID: "r1", Namespace: "admin", Name: "codearmory", HttpUrl: "http://git/admin/codearmory.git"}

	t.Run("reports the prior SHA of the ref that moved", func(t *testing.T) {
		ref, fields := pushFields(repo, "user-1", "main",
			[]string{"main"},
			map[string]string{"main": "aaaaaaa", "topic": "bbbbbbb"})

		if ref != "main" {
			t.Fatalf("ref = %q, want main", ref)
		}
		if got := fields["before"]; got != "aaaaaaa" {
			t.Errorf("before = %q, want aaaaaaa — the prior SHA of the pushed ref", got)
		}
	})

	// The event names one ref (refs[0]), so "before" must be that ref's prior SHA and
	// not whatever the map happens to iterate to first.
	t.Run("uses the reported ref, not another ref in the snapshot", func(t *testing.T) {
		ref, fields := pushFields(repo, "user-1", "main",
			[]string{"topic"},
			map[string]string{"main": "aaaaaaa", "topic": "bbbbbbb"})

		if ref != "topic" {
			t.Fatalf("ref = %q, want topic", ref)
		}
		if got := fields["before"]; got != "bbbbbbb" {
			t.Errorf("before = %q, want bbbbbbb — the pushed ref's SHA, not the default branch's", got)
		}
	})

	// A brand-new branch has no prior SHA. Absent must mean "cannot tell" so the
	// consumer falls back to its own default; a zero SHA would read as a real value
	// and diff against the empty tree, marking every file in the repo as changed.
	t.Run("omitted for a newly created branch", func(t *testing.T) {
		_, fields := pushFields(repo, "user-1", "main",
			[]string{"brand-new"},
			map[string]string{"main": "aaaaaaa"})

		if v, present := fields["before"]; present {
			t.Errorf("before = %q for a branch with no prior SHA; want the field absent", v)
		}
	})

	t.Run("omitted when the caller has no snapshot", func(t *testing.T) {
		_, fields := pushFields(repo, "user-1", "main", []string{"main"}, nil)

		if v, present := fields["before"]; present {
			t.Errorf("before = %q with a nil snapshot; want the field absent", v)
		}
	})

	// The rest of the payload is what existing rules match on, so it has to survive
	// the change unaltered.
	t.Run("keeps the existing payload intact", func(t *testing.T) {
		_, fields := pushFields(repo, "user-1", "main", []string{"main", "topic"},
			map[string]string{"main": "aaaaaaa"})

		for k, want := range map[string]string{
			"repo_id":        "r1",
			"repo":           "admin/codearmory",
			"namespace":      "admin",
			"name":           "codearmory",
			"default_branch": "main",
			"pusher":         "user-1",
			"clone_url":      "http://git/admin/codearmory.git",
			"ref_count":      "2",
			"ref":            "main",
		} {
			if got := fields[k]; got != want {
				t.Errorf("%s = %q, want %q", k, got, want)
			}
		}
	})

	// notifyPush falls back to the default branch when no ref moved; "before" must
	// follow the same ref so the two can never disagree.
	t.Run("falls back to the default branch with no refs", func(t *testing.T) {
		ref, fields := pushFields(repo, "user-1", "main", nil,
			map[string]string{"main": "aaaaaaa"})

		if ref != "main" {
			t.Fatalf("ref = %q, want the default branch main", ref)
		}
		if got := fields["before"]; got != "aaaaaaa" {
			t.Errorf("before = %q, want aaaaaaa", got)
		}
	})
}
