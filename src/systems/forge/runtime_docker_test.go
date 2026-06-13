package main

import "testing"

// The Docker runtime is forge's DEFAULT sandbox. Its security-relevant logic that
// is reachable without a live Docker daemon lives in newDockerRuntime: rejection of
// dangerous network modes and capture of the egress-proxy URL. The container
// hardening (CapDrop ALL, no-new-privileges, ReadonlyRootfs, tmpfs, PidsLimit) and
// the proxy-env injection are built inline inside Run(), which requires a running
// daemon and database to exercise; see the note at the bottom of this file.

// TestNewDockerRuntime_RejectsDangerousNetModes is the crux of the local sandbox
// isolation guarantee: a "host" or "bridge" network mode would give the container
// the host network stack or the default Docker bridge, defeating the egress proxy.
// newDockerRuntime must refuse to start with either.
func TestNewDockerRuntime_RejectsDangerousNetModes(t *testing.T) {
	for _, mode := range []string{"host", "bridge"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("FORGE_NETWORK_MODE", mode)
			r, err := newDockerRuntime()
			if err == nil {
				t.Fatalf("FORGE_NETWORK_MODE=%q was accepted, want rejection", mode)
			}
			if r != nil {
				t.Errorf("runtime = %v, want nil on rejection", r)
			}
		})
	}
}

// TestNewDockerRuntime_AcceptsSafeNetModes checks that the non-dangerous modes are
// allowed and the chosen value is plumbed onto the runtime as allowedNet (it is
// later applied verbatim as the container NetworkMode).
func TestNewDockerRuntime_AcceptsSafeNetModes(t *testing.T) {
	for _, mode := range []string{"none", "forge-exec", "my-custom-bridge"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("FORGE_NETWORK_MODE", mode)
			r, err := newDockerRuntime()
			if err != nil {
				t.Fatalf("FORGE_NETWORK_MODE=%q rejected unexpectedly: %v", mode, err)
			}
			if r.allowedNet != mode {
				t.Errorf("allowedNet = %q, want %q", r.allowedNet, mode)
			}
		})
	}
}

// TestNewDockerRuntime_DefaultsToNoneNetwork guards the secure default: with no
// FORGE_NETWORK_MODE set, the sandbox must fall back to the isolated "none"
// network rather than inheriting a permissive default.
func TestNewDockerRuntime_DefaultsToNoneNetwork(t *testing.T) {
	t.Setenv("FORGE_NETWORK_MODE", "")
	r, err := newDockerRuntime()
	if err != nil {
		t.Fatalf("newDockerRuntime with unset network mode: %v", err)
	}
	if r.allowedNet != "none" {
		t.Errorf("default allowedNet = %q, want \"none\"", r.allowedNet)
	}
}

// TestNewDockerRuntime_CapturesEgressProxy checks the egress-proxy URL is read from
// FORGE_EGRESS_PROXY and stored on the runtime. Run() injects this value into
// HTTP_PROXY/HTTPS_PROXY for the container; an empty value means no injection.
func TestNewDockerRuntime_CapturesEgressProxy(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		t.Setenv("FORGE_NETWORK_MODE", "none")
		t.Setenv("FORGE_EGRESS_PROXY", "http://egress-proxy:3128")
		r, err := newDockerRuntime()
		if err != nil {
			t.Fatalf("newDockerRuntime: %v", err)
		}
		if r.egressProxy != "http://egress-proxy:3128" {
			t.Errorf("egressProxy = %q, want %q", r.egressProxy, "http://egress-proxy:3128")
		}
	})
	t.Run("unset means empty", func(t *testing.T) {
		t.Setenv("FORGE_NETWORK_MODE", "none")
		t.Setenv("FORGE_EGRESS_PROXY", "")
		r, err := newDockerRuntime()
		if err != nil {
			t.Fatalf("newDockerRuntime: %v", err)
		}
		if r.egressProxy != "" {
			t.Errorf("egressProxy = %q, want empty when FORGE_EGRESS_PROXY unset", r.egressProxy)
		}
	})
}

// TestDangerousNetModes pins the allow/deny classification used by
// newDockerRuntime so the set cannot silently widen to permit host/bridge.
func TestDangerousNetModes(t *testing.T) {
	dangerous := []string{"host", "bridge"}
	for _, mode := range dangerous {
		if !dangerousNetModes[mode] {
			t.Errorf("%q should be classified dangerous", mode)
		}
	}
	for _, mode := range []string{"none", "forge-exec", "container:abc", ""} {
		if dangerousNetModes[mode] {
			t.Errorf("%q should NOT be classified dangerous", mode)
		}
	}
	// Exactly host and bridge — no more, no fewer.
	if len(dangerousNetModes) != len(dangerous) {
		t.Errorf("dangerousNetModes has %d entries, want %d (%v)", len(dangerousNetModes), len(dangerous), dangerous)
	}
}

// NOTE on coverage limits (infra-free constraint):
// The container/HostConfig hardening — CapDrop ["ALL"], SecurityOpt
// "no-new-privileges", ReadonlyRootfs, the /tmp tmpfs, PidsLimit/Memory/CPU
// limits — and the egress proxy env-var injection (HTTP_PROXY/HTTPS_PROXY/NO_PROXY
// plus protection against a user-supplied HTTP_PROXY override) are constructed
// inline inside DockerRuntime.Run(). Run() first calls runnerClassSpec (Postgres)
// and the live Docker client (ImageInspect/ImagePull/ContainerCreate) before the
// config is materialised, with no seam to capture the config.
// These are exercised by the integration tests, not here. Extracting a pure
// buildContainerConfig/buildHostConfig helper would make them unit-testable, but
// that is a production-code change beyond the scope of adding tests.
