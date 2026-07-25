package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

func TestGitFactoryDefaultBaseURL(t *testing.T) {
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
