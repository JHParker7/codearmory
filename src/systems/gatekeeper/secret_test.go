package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSecret_PlainEnvVar verifies that secret() returns the value of the plain
// environment variable when the _FILE variant is not set.
func TestSecret_PlainEnvVar(t *testing.T) {
	const key = "GK_TEST_SECRET_PLAIN"
	t.Setenv(key, "my-plain-value")
	if got := secret(key); got != "my-plain-value" {
		t.Fatalf("secret(%q) = %q, want %q", key, got, "my-plain-value")
	}
}

// TestSecret_EmptyWhenUnset verifies that secret() returns an empty string
// when neither the variable nor its _FILE variant is set.
func TestSecret_EmptyWhenUnset(t *testing.T) {
	const key = "GK_TEST_SECRET_UNSET_XYZ123"
	os.Unsetenv(key)
	os.Unsetenv(key + "_FILE")
	if got := secret(key); got != "" {
		t.Fatalf("secret(%q) = %q, want empty string", key, got)
	}
}

// TestSecret_FilePathTakesPrecedence verifies that when both a plain variable
// and a _FILE variable are set, the file content wins.
func TestSecret_FilePathTakesPrecedence(t *testing.T) {
	const key = "GK_TEST_SECRET_FILE_PREC"
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(path, []byte("from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(key, "from-env")
	t.Setenv(key+"_FILE", path)
	if got := secret(key); got != "from-file" {
		t.Fatalf("secret(%q) = %q, want %q", key, got, "from-file")
	}
}

// TestSecret_FileTrimsWhitespace verifies that secret() strips trailing
// whitespace (newlines added by editors or Docker secret mounts) from file content.
func TestSecret_FileTrimsWhitespace(t *testing.T) {
	const key = "GK_TEST_SECRET_TRIM"
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(path, []byte("  trimmed-value  \n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(key+"_FILE", path)
	if got := secret(key); got != "trimmed-value" {
		t.Fatalf("secret(%q) = %q, want %q", key, got, "trimmed-value")
	}
}
