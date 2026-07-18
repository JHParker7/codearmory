package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	ok := []string{"gocache", "go-cache.tar", "armory_linux_amd64", "a", "cache.tar.gz"}
	for _, n := range ok {
		if msg := validateName(n); msg != "" {
			t.Errorf("validateName(%q) = %q, want valid", n, msg)
		}
	}
	// A name becomes a path segment, so traversal and separators are a security
	// boundary rather than a style preference.
	bad := map[string]string{
		"":                                "required",
		"../etc/passwd":                   "letters, digits",
		"a/b":                             "letters, digits",
		"..":                              "letters, digits",
		".hidden":                         "start alphanumeric",
		"has space":                       "letters, digits",
		strings.Repeat("x", maxNameLen+1): "exceeds",
	}
	for n, want := range bad {
		msg := validateName(n)
		if msg == "" {
			t.Errorf("validateName(%q) accepted a name it must reject", n)
			continue
		}
		if !strings.Contains(msg, want) {
			t.Errorf("validateName(%q) = %q, want it to mention %q", n, msg, want)
		}
	}
}

// A crafted user id must not be able to walk out of the data dir either.
func TestSafeSegment_ContainsNoPathParts(t *testing.T) {
	for _, id := range []string{"../../etc", "a/b", "..", "b7127b8e-0d36-497e-b8dc-e0ba9f0585f0"} {
		seg := safeSegment(id)
		if strings.ContainsAny(seg, "/.") {
			t.Errorf("safeSegment(%q) = %q, must be a plain segment", id, seg)
		}
	}
	// Distinct users must not collide into one directory.
	if safeSegment("user-a") == safeSegment("user-b") {
		t.Error("two users share a directory")
	}
}

func TestFSStore_PathStaysUnderDataDir(t *testing.T) {
	fs := newFSStore("/data")
	// A crafted user id is hashed to a plain segment, so it cannot escape the dir even
	// before name validation runs.
	p := fs.path("../../root", "cache.tar")
	if !strings.HasPrefix(filepath.Clean(p), "/data/") {
		t.Fatalf("fsStore path escaped the data dir: %s", p)
	}
}

func TestFSStore_RoundTrips(t *testing.T) {
	fs := newFSStore(t.TempDir())
	ctx := context.Background()
	size, digest, err := fs.Write(ctx, "u1", "gocache", strings.NewReader("hello cache"), 1024)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if size != 11 {
		t.Errorf("size = %d, want 11", size)
	}
	if digest == "" {
		t.Error("want a digest so a caller can skip a download it already has")
	}
	f, err := fs.Open(ctx, "u1", "gocache")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != "hello cache" {
		t.Errorf("read back %q", got)
	}
}

// The limit is enforced as the body streams, because Content-Length is a claim: a
// client that under-declares must still be stopped.
func TestFSStore_StopsAtTheLimit(t *testing.T) {
	dir := t.TempDir()
	fs := newFSStore(dir)
	ctx := context.Background()
	_, _, err := fs.Write(ctx, "u1", "big", strings.NewReader(strings.Repeat("x", 100)), 10)
	if !errors.Is(err, errOverQuota) {
		t.Fatalf("err = %v, want errOverQuota", err)
	}
	// An over-quota upload must not leave the artifact — nor a temp file behind.
	if _, err := fs.Open(ctx, "u1", "big"); !os.IsNotExist(err) {
		t.Error("a rejected upload left an artifact behind")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, safeSegment("u1")))
	if len(entries) != 0 {
		t.Errorf("rejected upload left %d file(s) behind: %v", len(entries), entries)
	}
}

// A body exactly at the limit is allowed: the boundary is inclusive, so a cache sized
// to the quota still saves.
func TestFSStore_ExactlyAtLimitSucceeds(t *testing.T) {
	fs := newFSStore(t.TempDir())
	size, _, err := fs.Write(context.Background(), "u1", "exact", strings.NewReader(strings.Repeat("x", 10)), 10)
	if err != nil {
		t.Fatalf("a body exactly at the limit must succeed, got %v", err)
	}
	if size != 10 {
		t.Errorf("size = %d, want 10", size)
	}
}

// Replacing an artifact must not leave the old bytes; a rejected replace must not
// destroy the good one.
func TestFSStore_ReplaceIsAtomic(t *testing.T) {
	fs := newFSStore(t.TempDir())
	ctx := context.Background()
	if _, _, err := fs.Write(ctx, "u1", "c", strings.NewReader("first-version"), 1024); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fs.Write(ctx, "u1", "c", strings.NewReader("second"), 1024); err != nil {
		t.Fatal(err)
	}
	f, _ := fs.Open(ctx, "u1", "c")
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "second" {
		t.Errorf("read %q, want the replacement only", got)
	}
	// A failed replace must leave the GOOD artifact in place, not a partial one.
	if _, _, err := fs.Write(ctx, "u1", "c", strings.NewReader(strings.Repeat("x", 99)), 5); !errors.Is(err, errOverQuota) {
		t.Fatalf("want errOverQuota, got %v", err)
	}
	f2, err := fs.Open(ctx, "u1", "c")
	if err != nil {
		t.Fatal("a rejected replace destroyed the existing artifact")
	}
	got2, _ := io.ReadAll(f2)
	f2.Close()
	if string(got2) != "second" {
		t.Errorf("after a rejected replace the artifact is %q, want it untouched", got2)
	}
}

func TestFSStore_RemoveMissingIsNotAnError(t *testing.T) {
	fs := newFSStore(t.TempDir())
	if err := fs.Remove(context.Background(), "u1", "never-existed"); err != nil {
		t.Errorf("removing a missing blob must be a no-op, got %v", err)
	}
}

// The object key is the same layout for both backends: a hashed user segment and the
// raw (validated) name, '/'-joined. This is what an S3 key and a filesystem path share.
func TestObjectKey(t *testing.T) {
	k := objectKey("user-a", "gocache")
	if !strings.HasSuffix(k, "/gocache") || strings.Contains(k, "user-a") {
		t.Errorf("objectKey = %q, want <hashed-user>/gocache", k)
	}
	if strings.Count(k, "/") != 1 {
		t.Errorf("objectKey = %q, want exactly one separator", k)
	}
}

// The S3 backend is selected by the presence of a bucket; without one, newS3Store
// refuses rather than silently misbehaving.
func TestNewS3Store_RequiresBucket(t *testing.T) {
	t.Setenv("ARTIFACTS_S3_BUCKET", "")
	if _, err := newS3Store(context.Background()); err == nil {
		t.Error("newS3Store must require a bucket")
	}
}
