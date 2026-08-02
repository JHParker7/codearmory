package main

import (
	"context"
	"sort"
	"testing"
	"time"
)

// fakeBackend records ensure/remove calls and reports a fixed managed set.
type fakeBackend struct {
	managed   []string
	ensured   []string
	removed   []string
	ensureErr error
	// rollouts is the state RolloutState reports per service; absent => unmanaged.
	rollouts   map[string]rolloutState
	rolloutErr error
	// checked records every service RolloutState was asked about, in order.
	checked []string
}

func (f *fakeBackend) EnsureService(_ context.Context, spec workloadSpec) error {
	f.ensured = append(f.ensured, spec.Service)
	return f.ensureErr
}
func (f *fakeBackend) RemoveService(_ context.Context, service string) error {
	f.removed = append(f.removed, service)
	return nil
}
func (f *fakeBackend) ListManaged(context.Context) ([]string, error) { return f.managed, nil }

func (f *fakeBackend) RolloutState(_ context.Context, service string, _ time.Duration) (rolloutState, error) {
	f.checked = append(f.checked, service)
	if f.rolloutErr != nil {
		return rolloutState{}, f.rolloutErr
	}
	return f.rollouts[service], nil
}

// newTestReconciler builds a reconciler whose status writes land in a map instead of
// the database, with rollout observation on.
func newTestReconciler(fb *fakeBackend) (*reconciler, map[string][]rolloutState) {
	written := map[string][]rolloutState{}
	rec := newReconciler(fb, time.Minute)
	rec.rolloutDeadline = time.Minute
	rec.writeStatus = func(_ context.Context, service string, st rolloutState) error {
		written[service] = append(written[service], st)
		return nil
	}
	return rec, written
}

func TestApplyDesired_EnsuresAndTearsDown(t *testing.T) {
	// forge+workflows desired; cluster currently runs forge+tickets.
	fb := &fakeBackend{managed: []string{"forge", "tickets"}}
	rec, _ := newTestReconciler(fb)
	desired := map[string]workloadSpec{
		"forge":     {Service: "forge"},
		"workflows": {Service: "workflows"},
	}
	if err := rec.applyDesired(context.Background(), desired); err != nil {
		t.Fatalf("applyDesired: %v", err)
	}
	sort.Strings(fb.ensured)
	if got := fb.ensured; len(got) != 2 || got[0] != "forge" || got[1] != "workflows" {
		t.Errorf("ensured = %v, want [forge workflows]", got)
	}
	// tickets is managed but no longer desired → torn down; forge stays.
	if got := fb.removed; len(got) != 1 || got[0] != "tickets" {
		t.Errorf("removed = %v, want [tickets]", got)
	}
}

func TestApplyDesired_NoDesiredTearsDownAll(t *testing.T) {
	fb := &fakeBackend{managed: []string{"forge", "workflows"}}
	rec, _ := newTestReconciler(fb)
	if err := rec.applyDesired(context.Background(), map[string]workloadSpec{}); err != nil {
		t.Fatalf("applyDesired: %v", err)
	}
	sort.Strings(fb.removed)
	if got := fb.removed; len(got) != 2 || got[0] != "forge" || got[1] != "workflows" {
		t.Errorf("removed = %v, want [forge workflows]", got)
	}
	if len(fb.ensured) != 0 {
		t.Errorf("ensured = %v, want none", fb.ensured)
	}
}

func TestSpecFromRow(t *testing.T) {
	row := OrgService{
		ServiceName: "mysvc",
		Image:       "repo/mysvc:1",
		Port:        9000,
		Config:      map[string]any{"ALLOWED_IMAGES": "alpine:3.19"},
	}
	spec := specFromRow(row)
	if spec.Service != "mysvc" || spec.Image != "repo/mysvc:1" || spec.Port != 9000 {
		t.Fatalf("unexpected spec %+v", spec)
	}
	if spec.Env["ALLOWED_IMAGES"] != "alpine:3.19" {
		t.Errorf("env = %v, want ALLOWED_IMAGES set", spec.Env)
	}
}

func TestConfigToEnv(t *testing.T) {
	env := configToEnv(map[string]any{
		"STR":  "x",
		"NUM":  float64(600),
		"FRAC": float64(1.5),
		"FLAG": true,
		"LIST": []any{"a", "b"},
		"NULL": nil,
	})
	if env["STR"] != "x" {
		t.Errorf("STR = %q", env["STR"])
	}
	if env["NUM"] != "600" {
		t.Errorf("NUM = %q, want 600 (no decimal)", env["NUM"])
	}
	if env["FRAC"] != "1.5" {
		t.Errorf("FRAC = %q", env["FRAC"])
	}
	if env["FLAG"] != "true" {
		t.Errorf("FLAG = %q", env["FLAG"])
	}
	if env["LIST"] != `["a","b"]` {
		t.Errorf("LIST = %q, want JSON array", env["LIST"])
	}
	if _, ok := env["NULL"]; ok {
		t.Errorf("NULL should be skipped, got %q", env["NULL"])
	}
}

func TestConfigToEnv_Empty(t *testing.T) {
	if got := configToEnv(nil); got != nil {
		t.Errorf("nil config should give nil env, got %v", got)
	}
}

// noopBackend keeps reconciliation inert when disabled; sanity-check it does
// nothing and reports no managed services.
func TestNoopBackend(t *testing.T) {
	var b clusterBackend = noopBackend{}
	if err := b.EnsureService(context.Background(), workloadSpec{Service: "x"}); err != nil {
		t.Errorf("EnsureService: %v", err)
	}
	if err := b.RemoveService(context.Background(), "x"); err != nil {
		t.Errorf("RemoveService: %v", err)
	}
	if m, err := b.ListManaged(context.Background()); err != nil || m != nil {
		t.Errorf("ListManaged = (%v,%v), want (nil,nil)", m, err)
	}
	if st, err := b.RolloutState(context.Background(), "x", time.Minute); err != nil || st.Status != rolloutUnmanaged {
		t.Errorf("RolloutState = (%v,%v), want unmanaged", st, err)
	}
}
