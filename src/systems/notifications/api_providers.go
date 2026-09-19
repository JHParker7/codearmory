package main

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func handleListProviders(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listProvider", "notifications/providers"); !ok {
		return
	}
	writeJSON(w, http.StatusOK, providers())
}
