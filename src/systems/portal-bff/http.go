package main

import (
	"context"
	"encoding/json"
	"net/http"
)

// writeJSON writes v as a JSON body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONError writes a {"error": msg} body with the given status, matching the
// error shape the Node BFF returned.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func recordCacheHit(ctx context.Context) {
	if stateCacheHits != nil {
		stateCacheHits.Add(ctx, 1)
	}
}

func recordCacheMiss(ctx context.Context) {
	if stateCacheMisses != nil {
		stateCacheMisses.Add(ctx, 1)
	}
}
