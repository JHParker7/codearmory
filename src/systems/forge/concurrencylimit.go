package main

import (
	"fmt"
	"log/slog"
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
// times out. Both default to 0 (unlimited), preserving the original behaviour where
// admission is gated only by the per-org/user counts and the worker-pool size.
// Set them to a fraction of cluster capacity to reserve headroom for other workloads.
var (
	maxTotalCPUMillicores int64
	maxTotalMemoryMB      int64
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
	maxTotalCPUMillicores = int64(envIntOrDefault("FORGE_MAX_TOTAL_CPU_MILLICORES", 0))
	maxTotalMemoryMB = int64(envIntOrDefault("FORGE_MAX_TOTAL_MEMORY_MB", 0))
	if maxTotalCPUMillicores < 0 {
		maxTotalCPUMillicores = 0
	}
	if maxTotalMemoryMB < 0 {
		maxTotalMemoryMB = 0
	}
	slog.Info("concurrency limits configured",
		"default_per_org", defaultMaxConcurrentPerOrg,
		"default_per_user", defaultMaxConcurrentPerUser,
		"max_total_cpu_millicores", maxTotalCPUMillicores,
		"max_total_memory_mb", maxTotalMemoryMB)
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
