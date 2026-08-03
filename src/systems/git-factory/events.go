package main

import (
	"context"
	"log/slog"
	"strconv"

	sdkevents "github.com/code-armory-app/codearmory_sdk/events"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Push events. Until now a push reached no other service, so nothing on the platform could
// react to one: no CI, no notifications, no status reporting. The events service already
// ingests authenticated events from trusted services at /internal/events, so this is wiring
// rather than new machinery — trigger filters then match a push like any other event.
//
// Three properties matter, and all three are about not harming the push:
//
//   - it fires AFTER the pack is accepted and HEAD reconciled, never inside the streaming
//     path;
//   - it is detached from the request, so a slow events service cannot hold a git client open;
//   - a delivery failure is logged and dropped. A push that reached the disk has succeeded;
//     failing it afterwards because a downstream listener was unreachable would be a lie to
//     the client and would leave the repo and the client disagreeing.

const (
	gitEventSource = "codearmory_git_factory"
	eventPush      = "repo.push"
)

var (
	// eventsURL is the events service base URL. Empty disables the integration, which is the
	// correct default: the platform runs fine without it.
	eventsURL = envOrDefault("EVENTS_URL", "")
	// eventsKey is the shared HMAC key authenticating events to the events service.
	eventsKey = secret("EVENTS_TRIGGER_KEY")

	eventEmitter *sdkevents.Emitter
)

// initEventEmitter builds the emitter once the HTTP client exists. Called from main.
func initEventEmitter() {
	eventEmitter = sdkevents.New(eventsURL, eventsKey, gitEventSource, httpClient)
}

func eventsEnabled() bool { return eventEmitter.Enabled() }

// pushFields builds the ref and the payload a push event reports.
//
// Split out of notifyPush purely so it can be tested: notifyPush reaches headBranch, and the
// read-side DB handle exits the process when there is no database, so a test that called it
// would kill the test binary rather than fail. Everything here is a pure function of its
// arguments — head is passed in for the same reason.
func pushFields(re Repo, pusher, head string, refs []string, before map[string]string) (string, map[string]any) {
	fields := map[string]any{
		"repo_id":        re.ID,
		"repo":           re.Namespace + "/" + re.Name,
		"namespace":      re.Namespace,
		"name":           re.Name,
		"default_branch": head,
		"pusher":         pusher,
		"clone_url":      re.HttpUrl,
		"ref_count":      strconv.Itoa(len(refs)),
	}
	// The single most useful field for a filter ("build when main changes") is the ref, so it
	// is both a top-level field and part of the payload.
	ref := head
	if len(refs) > 0 {
		ref = refs[0]
	}
	fields["ref"] = ref
	// The SHA this ref pointed at before the push. Without it a consumer can only ask git what
	// the LAST COMMIT changed, which is not what the push changed: a push of n commits looks
	// like a push of one, and everything the other n-1 touched is invisible. That is not a
	// cosmetic difference for a CI pipeline deciding what to rebuild — it silently skips
	// services and still reports success.
	//
	// Omitted rather than zero-filled when unknown (a newly created branch, or a caller with
	// no snapshot): absent means "cannot tell", which a consumer must handle by falling back
	// to its own default. A zero SHA would look like a real value and diff against the empty
	// tree.
	if b := before[ref]; b != "" {
		fields["before"] = b
	}
	return ref, fields
}

// notifyPush emits a push event describing what landed. pusher is the authenticated user id;
// refs are the branches the push updated; before maps ref name to the SHA it pointed at BEFORE
// the push (nil when the caller has no such snapshot).
func notifyPush(ctx context.Context, re Repo, pusher string, refs []string, before map[string]string) {
	if !eventsEnabled() {
		return
	}
	_, fields := pushFields(re, pusher, headBranch(ctx, re.ID), refs, before)

	// The tenant is the repo's OWNER, not the pusher. Triggers are matched per tenant, and the
	// thing that should react to a push is the CI the repo owner configured — a collaborator
	// pushing to someone else's repo must not silently run their own triggers instead, nor
	// suppress the owner's. (The former hooks service scoped by the pusher, so a push by a
	// collaborator fired the collaborator's rules and never the owner's.) The pusher is still
	// on the payload as data.pusher for filters that care who pushed.
	ev := sdkevents.Event{
		Type:    eventPush,
		Source:  gitEventSource,
		Subject: re.Namespace + "/" + re.Name,
		Actor:   sdkevents.Actor{UserID: re.Owner},
		Data:    fields,
	}

	// Detached from the request so the emit outlives the response, but carrying the trace so
	// the event correlates with the push that caused it.
	emitCtx := trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))
	go emitEvent(emitCtx, ev, re.ID)
}

func emitEvent(ctx context.Context, ev sdkevents.Event, repoID string) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "notifyEvents")
	defer span.End()

	if err := eventEmitter.Emit(ctx, ev); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "emit failed")
		slog.WarnContext(ctx, "push event: emit failed", "repo_id", repoID, "error", err)
		return
	}
	slog.InfoContext(ctx, "push event emitted", "repo_id", repoID, "event", ev.Type)
}
