package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// Git LFS.
//
// Without it, a large binary goes straight into a packfile and stays there forever:
// every clone pays for it, and the repack maintenance this service runs cannot help,
// because the object is history. LFS moves the bytes out of git — the commit carries a
// small pointer file, and the content lives in a content-addressed store keyed by its
// own sha256.
//
// Only the "basic" transfer adapter is implemented. It is what the client uses by
// default and the only one required: the batch endpoint answers with plain PUT/GET
// hrefs on this service, and the client uploads and downloads through them. There is no
// separate object service to deploy and no signed-URL machinery to get wrong.
//
// Auth is the GIT WIRE's, not the JSON API's: an LFS client is a git client, and it
// presents the same credential to the same host. Download authorizes as a read
// (upload-pack) and upload as a write (receive-pack), so LFS can never grant access the
// underlying repo would refuse — including for a public repo, where anonymous read works
// and anonymous write does not.

// lfsOIDRe constrains an object id. LFS oids are sha256 hex, and the value becomes a
// PATH SEGMENT in the object store, so this is the traversal guard: no dots, no
// separators, nothing but 64 hex characters.
var lfsOIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// lfsMaxObjectBytes caps a single uploaded object. 0 disables the cap.
var lfsMaxObjectBytes = func() int64 {
	if v := strings.TrimSpace(envOrDefault("GIT_LFS_MAX_OBJECT_MB", "")); v != "" {
		if mb, err := strconv.ParseInt(v, 10, 64); err == nil && mb > 0 {
			return mb * 1024 * 1024
		}
	}
	return 0
}()

// lfsEnabled reports whether the feature is on. Off by default: LFS needs storage
// headroom an operator should opt into rather than discover.
func lfsEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(envOrDefault("GIT_LFS_ENABLED", "false")), "true")
}

// lfsObjectPath is where an object's bytes live: <storage>/lfs/<repo-id>/<ab>/<oid>.
//
// Per REPO rather than globally content-addressed. Sharing one store across repos would
// deduplicate identical objects, which sounds like a win right up to the point where it
// becomes an access-control hole: possession of an oid — which is just the sha256 of a
// file you already have — would let you read an object uploaded to a private repo you
// cannot see. Storing per repo keeps the authorization boundary the same as the repo's.
func lfsObjectPath(repoID, oid string) (string, error) {
	if !lfsOIDRe.MatchString(oid) {
		return "", errors.New("invalid lfs oid")
	}
	// Canonical uuid, checked the same way repoDiskPath checks it and for the same
	// reason: this builds a filesystem path, so a merely uuid-PARSEABLE id is not
	// enough — it must round-trip to exactly the string given.
	if u, err := uuid.Parse(repoID); err != nil || u.String() != repoID {
		return "", errors.New("invalid repo id")
	}
	// Same root and default as repoDiskPath, so LFS lands beside the repos rather than
	// somewhere a deployment forgot to mount.
	root := secretOrDefault("GIT_STORAGE_ROOT", "temp/repos")
	return filepath.Join(root, "lfs", repoID, oid[:2], oid), nil
}

// lfsBatchRequest is the client's ask: which objects it wants to move, and which way.
type lfsBatchRequest struct {
	Operation string       `json:"operation"` // "download" | "upload"
	Transfers []string     `json:"transfers,omitempty"`
	Objects   []lfsPointer `json:"objects"`
}

type lfsPointer struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type lfsAction struct {
	Href   string            `json:"href"`
	Header map[string]string `json:"header,omitempty"`
}

type lfsObjectResponse struct {
	OID     string               `json:"oid"`
	Size    int64                `json:"size"`
	Actions map[string]lfsAction `json:"actions,omitempty"`
	Error   *lfsObjectError      `json:"error,omitempty"`
}

type lfsObjectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// lfsContentType is the media type the LFS spec requires on batch requests and
// responses. Clients check it, so it is not optional decoration.
const lfsContentType = "application/vnd.git-lfs+json"

// handleLFSBatch answers the batch endpoint: for each object, tell the client where to
// PUT or GET it, or why it cannot.
func handleLFSBatch(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer(serviceName).Start(r.Context(), "handleLFSBatch")
	defer span.End()

	if !lfsEnabled() {
		writeLFSError(w, http.StatusNotImplemented, "git lfs is not enabled on this server")
		return
	}
	var req lfsBatchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeLFSError(w, http.StatusBadRequest, "invalid batch request")
		return
	}
	// The operation decides the permission, so it is resolved BEFORE authorizing —
	// an upload must not be authorized as a read.
	var svc gitService
	switch req.Operation {
	case "download":
		svc = svcUploadPack
	case "upload":
		svc = svcReceivePack
	default:
		writeLFSError(w, http.StatusBadRequest, "operation must be 'download' or 'upload'")
		return
	}
	re, _, ok := authorizeGitRepo(w, r, svc)
	if !ok {
		return
	}

	base := strings.TrimRight(gitHTTPBaseURL(), "/") + "/" + re.Namespace + "/" + re.Name + ".git/info/lfs/objects/"
	out := make([]lfsObjectResponse, 0, len(req.Objects))
	for _, o := range req.Objects {
		resp := lfsObjectResponse{OID: o.OID, Size: o.Size}
		if !lfsOIDRe.MatchString(o.OID) {
			resp.Error = &lfsObjectError{Code: http.StatusUnprocessableEntity, Message: "invalid oid"}
			out = append(out, resp)
			continue
		}
		path, err := lfsObjectPath(re.ID, o.OID)
		if err != nil {
			resp.Error = &lfsObjectError{Code: http.StatusInternalServerError, Message: "storage unavailable"}
			out = append(out, resp)
			continue
		}
		_, statErr := os.Stat(path)
		switch req.Operation {
		case "download":
			if statErr != nil {
				// 404 per object, not for the whole batch: the rest may well be here.
				resp.Error = &lfsObjectError{Code: http.StatusNotFound, Message: "object not found"}
			} else {
				resp.Actions = map[string]lfsAction{"download": {Href: base + o.OID}}
			}
		case "upload":
			if lfsMaxObjectBytes > 0 && o.Size > lfsMaxObjectBytes {
				resp.Error = &lfsObjectError{Code: http.StatusRequestEntityTooLarge,
					Message: "object exceeds the server's per-object limit"}
				break
			}
			// An object already present needs no action at all — that is how LFS avoids
			// re-uploading what the server has, and omitting "actions" is the signal.
			if statErr == nil {
				break
			}
			resp.Actions = map[string]lfsAction{"upload": {Href: base + o.OID}}
		}
		out = append(out, resp)
	}

	w.Header().Set("Content-Type", lfsContentType)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": out})
	span.SetStatus(codes.Ok, "")
}

// handleLFSUpload stores one object. The oid is the content's sha256, so it is VERIFIED
// rather than trusted: a client that uploads bytes under the wrong oid would corrupt
// every future checkout that resolves that pointer, and the check costs one pass we are
// already making.
func handleLFSUpload(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleLFSUpload")
	defer span.End()

	if !lfsEnabled() {
		writeLFSError(w, http.StatusNotImplemented, "git lfs is not enabled on this server")
		return
	}
	re, _, ok := authorizeGitRepo(w, r, svcReceivePack)
	if !ok {
		return
	}
	oid := r.PathValue("oid")
	path, err := lfsObjectPath(re.ID, oid)
	if err != nil {
		writeLFSError(w, http.StatusUnprocessableEntity, "invalid oid")
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		slog.ErrorContext(ctx, "lfs: mkdir", "Repo_id", re.ID, "error", err)
		writeLFSError(w, http.StatusInternalServerError, "could not store the object")
		return
	}

	// Stage to a temp file in the SAME directory, so the rename that publishes it is
	// atomic and a failed upload can never be observed as a valid object.
	tmp, err := os.CreateTemp(filepath.Dir(path), "upload-*")
	if err != nil {
		slog.ErrorContext(ctx, "lfs: temp file", "Repo_id", re.ID, "error", err)
		writeLFSError(w, http.StatusInternalServerError, "could not store the object")
		return
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) //nolint:errcheck — best effort; a successful rename makes this a no-op
	}()

	var body io.Reader = r.Body
	if lfsMaxObjectBytes > 0 {
		// +1 so a body exactly at the limit succeeds and the first byte past it is still
		// read, which is what makes "too large" detectable rather than a silent truncation.
		body = io.LimitReader(r.Body, lfsMaxObjectBytes+1)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if err != nil {
		writeLFSError(w, http.StatusInternalServerError, "could not read the object")
		return
	}
	if lfsMaxObjectBytes > 0 && n > lfsMaxObjectBytes {
		writeLFSError(w, http.StatusRequestEntityTooLarge, "object exceeds the server's per-object limit")
		return
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != oid {
		// The whole point of a content-addressed store: the name must be the content.
		writeLFSError(w, http.StatusUnprocessableEntity, "content does not match the oid")
		return
	}
	if err := tmp.Close(); err != nil {
		writeLFSError(w, http.StatusInternalServerError, "could not store the object")
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		slog.ErrorContext(ctx, "lfs: publish object", "Repo_id", re.ID, "error", err)
		writeLFSError(w, http.StatusInternalServerError, "could not store the object")
		return
	}
	slog.InfoContext(ctx, "lfs object stored", "Repo_id", re.ID, "oid", oid, "bytes", n)
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusOK)
}

// handleLFSDownload streams one object back.
func handleLFSDownload(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleLFSDownload")
	defer span.End()

	if !lfsEnabled() {
		writeLFSError(w, http.StatusNotImplemented, "git lfs is not enabled on this server")
		return
	}
	re, _, ok := authorizeGitRepo(w, r, svcUploadPack)
	if !ok {
		return
	}
	path, err := lfsObjectPath(re.ID, r.PathValue("oid"))
	if err != nil {
		writeLFSError(w, http.StatusUnprocessableEntity, "invalid oid")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeLFSError(w, http.StatusNotFound, "object not found")
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := io.Copy(w, f); err != nil {
		slog.WarnContext(ctx, "lfs: download interrupted", "Repo_id", re.ID, "error", err)
	}
	span.SetStatus(codes.Ok, "")
}

// writeLFSError answers in the shape the LFS spec defines, so a client reports the
// reason rather than "unknown error".
func writeLFSError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", lfsContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}
