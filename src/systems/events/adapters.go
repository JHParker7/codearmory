package main

import (
	"net/http"

	"go.opentelemetry.io/otel"
)

// gitWebhook is a provider-neutral push payload. Provider-specific adapters (Gitea, GitHub)
// verify their own signature and normalize into this shape; this handler turns it into a
// repo.push envelope and dispatches it. External signature verification for each provider
// folds in next — this is the normalized intake.
type gitWebhook struct {
	Repo    string `json:"repo"`    // becomes the event subject (namespace/name)
	Event   string `json:"event"`   // push, tag, …
	Ref     string `json:"ref"`     // short branch/tag
	Commit  string `json:"commit"`
	Pusher  string `json:"pusher"`
	Message string `json:"message"`
	OrgID   string `json:"org_id"`
	UserID  string `json:"user_id"`
}

func handleGitWebhook(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleGitWebhook")
	defer span.End()

	var p gitWebhook
	if err := decodeBody(r, &p); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if p.Repo == "" || p.Event == "" {
		http.Error(w, "repo and event are required", http.StatusBadRequest)
		return
	}
	e := Event{
		Type:    "repo." + p.Event,
		Source:  "git",
		Subject: p.Repo,
		Actor:   Actor{OrgID: p.OrgID, UserID: p.UserID},
		Data: map[string]any{
			"ref": p.Ref, "commit": p.Commit, "pusher": p.Pusher, "message": p.Message,
		},
	}
	if msg := validateEvent(&e); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if err := addEvent(ctx, e); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	go evaluateAndDispatch(detach(ctx), e)
	w.WriteHeader(http.StatusAccepted)
}
