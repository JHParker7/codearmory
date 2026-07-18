package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// defaultMaxConcurrentPerOrg / PerUser are the global fallback caps applied to any
// org/user without an explicit ConcurrencyLimit row. A value <= 0 means unlimited.
// They are read once at startup from FORGE_MAX_CONCURRENT_PER_ORG /
// FORGE_MAX_CONCURRENT_PER_USER; per-tenant overrides live in concurrency_limits
// and are read live at claim time.
var (
	defaultMaxConcurrentPerOrg  int
	defaultMaxConcurrentPerUser int
)

// maxTotalCPUMillicores / maxTotalMemoryMB are the cluster-wide resource budgets the
// scheduler admits runners against: the next pending execution is only claimed (and
// its Job sent to the backend) when the summed CPU/memory of already-running runners
// plus its own runner class fits the budget. This makes forge pace a fan-out (e.g. a
// matrix of `large` runners) to what the cluster can actually run, instead of dumping
// every Job on the kube-scheduler at once and letting the excess sit Pending until it
// times out. 0 means unlimited (admission gated only by per-tenant counts).
//
// ATOMIC because two goroutines touch them: the claim path reads them (db.go), and the
// budget refresher (startResourceBudget) writes them when the value is derived from a
// PERCENTAGE of live cluster capacity — which changes as the cluster autoscales.
var (
	maxTotalCPUMillicores atomic.Int64
	maxTotalMemoryMB      atomic.Int64
)

// maxTotalCPUPercent / maxTotalMemoryPercent express the budget as a percentage of
// total allocatable cluster capacity instead of an absolute figure. This is the right
// knob for a real cluster: an absolute millicore budget is meaningless across dev,
// staging and prod (each has different capacity) and actively wrong on an autoscaling
// cluster, where the schedulable capacity changes under you. A percentage tracks the
// cluster — 60% of a 3-node cluster and 60% of a 30-node cluster are both "leave 40%
// for everything else". 0 means "not set; use the absolute value". [1,100].
var (
	maxTotalCPUPercent    int
	maxTotalMemoryPercent int
)

// initConcurrencyConfig loads the global default concurrency caps from the
// environment. All default to 0 (unlimited), so an operator that configures
// nothing keeps forge's original behaviour — claimPendingExecution then dequeues
// strictly FIFO with no per-tenant or resource gating.
func initConcurrencyConfig() {
	defaultMaxConcurrentPerOrg = envIntOrDefault("FORGE_MAX_CONCURRENT_PER_ORG", 0)
	defaultMaxConcurrentPerUser = envIntOrDefault("FORGE_MAX_CONCURRENT_PER_USER", 0)
	if defaultMaxConcurrentPerOrg < 0 {
		defaultMaxConcurrentPerOrg = 0
	}
	if defaultMaxConcurrentPerUser < 0 {
		defaultMaxConcurrentPerUser = 0
	}
	// Absolute budgets: the fallback, and the only budget available in docker mode
	// (no cluster to take a percentage of). Percent-based budgets overwrite these once
	// capacity is known — see startResourceBudget.
	maxTotalCPUMillicores.Store(nonNegInt64(envIntOrDefault("FORGE_MAX_TOTAL_CPU_MILLICORES", 0)))
	maxTotalMemoryMB.Store(nonNegInt64(envIntOrDefault("FORGE_MAX_TOTAL_MEMORY_MB", 0)))
	maxTotalCPUPercent = clampPercent(envIntOrDefault("FORGE_MAX_TOTAL_CPU_PERCENT", 0))
	maxTotalMemoryPercent = clampPercent(envIntOrDefault("FORGE_MAX_TOTAL_MEMORY_PERCENT", 0))
	slog.Info("concurrency limits configured",
		"default_per_org", defaultMaxConcurrentPerOrg,
		"default_per_user", defaultMaxConcurrentPerUser,
		"max_total_cpu_millicores", maxTotalCPUMillicores.Load(),
		"max_total_memory_mb", maxTotalMemoryMB.Load(),
		"max_total_cpu_percent", maxTotalCPUPercent,
		"max_total_memory_percent", maxTotalMemoryPercent)
}

func nonNegInt64(v int) int64 {
	if v < 0 {
		return 0
	}
	return int64(v)
}

// clampPercent constrains a percentage to [0,100]; 0 means "unset". A value above 100
// is clamped rather than rejected — "use everything" is a coherent (if reckless)
// intent, and clamping is friendlier than refusing to boot.
func clampPercent(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// clusterCapacity reports the total ALLOCATABLE resources of the cluster — capacity
// minus what kubelet/system-reserved already holds back, i.e. what is actually
// schedulable. Implemented by the kubernetes runtime; docker has no cluster, so a
// percent-based budget there falls back to the absolute values.
type clusterCapacity interface {
	ClusterAllocatable(ctx context.Context) (cpuMillicores, memMB int64, err error)
}

// budgetRefreshInterval is how often a percent-based budget is re-derived from live
// capacity, so it tracks cluster autoscaling. Env-overridable; default 2 minutes.
func budgetRefreshInterval() time.Duration {
	return time.Duration(envIntOrDefault("FORGE_BUDGET_REFRESH_SECONDS", 120)) * time.Second
}

// startResourceBudget derives the CPU/memory admission budget from a percentage of
// total allocatable cluster capacity, and keeps it current as the cluster scales.
//
// A no-op unless a percentage is configured — then the absolute FORGE_MAX_TOTAL_*
// values stand untouched. If capacity cannot be read (docker mode, or the node-list
// RBAC is missing), the absolute values stand and a warning is logged: percent mode
// never silently drops the budget to 0, which would wedge every run.
//
// The first refresh is SYNCHRONOUS so the budget is right before the worker pool
// starts claiming; subsequent refreshes run on a ticker.
func startResourceBudget(ctx context.Context, cap clusterCapacity) {
	if maxTotalCPUPercent <= 0 && maxTotalMemoryPercent <= 0 {
		return
	}
	if cap == nil {
		slog.Warn("percent-based forge budget is configured but this runtime has no cluster capacity (docker mode?); using the absolute FORGE_MAX_TOTAL_* values instead")
		return
	}
	refresh := func() {
		cpu, mem, err := cap.ClusterAllocatable(ctx)
		if err != nil {
			slog.Warn("could not read cluster capacity; keeping the current forge budget", "error", err)
			return
		}
		if maxTotalCPUPercent > 0 {
			maxTotalCPUMillicores.Store(cpu * int64(maxTotalCPUPercent) / 100)
		}
		if maxTotalMemoryPercent > 0 {
			maxTotalMemoryMB.Store(mem * int64(maxTotalMemoryPercent) / 100)
		}
		slog.Info("forge resource budget derived from cluster capacity",
			"allocatable_cpu_millicores", cpu, "allocatable_memory_mb", mem,
			"budget_cpu_millicores", maxTotalCPUMillicores.Load(),
			"budget_memory_mb", maxTotalMemoryMB.Load())
	}
	refresh()
	go func() {
		t := time.NewTicker(budgetRefreshInterval())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
}

// migrateConcurrencyLimits creates the concurrency_limits table. There is nothing
// to seed — an empty table means every scope falls back to the env defaults.
func migrateConcurrencyLimits() error {
	if err := connect().AutoMigrate(&ConcurrencyLimit{}); err != nil {
		return fmt.Errorf("migrate concurrency_limits: %w", err)
	}
	return nil
}

// validateConcurrencyScope reports whether scope is one of the two supported
// scope kinds.
func validateConcurrencyScope(scope string) bool {
	return scope == ConcurrencyScopeOrg || scope == ConcurrencyScopeUser
}
