package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Blob storage: one file per artifact under DATA_DIR/<user_id>/<name>.
//
// A plain filesystem rather than an object store because the deployment already
// has a PVC to hand and the alternative is a new external dependency. The layout is
// user-first so a user's tree can be sized, listed, or removed without touching the
// database.

// nameRe constrains an artifact name. Names come from users and become PATH
// SEGMENTS, so this is a security boundary, not a style rule: anything outside this
// set (notably '/' and '.') cannot escape the user's directory.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateName rejects a name that is unusable or unsafe as a path segment.
// Returns "" when valid.
func validateName(name string) string {
	if name == "" {
		return "name is required"
	}
	if len(name) > maxNameLen {
		return fmt.Sprintf("name exceeds %d characters", maxNameLen)
	}
	if !nameRe.MatchString(name) {
		return "name must start alphanumeric and contain only letters, digits, dot, dash or underscore"
	}
	// Belt and braces: nameRe already excludes '/', but traversal is the failure
	// that matters most, so it is checked explicitly rather than inferred.
	if strings.Contains(name, "..") {
		return "name must not contain .."
	}
	return ""
}

// blobPath is where a user's artifact lives. Callers MUST have validated the name.
func blobPath(userID, name string) string {
	return filepath.Join(dataDir(), safeSegment(userID), name)
}

// safeSegment makes a user id safe as a directory name. Ids are gatekeeper UUIDs,
// but this never trusts that: a crafted id must not be able to walk the tree.
func safeSegment(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// writeBlob streams r to the user's artifact, enforcing `limit` as it goes and
// returning the size and digest.
//
// The limit is checked DURING the copy, not after: trusting Content-Length would let
// a client under-declare and write past the quota, and buffering the whole body to
// measure it first would trade a disk overrun for a memory one. The write goes to a
// temp file and is renamed on success, so a failed or over-quota upload cannot
// replace a good artifact with a partial one.
func writeBlob(userID, name string, r io.Reader, limit int64) (size int64, digest string, err error) {
	dir := filepath.Dir(blobPath(userID, name))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 0, "", err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return 0, "", err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		if err != nil {
			os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	// limit+1 so a body exactly at the limit succeeds and the first byte past it is
	// still read — which is what makes "over quota" detectable rather than silently
	// truncating at the boundary.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, limit+1))
	if err != nil {
		return 0, "", err
	}
	if n > limit {
		return n, "", errOverQuota
	}
	if err = tmp.Close(); err != nil {
		return 0, "", err
	}
	if err = os.Rename(tmpName, blobPath(userID, name)); err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// openBlob opens an artifact for reading.
func openBlob(userID, name string) (*os.File, error) {
	return os.Open(blobPath(userID, name))
}

// removeBlob deletes an artifact's file. A missing file is not an error: the row is
// the source of truth, and a half-deleted artifact should still delete cleanly.
func removeBlob(userID, name string) error {
	err := os.Remove(blobPath(userID, name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
