package main

import (
	"testing"
	"time"
)

func TestCache_SetGet(t *testing.T) {
	c := newStateCache(time.Minute)
	c.set("ws/a", "tokenA", "value-a")
	if v, ok := c.get("ws/a", "tokenA"); !ok || v != "value-a" {
		t.Fatalf("expected cached value, got %v ok=%v", v, ok)
	}
}

func TestCache_PerTokenIsolation(t *testing.T) {
	c := newStateCache(time.Minute)
	c.set("ws/a", "tokenA", "value-a")
	if _, ok := c.get("ws/a", "tokenB"); ok {
		t.Fatal("one token's cached state must not be served to another")
	}
}

func TestCache_Expiry(t *testing.T) {
	c := newStateCache(time.Millisecond)
	c.set("ws/a", "t", "v")
	time.Sleep(5 * time.Millisecond)
	if _, ok := c.get("ws/a", "t"); ok {
		t.Fatal("expected entry to expire")
	}
}

func TestCache_InvalidateAllTokens(t *testing.T) {
	c := newStateCache(time.Minute)
	c.set("ws/a", "tokenA", "va")
	c.set("ws/a", "tokenB", "vb")
	c.invalidate("ws/a")
	if _, ok := c.get("ws/a", "tokenA"); ok {
		t.Fatal("invalidate must drop every token for the path")
	}
	if _, ok := c.get("ws/a", "tokenB"); ok {
		t.Fatal("invalidate must drop every token for the path")
	}
}
