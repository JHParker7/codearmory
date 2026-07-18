package main

import "testing"

func TestParseCredentialRef(t *testing.T) {
	cases := []struct {
		ref        string
		wantScheme string
		wantArg    string
		wantErr    bool
	}{
		{"secret:github-deploy-key", "secret", "github-deploy-key", false},
		{"gitea:acme/widgets", "gitea", "acme/widgets", false},
		{"git:https://github.com/acme/widgets.git", "git", "https://github.com/acme/widgets.git", false},
		{"git:http://git.internal/team/app.git", "git", "http://git.internal/team/app.git", false},
		{"secret:", "", "", true},
		{"", "", "", true},
		{"bogus:x", "", "", true},
		{"gitea:acme", "", "", true},               // missing repo
		{"gitea:acme/widgets/extra", "", "", true}, // too many segments
		{"gitea:/widgets", "", "", true},           // empty owner
		{"git:ssh://git@host/a/b", "", "", true},   // non-http(s) url
		{"git:not-a-url", "", "", true},            // no scheme/host
	}
	for _, c := range cases {
		scheme, arg, err := parseCredentialRef(c.ref)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseCredentialRef(%q): expected error, got scheme=%q arg=%q", c.ref, scheme, arg)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCredentialRef(%q): unexpected error %v", c.ref, err)
			continue
		}
		if scheme != c.wantScheme || arg != c.wantArg {
			t.Errorf("parseCredentialRef(%q) = (%q,%q), want (%q,%q)", c.ref, scheme, arg, c.wantScheme, c.wantArg)
		}
	}
}

func TestValidateSecretRefs(t *testing.T) {
	cases := []struct {
		name    string
		refs    map[string]string
		env     map[string]string
		orgID   string
		userID  string
		wantErr bool
	}{
		{"secret with org", map[string]string{"GIT_SSH_KEY": "secret:k"}, nil, "org1", "", false},
		{"secret with user no org", map[string]string{"GIT_SSH_KEY": "secret:k"}, nil, "", "u1", false},
		{"gitea needs no org", map[string]string{"REPO_URL": "gitea:acme/widgets"}, nil, "", "", false},
		{"git needs no org", map[string]string{"REPO_URL": "git:https://github.com/acme/widgets.git"}, nil, "", "", false},
		{"secret without org or user", map[string]string{"X": "secret:k"}, nil, "", "", true},
		{"invalid target key", map[string]string{"1BAD": "gitea:a/b"}, nil, "", "", true},
		{"blocked target key", map[string]string{"LD_PRELOAD": "gitea:a/b"}, nil, "", "", true},
		{"collides with env", map[string]string{"TOKEN": "gitea:a/b"}, map[string]string{"TOKEN": "x"}, "", "", true},
		{"malformed ref", map[string]string{"X": "nope"}, nil, "org1", "", true},
	}
	for _, c := range cases {
		err := validateSecretRefs(c.refs, c.env, c.orgID, c.userID)
		if c.wantErr && err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
	}
}
