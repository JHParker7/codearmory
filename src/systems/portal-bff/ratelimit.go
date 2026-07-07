package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ipRateLimiter throttles per client IP, mirroring the Node BFF's
// 100-requests-per-15-minutes guard on the SPA index fallback. Static assets are
// not limited. The token-bucket refill approximates the fixed window: a burst of
// `perWindow` requests is allowed, refilling at perWindow/window.
type ipRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	lastSeen map[string]time.Time
	limit    rate.Limit
	burst    int
}

func newIPRateLimiter(perWindow int, window time.Duration) *ipRateLimiter {
	return &ipRateLimiter{
		limiters: make(map[string]*rate.Limiter),
		lastSeen: make(map[string]time.Time),
		limit:    rate.Limit(float64(perWindow) / window.Seconds()),
		burst:    perWindow,
	}
}

// allow reports whether a request from ip may proceed, consuming one token.
func (l *ipRateLimiter) allow(ip string) bool {
	l.mu.Lock()
	lim, ok := l.limiters[ip]
	if !ok {
		lim = rate.NewLimiter(l.limit, l.burst)
		l.limiters[ip] = lim
	}
	l.lastSeen[ip] = time.Now()
	l.mu.Unlock()
	return lim.Allow()
}

// cleanup drops limiter state for IPs unseen for longer than ttl, so the maps do
// not grow without bound. Run periodically.
func (l *ipRateLimiter) cleanup(ttl time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for ip, seen := range l.lastSeen {
		if now.Sub(seen) > ttl {
			delete(l.limiters, ip)
			delete(l.lastSeen, ip)
		}
	}
}

// clientIP returns the best-effort client IP for rate-limit bucketing. When
// trustProxy is not "false"/"0" the leftmost X-Forwarded-For entry (the real
// client behind the ingress) is used; otherwise the TCP peer. Mirrors Express's
// `trust proxy` so one user's traffic can't collapse everyone into one bucket.
func clientIP(r *http.Request) string {
	if trustProxy != "false" && trustProxy != "0" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
