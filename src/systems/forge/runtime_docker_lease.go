package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
)

// The Docker half of LeaseRuntime. It carries the same sandbox settings Run
// applies — dropped capabilities, no-new-privileges, read-only rootfs, the
// configured network mode, the runner class's limits — because a lease must not
// be a way to obtain a laxer sandbox than a one-shot execution gets.

// leaseWorkspaceVolume is the per-lease docker volume backing its working
// directory when no shared volume claims it.
//
// A tmpfs would be the easier choice and the wrong one: the working directory
// holds a source tree and build output, and tmpfs charges those to the container's
// memory limit — so a lease would mysteriously OOM partway through a build while
// reporting plenty of headroom. A volume keeps the runner class's memory figure
// meaning what it says. It is created with the lease and removed with it.
func leaseWorkspaceVolume(leaseID string) string { return "forge-lease-ws-" + leaseID }

// leaseContainerLabel marks a container as backing a lease, so the orphan sweep can
// find sandboxes whose lease row is gone.
const leaseContainerLabel = "forge.lease-id"

// StartLease creates the lease's container and blocks until it reports ready.
func (r *DockerRuntime) StartLease(ctx context.Context, lease Lease) error {
	spec, err := runnerClassSpec(ctx, lease.RunnerClass)
	if err != nil {
		return fmt.Errorf("runner class: %w", err)
	}

	if _, err := r.client.ImageInspect(ctx, lease.Image); err != nil {
		if !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("image inspect: %w", err)
		}
		reader, pullErr := r.client.ImagePull(ctx, lease.Image, image.PullOptions{})
		if pullErr != nil {
			return fmt.Errorf("image pull: %w", pullErr)
		}
		io.Copy(io.Discard, reader) //nolint:errcheck — progress stream only
		reader.Close()
	}

	envList := make([]string, 0, len(lease.Env))
	for k, v := range lease.Env {
		envList = append(envList, k+"="+v)
	}
	if r.egressProxy != "" {
		for _, pair := range proxyEnvPairs(r.egressProxy) {
			if _, exists := lease.Env[pair[0]]; !exists {
				envList = append(envList, pair[0]+"="+pair[1])
			}
		}
	}

	var volMounts []mount.Mount
	workingDir := ""
	for _, rm := range resolveVolumeMounts(lease.Volumes) {
		volMounts = append(volMounts, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   rm.resourceName,
			Target:   rm.mountPath,
			ReadOnly: rm.readOnly,
		})
		if rm.workdir {
			workingDir = rm.mountPath
		}
	}
	if workingDir == "" {
		workingDir = leaseWorkDir
		wsName := leaseWorkspaceVolume(lease.LeaseID)
		if _, err := r.client.VolumeCreate(ctx, volume.CreateOptions{Name: wsName, Driver: "local"}); err != nil {
			return fmt.Errorf("create lease workspace volume: %w", err)
		}
		volMounts = append(volMounts, mount.Mount{Type: mount.TypeVolume, Source: wsName, Target: leaseWorkDir})
	}

	pidsLimit := spec.PidsLimit
	resp, err := r.client.ContainerCreate(ctx,
		&container.Config{
			Image:      lease.Image,
			Cmd:        lease.startCommand(),
			Env:        envList,
			WorkingDir: workingDir,
			Labels:     map[string]string{leaseContainerLabel: lease.LeaseID},
		},
		&container.HostConfig{
			NetworkMode:    container.NetworkMode(r.allowedNet),
			ReadonlyRootfs: true,
			Tmpfs:          map[string]string{"/tmp": fmt.Sprintf("size=%dm", spec.TmpfsMB)},
			Mounts:         volMounts,
			Resources: container.Resources{
				Memory:    spec.MemoryMB * bytesPerMiB,
				CPUQuota:  spec.CPUMillicores * 100,
				PidsLimit: &pidsLimit,
			},
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
		},
		nil, nil, lease.resourceName(),
	)
	if err != nil {
		r.removeLeaseWorkspace(context.Background(), lease)
		return fmt.Errorf("create lease container: %w", err)
	}

	if err := r.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		r.stopLeaseByName(context.Background(), lease.resourceName())
		r.removeLeaseWorkspace(context.Background(), lease)
		return fmt.Errorf("start lease container: %w", err)
	}

	if err := r.waitLeaseReady(ctx, lease); err != nil {
		// Same reasoning as the kubernetes path: a sandbox that never became usable
		// is torn down now rather than left holding memory the caller cannot see.
		r.stopLeaseByName(context.Background(), lease.resourceName())
		r.removeLeaseWorkspace(context.Background(), lease)
		return err
	}
	return nil
}

// waitLeaseReady polls the container's logs for the ready marker.
func (r *DockerRuntime) waitLeaseReady(ctx context.Context, lease Lease) error {
	name := lease.resourceName()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := r.client.ContainerInspect(ctx, name)
		if err != nil {
			return fmt.Errorf("inspect lease container: %w", err)
		}
		logs := r.leaseLogs(ctx, name)
		if strings.Contains(logs, leaseReadyMarker) {
			slog.InfoContext(ctx, "lease sandbox ready", "lease_id", lease.LeaseID, "container", name)
			return nil
		}
		if info.State != nil && !info.State.Running {
			// The start command blocks on `sleep` forever, so a stopped container means
			// the checkout failed under `set -e` — or the image has no /bin/sh.
			return fmt.Errorf("lease sandbox exited while starting: %s", firstNonEmptyLine(logs, info.State.Error))
		}
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("lease sandbox did not become ready within %ds", leaseStartTimeoutSecs)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *DockerRuntime) leaseLogs(ctx context.Context, name string) string {
	rc, err := r.client.ContainerLogs(ctx, name, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return ""
	}
	defer rc.Close()
	var out, errOut bytes.Buffer
	stdcopy.StdCopy(&out, &errOut, rc) //nolint:errcheck — best-effort diagnostic
	return out.String() + errOut.String()
}

// Exec runs one command inside a started lease via docker exec — a new process in
// the existing container, so no image pull, no container create, no start.
func (r *DockerRuntime) Exec(ctx context.Context, lease Lease, exec Execution) (RunResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(exec.TimeoutSecs)*time.Second)
	defer cancel()

	created, err := r.client.ContainerExecCreate(runCtx, lease.resourceName(), container.ExecOptions{
		Cmd:          exec.Command,
		WorkingDir:   lease.workDir(),
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("create lease exec: %w", err)
	}

	attached, err := r.client.ContainerExecAttach(runCtx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return RunResult{}, fmt.Errorf("attach lease exec: %w", err)
	}
	defer attached.Close()

	var stdout, stderr bytes.Buffer
	copyErr := make(chan error, 1)
	go func() {
		_, cerr := stdcopy.StdCopy(&stdout, &stderr, attached.Reader)
		copyErr <- cerr
	}()

	select {
	case <-runCtx.Done():
		result := RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
		if runCtx.Err() == context.DeadlineExceeded {
			return result, fmt.Errorf("command timed out after %ds", exec.TimeoutSecs)
		}
		return result, runCtx.Err()
	case cerr := <-copyErr:
		result := RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
		if cerr != nil && cerr != io.EOF {
			return result, fmt.Errorf("read lease exec output: %w", cerr)
		}
		// Inspect resolves the exit code. Use ctx rather than runCtx: the command has
		// already finished, and letting an expired command timeout deny us its exit
		// code would turn a completed failure into an unexplained platform error.
		insp, ierr := r.client.ContainerExecInspect(ctx, created.ID)
		if ierr != nil {
			return result, fmt.Errorf("inspect lease exec: %w", ierr)
		}
		code := insp.ExitCode
		result.ExitCode = &code
		return result, nil
	}
}

// StopLease removes the lease's container and its workspace volume. Idempotent.
func (r *DockerRuntime) StopLease(ctx context.Context, lease Lease) error {
	if err := r.stopLeaseByName(ctx, lease.resourceName()); err != nil {
		return err
	}
	r.removeLeaseWorkspace(ctx, lease)
	return nil
}

func (r *DockerRuntime) stopLeaseByName(ctx context.Context, name string) error {
	err := r.client.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove lease container %s: %w", name, err)
	}
	return nil
}

// removeLeaseWorkspace deletes the per-lease workspace volume. Failure is logged
// rather than returned: the container is already gone by this point, and refusing
// to finish a teardown over a leftover volume would leave the lease row active and
// the caller's quota consumed.
func (r *DockerRuntime) removeLeaseWorkspace(ctx context.Context, lease Lease) {
	name := leaseWorkspaceVolume(lease.LeaseID)
	if err := r.client.VolumeRemove(ctx, name, true); err != nil && !cerrdefs.IsNotFound(err) {
		slog.WarnContext(ctx, "could not remove lease workspace volume", "lease_id", lease.LeaseID, "volume", name, "error", err)
	}
}

// OrphanedLeasePods lists lease containers whose lease id is not in known, matching
// the kubernetes runtime's contract so the reaper can treat both alike.
func (r *DockerRuntime) OrphanedLeasePods(ctx context.Context, known map[string]bool) ([]string, error) {
	list, err := r.client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var orphans []string
	for _, c := range list {
		if id := c.Labels[leaseContainerLabel]; id != "" && !known[id] {
			orphans = append(orphans, strings.TrimPrefix(firstOr(c.Names, ""), "/"))
		}
	}
	return orphans, nil
}

// DeleteLeasePodByName removes an orphaned lease container by name.
func (r *DockerRuntime) DeleteLeasePodByName(ctx context.Context, name string) error {
	if err := r.stopLeaseByName(ctx, name); err != nil {
		return err
	}
	// The workspace volume is named from the lease id, which is recoverable from the
	// container name — so an orphan's volume goes with it rather than accumulating.
	if id := strings.TrimPrefix(name, "forge-lease-"); id != "" && id != name {
		if err := r.client.VolumeRemove(ctx, leaseWorkspaceVolume(id), true); err != nil && !cerrdefs.IsNotFound(err) {
			slog.WarnContext(ctx, "could not remove orphaned lease workspace volume", "lease_id", id, "error", err)
		}
	}
	return nil
}

func firstOr(xs []string, def string) string {
	if len(xs) == 0 {
		return def
	}
	return xs[0]
}
