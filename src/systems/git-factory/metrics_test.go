package main

// Unit test for metrics.go. ARCHITECTURE §5-Step1 counts repo creations; this
// pins that initMetrics wires the instrument and that recording is safe (a nil
// counter would panic on Add). Touches the package-global instrument — not
// parallel.

import (
	"context"
	"testing"
)

func TestInitMetrics_WiresCounter(t *testing.T) {
	meterReposCreated = nil
	initMetrics()
	if meterReposCreated == nil {
		t.Fatal("initMetrics did not initialize meterReposCreated")
	}
	// Recording must not panic once the instrument exists.
	meterReposCreated.Add(context.Background(), 1)
}
