package cmd

import "testing"

// resolvePassword is the whole point of the non-interactive login path: without a
// terminal it must never reach term.ReadPassword (which fails with "inappropriate ioctl
// for device"), and it must prefer the most explicit source given.
func TestResolvePassword_PrefersStdinOverEnv(t *testing.T) {
	t.Setenv(envPassword, "from-env")
	restore := stubPasswordSources(t, "from-stdin", "from-prompt", true)
	defer restore()

	got, err := resolvePassword(true)
	if err != nil {
		t.Fatalf("resolvePassword: %v", err)
	}
	if got != "from-stdin" {
		t.Fatalf("--password-stdin should win, got %q", got)
	}
}

func TestResolvePassword_EnvUsedWhenNoTerminal(t *testing.T) {
	t.Setenv(envPassword, "from-env")
	restore := stubPasswordSources(t, "from-stdin", "from-prompt", false)
	defer restore()

	got, err := resolvePassword(false)
	if err != nil {
		t.Fatalf("resolvePassword: %v", err)
	}
	if got != "from-env" {
		t.Fatalf("want env password, got %q", got)
	}
}

func TestResolvePassword_PromptsWhenInteractiveAndNoEnv(t *testing.T) {
	t.Setenv(envPassword, "")
	restore := stubPasswordSources(t, "from-stdin", "from-prompt", true)
	defer restore()

	got, err := resolvePassword(false)
	if err != nil {
		t.Fatalf("resolvePassword: %v", err)
	}
	if got != "from-prompt" {
		t.Fatalf("want prompted password, got %q", got)
	}
}

// The regression this whole change exists for: no terminal, no credential — the user
// must get an actionable message, not a raw ioctl error.
func TestResolvePassword_NoTerminalNoEnv_ExplainsHow(t *testing.T) {
	t.Setenv(envPassword, "")
	restore := stubPasswordSources(t, "from-stdin", "from-prompt", false)
	defer restore()

	_, err := resolvePassword(false)
	if err == nil {
		t.Fatal("want an error when there is no terminal and no password")
	}
	for _, want := range []string{envPassword, "--password-stdin"} {
		if !contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
}

func TestReadPasswordStdin_StripsTrailingNewline(t *testing.T) {
	// The real reader, not the stub: `echo secret | armory ...` appends a newline that
	// must not become part of the password.
	if got := trimPassword("s3cret\n"); got != "s3cret" {
		t.Fatalf("want s3cret, got %q", got)
	}
	if got := trimPassword("s3cret\r\n"); got != "s3cret" {
		t.Fatalf("want s3cret (CRLF), got %q", got)
	}
	// A password may legitimately contain spaces — only line endings are stripped.
	if got := trimPassword("two words\n"); got != "two words" {
		t.Fatalf("want 'two words', got %q", got)
	}
}

func stubPasswordSources(t *testing.T, stdin, prompt string, terminal bool) func() {
	t.Helper()
	origStdin, origPrompt, origTerm := readPasswordStdin, readPassword, stdinIsTerminal
	readPasswordStdin = func() (string, error) { return stdin, nil }
	readPassword = func() (string, error) { return prompt, nil }
	stdinIsTerminal = func() bool { return terminal }
	return func() {
		readPasswordStdin, readPassword, stdinIsTerminal = origStdin, origPrompt, origTerm
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
