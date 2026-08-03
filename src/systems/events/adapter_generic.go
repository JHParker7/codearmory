package main

import (
	"encoding/json"
	"io"
	"net/http"

	"go.opentelemetry.io/otel"
)

// Generic webhook adapter — POST /hooks.
//
// The catch-all for systems with no dedicated adapter: CI servers, monitoring, anything that
// can POST JSON. The former hooks service exposed this as its primary intake and matched rules
// on the free-text (source, event) pair; here the same body becomes a proper envelope, so a
// trigger filters it with the same field expressions it uses for every other event.
//
// The mapping is deliberately literal:
//
//	{"source": "ci/myapp", "event": "build.failed", "payload": {"branch": "main"}}
//	  → type=ci.build.failed  source=ci/myapp  subject=ci/myapp  data.branch=main
//
// `type` is prefixed with "ci." — a namespace for events the platform did not originate — so a
// caller cannot mint a `repo.push` that looks like it came from a verified git adapter.

// genericPayload is the JSON body expected on POST /hooks. Payload values are strings, matching
// the former hooks contract; richer structure belongs on a typed adapter or /internal/events.
type genericPayload struct {
	Source  string            `json:"source"`
	Event   string            `json:"event"`
	Payload map[string]string `json:"payload"`
}

func handleGenericWebhook(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleGenericWebhook")
	defer span.End()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	// Same deployment-wide secret as the git adapters: this endpoint is public and a matched
	// trigger starts pipeline runs, so an unsigned payload is never accepted.
	if !verifyGitSignature(body, r.Header) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var p genericPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	// The X-Hook-Event header overrides the body's event, so a sender that cannot shape its
	// payload can still say what happened. Carried over from the hooks contract.
	if h := r.Header.Get("X-Hook-Event"); h != "" {
		p.Event = h
	}
	if p.Source == "" {
		http.Error(w, "source is required", http.StatusBadRequest)
		return
	}
	if p.Event == "" {
		http.Error(w, "event is required", http.StatusBadRequest)
		return
	}

	data := make(map[string]any, len(p.Payload))
	for k, v := range p.Payload {
		data[k] = v
	}
	e := Event{
		Type:    "ci." + p.Event,
		Source:  p.Source,
		Subject: p.Source,
		Actor:   tenantFromQuery(r),
		Data:    data,
	}
	if !ingestAdapterEvent(ctx, w, e, nil) {
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": e.ID})
}
