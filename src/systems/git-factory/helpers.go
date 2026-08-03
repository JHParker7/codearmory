package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// instanceID names THIS process, distinctly enough to tell two replicas of the same
// Deployment apart. Hostname alone is not enough — a restarted pod keeps its name — so
// the pid is included, which makes the id change whenever the process does.
//
// Used wherever something is written to SHARED storage or a shared table and has to be
// attributable to one replica: the startup probes (storagecheck.go) and the maintenance
// lease holder (lease.go). Both are cases where a fixed name means two replicas silently
// operating on each other's state.
func instanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// writeJSON writes v as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// isUniqueViolation reports whether err is a unique-constraint violation, across
// Postgres (pgx SQLSTATE 23505) and SQLite (used by the unit tests).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "23505") ||
		strings.Contains(msg, "duplicate key value") ||
		strings.Contains(msg, "UNIQUE constraint failed")
}
