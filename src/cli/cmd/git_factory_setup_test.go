package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

func TestGitFactoryDefaultBaseURL(t *testing.T) {
	// isolateHome, or the "" case reads the DEVELOPER'S real config: conductorURL()
	// falls through flag -> env -> config, so on any machine that has run `armory
	// setup` the fallback assertion fails against whatever URL happens to be saved.
	isolateHome(t)
	t.Cleanup(func() { flagURL = "" })
	cases := map[string]string{
		"http://localhost:8090":  "http://localhost:9002",
		"https://ca.example:443": "https://ca.example:9002",
		"http://10.0.0.5:8080":   "http://10.0.0.5:9002",
		"":                       "http://localhost:9002", // falls back
	}
	for in, want := range cases {
		flagURL = in
		if got := gitFactoryDefaultBaseURL(); got != want {
			t.Errorf("gitFactoryDefaultBaseURL(conductor=%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGitCredentialHelper_GetOutputsToken(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	tok := makeTestJWT(time.Now().Add(time.Hour).Unix())
	t.Setenv("CODEARMORY_TOKEN", tok)
	t.Cleanup(func() { flagToken = ""; gitCredentialHelperCmd.SetIn(nil); gitCredentialHelperCmd.SetOut(nil) })
	flagToken = ""

	var out bytes.Buffer
	gitCredentialHelperCmd.SetIn(strings.NewReader("protocol=http\nhost=localhost:9002\n\n"))
	gitCredentialHelperCmd.SetOut(&out)

	if err := gitCredentialHelperCmd.RunE(gitCredentialHelperCmd, []string{"get"}); err != nil {
		t.Fatalf("credential-helper get: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "password="+tok) {
		t.Errorf("output missing token password; got %q", got)
	}
	if !strings.Contains(got, "username="+gitCredentialUsername) {
		t.Errorf("output missing username; got %q", got)
	}
}

func TestGitCredentialHelper_StoreAndEraseAreSilent(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Setenv("CODEARMORY_TOKEN", makeTestJWT(time.Now().Add(time.Hour).Unix()))
	t.Cleanup(func() { flagToken = ""; gitCredentialHelperCmd.SetIn(nil); gitCredentialHelperCmd.SetOut(nil) })
	flagToken = ""

	for _, op := range []string{"store", "erase"} {
		var out bytes.Buffer
		gitCredentialHelperCmd.SetIn(strings.NewReader(""))
		gitCredentialHelperCmd.SetOut(&out)
		if err := gitCredentialHelperCmd.RunE(gitCredentialHelperCmd, []string{op}); err != nil {
			t.Fatalf("credential-helper %s: %v", op, err)
		}
		if out.Len() != 0 {
			t.Errorf("%s should produce no output, got %q", op, out.String())
		}
	}
}

func TestGitCredentialHelper_NoTokenSilent(t *testing.T) {
	keyring.MockInit()
	isolateHome(t)
	t.Setenv("CODEARMORY_TOKEN", "")
	t.Cleanup(func() { flagToken = ""; gitCredentialHelperCmd.SetIn(nil); gitCredentialHelperCmd.SetOut(nil) })
	flagToken = ""

	var out bytes.Buffer
	gitCredentialHelperCmd.SetIn(strings.NewReader(""))
	gitCredentialHelperCmd.SetOut(&out)
	if err := gitCredentialHelperCmd.RunE(gitCredentialHelperCmd, []string{"get"}); err != nil {
		t.Fatalf("credential-helper get (no token): %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("with no token the helper must stay silent, got %q", out.String())
	}
}

// TestBaseFromCloneURL pins the inverse of git_factory's cloneURL composition
// (base + "/<namespace>/<name>.git"). Both trailing segments come off: the namespace
// belongs to the repo path, not the base, so dropping only the last one would leave a
// base ending in someone's username.
func TestBaseFromCloneURL(t *testing.T) {
	ok := map[string]string{
		"https://git.example.com/admin/codearmory.git": "https://git.example.com",
		"http://localhost:9002/alice/proj.git":         "http://localhost:9002",
		// A base that itself carries a path prefix keeps it.
		"https://example.com/git/admin/codearmory.git": "https://example.com/git",
	}
	for in, want := range ok {
		got, found := baseFromCloneURL(in)
		if !found || got != want {
			t.Errorf("baseFromCloneURL(%q) = (%q, %v), want (%q, true)", in, got, found, want)
		}
	}
	// Anything that is not a clone URL must be rejected rather than half-parsed into a
	// plausible-looking base — the caller falls back to its guess, which is safer than
	// configuring a credential helper for the wrong host.
	for _, bad := range []string{
		"",
		"https://git.example.com",         // no repo path
		"https://git.example.com/one.git", // only one segment
		"not-a-url",
		"/admin/codearmory.git", // no host
	} {
		if got, found := baseFromCloneURL(bad); found {
			t.Errorf("baseFromCloneURL(%q) = (%q, true), want rejected", bad, got)
		}
	}
}
