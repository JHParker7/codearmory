package main

import (
	"encoding/json"
	"io"
	"net/http"

	"go.opentelemetry.io/otel"
)

// Forgejo / Gitea webhook adapter — POST /hooks/gitea.
//
// Forgejo and Gitea send a nested payload (repository.full_name, ref, head_commit, pusher) —
// the same shape GitHub uses. Unlike the GitHub adapter there is no App behind it: no JWT, no
// installation tokens, no check runs, so this is a plain normalize-and-ingest handler.
//
// Signature: Forgejo sends the GitHub-compatible `X-Hub-Signature-256: sha256=<hex>`, and
// Gitea additionally sends `X-Gitea-Signature: <hex>`. verifyGitSignature accepts both.

// giteaPush / giteaPullRequest are the fields we read off the provider payload. Everything
// else in the (large) webhook body is ignored rather than stored, so a filter sees a small,
// stable surface rather than whichever keys the provider happened to send this release.
type giteaPush struct {
	Ref        string `json:"ref"`
	HeadCommit struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	} `json:"head_commit"`
	// Gitea's pusher is a User object (username/login) where GitHub's is {name}; read both so
	// the field is populated on either provider.
	Pusher struct {
		Username string `json:"username"`
		Login    string `json:"login"`
	} `json:"pusher"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type giteaPullRequest struct {
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

// normalizeGiteaEvent turns a Forgejo/Gitea webhook into an envelope. eventType is the
// X-Gitea-Event / X-Forgejo-Event header. ok=false means an event type we do not translate,
// which the caller acknowledges and ignores so the provider does not retry or mark the
// delivery failed.
func normalizeGiteaEvent(eventType string, body []byte) (Event, bool) {
	switch eventType {
	case "push":
		var p giteaPush
		if err := json.Unmarshal(body, &p); err != nil || p.Repository.FullName == "" {
			return Event{}, false
		}
		pusher := p.Pusher.Username
		if pusher == "" {
			pusher = p.Pusher.Login
		}
		return Event{
			Type:    "repo.push",
			Source:  "gitea",
			Subject: p.Repository.FullName,
			Data:    gitPushData(stripRef(p.Ref), p.HeadCommit.ID, pusher, p.HeadCommit.Message),
		}, true

	case "pull_request":
		var p giteaPullRequest
		if err := json.Unmarshal(body, &p); err != nil || p.Repository.FullName == "" {
			return Event{}, false
		}
		user := p.PullRequest.User.Username
		if user == "" {
			user = p.PullRequest.User.Login
		}
		return Event{
			Type:    "repo.pull_request." + p.Action,
			Source:  "gitea",
			Subject: p.Repository.FullName,
			Data:    gitPushData(p.PullRequest.Head.Ref, p.PullRequest.Head.SHA, user, p.PullRequest.Title),
		}, true

	default:
		return Event{}, false
	}
}

// giteaEventType reads the event name from whichever header the provider sent.
func giteaEventType(r *http.Request) string {
	if e := r.Header.Get("X-Gitea-Event"); e != "" {
		return e
	}
	return r.Header.Get("X-Forgejo-Event")
}

func handleGiteaWebhook(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleGiteaWebhook")
	defer span.End()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !verifyGitSignature(body, r.Header) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	eventType := giteaEventType(r)
	if eventType == "ping" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if eventType == "" {
		http.Error(w, "missing X-Gitea-Event / X-Forgejo-Event header", http.StatusBadRequest)
		return
	}

	e, ok := normalizeGiteaEvent(eventType, body)
	if !ok {
		// An event we do not translate (release, issue, …): acknowledge and ignore.
		w.WriteHeader(http.StatusOK)
		return
	}
	e.Actor = tenantFromQuery(r)
	if !ingestAdapterEvent(ctx, w, e, nil) {
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": e.ID})
}
