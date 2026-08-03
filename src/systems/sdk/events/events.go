// Package events emits platform events to the events service.
//
// Every service that reports something worth reacting to — a push landed, a ticket changed
// state, a run finished — POSTs an envelope to the events service's /internal/events, and
// triggers whose filters match dispatch actions. This package owns the envelope shape and the
// HMAC that authenticates it, so no emitter reimplements either; getting the signature
// marginally wrong produces a 401 with nothing to point at.
//
// Emission is best-effort by design. The thing that produced the event has already succeeded
// (the push is on disk, the ticket is saved); failing it afterwards because a downstream
// listener was unreachable would be a lie to the caller. Emit therefore returns an error for
// logging, and callers drop it.
package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SpecVersion is the envelope version this SDK emits. It must match the events service's
// constant of the same name.
const SpecVersion = "1"

// Actor is the tenant an event belongs to. At least one field must be set — an event with
// neither belongs to no one and can never match a trigger.
type Actor struct {
	OrgID  string `json:"org_id,omitempty"`
	UserID string `json:"user_id,omitempty"`
}

// Event is the JSON envelope. Subject is the specific resource the event is about (a repo, a
// run, a ticket id) and is what makes triggers addressable per-resource. Data is free-form,
// type-specific payload that filters reach into by dotted path.
type Event struct {
	ID          string         `json:"id"`
	SpecVersion string         `json:"spec_version,omitempty"`
	Type        string         `json:"type"`
	Source      string         `json:"source"`
	Subject     string         `json:"subject"`
	Actor       Actor          `json:"actor"`
	OccurredAt  string         `json:"occurred_at"`
	TraceID     string         `json:"trace_id,omitempty"`
	CausationID string         `json:"causation_id,omitempty"`
	Data        map[string]any `json:"data,omitempty"`
}

// Emitter posts signed events to the events service. The zero value is unusable; build one
// with New. An Emitter with an empty URL or Key is disabled and Emit is a no-op, which is the
// correct behaviour for a deployment that does not run events.
type Emitter struct {
	URL        string // events service base URL
	Key        string // shared HMAC key (EVENTS_TRIGGER_KEY)
	Source     string // the emitting service name, stamped on every event
	HTTPClient *http.Client
}

// New builds an Emitter. A nil client gets a 5-second-timeout default: emission is detached
// from whatever produced the event, but it must not accumulate goroutines forever.
func New(url, key, source string, client *http.Client) *Emitter {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &Emitter{URL: strings.TrimRight(url, "/"), Key: key, Source: source, HTTPClient: client}
}

// Enabled reports whether this emitter is configured to send anything.
func (e *Emitter) Enabled() bool { return e != nil && e.URL != "" && e.Key != "" }

// dataDigest binds the free-form payload into the MAC without depending on the exact bytes on
// the wire: both sides hash the canonical JSON encoding of the map, and encoding/json emits
// object keys in sorted order, so emitter and receiver agree on the digest for the same data.
func dataDigest(d map[string]any) string {
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Sign produces the token for an event at a given timestamp. It binds every field that decides
// what the event does — identity, tenant, subject and payload digest — plus the timestamp, so
// a captured token cannot be reused to attribute a different tenant or swap the payload a
// trigger reacts to.
func Sign(key string, ev Event, ts string) string {
	mac := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(mac, "event:%s:%s:%s:%s:%s:%s:%s:%s",
		ev.ID, ev.Type, ev.Source, ev.Subject, ev.Actor.OrgID, ev.Actor.UserID, dataDigest(ev.Data), ts)
	return hex.EncodeToString(mac.Sum(nil))
}

// Emit sends one event. Missing id, spec_version, source and occurred_at are filled in.
// Returns nil immediately when the emitter is disabled.
func (e *Emitter) Emit(ctx context.Context, ev Event) error {
	if !e.Enabled() {
		return nil
	}
	if ev.ID == "" {
		ev.ID = uuid.NewString()
	}
	if ev.SpecVersion == "" {
		ev.SpecVersion = SpecVersion
	}
	if ev.Source == "" {
		ev.Source = e.Source
	}
	if ev.OccurredAt == "" {
		ev.OccurredAt = time.Now().UTC().Format(time.RFC3339)
	}
	if ev.Actor.OrgID == "" && ev.Actor.UserID == "" {
		return fmt.Errorf("events: actor.org_id or actor.user_id is required")
	}

	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("events: marshal envelope: %w", err)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL+"/internal/events", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("events: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Events-Token", Sign(e.Key, ev, ts))
	req.Header.Set("X-Events-Timestamp", ts)

	resp, err := e.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("events: dispatch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("events: rejected with %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
