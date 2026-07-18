package main

import (
	"context"
	"fmt"
)

// defaultRunnerClasses are seeded on startup if absent. Operators can edit them
// at runtime via the API — changes take effect immediately without a restart.
var defaultRunnerClasses = []RunnerClass{
	{Name: "standard", MemoryMB: 256, CPUMillicores: 500, PidsLimit: 64, TmpfsMB: 64, DiskGB: 10, Backend: "default", Enabled: true},
	{Name: "large", MemoryMB: 2048, CPUMillicores: 2000, PidsLimit: 256, TmpfsMB: 512, DiskGB: 20, Backend: "default", Enabled: true},
	{Name: "xlarge", MemoryMB: 8192, CPUMillicores: 4000, PidsLimit: 512, TmpfsMB: 2048, DiskGB: 40, Backend: "default", Enabled: true},
}

// defaultRunnersPrivileged reports whether the seeded default runner classes should
// run privileged. True exactly when the "default" backend is kernel-isolated
// (RUNTIME=kata or gvisor): there a guest/userspace kernel — not the shared
// host kernel — is the boundary, so root + writable rootfs is safe and lets package
// managers (apt/pacman/dnf) work. On a shared-kernel default backend
// (docker/kubernetes) privileged would be a host-kernel escape, so the defaults stay
// locked-down.
func defaultRunnersPrivileged() bool {
	return isKernelIsolatedBackendType(defaultRuntimeType())
}

// migrateAndSeedRunnerClasses creates the runner_classes table and seeds the default
// classes if absent. The seeded classes are privileged when the default backend is
// VM-isolated (see defaultRunnersPrivileged). Seeding is idempotent (FirstOrCreate);
// an operator can edit the classes via the API afterwards.
func migrateAndSeedRunnerClasses() error {
	g := connect()
	if err := g.AutoMigrate(&RunnerClass{}); err != nil {
		return fmt.Errorf("migrate runner_classes: %w", err)
	}
	privileged := defaultRunnersPrivileged()
	for i := range defaultRunnerClasses {
		c := defaultRunnerClasses[i]
		c.Privileged = privileged
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
