package main

import (
	"context"
	"fmt"
)

// defaultRunnerClasses are seeded on startup if absent. Operators can edit them
// at runtime via the API — changes take effect immediately without a restart.
var defaultRunnerClasses = []RunnerClass{
	{Name: "standard", MemoryMB: 256, CPUMillicores: 500, PidsLimit: 64, TmpfsMB: 64, Enabled: true},
	{Name: "large", MemoryMB: 2048, CPUMillicores: 2000, PidsLimit: 256, TmpfsMB: 512, Enabled: true},
	{Name: "xlarge", MemoryMB: 8192, CPUMillicores: 4000, PidsLimit: 512, TmpfsMB: 2048, Enabled: true},
}

func migrateAndSeedRunnerClasses() error {
	g := connect()
	if err := g.AutoMigrate(&RunnerClass{}); err != nil {
		return fmt.Errorf("migrate runner_classes: %w", err)
	}
	for i := range defaultRunnerClasses {
		c := defaultRunnerClasses[i]
		g.Where(RunnerClass{Name: c.Name}).FirstOrCreate(&c)
	}
	return nil
}

// runnerClassSpec fetches the named class from the database. Returns an error if
// the class does not exist or is disabled.
func runnerClassSpec(ctx context.Context, name string) (RunnerClass, error) {
	row, err := (RunnerClass{Name: name}).Get(ctx)
	if err != nil {
		return RunnerClass{}, fmt.Errorf("runner class %q not found", name)
	}
	rc := row.(RunnerClass)
	if !rc.Enabled {
		return RunnerClass{}, fmt.Errorf("runner class %q is disabled", name)
	}
	return rc, nil
}
