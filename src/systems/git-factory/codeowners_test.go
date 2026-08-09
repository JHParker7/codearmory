package main

import (
	"reflect"
	"testing"
)

func TestParseCodeowners(t *testing.T) {
	rules := parseCodeowners(`
# Owners for this repo
*           @default-team

# Comments and blanks are skipped
/src/       @backend @platform
*.md        @docs
Makefile    @build
pattern-with-no-owners
`)
	want := []codeownersRule{
		{Pattern: "*", Owners: []string{"@default-team"}},
		{Pattern: "/src/", Owners: []string{"@backend", "@platform"}},
		{Pattern: "*.md", Owners: []string{"@docs"}},
		{Pattern: "Makefile", Owners: []string{"@build"}},
	}
	if !reflect.DeepEqual(rules, want) {
		t.Errorf("parseCodeowners =\n%#v\nwant\n%#v", rules, want)
	}
}

// A trailing comment must not become part of the owner list.
func TestParseCodeowners_StripsInlineComments(t *testing.T) {
	rules := parseCodeowners("*.go @gophers # the go team\n")
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(rules))
	}
	if !reflect.DeepEqual(rules[0].Owners, []string{"@gophers"}) {
		t.Errorf("owners = %v, want [@gophers] — the comment leaked in", rules[0].Owners)
	}
}

func TestCodeownersMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"*", "anything/at/all.go", true},
		{"*.go", "main.go", true},
		{"*.go", "pkg/sub/main.go", true}, // unanchored: matches at any depth
		{"*.go", "main.rs", false},
		{"/src/", "src/main.go", true},
		{"/src/", "other/src/main.go", false}, // anchored to the root
		{"docs/", "a/docs/x.md", true},        // unanchored directory, any depth
		{"src", "src/main.go", true},          // a bare directory owns what is under it
		{"Makefile", "Makefile", true},
		{"Makefile", "sub/Makefile", true}, // unanchored bare name, any depth
		{"Makefile", "Makefile.in", false},
		{"/Makefile", "sub/Makefile", false}, // anchored
	}
	for _, c := range cases {
		if got := codeownersMatch(c.pattern, c.path); got != c.want {
			t.Errorf("codeownersMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

// The last matching rule wins — the convention the format has everywhere, and the whole
// reason a file reads general-to-specific.
func TestMatchCodeowners_LastRuleWins(t *testing.T) {
	rules := parseCodeowners("*  @everyone\n/src/  @backend\n")
	if got := matchCodeowners(rules, "src/main.go"); !reflect.DeepEqual(got, []string{"@backend"}) {
		t.Errorf("owners for src/main.go = %v, want [@backend] — the specific rule must win", got)
	}
	if got := matchCodeowners(rules, "README.md"); !reflect.DeepEqual(got, []string{"@everyone"}) {
		t.Errorf("owners for README.md = %v, want [@everyone]", got)
	}
}

func TestOwnersFor_DedupesAndSorts(t *testing.T) {
	rules := parseCodeowners("*.go @gophers\n*.md @docs\n/src/ @gophers\n")
	changes := []fileChange{
		{Path: "src/a.go"},
		{Path: "src/b.go"},
		{Path: "README.md"},
	}
	got := ownersFor(rules, changes)
	want := []string{"@docs", "@gophers"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ownersFor = %v, want %v (deduped and sorted)", got, want)
	}
}

// A change touching nothing owned yields no owners rather than a spurious match.
func TestOwnersFor_NoMatchIsEmpty(t *testing.T) {
	rules := parseCodeowners("/src/ @backend\n")
	got := ownersFor(rules, []fileChange{{Path: "docs/readme.md"}})
	if len(got) != 0 {
		t.Errorf("ownersFor = %v, want none", got)
	}
}
