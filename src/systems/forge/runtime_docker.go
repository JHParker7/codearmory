package main

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
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
			NetworkMode:    "none",
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

	defer r.client.ContainerRemove(context.Background(), containerID, container.RemoveOptions{Force: true})

	if err := r.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return RunResult{}, fmt.Errorf("container start: %w", err)
	}

	statusCh, errCh := r.client.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)

	var exitCode int
	select {
	case <-ctx.Done():
		r.client.ContainerStop(context.Background(), containerID, container.StopOptions{}) //nolint
		return RunResult{}, ctx.Err()
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

	fetch := func(stdoutOnly bool) (string, error) {
		logs, err := r.client.ContainerLogs(ctx, containerID, container.LogsOptions{
			ShowStdout: stdoutOnly,
			ShowStderr: !stdoutOnly,
		})
		if err != nil {
			return "", err
		}
		defer logs.Close()
		var buf bytes.Buffer
		io.Copy(&buf, io.LimitReader(logs, maxOutputBytes))
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
