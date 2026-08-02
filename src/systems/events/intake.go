package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// eventTokenWindow is how far the emitter's timestamp may be from ours before the token is
// stale. 30s is the window every internal HMAC in the platform uses (see workflows'
// verifyHooksTrigger), and it is what stops a captured token from being replayed later.
const eventTokenWindow = 30 * time.Second

// signEvent produces the HMAC an emitter signs an event with. It binds every field that
// decides what the event does — identity, tenant (actor), subject and payload digest — plus
// the timestamp, so a captured token cannot be reused to attribute a different tenant or
// swap the payload a trigger reacts to. Emitters use the identical function via the SDK.
func signEvent(e Event, ts string) string {
	mac := hmac.New(sha256.New, []byte(eventsTriggerKey))
	fmt.Fprintf(mac, "event:%s:%s:%s:%s:%s:%s:%s:%s",
		e.ID, e.Type, e.Source, e.Subject, e.Actor.OrgID, e.Actor.UserID, dataDigest(e.Data), ts)
	return hex.EncodeToString(mac.Sum(nil))
}

// dataDigest binds the free-form payload into the MAC without depending on the exact bytes
// on the wire: both sides hash the canonical JSON encoding of the map, and encoding/json
// emits object keys in sorted order, so an emitter and events agree on the digest for the
// same data. A non-Go emitter must therefore hash compact, sorted-key JSON.
func dataDigest(d map[string]any) string {
	b, _ := json.Marshal(d) // a map decoded from JSON always re-marshals
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// verifyEventToken checks the emitter's token and that its timestamp is inside the freshness
// window. Both must hold: the MAC alone would otherwise replay forever.
func verifyEventToken(e Event, token, ts string) bool {
	if eventsTriggerKey == "" || token == "" {
		return false
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if skew := time.Since(time.Unix(secs, 0)); skew > eventTokenWindow || skew < -eventTokenWindow {
		return false
	}
	return hmac.Equal([]byte(token), []byte(signEvent(e, ts)))
}

// handleInternalEvent ingests an authenticated event from a trusted internal emitter, stores
// it in the log, and kicks off trigger evaluation. Storage is idempotent on the event id, so
// an at-least-once redelivery is safe.
func handleInternalEvent(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleInternalEvent")
	defer span.End()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !verifyEventToken(e, r.Header.Get("X-Events-Token"), r.Header.Get("X-Events-Timestamp")) {
		slog.WarnContext(ctx, "internal event: invalid token", "type", e.Type, "source", e.Source)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if msg := validateEvent(&e); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if err := addEvent(ctx, e); err != nil {
		slog.ErrorContext(ctx, "store event", "error", err, "id", e.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Evaluate + dispatch off the request path; the retry loop covers a crash before it runs.
	go evaluateAndDispatch(detach(ctx), e)

	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": e.ID})
}

// validateEvent fills defaults and reports the first problem (empty string = valid). type,
// source and subject are required; an event must belong to a tenant (org or user).
func validateEvent(e *Event) string {
	if e.Type == "" {
		return "type is required"
	}
	if e.Source == "" {
		return "source is required"
	}
	if e.Subject == "" {
		return "subject is required"
	}
	if e.Actor.OrgID == "" && e.Actor.UserID == "" {
		return "actor.org_id or actor.user_id is required"
	}
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.SpecVersion == "" {
		e.SpecVersion = SpecVersion
	}
	if e.OccurredAt == "" {
		e.OccurredAt = time.Now().UTC().Format(time.RFC3339)
	}
	return ""
}
