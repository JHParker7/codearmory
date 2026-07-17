package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Blob storage. Two backends behind one interface:
//
//   - fsStore: one file per artifact under DATA_DIR/<user_id>/<name>. Simple, but a
//     ReadWriteOnce PVC pins the whole store to ONE node/replica — fine for a single
//     dev box, a bottleneck and a SPOF on a real multi-node cluster.
//   - s3Store: an S3-compatible object store (MinIO, Ceph RGW, AWS). Shared across
//     nodes, so the artifacts service scales to N replicas — this is the multi-node
//     answer. See store_s3.go.
//
// The backend is chosen at startup from config (newBlobStore); everything above the
// interface — quota accounting, the API, the name rules — is identical for both.

// blobStore is the storage backend for artifact bytes. The database is the source of
// truth for existence and size; this only moves the bytes.
type blobStore interface {
	// Write streams r to the user's artifact, enforcing `limit` bytes as it goes,
	// returning the stored size and its sha256. A write that exceeds the limit, or
	// otherwise fails, must not replace an existing good artifact.
	Write(ctx context.Context, userID, name string, r io.Reader, limit int64) (size int64, digest string, err error)
	// Open returns the artifact's bytes for reading. The caller closes it.
	Open(ctx context.Context, userID, name string) (io.ReadCloser, error)
	// Remove deletes the artifact. A missing blob is NOT an error — the row is the
	// source of truth, and a half-deleted artifact must still delete cleanly.
	Remove(ctx context.Context, userID, name string) error
}

// store is the process-wide backend, set once by newBlobStore in main.
var store blobStore

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

// objectKey is a user's artifact key: "<hashed-user>/<name>". The same layout serves
// as a filesystem path (fsStore) and an S3 object key (s3Store) — both use '/' as the
// separator, and the name is already validated to contain none.
func objectKey(userID, name string) string {
	return safeSegment(userID) + "/" + name
}

// safeSegment makes a user id safe as a path/key segment. Ids are gatekeeper UUIDs,
// but this never trusts that: a crafted id must not be able to walk the tree.
func safeSegment(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// stageBlob streams r to a temporary file, enforcing `limit` as it copies and
// computing the sha256. It returns the temp file path (caller must remove it), the
// size, and the digest.
//
// The limit is checked DURING the copy, not after: trusting Content-Length would let a
// client under-declare and write past the quota, and buffering the whole body in
// memory to measure it would trade a disk overrun for a memory one. Both backends
// stage here first — fsStore then renames it into place; s3Store uploads it — so
// neither can replace a good artifact with a partial one on failure or over-quota.
//
// The caller chooses `dir`: fsStore stages in the SAME directory as the final file so
// the rename that follows is atomic (rename cannot cross filesystems); s3Store stages
// in a scratch dir. Neither uses the OS default temp dir blindly — the pod runs with a
// read-only root filesystem, so the only writable places are the volumes we mount.
func stageBlob(dir string, r io.Reader, limit int64) (path string, size int64, digest string, err error) {
	tmp, err := os.CreateTemp(dir, "artifact-upload-*")
	if err != nil {
		return "", 0, "", err
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
		return "", 0, "", err
	}
	if n > limit {
		return "", n, "", errOverQuota
	}
	if err = tmp.Close(); err != nil {
		return "", 0, "", err
	}
	return tmpName, n, hex.EncodeToString(h.Sum(nil)), nil
}

// fsStore keeps blobs as files under a data directory.
type fsStore struct{ dir string }

func newFSStore(dir string) *fsStore { return &fsStore{dir: dir} }

func (s *fsStore) path(userID, name string) string {
	return filepath.Join(s.dir, safeSegment(userID), name)
}

func (s *fsStore) Write(_ context.Context, userID, name string, r io.Reader, limit int64) (int64, string, error) {
	final := s.path(userID, name)
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 0, "", err
	}
	tmp, size, digest, err := stageBlob(dir, r, limit)
	if err != nil {
		return size, "", err
	}
	// Rename into place — atomic on the same filesystem, so a reader never sees a
	// half-written blob and a good artifact is only replaced once the new one is whole.
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return 0, "", err
	}
	return size, digest, nil
}

func (s *fsStore) Open(_ context.Context, userID, name string) (io.ReadCloser, error) {
	return os.Open(s.path(userID, name))
}

func (s *fsStore) Remove(_ context.Context, userID, name string) error {
	err := os.Remove(s.path(userID, name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
