package main

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"
)

// Runtime is the interface for running sandboxed container commands. Concrete
// runtimes are built lazily by the runtimeRegistry from a RuntimeBackend; see
// registry.go.
type Runtime interface {
	// Run executes the command described by exec. It blocks until the command
	// completes, the context is cancelled, or the execution times out.
	// Returning a non-nil error does not imply a specific exit code — the worker
	// inspects ctx.Err() to distinguish cancellation from failure.
	Run(ctx context.Context, exec Execution) (RunResult, error)

	// Cancel terminates a running execution identified by executionID.
	// Called when the user issues DELETE /executions/{id} while the job is running.
	Cancel(ctx context.Context, executionID string) error
}

// defaultRuntimeType returns the Type seeded onto the "default" runtime backend
// from the legacy RUNTIME env var, preserving the single-runtime deployment
// contract: RUNTIME unset means docker. An unrecognised value is passed through
// and fails when the registry tries to build it, mirroring the old behaviour.
func defaultRuntimeType() string {
	if rt := os.Getenv("RUNTIME"); rt != "" {
		return rt
	}
	return "docker"
}

// proxyEnvPairs is the set of egress-proxy environment variables forge injects
// into a sandboxed job (upper- and lower-case forms, plus NO_PROXY for loopback and
// the in-cluster endpoints a sandbox must reach directly).
// Used by the docker runtime; the kubernetes runtime injects the same set separately.
func proxyEnvPairs(proxy string) [][2]string {
	noProxy := strings.Join(noProxyHosts(), ",")
	return [][2]string{
		{"HTTP_PROXY", proxy},
		{"HTTPS_PROXY", proxy},
		{"NO_PROXY", noProxy},
		{"http_proxy", proxy},
		{"https_proxy", proxy},
		{"no_proxy", noProxy},
	}
}

// noProxyHosts is the NO_PROXY list injected alongside HTTP(S)_PROXY: loopback, plus
// every in-cluster endpoint forge itself points a sandbox at.
//
// Why those have to bypass the proxy: the egress proxy refuses to DIAL a private
// address unconditionally (its IP guard blocks loopback/RFC1918/link-local so an
// allowlisted — or attacker-controlled — name cannot resolve to a cluster service).
// The artifact store and the base-image mirror are exactly such addresses: forge hands
// them to the sandbox as in-cluster ClusterIP URLs, so routing them through the proxy
// means save/restore-artifact and every mirrored base-image pull fail with "egress to
// non-public address blocked". Naming them in NO_PROXY sends that traffic straight to
// the service instead, which is what the sandbox NetworkPolicy opens a scoped hole for
// (forge-egress-networkpolicy.yaml) — nothing else internal becomes reachable.
//
// These are derived from forge's OWN config, never from a request, so a sandbox cannot
// add a host to its own bypass list.
func noProxyHosts() []string {
	hosts := []string{"localhost", "127.0.0.1"}
	seen := map[string]bool{"localhost": true, "127.0.0.1": true}
	add := func(raw string) {
		h := hostOnly(raw)
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		hosts = append(hosts, h)
	}

	// The artifact store the save/restore helper curls.
	if u, err := url.Parse(artifactsURL()); err == nil {
		add(u.Host)
	}
	// The base-image mirror kaniko is remapped to ("origin=mirror;origin2=mirror2"),
	// the layer-cache repo on it ("host:port/repo"), and any operator-listed private
	// registry — all of which a build reaches over the pod network, not the internet.
	for _, m := range strings.Split(registryMirrors(), ";") {
		if _, mirror, ok := strings.Cut(m, "="); ok {
			add(mirror)
		}
	}
	if repo := buildCacheRepo(); repo != "" {
		add(strings.SplitN(repo, "/", 2)[0])
	}
	for _, r := range insecureRegistries() {
		add(r)
	}
	// The git-factory a `git:` secret_ref hands the sandbox a clone URL for. A push
	// or a PR-create reaches it over the pod network (a ClusterIP), never the
	// internet, so — exactly like the artifact store and the base-image mirror above
	// — routing it through the egress proxy fails on the proxy's private-address IP
	// guard ("egress to non-public address blocked"). Naming it in NO_PROXY sends the
	// git wire straight to the service. Empty (unset) keeps the old behaviour.
	if gf := gitFactoryURL(); gf != "" {
		add(gf)
	}
	return hosts
}

// gitFactoryURL is the in-cluster git-factory a sandbox pushes to and opens PRs
// against, named in NO_PROXY so it bypasses the egress proxy (see noProxyHosts).
// Derived from forge's OWN config (FORGE_GIT_FACTORY_URL), never a request.
func gitFactoryURL() string {
	return strings.TrimRight(envOrDefault("FORGE_GIT_FACTORY_URL", ""), "/")
}

// hostOnly strips a port (and any surrounding whitespace) from a "host[:port]" or
// "scheme://host[:port]" authority. A NO_PROXY entry without a port matches the host
// on every port, which is what we want — and is how both curl and Go's own proxy
// resolution read it.
func hostOnly(raw string) string {
	h := strings.TrimSpace(raw)
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

func ptr[T any](v T) *T { return &v }

// bytesPerMiB is the divisor for converting a byte count to whole MiB, used by
// the docker and kubernetes runtimes when recording peak memory usage so the
// conversion lives in one place.
const bytesPerMiB = 1024 * 1024
