package main

import (
	"strings"
	"testing"
)

func intPtr(n int) *int { return &n }

func TestValidateCheckout(t *testing.T) {
	shell := []string{"sh", "-c", "make build"}
	refs := map[string]string{"GIT_CLONE_URL": "git:https://github.com/acme/widgets.git"}

	tests := []struct {
		name    string
		spec    *CheckoutSpec
		cmd     []string
		refs    map[string]string
		env     map[string]string
		wantErr string
	}{
		{name: "nil spec is allowed", spec: nil, cmd: shell},
		{name: "default env from secret_ref", spec: &CheckoutSpec{}, cmd: shell, refs: refs},
		{name: "url from plain env", spec: &CheckoutSpec{}, cmd: shell, env: map[string]string{"GIT_CLONE_URL": "https://example.com/x.git"}},
		{name: "custom env resolved", spec: &CheckoutSpec{Env: "REPO"}, cmd: shell, refs: map[string]string{"REPO": "git:https://h/a/b.git"}},
		{name: "full spec", spec: &CheckoutSpec{Path: "src/app", Ref: "release/1.2", Depth: intPtr(0)}, cmd: shell, refs: refs},

		{name: "non-shell command rejected", spec: &CheckoutSpec{}, cmd: []string{"make", "build"}, refs: refs, wantErr: "shell command"},
		{name: "env not provided", spec: &CheckoutSpec{}, cmd: shell, wantErr: "not set by secret_refs or env"},
		{name: "custom env not provided", spec: &CheckoutSpec{Env: "REPO"}, cmd: shell, refs: refs, wantErr: "not set"},
		{name: "invalid env name", spec: &CheckoutSpec{Env: "bad-name"}, cmd: shell, refs: refs, wantErr: "checkout.env"},
		{name: "blocked env name", spec: &CheckoutSpec{Env: "LD_PRELOAD"}, cmd: shell, refs: map[string]string{"LD_PRELOAD": "x"}, wantErr: "not permitted"},
		{name: "path escape rejected", spec: &CheckoutSpec{Path: "../etc"}, cmd: shell, refs: refs, wantErr: "checkout.path"},
		{name: "absolute path rejected", spec: &CheckoutSpec{Path: "/abs"}, cmd: shell, refs: refs, wantErr: "checkout.path"},
		{name: "ref with metachars rejected", spec: &CheckoutSpec{Ref: "main;rm -rf /"}, cmd: shell, refs: refs, wantErr: "checkout.ref"},
		{name: "ref leading dash rejected", spec: &CheckoutSpec{Ref: "-x"}, cmd: shell, refs: refs, wantErr: "checkout.ref"},
		{name: "negative depth rejected", spec: &CheckoutSpec{Depth: intPtr(-1)}, cmd: shell, refs: refs, wantErr: "checkout.depth"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCheckout(tc.spec, tc.cmd, tc.refs, tc.env)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestCheckoutScript(t *testing.T) {
	refs := map[string]string{"GIT_CLONE_URL": "git:https://github.com/acme/widgets.git"}

	t.Run("defaults: shallow, dir from ref, default env", func(t *testing.T) {
		s := (&CheckoutSpec{}).script(refs)
		if !strings.Contains(s, `"$GIT_CLONE_URL"`) {
			t.Errorf("missing default env expansion:\n%s", s)
		}
		if !strings.Contains(s, "--depth=1") {
			t.Errorf("expected shallow default:\n%s", s)
		}
		if !strings.Contains(s, "clone --depth=1 -- \"$GIT_CLONE_URL\" 'widgets'") {
			t.Errorf("expected dir derived from ref (widgets):\n%s", s)
		}
		if !strings.Contains(s, "cd 'widgets'") {
			t.Errorf("expected cd into derived dir:\n%s", s)
		}
	})

	t.Run("full clone omits depth flag", func(t *testing.T) {
		s := (&CheckoutSpec{Depth: intPtr(0)}).script(refs)
		if strings.Contains(s, "--depth") {
			t.Errorf("depth 0 should be a full clone (no --depth):\n%s", s)
		}
	})

	t.Run("ref and explicit path and depth", func(t *testing.T) {
		s := (&CheckoutSpec{Path: "app", Ref: "v2.0", Depth: intPtr(5)}).script(refs)
		if !strings.Contains(s, "--depth=5") || !strings.Contains(s, "--branch='v2.0'") {
			t.Errorf("expected depth and branch flags:\n%s", s)
		}
		if !strings.Contains(s, "-- \"$GIT_CLONE_URL\" 'app'") || !strings.Contains(s, "cd 'app'") {
			t.Errorf("expected explicit path:\n%s", s)
		}
	})

	t.Run("custom env and no ref", func(t *testing.T) {
		s := (&CheckoutSpec{Env: "REPO_URL"}).script(map[string]string{"REPO_URL": "git:https://h/x/y.git"})
		if !strings.Contains(s, `"$REPO_URL"`) || !strings.Contains(s, "${REPO_URL:-}") {
			t.Errorf("expected custom env var used and guarded:\n%s", s)
		}
		if strings.Contains(s, "--branch") {
			t.Errorf("no ref should mean no --branch:\n%s", s)
		}
	})

	t.Run("clone failure aborts the job", func(t *testing.T) {
		s := (&CheckoutSpec{}).script(refs)
		if !strings.Contains(s, "exit 1") {
			t.Errorf("expected the prologue to exit on clone failure:\n%s", s)
		}
	})
}

func TestRepoBasename(t *testing.T) {
	tests := []struct {
		ref  string
		want string
	}{
		{"git:https://github.com/acme/widgets.git", "widgets"},
		{"git:https://gitlab.example.com/team/sub/app.git", "app"},
		{"git:https://host/acme/widgets", "widgets"},
		{"gitea:acme/widgets", "widgets"},
		{"secret:some-token", "some-token"}, // still yields a basename; fine as a dir name
		{"", ""},
		{"git:", ""},
		{"git:https://host/", ""},
	}
	for _, tc := range tests {
		if got := repoBasename(tc.ref); got != tc.want {
			t.Errorf("repoBasename(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestApplyCheckout(t *testing.T) {
	refs := map[string]string{"GIT_CLONE_URL": "git:https://github.com/acme/widgets.git"}
	cmd := []string{"sh", "-c", "make build"}

	t.Run("prepends prologue, preserves shell wrapper and user script", func(t *testing.T) {
		out := applyCheckout(cmd, &CheckoutSpec{}, refs)
		if out[0] != "sh" || out[1] != "-c" {
			t.Fatalf("shell wrapper changed: %v", out[:2])
		}
		if !strings.HasSuffix(out[2], "\nmake build") {
			t.Errorf("user script should be last:\n%s", out[2])
		}
		if !strings.Contains(out[2], "git clone") {
			t.Errorf("clone prologue missing:\n%s", out[2])
		}
		if cmd[2] != "make build" {
			t.Errorf("input command was mutated: %q", cmd[2])
		}
	})

	t.Run("nil spec is a no-op", func(t *testing.T) {
		out := applyCheckout(cmd, nil, refs)
		if out[2] != "make build" {
			t.Errorf("nil checkout should not alter command: %q", out[2])
		}
	})

	t.Run("non-shell command is a no-op", func(t *testing.T) {
		raw := []string{"make", "build"}
		out := applyCheckout(raw, &CheckoutSpec{}, refs)
		if len(out) != 2 || out[0] != "make" {
			t.Errorf("non-shell command should be unchanged: %v", out)
		}
	})
}

// TestCheckoutThenOutputEnvOrdering asserts the clone runs first and the
// output-env capture trailer stays at the very end when both are applied — the
// order the worker uses.
func TestCheckoutThenOutputEnvOrdering(t *testing.T) {
	refs := map[string]string{"GIT_CLONE_URL": "git:https://github.com/acme/widgets.git"}
	cmd := []string{"sh", "-c", "make build"}

	withCheckout := applyCheckout(cmd, &CheckoutSpec{}, refs)
	final := wrapOutputEnv(withCheckout, []string{"VERSION"}, "MARKER")

	script := final[2]
	cloneAt := strings.Index(script, "git clone")
	userAt := strings.Index(script, "make build")
	markerAt := strings.Index(script, "MARKER")
	if !(cloneAt >= 0 && cloneAt < userAt && userAt < markerAt) {
		t.Fatalf("expected clone < user script < output trailer; got %d, %d, %d:\n%s", cloneAt, userAt, markerAt, script)
	}
}
