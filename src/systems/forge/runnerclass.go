package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// RunnerClass defines the resource limits for a named execution tier.
// The runner_classes table is managed by GORM AutoMigrate.
type RunnerClass struct {
	Name          string `gorm:"primaryKey"            json:"name"`
	MemoryMB      int64  `gorm:"not null"              json:"memory_mb"`
	CPUMillicores int64  `gorm:"not null"              json:"cpu_millicores"`
	PidsLimit     int64  `gorm:"not null;default:64"   json:"pids_limit"`
	TmpfsMB       int64  `gorm:"not null;default:64"   json:"tmpfs_mb"`
	Enabled       bool   `gorm:"not null;default:true" json:"enabled"`
}

var (
	rcDB   *gorm.DB
	rcDBMu sync.Mutex
)

func connectRC() *gorm.DB {
	rcDBMu.Lock()
	defer rcDBMu.Unlock()
	if rcDB != nil {
		return rcDB
	}
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/forge")), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "runnerclass: connect to database: %v\n", err)
		os.Exit(1)
	}
	rcDB = conn
	return rcDB
}

// defaultRunnerClasses are seeded on startup if absent. Operators can edit them
// at runtime via the API — changes take effect immediately without a restart.
var defaultRunnerClasses = []RunnerClass{
	{Name: "standard", MemoryMB: 256, CPUMillicores: 500, PidsLimit: 64, TmpfsMB: 64, Enabled: true},
	{Name: "large", MemoryMB: 2048, CPUMillicores: 2000, PidsLimit: 256, TmpfsMB: 512, Enabled: true},
	{Name: "xlarge", MemoryMB: 8192, CPUMillicores: 4000, PidsLimit: 512, TmpfsMB: 2048, Enabled: true},
}

func migrateAndSeedRunnerClasses() error {
	g := connectRC()
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
	var rc RunnerClass
	if err := connectRC().WithContext(ctx).Where("name = ?", name).First(&rc).Error; err != nil {
		return RunnerClass{}, fmt.Errorf("runner class %q not found", name)
	}
	if !rc.Enabled {
		return RunnerClass{}, fmt.Errorf("runner class %q is disabled", name)
	}
	return rc, nil
}
