package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// DockerRuntime runs executions as short-lived Docker containers on the local daemon.
// Each container gets a read-only root filesystem, a tmpfs /tmp, and resource limits
// determined by the execution's runner class. Network mode is set by FORGE_NETWORK_MODE
// (default: none).
type DockerRuntime struct {
	client      *client.Client
	allowedNet  string
	egressProxy string // HTTP proxy URL injected into containers, e.g. "http://egress-proxy:3128"
}

// dangerousNetModes are Docker network modes that grant containers access to the
// host network stack or the default Docker bridge, defeating sandbox isolation.
var dangerousNetModes = map[string]bool{"host": true, "bridge": true}

func newDockerRuntime() (*DockerRuntime, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	net := envOrDefault("FORGE_NETWORK_MODE", "none")
	if dangerousNetModes[net] {
		return nil, fmt.Errorf("FORGE_NETWORK_MODE=%q is not permitted; use \"none\" or a custom bridge network name", net)
	}
	return &DockerRuntime{
		client:      c,
		allowedNet:  net,
		egressProxy: envOrDefault("FORGE_EGRESS_PROXY", ""),
	}, nil
}

// Run pulls the image if not present, creates a sandboxed container, and blocks
// until completion or timeout. The container is always removed on return.
func (r *DockerRuntime) Run(ctx context.Context, exec Execution) (RunResult, error) {
	spec, err := runnerClassSpec(ctx, exec.RunnerClass)
	if err != nil {
		return RunResult{}, fmt.Errorf("runner class: %w", err)
	}
	memLimit := spec.MemoryMB * 1024 * 1024
	cpuQuota := spec.CPUMillicores * 100 // 1000m → 100000 (one full core)
	pidsLimit := spec.PidsLimit
	tmpfsOpt := fmt.Sprintf("size=%dm", spec.TmpfsMB)

	envList := make([]string, 0, len(exec.Env))
	for k, v := range exec.Env {
		envList = append(envList, k+"="+v)
	}
	if r.egressProxy != "" {
		for _, pair := range proxyEnvPairs(r.egressProxy) {
			if _, exists := exec.Env[pair[0]]; !exists {
				envList = append(envList, pair[0]+"="+pair[1])
			}
		}
	}

	if _, err := r.client.ImageInspect(ctx, exec.Image); err != nil {
		if !cerrdefs.IsNotFound(err) {
			return RunResult{}, fmt.Errorf("image inspect: %w", err)
		}
		reader, pullErr := r.client.ImagePull(ctx, exec.Image, image.PullOptions{})
		if pullErr != nil {
			return RunResult{}, fmt.Errorf("image pull: %w", pullErr)
		}
		io.Copy(io.Discard, reader) //nolint:errcheck — pull progress stream; failure here doesn't affect image availability
		reader.Close()
	}

	resp, err := r.client.ContainerCreate(ctx,
		&container.Config{
			Image:        exec.Image,
			Cmd:          exec.Command,
			Env:          envList,
			AttachStdout: true,
			AttachStderr: true,
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(r.allowedNet),
			// ReadonlyRootfs prevents writes to the image layers. /tmp is a writable
			// tmpfs mount so programs that need a scratch directory still work.
			ReadonlyRootfs: true,
			Tmpfs:          map[string]string{"/tmp": tmpfsOpt},
			Resources: container.Resources{
				Memory:    memLimit,
				CPUQuota:  cpuQuota,
				PidsLimit: &pidsLimit,
			},
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
		},
		nil, nil, "",
	)
	if err != nil {
		return RunResult{}, fmt.Errorf("container create: %w", err)
	}
	containerID := resp.ID

	// context.Background() ensures cleanup runs even when ctx is already cancelled
	// (timeout or user-initiated cancel).
	defer r.client.ContainerRemove(context.Background(), containerID, container.RemoveOptions{Force: true})

	if err := r.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return RunResult{}, fmt.Errorf("container start: %w", err)
	}

	// Sample memory in the background while the container runs: the cgroup stats
	// disappear when it stops, so a post-exit read returns zero. peakMemBytes holds
	// the highest sample seen (atomic — the sampler goroutine writes, Run reads).
	var peakMemBytes int64
	samplerDone := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-samplerDone:
				return
			case <-t.C:
				if b, ok := r.sampleMemoryBytes(containerID); ok && b > atomic.LoadInt64(&peakMemBytes) {
					atomic.StoreInt64(&peakMemBytes, b)
				}
			}
		}
	}()
	defer close(samplerDone)

	// Wrap ctx with the execution's own timeout so the container is stopped and
	// the result recorded as timed_out rather than running forever.
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, time.Duration(exec.TimeoutSecs)*time.Second)
	defer timeoutCancel()

	statusCh, errCh := r.client.ContainerWait(timeoutCtx, containerID, container.WaitConditionNotRunning)

	var exitCode int
	select {
	case <-timeoutCtx.Done():
		r.client.ContainerStop(context.Background(), containerID, container.StopOptions{}) //nolint
		return RunResult{}, timeoutCtx.Err()
	case err := <-errCh:
		if err != nil {
			return RunResult{}, fmt.Errorf("container wait: %w", err)
		}
	case status := <-statusCh:
		exitCode = int(status.StatusCode)
	}

	// The container has exited; record the memory ceiling and peak usage so far.
	memLimitMB := ptr(spec.MemoryMB)
	var memUsedMB *int64
	if b := atomic.LoadInt64(&peakMemBytes); b > 0 {
		memUsedMB = ptr(b / (1024 * 1024))
	}

	stdout, stderr, err := r.collectLogs(containerID)
	if err != nil {
		// Keep the exit code. Only surface the collection failure as stderr when
		// the command itself failed; on a successful run a log-collection note
		// would masquerade as the job's own stderr.
		res := RunResult{ExitCode: ptr(exitCode), MemoryUsedMB: memUsedMB, MemoryLimitMB: memLimitMB}
		if exitCode != 0 {
			res.Stderr = fmt.Sprintf("forge: failed to collect container logs: %v", err)
		}
		return res, nil
	}
	return RunResult{Stdout: stdout, Stderr: stderr, ExitCode: ptr(exitCode), MemoryUsedMB: memUsedMB, MemoryLimitMB: memLimitMB}, nil
}

// sampleMemoryBytes reads the container's current memory usage from the Docker
// stats endpoint, preferring the cgroup-v1 peak (max_usage) and falling back to
// current usage minus page cache on cgroup v2 (which has no max_usage). ok=false
// on any error or a zero reading. Decoded into a minimal local struct so it is
// independent of Docker client type churn.
func (r *DockerRuntime) sampleMemoryBytes(containerID string) (int64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := r.client.ContainerStats(ctx, containerID, false)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	var s struct {
		MemoryStats struct {
			Usage    int64            `json:"usage"`
			MaxUsage int64            `json:"max_usage"`
			Stats    map[string]int64 `json:"stats"`
		} `json:"memory_stats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return 0, false
	}
	used := s.MemoryStats.MaxUsage
	if used == 0 {
		used = s.MemoryStats.Usage
		if inactive := s.MemoryStats.Stats["inactive_file"]; inactive > 0 && inactive < used {
			used -= inactive
		}
	}
	if used <= 0 {
		return 0, false
	}
	return used, true
}

func (r *DockerRuntime) collectLogs(containerID string) (stdout, stderr string, err error) {
	ctx := context.Background()

	// Docker's ContainerLogs stream is multiplexed: each frame is prefixed with an
	// 8-byte header (stream type + length). stdcopy.StdCopy strips those headers.
	// Using plain io.Copy would embed the binary headers in the stored output.
	fetch := func(showStdout bool) (string, error) {
		logs, err := r.client.ContainerLogs(ctx, containerID, container.LogsOptions{
			ShowStdout: showStdout,
			ShowStderr: !showStdout,
		})
		if err != nil {
			return "", err
		}
		defer logs.Close()
		var buf bytes.Buffer
		if showStdout {
			stdcopy.StdCopy(&buf, io.Discard, io.LimitReader(logs, maxOutputBytes))
		} else {
			stdcopy.StdCopy(io.Discard, &buf, io.LimitReader(logs, maxOutputBytes))
		}
		return buf.String(), nil
	}

	stdout, err = fetch(true)
	if err != nil {
		return "", "", err
	}
	stderr, err = fetch(false)
	return stdout, stderr, err
}

// Cancel is a no-op for the Docker runtime: cancellation is driven by context
// cancellation inside Run, which stops the container via ContainerStop.
func (r *DockerRuntime) Cancel(_ context.Context, executionID string) error {
	// Cancellation is handled by context cancellation in Run().
	// The worker cancels the context, which unblocks ContainerWait and stops the container.
	return nil
}
