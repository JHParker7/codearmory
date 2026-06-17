package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// fakeRuntime is a Runtime test double: it records the last execution it Ran and
// the last id it was asked to Cancel, so tests can assert wiring without a live
// docker/k8s/proxmox backend.
type fakeRuntime struct {
	result    RunResult
	runErr    error
	ran       string
	cancelled string
}

func (f *fakeRuntime) Run(_ context.Context, exec Execution) (RunResult, error) {
	f.ran = exec.ExecutionID
	return f.result, f.runErr
}

func (f *fakeRuntime) Cancel(_ context.Context, executionID string) error {
	f.cancelled = executionID
	return nil
}

// insertRuntimeBackend adds a backend row and registers cleanup.
func insertRuntimeBackend(t *testing.T, name, typ string, enabled bool) {
	t.Helper()
	b := RuntimeBackend{Name: name, Type: typ, Enabled: enabled, Config: map[string]string{}, SecretRefs: map[string]string{}}
	if err := b.Add(context.Background()); err != nil {
		t.Fatalf("insertRuntimeBackend: %v", err)
	}
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM runtime_backends WHERE name = ?`, name) //nolint:errcheck
	})
}

func TestRuntimeRegistry_BuildsAndCaches(t *testing.T) {
	requireForgeDB(t)
	name := "be-" + uuid.New().String()
	insertRuntimeBackend(t, name, "docker", true)

	builds := 0
	reg := newRuntimeRegistry()
	reg.build = func(RuntimeBackend) (Runtime, error) {
		builds++
		return &fakeRuntime{}, nil
	}

	rt1, err := reg.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	rt2, err := reg.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if builds != 1 {
		t.Fatalf("build called %d times, want 1 (second Get should hit cache)", builds)
	}
	if rt1 != rt2 {
		t.Fatal("expected the cached runtime to be returned on the second Get")
	}
}

func TestRuntimeRegistry_EvictRebuilds(t *testing.T) {
	requireForgeDB(t)
	name := "be-" + uuid.New().String()
	insertRuntimeBackend(t, name, "docker", true)

	builds := 0
	reg := newRuntimeRegistry()
	reg.build = func(RuntimeBackend) (Runtime, error) {
		builds++
		return &fakeRuntime{}, nil
	}

	if _, err := reg.Get(context.Background(), name); err != nil {
		t.Fatalf("Get: %v", err)
	}
	reg.Evict(name)
	if _, err := reg.Get(context.Background(), name); err != nil {
		t.Fatalf("Get after evict: %v", err)
	}
	if builds != 2 {
		t.Fatalf("build called %d times, want 2 (evict should force a rebuild)", builds)
	}
}

func TestRuntimeRegistry_MissingBackend(t *testing.T) {
	requireForgeDB(t)
	reg := newRuntimeRegistry()
	reg.build = func(RuntimeBackend) (Runtime, error) { return &fakeRuntime{}, nil }
	if _, err := reg.Get(context.Background(), "no-such-backend-"+uuid.New().String()); err == nil {
		t.Fatal("expected an error for a missing backend")
	}
}

func TestRuntimeRegistry_DisabledBackend(t *testing.T) {
	requireForgeDB(t)
	name := "be-" + uuid.New().String()
	insertRuntimeBackend(t, name, "docker", false) // disabled

	reg := newRuntimeRegistry()
	reg.build = func(RuntimeBackend) (Runtime, error) { return &fakeRuntime{}, nil }
	if _, err := reg.Get(context.Background(), name); err == nil {
		t.Fatal("expected an error for a disabled backend")
	}
}

func TestRuntimeRegistry_BuildError(t *testing.T) {
	requireForgeDB(t)
	name := "be-" + uuid.New().String()
	insertRuntimeBackend(t, name, "docker", true)

	reg := newRuntimeRegistry()
	reg.build = func(RuntimeBackend) (Runtime, error) { return nil, fmt.Errorf("boom") }
	if _, err := reg.Get(context.Background(), name); err == nil {
		t.Fatal("expected the build error to propagate")
	}
}
