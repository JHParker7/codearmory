package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Push events. Until now a push reached no other service, so nothing on the platform
// could react to one: no CI, no notifications, no status reporting. hooks already
// ingests authenticated events from trusted services at /internal/events (the tickets
// service does the same), so this is wiring rather than new machinery — the existing
// rule engine then matches a push like any other event.
//
// Three properties matter, and all three are about not harming the push:
//
//   - it fires AFTER the pack is accepted and HEAD reconciled, never inside the
//     streaming path;
//   - it is detached from the request, so a slow hooks service cannot hold a git
//     client open;
//   - a delivery failure is logged and dropped. A push that reached the disk has
//     succeeded; failing it afterwards because a downstream listener was unreachable
//     would be a lie to the client and would leave the repo and the client disagreeing.

const (
	gitHookSource = "codearmory_git_factory"
	eventPush     = "git.push"
)

var (
	// hooksURL is the hooks service base URL. Empty disables the integration, which
	// is the correct default: the platform runs fine without it.
	hooksURL = envOrDefault("HOOKS_URL", "")
	// hooksEventKey is the shared HMAC key authenticating events to hooks.
	hooksEventKey = secret("HOOKS_TRIGGER_KEY")
)

func hooksEnabled() bool { return hooksURL != "" && hooksEventKey != "" }

// gitEventPayload mirrors the shape hooks' /internal/events expects. Payload values
// are flat strings because that is what rule input_mapping consumes.
type gitEventPayload struct {
	Source    string            `json:"source"`
	Event     string            `json:"event"`
	Ref       string            `json:"ref"`
	OrgID     string            `json:"org_id"`
	CreatedBy string            `json:"created_by"`
	Payload   map[string]string `json:"payload"`
}

func signHookEvent(source, event, orgID, createdBy string) (token, ts string) {
	ts = strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(hooksEventKey))
	fmt.Fprintf(mac, "event:%s:%s:%s:%s:%s", source, event, orgID, createdBy, ts)
	return hex.EncodeToString(mac.Sum(nil)), ts
}

// pushFields builds the ref and payload a push event reports, and returns them.
//
// Split out of notifyPush purely so it can be tested: notifyPush reaches headBranch,
// and the read-side DB handle exits the process when there is no database, so a test
// that called it would kill the test binary rather than fail. Everything here is a
// pure function of its arguments — head is passed in for the same reason.
func pushFields(re Repo, pusher, head string, refs []string, before map[string]string) (string, map[string]string) {
	fields := map[string]string{
		"repo_id":        re.ID,
		"repo":           re.Namespace + "/" + re.Name,
		"namespace":      re.Namespace,
		"name":           re.Name,
		"default_branch": head,
		"pusher":         pusher,
		"clone_url":      re.HttpUrl,
		"ref_count":      strconv.Itoa(len(refs)),
	}
	// The single most useful field for a rule ("build when main changes") is the ref,
	// so it is both a top-level field and part of the payload.
	ref := head
	if len(refs) > 0 {
		ref = refs[0]
	}
	fields["ref"] = ref
	// The SHA this ref pointed at before the push. Without it a consumer can only ask
	// git what the LAST COMMIT changed, which is not what the push changed: a push of
	// n commits looks like a push of one, and everything the other n-1 touched is
	// invisible. That is not a cosmetic difference for a CI pipeline deciding what to
	// rebuild — it silently skips services and still reports success.
	//
	// Omitted rather than zero-filled when unknown (a newly created branch, or a
	// caller with no snapshot): absent means "cannot tell", which a consumer must
	// handle by falling back to its own default. A zero SHA would look like a real
	// value and diff against the empty tree.
	if b := before[ref]; b != "" {
		fields["before"] = b
	}
	return ref, fields
}

// notifyPush emits a push event describing what landed. pusher is the authenticated
// user id; refs are the branches the push updated; before maps ref name to the SHA it
// pointed at BEFORE the push (nil when the caller has no such snapshot).
func notifyPush(ctx context.Context, re Repo, pusher string, refs []string, before map[string]string) {
	if !hooksEnabled() {
		return
	}
	ref, fields := pushFields(re, pusher, headBranch(ctx, re.ID), refs, before)

	raw, err := json.Marshal(gitEventPayload{
		Source: gitHookSource, Event: eventPush, Ref: ref,
		CreatedBy: pusher, Payload: fields,
	})
	if err != nil {
		slog.WarnContext(ctx, "push event: marshal failed", "Repo_id", re.ID, "error", err)
		return
	}

	// Detached from the request so the emit outlives the response, but carrying the
	// trace so the event correlates with the push that caused it.
	emitCtx := trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))
	go sendHookEvent(emitCtx, eventPush, "", pusher, raw, re.ID)
}

func sendHookEvent(ctx context.Context, event, orgID, createdBy string, raw []byte, repoID string) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "notifyHooks")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	token, ts := signHookEvent(gitHookSource, event, orgID, createdBy)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hooksURL+"/internal/events", bytes.NewReader(raw))
	if err != nil {
		span.RecordError(err)
		slog.WarnContext(ctx, "push event: build request failed", "Repo_id", repoID, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hooks-Token", token)
	req.Header.Set("X-Hooks-Timestamp", ts)

	resp, err := httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "dispatch failed")
		slog.WarnContext(ctx, "push event: dispatch failed", "Repo_id", repoID, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		span.SetStatus(codes.Error, "hooks rejected the event")
		slog.WarnContext(ctx, "push event: hooks rejected", "Repo_id", repoID, "status", resp.StatusCode)
		return
	}
	slog.InfoContext(ctx, "push event emitted", "Repo_id", repoID, "event", event)
}
