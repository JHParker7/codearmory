package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// Forgejo / Gitea webhook adapter.
//
// Forgejo and Gitea send a nested payload (repository.full_name, ref, head_commit,
// pusher) — the same shape GitHub uses, which normalizeGitHubPayload already parses.
// The GitHub handler is tied to a GitHub App (JWT, check-runs, installation tokens);
// none of that applies here, so this is a lightweight handler that reuses the generic
// matchAndDispatch flow (as /hooks/git does) with the nested payload flattened.
//
// The event comes from the X-Gitea-Event / X-Forgejo-Event header. The HMAC is
// verified as X-Hub-Signature-256 (Forgejo sends it in the GitHub-compatible
// "sha256=<hex>" form), so the existing rule-secret verification path applies unchanged.

// normalizeGiteaPayload flattens a Gitea/Forgejo webhook into the generic gitPayload.
// eventType is the X-Gitea-Event header ("push", "pull_request", ...). ok=false for an
// event we do not translate, so the caller acknowledges and ignores it.
func normalizeGiteaPayload(eventType string, rawBody []byte) (gitPayload, bool) {
	switch eventType {
	case "push":
		var push struct {
			Ref        string `json:"ref"`
			HeadCommit struct {
				ID      string `json:"id"`
				Message string `json:"message"`
			} `json:"head_commit"`
			// Gitea's pusher is a User object (username/login), unlike GitHub's
			// {name}. Try both so the pusher is populated either way.
			Pusher struct {
				Username string `json:"username"`
				Login    string `json:"login"`
			} `json:"pusher"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(rawBody, &push); err != nil {
			return gitPayload{}, false
		}
		pusher := push.Pusher.Username
		if pusher == "" {
			pusher = push.Pusher.Login
		}
		return gitPayload{
			Repo:  push.Repository.FullName,
			Event: "push",
			// Strip refs/heads/ and refs/tags/ so a rule's ref_filter matches the
			// bare branch name ("main"), mirroring the GitHub adapter. The full ref is
			// still recoverable, and HOOK_COMMIT carries the exact SHA to check out.
			Ref:     stripRef(push.Ref),
			Commit:  push.HeadCommit.ID,
			Pusher:  pusher,
			Message: push.HeadCommit.Message,
		}, true

	case "pull_request":
		var pr struct {
			Action      string `json:"action"`
			PullRequest struct {
				Head struct {
					Ref string `json:"ref"`
					SHA string `json:"sha"`
				} `json:"head"`
				User struct {
					Username string `json:"username"`
					Login    string `json:"login"`
				} `json:"user"`
				Title string `json:"title"`
			} `json:"pull_request"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(rawBody, &pr); err != nil {
			return gitPayload{}, false
		}
		user := pr.PullRequest.User.Username
		if user == "" {
			user = pr.PullRequest.User.Login
		}
		return gitPayload{
			Repo:    pr.Repository.FullName,
			Event:   "pull_request." + pr.Action,
			Ref:     pr.PullRequest.Head.Ref,
			Commit:  pr.PullRequest.Head.SHA,
			Pusher:  user,
			Message: pr.PullRequest.Title,
		}, true

	default:
		return gitPayload{}, false
	}
}

func stripRef(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	return ref
}

// giteaEvent reads the event type from the Forgejo/Gitea headers.
func giteaEvent(r *http.Request) string {
	if e := r.Header.Get("X-Gitea-Event"); e != "" {
		return e
	}
	return r.Header.Get("X-Forgejo-Event")
}

func handleGiteaWebhook(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleGiteaWebhook")
	defer span.End()

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		span.SetStatus(codes.Error, "read body failed")
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	eventType := giteaEvent(r)
	if eventType == "ping" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if eventType == "" {
		http.Error(w, "missing X-Gitea-Event / X-Forgejo-Event header", http.StatusBadRequest)
		return
	}

	payload, ok := normalizeGiteaPayload(eventType, rawBody)
	if !ok {
		// An event we don't translate (release, issue, ...): acknowledge and ignore,
		// so Forgejo doesn't retry or mark the delivery failed.
		w.WriteHeader(http.StatusOK)
		return
	}
	if payload.Repo == "" {
		http.Error(w, "repository.full_name missing from payload", http.StatusBadRequest)
		return
	}

	meterHooksReceived.Add(ctx, 1, metric.WithAttributes(attribute.String("source", payload.Repo)))

	payloadMap := map[string]string{
		"repo":    payload.Repo,
		"event":   payload.Event,
		"ref":     payload.Ref,
		"commit":  payload.Commit,
		"pusher":  payload.Pusher,
		"message": payload.Message,
	}

	eventID := uuid.New().String()
	newEvent := HookEvent{
		EventID:   eventID,
		Source:    payload.Repo,
		EventType: payload.Event,
		Ref:       payload.Ref,
		Payload:   payloadMap,
		Status:    "received",
		CreatedAt: time.Now().UTC(),
	}
	if err := newEvent.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert event failed")
		slog.ErrorContext(ctx, "gitea webhook: insert event", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Same dispatch path as /hooks/git: the rule's secret (if any) is verified against
	// X-Hub-Signature-256, which Forgejo sends.
	trigResults, successCount, failCount := matchAndDispatch(
		ctx, eventID,
		payload.Repo, payload.Event, payload.Ref,
		payloadMap, gitBaseInputs(payload),
		rawBody, r.Header.Get("X-Hub-Signature-256"), false, "", "",
	)

	triggers := make([]HookTrigger, 0, len(trigResults))
	for _, tr := range trigResults {
		triggers = append(triggers, tr.Trigger)
	}

	total := successCount + failCount
	eventStatus := "received"
	switch {
	case total == 0:
		eventStatus = "received"
	case failCount == 0:
		eventStatus = "triggered"
	case successCount == 0:
		eventStatus = "failed"
	default:
		eventStatus = "partial"
	}
	updateEventStatus(eventID, total, eventStatus)

	event := HookEvent{
		EventID:      eventID,
		Source:       payload.Repo,
		EventType:    payload.Event,
		Ref:          payload.Ref,
		Payload:      payloadMap,
		RulesMatched: total,
		Status:       eventStatus,
		Triggers:     triggers,
		CreatedAt:    time.Now().UTC(),
	}
	if event.Triggers == nil {
		event.Triggers = []HookTrigger{}
	}

	span.SetAttributes(
		attribute.String("event.id", eventID),
		attribute.String("source", payload.Repo),
		attribute.Int("rules.matched", total),
	)
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(event) //nolint:errcheck
}
