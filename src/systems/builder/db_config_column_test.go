package main

import (
	"encoding/json"
	"testing"
)

// The config column is written two ways: GORM's serializer:json tag on Create, and
// the hand-built Updates map on update. Only the first is served by the tag, so the
// second has to encode it itself — these pin that encoding to what the serializer
// produces, since a mismatch is invisible until a read comes back wrong.
func TestConfigColumnValue(t *testing.T) {
	t.Run("nil map stores NULL", func(t *testing.T) {
		got, err := configColumnValue(nil)
		if err != nil {
			t.Fatalf("configColumnValue(nil) error: %v", err)
		}
		if got != nil {
			t.Fatalf("configColumnValue(nil) = %#v, want nil (SQL NULL)", got)
		}
	})

	t.Run("empty map stores an empty object", func(t *testing.T) {
		got, err := configColumnValue(map[string]any{})
		if err != nil {
			t.Fatalf("configColumnValue(empty) error: %v", err)
		}
		if got != "{}" {
			t.Fatalf("configColumnValue(empty) = %#v, want \"{}\"", got)
		}
	})

	t.Run("populated map stores JSON text the driver can bind", func(t *testing.T) {
		got, err := configColumnValue(map[string]any{"GIT_HTTP_BASE_URL": "https://git.example.com"})
		if err != nil {
			t.Fatalf("configColumnValue error: %v", err)
		}
		// A string (not a map) is the whole point: a map[string]any bound to a text
		// column is what made every config update fail.
		text, ok := got.(string)
		if !ok {
			t.Fatalf("configColumnValue returned %T, want string", got)
		}
		var round map[string]any
		if err := json.Unmarshal([]byte(text), &round); err != nil {
			t.Fatalf("stored value is not JSON: %v (%q)", err, text)
		}
		if round["GIT_HTTP_BASE_URL"] != "https://git.example.com" {
			t.Fatalf("round-tripped config = %#v", round)
		}
	})
}
