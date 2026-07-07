package main

import "testing"

// TestInitConcurrencyConfig_ResourceBudgets checks the cluster-wide resource budgets
// parse from the environment, clamp negatives to 0 (unlimited), and default to 0 when
// unset so an unconfigured operator keeps the original count-only admission.
func TestInitConcurrencyConfig_ResourceBudgets(t *testing.T) {
	t.Run("parsed from env", func(t *testing.T) {
		t.Setenv("FORGE_MAX_TOTAL_CPU_MILLICORES", "8000")
		t.Setenv("FORGE_MAX_TOTAL_MEMORY_MB", "16384")
		initConcurrencyConfig()
		if maxTotalCPUMillicores != 8000 {
			t.Errorf("maxTotalCPUMillicores = %d, want 8000", maxTotalCPUMillicores)
		}
		if maxTotalMemoryMB != 16384 {
			t.Errorf("maxTotalMemoryMB = %d, want 16384", maxTotalMemoryMB)
		}
	})

	t.Run("negative clamps to unlimited", func(t *testing.T) {
		t.Setenv("FORGE_MAX_TOTAL_CPU_MILLICORES", "-1")
		t.Setenv("FORGE_MAX_TOTAL_MEMORY_MB", "-100")
		initConcurrencyConfig()
		if maxTotalCPUMillicores != 0 || maxTotalMemoryMB != 0 {
			t.Errorf("negative budgets should clamp to 0, got cpu=%d mem=%d", maxTotalCPUMillicores, maxTotalMemoryMB)
		}
	})

	t.Run("unset defaults to unlimited", func(t *testing.T) {
		initConcurrencyConfig()
		if maxTotalCPUMillicores != 0 || maxTotalMemoryMB != 0 {
			t.Errorf("unset budgets should be 0, got cpu=%d mem=%d", maxTotalCPUMillicores, maxTotalMemoryMB)
		}
	})
}
