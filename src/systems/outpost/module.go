package main

import "context"

// Command is a control→outpost instruction. The envelope is transport-agnostic
// so a WebSocket carrier could replace long-poll without touching modules.
type Command struct {
	ID          string         `json:"id"`
	Integration string         `json:"integration"`
	Type        string         `json:"type"`
	Payload     map[string]any `json:"payload"`
}

// Event is an outpost→control message a module emits (command result or a watch
// update). EventID makes delivery idempotent end-to-end.
type Event struct {
	EventID     string         `json:"event_id"`
	Integration string         `json:"integration"`
	Type        string         `json:"type"`
	Payload     map[string]any `json:"payload"`
}

// Module is one pluggable integration. The outpost core knows nothing about
// Kubernetes, Litmus, or Argo — all of that lives behind this interface. Adding
// an integration means adding a Module implementation and a consumer service;
// the core and gateway are untouched.
type Module interface {
	// Name is the integration key (e.g. "chaos"); it must match the command
	// Integration field and the consumer service routing.
	Name() string

	// HandleCommand performs an imperative action and returns any immediate
	// events (e.g. an acknowledgement). Long-lived results are surfaced via
	// Start's emit callback instead.
	HandleCommand(ctx context.Context, c Command) ([]Event, error)

	// Start begins any watches/streams the module needs, emitting events as
	// cluster state changes. It returns once the informers are running; it must
	// not block. Cancellation is via ctx.
	Start(ctx context.Context, emit func(Event)) error
}

// payloadString reads a string field from a command/event payload defensively.
func payloadString(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	if v, ok := p[key].(string); ok {
		return v
	}
	return ""
}

// payloadBool reads a bool field defensively, treating a missing or non-bool value
// as false.
func payloadBool(p map[string]any, key string) bool {
	if p == nil {
		return false
	}
	v, _ := p[key].(bool)
	return v
}

// payloadInt reads an int field defensively (JSON decodes numbers as float64),
// falling back to def when absent or malformed.
func payloadInt(p map[string]any, key string, def int) int {
	if p == nil {
		return def
	}
	switch v := p[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return def
}

// payloadStringSlice reads a []string field defensively (JSON decodes arrays as
// []any), skipping non-string elements.
func payloadStringSlice(p map[string]any, key string) []string {
	if p == nil {
		return nil
	}
	raw, ok := p[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// payloadStringMap reads a map[string]string field (JSON-decoded as
// map[string]any) defensively.
func payloadStringMap(p map[string]any, key string) map[string]string {
	out := map[string]string{}
	if p == nil {
		return out
	}
	raw, ok := p[key].(map[string]any)
	if !ok {
		return out
	}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
