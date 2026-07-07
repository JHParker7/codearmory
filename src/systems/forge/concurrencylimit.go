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

// initConcurrencyConfig loads the global default concurrency caps from the
// environment. Both default to 0 (unlimited), so an operator that configures
// nothing keeps forge's original behaviour — claimPendingExecution then dequeues
// strictly FIFO with no per-tenant gating.
func initConcurrencyConfig() {
	defaultMaxConcurrentPerOrg = envIntOrDefault("FORGE_MAX_CONCURRENT_PER_ORG", 0)
	defaultMaxConcurrentPerUser = envIntOrDefault("FORGE_MAX_CONCURRENT_PER_USER", 0)
	if defaultMaxConcurrentPerOrg < 0 {
		defaultMaxConcurrentPerOrg = 0
	}
	if defaultMaxConcurrentPerUser < 0 {
		defaultMaxConcurrentPerUser = 0
	}
	slog.Info("concurrency limits configured",
		"default_per_org", defaultMaxConcurrentPerOrg,
		"default_per_user", defaultMaxConcurrentPerUser)
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
