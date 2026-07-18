package main

import "testing"

// defaultRunnersPrivileged drives whether the seeded default runner classes are
// privileged: on only for kernel-isolated default backends (kata/gvisor), where a
// guest/userspace kernel is the isolation boundary; off for shared-kernel backends
// (docker/kubernetes/unset).
func TestDefaultRunnersPrivileged(t *testing.T) {
	cases := map[string]bool{
		"":           false, // unset → docker
		"docker":     false,
		"kubernetes": false,
		"kata":       true,
		"gvisor":     true,
	}
	for runtime, want := range cases {
		t.Run(runtime, func(t *testing.T) {
			t.Setenv("RUNTIME", runtime)
			if got := defaultRunnersPrivileged(); got != want {
				t.Errorf("RUNTIME=%q: defaultRunnersPrivileged() = %v, want %v", runtime, got, want)
			}
		})
	}
}
