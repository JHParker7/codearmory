package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
)

// gitWebhook is a provider-neutral push payload. Provider-specific adapters (Gitea, GitHub)
// normalize into this shape; this handler verifies the provider HMAC over the raw body, then
// turns the payload into a repo.push envelope and dispatches it.
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

// verifyGitSignature checks the provider HMAC over the raw request body against
// EVENTS_WEBHOOK_SECRET. Two header shapes are accepted: GitHub's (and Forgejo's)
// `X-Hub-Signature-256: sha256=<hex>` and Gitea/Forgejo's `X-Gitea-Signature: <hex>`. Both
// are HMAC-SHA256 over the exact bytes received, compared in constant time.
//
// With no secret configured this returns false, so /hooks/git rejects everything rather than
// accepting unsigned payloads: the endpoint is public and a dispatched trigger starts
// pipeline runs.
func verifyGitSignature(body []byte, h http.Header) bool {
	if eventsWebhookSecret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(eventsWebhookSecret))
	mac.Write(body)
	want := []byte(hex.EncodeToString(mac.Sum(nil)))
	for _, got := range []string{
		strings.TrimPrefix(h.Get("X-Hub-Signature-256"), "sha256="),
		h.Get("X-Gitea-Signature"),
	} {
		if got != "" && hmac.Equal([]byte(got), want) {
			return true
		}
	}
	return false
}

func handleGitWebhook(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleGitWebhook")
	defer span.End()

	// Read the raw body first — the MAC covers the exact bytes sent, so it has to be computed
	// before any decoding.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !verifyGitSignature(body, r.Header) {
		slog.WarnContext(ctx, "git webhook: invalid or missing provider signature")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var p gitWebhook
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if p.Repo == "" || p.Event == "" {
		http.Error(w, "repo and event are required", http.StatusBadRequest)
		return
	}
	// The tenant still comes from the body, but only because the MAC covers the whole body:
	// org_id/user_id are attested by whoever holds the webhook secret, never by an anonymous
	// caller. The secret is deployment-wide, so it proves "the operator configured this
	// webhook", not "this repo belongs to that tenant" — and events has no repo→tenant
	// binding to derive it from (git_connector's /internal/repos/sync-config is keyed by
	// clone URL and only answers for repos with GitOps sync enabled). Per-repo webhook
	// secrets are what would make the attestation tenant-specific; until then a signed
	// payload is trusted for the tenant it names.
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
