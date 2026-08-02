package main

// Unit tests for the small cross-cutting helpers (helpers.go): JSON response
// writing and the cross-driver unique-violation classifier that maps a duplicate
// name to 409 (ARCHITECTURE §2a create contract). These are pure/deterministic
// and pass today.

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, 201, map[string]any{"name": "alpha", "n": 2})

	if rec.Code != 201 {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if got["name"] != "alpha" {
		t.Errorf("round-trip name = %v, want alpha", got["name"])
	}
}

func TestIsUniqueViolation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"postgres sqlstate", errString("ERROR: duplicate key value violates unique constraint (SQLSTATE 23505)"), true},
		{"postgres text", errString("duplicate key value violates unique constraint"), true},
		{"sqlite", errString("UNIQUE constraint failed: repos.namespace, repos.name"), true},
		{"unrelated", errString("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUniqueViolation(tc.err); got != tc.want {
				t.Errorf("isUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// errString is a tiny error whose message is the string itself.
type errString string

func (e errString) Error() string { return string(e) }
