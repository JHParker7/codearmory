// Package events is the platform's event collector and reactor: services emit JSON
// events here, and triggers whose field-based filters match dispatch actions (run a
// pipeline, open a ticket, notify, …). It folds in the former `hooks` service — the
// git-webhook adapters and pipeline dispatch move here, the git-specific rule model is
// replaced by the general envelope + filter defined in this file. See docs/events/design.md.
package main

import (
	"encoding/json"
	"time"
)

// SpecVersion is the current event-envelope version. Consumers may switch on it; new
// optional fields are backward compatible, so this only bumps on a breaking change.
const SpecVersion = "1"

// Event is the JSON envelope every emitter sends to POST /internal/events. `Subject` is
// the specific resource the event is about (a repo, a run, a ticket id) and is what makes
// triggers addressable per-resource — the thing the old shared `source` constant could not
// express. `Data` is free-form, type-specific payload that filters reach into by path.
type Event struct {
	ID          string         `json:"id"`                     // ULID, unique — the idempotency key
	SpecVersion string         `json:"spec_version,omitempty"` // defaults to SpecVersion on intake
	Type        string         `json:"type"`                   // dotted, namespaced, stable: repo.push, pipeline.failed, …
	Source      string         `json:"source"`                 // emitting service
	Subject     string         `json:"subject"`                // the resource the event is about
	Actor       Actor          `json:"actor"`                  // tenant scope
	OccurredAt  string         `json:"occurred_at"`            // RFC3339
	TraceID     string         `json:"trace_id,omitempty"`     // W3C traceparent for correlation
	CausationID string         `json:"causation_id,omitempty"` // the event/action that caused this one (loop guard)
	Data        map[string]any `json:"data,omitempty"`         // type-specific payload
}

// Actor is the tenant an event belongs to. Either field may be empty (a user event with no
// org, a service event with neither), but a trigger only ever sees events of its own tenant.
type Actor struct {
	OrgID  string `json:"org_id,omitempty"`
	UserID string `json:"user_id,omitempty"`
}

// asMap renders the envelope as the decoded-JSON map the filter engine walks. Filters use
// dotted paths over this shape (type, source, subject, actor.org_id, data.ref, …).
func (e Event) asMap() (map[string]any, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Match is a filter node: either a GROUP (any of All/Any/Not set) or a LEAF condition
// (Field set). A group ANDs All, ORs Any, and negates Not; a leaf applies Op to the value
// at Field. The recursion lets a trigger express arbitrary boolean logic while the common
// case stays a flat `all: [ {field,op,value}, … ]`.
type Match struct {
	All []Match `json:"all,omitempty"`
	Any []Match `json:"any,omitempty"`
	Not *Match  `json:"not,omitempty"`

	// Leaf condition (used when All/Any/Not are all empty).
	Field  string `json:"field,omitempty"`
	Op     string `json:"op,omitempty"`
	Value  any    `json:"value,omitempty"`  // scalar operand (eq/ne/prefix/glob/regex/gt…)
	Values []any  `json:"values,omitempty"` // set operand (in/not_in)
}

// isGroup reports whether this node combines sub-matches rather than testing a field.
func (m Match) isGroup() bool { return len(m.All) > 0 || len(m.Any) > 0 || m.Not != nil }

// Trigger is a tenant-scoped subscription: when Match evaluates true against an event, its
// Actions dispatch. Stored/served by the CRUD + dispatch layers (added next); defined here
// so the filter engine and its tests are self-contained.
type Trigger struct {
	ID        string    `json:"id"                   gorm:"primaryKey"`
	OrgID     string    `json:"org_id,omitempty"     gorm:"index"`
	CreatedBy string    `json:"created_by,omitempty" gorm:"index"`
	Name      string    `json:"name"`
	Match     Match     `json:"match"                gorm:"serializer:json"`
	Actions   []Action  `json:"actions"              gorm:"serializer:json"`
	Enabled   bool      `json:"enabled"              gorm:"default:true"`
	CreatedAt time.Time `json:"created_at"           gorm:"autoCreateTime"`
	UpdatedAt time.Time `json:"updated_at"           gorm:"autoUpdateTime"`
}

// Action is one thing a matched trigger does. Kind selects the executor; Config carries its
// parameters (e.g. pipeline_id + templated inputs for run_pipeline). Kept as free-form JSON
// so new action kinds don't churn the schema.
type Action struct {
	Kind   string         `json:"kind"`   // run_pipeline | create_ticket | notify | webhook_out | enqueue_outpost_command
	Config map[string]any `json:"config"` // kind-specific
}
