//go:build !linux

package main

import "fmt"

// diskUsage is Linux-only: the pipeline runs this in a Linux sandbox, and the
// non-Linux build exists so `go build ./...` and the tests still work on a
// developer's machine. Reporting nothing is correct here — a wrong number would be
// worse than an absent one.
func diskUsage(path string) (total, avail uint64, err error) {
	return 0, 0, fmt.Errorf("disk usage is only reported on linux")
}
