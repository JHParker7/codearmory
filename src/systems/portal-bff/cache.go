package main

import (
	"sync"
	"time"
)

type cacheEntry struct {
	value     any
	expiresAt time.Time
}

// stateCache is an in-memory TTL cache of normalized workspace-state views,
// keyed by (workspace path, Authorization token) so one user's cached state is
// never served to another. Backs GET /state/*; any write invalidates the path.
type stateCache struct {
	mu        sync.Mutex
	ttl       time.Duration
	sweepEach time.Duration
	lastSweep time.Time
	data      map[string]map[string]cacheEntry
}

func newStateCache(ttl time.Duration) *stateCache {
	// Sweep no more often than the TTL, and never more than every 30s, so a tiny
	// TTL doesn't make every write do a full scan.
	sweep := ttl
	if sweep < 30*time.Second {
		sweep = 30 * time.Second
	}
	return &stateCache{
		ttl:       ttl,
		sweepEach: sweep,
		lastSweep: time.Now(),
		data:      make(map[string]map[string]cacheEntry),
	}
}

// get returns the cached view for (wsPath, auth), or (nil,false) if absent or
// expired. Expired entries are evicted on read.
func (c *stateCache) get(wsPath, auth string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	byAuth, ok := c.data[wsPath]
	if !ok {
		return nil, false
	}
	entry, ok := byAuth[auth]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(byAuth, auth)
		if len(byAuth) == 0 {
			delete(c.data, wsPath)
		}
		return nil, false
	}
	return entry.value, true
}

// set stores a view for (wsPath, auth) with a fresh TTL, opportunistically
// sweeping expired entries first.
func (c *stateCache) set(wsPath, auth string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maybeSweepLocked()
	byAuth := c.data[wsPath]
	if byAuth == nil {
		byAuth = make(map[string]cacheEntry)
		c.data[wsPath] = byAuth
	}
	byAuth[auth] = cacheEntry{value: value, expiresAt: time.Now().Add(c.ttl)}
}

// invalidate drops all cached views for a workspace path (every token). Called
// after any write to that path.
func (c *stateCache) invalidate(wsPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, wsPath)
}

// maybeSweepLocked drops expired entries (and emptied inner maps) at most once
// per sweepEach. Without it, entries keyed by a now-rotated Authorization token
// are only evicted when read again with that exact token — which never happens
// for an old token — so the maps would grow without bound. Caller holds c.mu.
func (c *stateCache) maybeSweepLocked() {
	now := time.Now()
	if now.Sub(c.lastSweep) < c.sweepEach {
		return
	}
	c.lastSweep = now
	for wsPath, byAuth := range c.data {
		for auth, entry := range byAuth {
			if now.After(entry.expiresAt) {
				delete(byAuth, auth)
			}
		}
		if len(byAuth) == 0 {
			delete(c.data, wsPath)
		}
	}
}
