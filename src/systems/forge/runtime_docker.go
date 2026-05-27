package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type DockerRuntime struct {
	client     *client.Client
	memLimit   int64
	cpuQuota   int64
	pidsLimit  int64
	allowedNet string
}

func newDockerRuntime() (*DockerRuntime, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &DockerRuntime{
		client:    c,
		memLimit:  256 * 1024 * 1024, // 256 MB
		cpuQuota:  50000,              // 50% of one core (100000 = full core)
		pidsLimit: 64,
	}, nil
}

func (r *DockerRuntime) Run(ctx context.Context, exec Execution) (RunResult, error) {
	envList := make([]string, 0, len(exec.Env))
	for k, v := range exec.Env {
		envList = append(envList, k+"="+v)
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
			// NetworkMode=none drops all network interfaces so the container cannot
			// reach the internet, the host, or other containers.
			NetworkMode: "none",
			// ReadonlyRootfs prevents writes to the image layers. /tmp is a writable
			// tmpfs mount so programs that need a scratch directory still work.
			ReadonlyRootfs: true,
			Tmpfs:          map[string]string{"/tmp": "size=64m"},
			Resources: container.Resources{
				Memory:    r.memLimit,
				CPUQuota:  r.cpuQuota,
				PidsLimit: &r.pidsLimit,
			},
			CapDrop:    []string{"ALL"},
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

	stdout, stderr, err := r.collectLogs(containerID)
	if err != nil {
		return RunResult{ExitCode: exitCode}, nil
	}
	return RunResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}, nil
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

func (r *DockerRuntime) Cancel(_ context.Context, executionID string) error {
	// Cancellation is handled by context cancellation in Run().
	// The worker cancels the context, which unblocks ContainerWait and stops the container.
	return nil
}
