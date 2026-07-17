package main

import (
	"context"
	"fmt"
	"testing"
)

// TestInitConcurrencyConfig_ResourceBudgets checks the cluster-wide resource budgets
// parse from the environment, clamp negatives to 0 (unlimited), and default to 0 when
// unset so an unconfigured operator keeps the original count-only admission.
func TestInitConcurrencyConfig_ResourceBudgets(t *testing.T) {
	t.Run("parsed from env", func(t *testing.T) {
		t.Setenv("FORGE_MAX_TOTAL_CPU_MILLICORES", "8000")
		t.Setenv("FORGE_MAX_TOTAL_MEMORY_MB", "16384")
		initConcurrencyConfig()
		if got := maxTotalCPUMillicores.Load(); got != 8000 {
			t.Errorf("maxTotalCPUMillicores = %d, want 8000", got)
		}
		if got := maxTotalMemoryMB.Load(); got != 16384 {
			t.Errorf("maxTotalMemoryMB = %d, want 16384", got)
		}
	})

	t.Run("negative clamps to unlimited", func(t *testing.T) {
		t.Setenv("FORGE_MAX_TOTAL_CPU_MILLICORES", "-1")
		t.Setenv("FORGE_MAX_TOTAL_MEMORY_MB", "-100")
		initConcurrencyConfig()
		if maxTotalCPUMillicores.Load() != 0 || maxTotalMemoryMB.Load() != 0 {
			t.Errorf("negative budgets should clamp to 0, got cpu=%d mem=%d", maxTotalCPUMillicores.Load(), maxTotalMemoryMB.Load())
		}
	})

	t.Run("unset defaults to unlimited", func(t *testing.T) {
		initConcurrencyConfig()
		if maxTotalCPUMillicores.Load() != 0 || maxTotalMemoryMB.Load() != 0 {
			t.Errorf("unset budgets should be 0, got cpu=%d mem=%d", maxTotalCPUMillicores.Load(), maxTotalMemoryMB.Load())
		}
	})
}

func TestClampPercent(t *testing.T) {
	for in, want := range map[int]int{-5: 0, 0: 0, 1: 1, 60: 60, 100: 100, 150: 100} {
		if got := clampPercent(in); got != want {
			t.Errorf("clampPercent(%d) = %d, want %d", in, got, want)
		}
	}
}

// fakeCapacity is a clusterCapacity stub.
type fakeCapacity struct {
	cpu, mem int64
	err      error
}

func (f fakeCapacity) ClusterAllocatable(context.Context) (int64, int64, error) {
	return f.cpu, f.mem, f.err
}

// A percentage budget is derived from live cluster capacity, so the same config is
// correct on any cluster and follows autoscaling — the whole reason it exists.
func TestStartResourceBudget_DerivesFromCapacity(t *testing.T) {
	t.Setenv("FORGE_MAX_TOTAL_CPU_MILLICORES", "0")
	t.Setenv("FORGE_MAX_TOTAL_MEMORY_MB", "0")
	t.Setenv("FORGE_MAX_TOTAL_CPU_PERCENT", "60")
	t.Setenv("FORGE_MAX_TOTAL_MEMORY_PERCENT", "50")
	initConcurrencyConfig()

	// 8 cores / 16 GiB cluster → 60% CPU, 50% memory.
	startResourceBudget(context.Background(), fakeCapacity{cpu: 8000, mem: 16384})
	if got := maxTotalCPUMillicores.Load(); got != 4800 {
		t.Errorf("cpu budget = %d, want 60%% of 8000 = 4800", got)
	}
	if got := maxTotalMemoryMB.Load(); got != 8192 {
		t.Errorf("mem budget = %d, want 50%% of 16384 = 8192", got)
	}
}

// No percentage configured → the absolute values are left exactly as they were. This
// is what keeps every existing deployment unchanged.
func TestStartResourceBudget_NoPercentLeavesAbsolute(t *testing.T) {
	t.Setenv("FORGE_MAX_TOTAL_CPU_MILLICORES", "3000")
	t.Setenv("FORGE_MAX_TOTAL_MEMORY_MB", "4000")
	initConcurrencyConfig()
	startResourceBudget(context.Background(), fakeCapacity{cpu: 99999, mem: 99999})
	if maxTotalCPUMillicores.Load() != 3000 || maxTotalMemoryMB.Load() != 4000 {
		t.Errorf("no percent set must leave absolute budgets untouched, got cpu=%d mem=%d",
			maxTotalCPUMillicores.Load(), maxTotalMemoryMB.Load())
	}
}

// If capacity cannot be read, the absolute fallback stands — percent mode must NEVER
// silently drop the budget to 0, which would wedge every run cluster-wide.
func TestStartResourceBudget_CapacityErrorKeepsFallback(t *testing.T) {
	t.Setenv("FORGE_MAX_TOTAL_CPU_MILLICORES", "2500")
	t.Setenv("FORGE_MAX_TOTAL_MEMORY_MB", "3500")
	t.Setenv("FORGE_MAX_TOTAL_CPU_PERCENT", "60")
	initConcurrencyConfig()
	startResourceBudget(context.Background(), fakeCapacity{err: fmt.Errorf("node list forbidden")})
	if maxTotalCPUMillicores.Load() != 2500 || maxTotalMemoryMB.Load() != 3500 {
		t.Errorf("a capacity read error must keep the absolute fallback, got cpu=%d mem=%d",
			maxTotalCPUMillicores.Load(), maxTotalMemoryMB.Load())
	}
}

// docker mode has no cluster (nil capacity); percent config must fall back, not panic.
func TestStartResourceBudget_NilCapacityFallsBack(t *testing.T) {
	t.Setenv("FORGE_MAX_TOTAL_CPU_MILLICORES", "1500")
	t.Setenv("FORGE_MAX_TOTAL_CPU_PERCENT", "80")
	initConcurrencyConfig()
	startResourceBudget(context.Background(), nil)
	if maxTotalCPUMillicores.Load() != 1500 {
		t.Errorf("nil capacity must keep the absolute fallback, got %d", maxTotalCPUMillicores.Load())
	}
}

// Only the configured dimension is overridden: a CPU percentage must not zero out a
// memory budget the operator set in absolute terms (or vice versa).
func TestStartResourceBudget_MixedPercentAndAbsolute(t *testing.T) {
	t.Setenv("FORGE_MAX_TOTAL_CPU_MILLICORES", "0")
	t.Setenv("FORGE_MAX_TOTAL_MEMORY_MB", "5000")
	t.Setenv("FORGE_MAX_TOTAL_CPU_PERCENT", "50")
	t.Setenv("FORGE_MAX_TOTAL_MEMORY_PERCENT", "0")
	initConcurrencyConfig()
	startResourceBudget(context.Background(), fakeCapacity{cpu: 10000, mem: 99999})
	if got := maxTotalCPUMillicores.Load(); got != 5000 {
		t.Errorf("cpu budget = %d, want 50%% of 10000", got)
	}
	if got := maxTotalMemoryMB.Load(); got != 5000 {
		t.Errorf("mem budget = %d, want the absolute 5000 (no mem percent set)", got)
	}
}
