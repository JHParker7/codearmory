package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestStartKeyRotation_DiscardsBootstrapAfterFirstSuccess verifies the bootstrap
// key is used only for the spin-up rotation; once a rotated key exists, every
// later rotation presents the rotated key, never the bootstrap key.
func TestStartKeyRotation_DiscardsBootstrapAfterFirstSuccess(t *testing.T) {
	var mu sync.Mutex
	var presented []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Service-Key")
		mu.Lock()
		presented = append(presented, key)
		n := len(presented)
		mu.Unlock()
		if n == 1 && key != "svc:bootstrap-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Hand out a fresh rotated key each time so rotation keeps progressing.
		_ = json.NewEncoder(w).Encode(map[string]string{"key": "rot" + strconv.Itoa(n)})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartKeyRotation(ctx, srv.URL, "svc", "bootstrap-key", 20*time.Millisecond)

	if !waitFor(3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(presented) >= 3
	}) {
		t.Fatal("timed out waiting for rotations")
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if presented[0] != "svc:bootstrap-key" {
		t.Fatalf("spin-up should present the bootstrap key, got %q", presented[0])
	}
	for i, k := range presented[1:] {
		if k == "svc:bootstrap-key" {
			t.Fatalf("bootstrap key reused after spin-up (attempt %d): %v", i+1, presented)
		}
	}
}

// TestStartKeyRotation_NoBootstrapFallbackAfterSpinUp verifies that once the
// bootstrap key is discarded, a later 401 does NOT fall back to it: the rejected
// rotated key is kept (the process must restart to recover), and the bootstrap key
// is presented exactly once.
func TestStartKeyRotation_NoBootstrapFallbackAfterSpinUp(t *testing.T) {
	var mu sync.Mutex
	var presented []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Service-Key")
		mu.Lock()
		presented = append(presented, key)
		n := len(presented)
		mu.Unlock()
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]string{"key": "rot1"})
			return
		}
		// After spin-up, reject everything to simulate a desync.
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	getKey := StartKeyRotation(ctx, srv.URL, "svc", "bootstrap-key", 20*time.Millisecond)

	if !waitFor(3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(presented) >= 4
	}) {
		t.Fatal("timed out waiting for rotation retries")
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	bootCount := 0
	for _, k := range presented {
		if k == "svc:bootstrap-key" {
			bootCount++
		}
	}
	if bootCount != 1 {
		t.Fatalf("bootstrap key should be presented exactly once (spin-up), got %d: %v", bootCount, presented)
	}
	if got := getKey(); got != "rot1" {
		t.Fatalf("expected current key to stay %q after spin-up, got %q", "rot1", got)
	}
}
