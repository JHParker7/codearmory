package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// ProxmoxRuntime runs each execution in a throwaway Proxmox VM: it clones a
// prepared template, sizes it to the runner class, boots it, runs the command via
// the qemu-guest-agent, captures the output, and destroys the VM. Unlike the
// docker/k8s runtimes it gives the job full root and a real Docker daemon — the
// isolation boundary is the VM, not a locked-down container. The cloned VM must
// sit on an operator-locked-down bridge/VLAN whose only egress is the egress
// proxy (mirrors the forge-exec network for Docker); that is trusted admin config.
type ProxmoxRuntime struct {
	client       *proxmoxClient
	templateVMID int
	storage      string
	bridge       string
	vlan         string
	fullClone    bool
	egressProxy  string
	bootDisk     string // PVE disk id to resize, e.g. "scsi0" / "virtio0"
}

// proxmox config/secret keys (jsonb on the RuntimeBackend).
const (
	pmKeyURL         = "url"
	pmKeyNode        = "node"
	pmKeyTemplate    = "template_vmid"
	pmKeyStorage     = "storage"
	pmKeyBridge      = "bridge"
	pmKeyVLAN        = "vlan"
	pmKeyBootDisk    = "boot_disk"    // disk id to resize to DiskGB; default scsi0
	pmKeyCloneMode   = "clone_mode"   // "linked" (default) | "full"
	pmKeyEgressProxy = "egress_proxy" // HTTP proxy URL injected into the job VM
	pmKeyTLSInsecure = "tls_insecure" // "true" to skip PVE cert verification
	pmKeyCAFile      = "ca_file"      // path to a PEM CA bundle for the PVE cert
	pmSecretToken    = "token"        // SecretRefs key → env var holding the API token
)

const (
	proxmoxAgentTimeout = 90 * time.Second // upper bound on waiting for the guest agent
	proxmoxVMNamePrefix = "forge-"
	proxmoxDefaultDisk  = "scsi0"
	proxmoxCloneRetries = 5 // re-allocate a VMID and retry when nextID races a sibling
)

// newProxmoxRuntime builds a ProxmoxRuntime from a backend's config + secrets.
// It does NOT sweep orphans here: the sweep destroys every forge-* VM and so is
// only safe before any worker is running (see sweepProxmoxOrphans, called once at
// startup). Building a runtime can happen mid-flight (a lazy first use, or a
// rebuild after an admin edits the backend), when other jobs' VMs are live.
func newProxmoxRuntime(b RuntimeBackend) (*ProxmoxRuntime, error) {
	cfg := b.Config
	for _, k := range requiredProxmoxConfigKeys {
		if strings.TrimSpace(cfg[k]) == "" {
			return nil, fmt.Errorf("proxmox backend %q: missing config key %q", b.Name, k)
		}
	}
	tmpl, err := strconv.Atoi(strings.TrimSpace(cfg[pmKeyTemplate]))
	if err != nil {
		return nil, fmt.Errorf("proxmox backend %q: %s must be an integer VMID: %w", b.Name, pmKeyTemplate, err)
	}

	tokenVar := b.SecretRefs[pmSecretToken]
	if tokenVar == "" {
		return nil, fmt.Errorf("proxmox backend %q: missing secret_ref %q", b.Name, pmSecretToken)
	}
	token := secret(tokenVar)
	if token == "" {
		return nil, fmt.Errorf("proxmox backend %q: secret %q is empty", b.Name, tokenVar)
	}

	httpClient, err := proxmoxHTTPClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("proxmox backend %q: %w", b.Name, err)
	}

	bootDisk := strings.TrimSpace(cfg[pmKeyBootDisk])
	if bootDisk == "" {
		bootDisk = proxmoxDefaultDisk
	}

	return &ProxmoxRuntime{
		client: &proxmoxClient{
			baseURL: strings.TrimRight(cfg[pmKeyURL], "/"),
			node:    cfg[pmKeyNode],
			token:   token,
			http:    httpClient,
		},
		templateVMID: tmpl,
		storage:      cfg[pmKeyStorage],
		bridge:       cfg[pmKeyBridge],
		vlan:         cfg[pmKeyVLAN],
		fullClone:    cfg[pmKeyCloneMode] == "full",
		egressProxy:  cfg[pmKeyEgressProxy],
		bootDisk:     bootDisk,
	}, nil
}

// proxmoxHTTPClient builds the HTTPS client for the PVE API. PVE hosts commonly
// present a self-signed cert; trust it by providing a CA file.
func proxmoxHTTPClient(cfg map[string]string) (*http.Client, error) {
	tlsCfg := &tls.Config{}
	if cfg[pmKeyTLSInsecure] == "true" {
		return nil, fmt.Errorf("%s=true is not supported; configure %s to trust the Proxmox CA certificate", pmKeyTLSInsecure, pmKeyCAFile)
	}
	if caFile := cfg[pmKeyCAFile]; caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", pmKeyCAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s contains no valid PEM certificates", pmKeyCAFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		Transport: otelhttp.NewTransport(&http.Transport{TLSClientConfig: tlsCfg}),
		Timeout:   30 * time.Second,
	}, nil
}

// Run executes one job: clone → size → start → wait-agent → exec → capture →
// destroy. The clone is always destroyed on return (success, failure, timeout, or
// cancellation) via a context.Background() defer, mirroring the k8s job delete.
func (r *ProxmoxRuntime) Run(ctx context.Context, exec Execution) (RunResult, error) {
	spec, err := runnerClassSpec(ctx, exec.RunnerClass)
	if err != nil {
		return RunResult{}, fmt.Errorf("runner class: %w", err)
	}

	name := proxmoxVMNamePrefix + exec.ExecutionID

	// /cluster/nextid only reads the lowest free id; it does not reserve it, so two
	// concurrent jobs can pick the same id and the loser's clone fails with "already
	// exists". Re-allocate and retry on that specific conflict.
	var newid int
	var upid string
	for attempt := 0; ; attempt++ {
		newid, err = r.client.nextID(ctx)
		if err != nil {
			return RunResult{}, fmt.Errorf("allocate vmid: %w", err)
		}
		upid, err = r.client.cloneVM(ctx, r.templateVMID, newid, name, r.fullClone, r.storage)
		if err == nil {
			break
		}
		if isVMIDConflict(err) && attempt < proxmoxCloneRetries {
			slog.WarnContext(ctx, "forge: proxmox vmid collision, retrying", "vmid", newid, "attempt", attempt+1)
			continue
		}
		return RunResult{}, fmt.Errorf("clone vm: %w", err)
	}
	// From here the VM exists and must always be torn down.
	defer r.destroy(newid)
	if err := r.client.waitTask(ctx, upid); err != nil {
		return RunResult{}, fmt.Errorf("clone task: %w", err)
	}

	if err := r.client.setVMConfig(ctx, newid, map[string]string{
		"cores":  strconv.FormatInt(vcpusFor(spec.CPUMillicores), 10),
		"memory": strconv.FormatInt(spec.MemoryMB, 10),
		"net0":   netConfig(r.bridge, r.vlan),
	}); err != nil {
		return RunResult{}, fmt.Errorf("configure vm: %w", err)
	}

	if spec.DiskGB > 0 {
		// Disk resize can only grow; a class smaller than the template is a config
		// mistake, not a job failure, so warn and carry on rather than abort.
		if err := r.client.resizeDisk(ctx, newid, r.bootDisk, fmt.Sprintf("%dG", spec.DiskGB)); err != nil {
			slog.WarnContext(ctx, "forge: proxmox resize disk failed", "vmid", newid, "disk", r.bootDisk, "error", err)
		}
	}

	upid, err = r.client.startVM(ctx, newid)
	if err != nil {
		return RunResult{}, fmt.Errorf("start vm: %w", err)
	}
	if err := r.client.waitTask(ctx, upid); err != nil {
		return RunResult{}, fmt.Errorf("start task: %w", err)
	}

	// Bound the rest of the run by the execution's timeout; a hit surfaces as
	// context.DeadlineExceeded so the worker records timed_out.
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(exec.TimeoutSecs)*time.Second)
	defer cancel()

	if err := r.waitAgent(timeoutCtx, newid); err != nil {
		return RunResult{}, err
	}

	pid, err := r.client.agentExec(timeoutCtx, newid, r.buildCommand(exec))
	if err != nil {
		return RunResult{}, fmt.Errorf("guest exec: %w", err)
	}

	return r.waitExec(timeoutCtx, newid, pid)
}

// waitAgent polls the guest agent until it answers or the context (bounded by the
// job timeout and an overall agent cap) ends.
func (r *ProxmoxRuntime) waitAgent(ctx context.Context, vmid int) error {
	deadline := time.Now().Add(proxmoxAgentTimeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := r.client.agentPing(ctx, vmid); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("guest agent did not become ready within %s", proxmoxAgentTimeout)
			}
		}
	}
}

// waitExec polls the guest command until it exits, then decodes and truncates its
// captured output.
func (r *ProxmoxRuntime) waitExec(ctx context.Context, vmid, pid int) (RunResult, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		st, err := r.client.agentExecStatus(ctx, vmid, pid)
		if err != nil {
			return RunResult{}, fmt.Errorf("guest exec-status: %w", err)
		}
		if bool(st.Exited) {
			return execStatusResult(st), nil
		}
		select {
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// execStatusResult turns a finished guest command into a RunResult: base64-decode
// each stream and apply the same 1 MB cap the other runtimes use. A command killed
// by a signal reports `signal` instead of `exitcode`; map that to 128+signal (the
// shell convention) so a SIGKILL/OOM is recorded as a failure rather than exit 0.
func execStatusResult(st *execStatus) RunResult {
	res := RunResult{
		Stdout: decodeTruncate(st.OutData),
		Stderr: decodeTruncate(st.ErrData),
	}
	switch {
	case st.ExitCode != nil:
		res.ExitCode = ptr(*st.ExitCode)
	case st.Signal != nil:
		res.ExitCode = ptr(128 + *st.Signal)
		if res.Stderr == "" {
			res.Stderr = fmt.Sprintf("forge: command terminated by signal %d", *st.Signal)
		}
	default:
		// exited with neither exitcode nor signal — abnormal; treat as a failure
		// rather than a silent success.
		res.ExitCode = ptr(1)
		if res.Stderr == "" {
			res.Stderr = "forge: command exited with no exit code reported by the guest agent"
		}
	}
	return res
}

// isVMIDConflict reports whether a clone error is PVE rejecting the target VMID
// because it is already taken (a lost nextID race), as opposed to a real failure.
func isVMIDConflict(err error) bool {
	var pe *proxmoxError
	if errors.As(err, &pe) {
		return strings.Contains(strings.ToLower(pe.Body), "already exists")
	}
	return false
}

// decodeTruncate base64-decodes a guest stream, falling back to the raw value if
// it is not base64, then caps it at maxOutputBytes.
func decodeTruncate(s string) string {
	out := s
	if dec, err := base64.StdEncoding.DecodeString(s); err == nil {
		out = string(dec)
	}
	if len(out) > maxOutputBytes {
		out = out[:maxOutputBytes]
	}
	return out
}

// buildCommand wraps the user's command in `sh -c` so per-execution env (and the
// egress-proxy vars) can be exported — the guest agent's exec has no env field.
// Proxy vars are only added when the user did not set them, matching the docker
// runtime's precedence.
func (r *ProxmoxRuntime) buildCommand(exec Execution) []string {
	env := map[string]string{}
	maps.Copy(env, exec.Env)
	if r.egressProxy != "" {
		for _, pair := range proxyEnvPairs(r.egressProxy) {
			if _, ok := env[pair[0]]; !ok {
				env[pair[0]] = pair[1]
			}
		}
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic output (and deterministic tests)

	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString("export ")
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(shellQuote(env[k]))
		sb.WriteByte('\n')
	}
	sb.WriteString("exec")
	for _, arg := range exec.Command {
		sb.WriteByte(' ')
		sb.WriteString(shellQuote(arg))
	}
	return []string{"/bin/sh", "-c", sb.String()}
}

// shellQuote single-quotes a value for safe embedding in an sh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// netConfig builds a PVE net0 string: a virtio NIC on the given bridge, optionally
// tagged onto a VLAN.
func netConfig(bridge, vlan string) string {
	net := "virtio,bridge=" + bridge
	if vlan != "" {
		net += ",tag=" + vlan
	}
	return net
}

// vcpusFor maps millicores to whole vCPUs (ceil), at least 1.
func vcpusFor(millicores int64) int64 {
	v := (millicores + 999) / 1000
	if v < 1 {
		return 1
	}
	return v
}

// destroy force-stops and purges a VM. Best-effort and always on a fresh context
// so teardown still runs when the job context is already cancelled or timed out.
func (r *ProxmoxRuntime) destroy(vmid int) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if upid, err := r.client.stopVM(ctx, vmid); err != nil {
		slog.Warn("forge: proxmox stop vm failed", "vmid", vmid, "error", err)
	} else if err := r.client.waitTask(ctx, upid); err != nil {
		slog.Warn("forge: proxmox stop task failed", "vmid", vmid, "error", err)
	}
	if upid, err := r.client.deleteVM(ctx, vmid); err != nil {
		slog.Warn("forge: proxmox delete vm failed", "vmid", vmid, "error", err)
	} else if err := r.client.waitTask(ctx, upid); err != nil {
		slog.Warn("forge: proxmox delete task failed", "vmid", vmid, "error", err)
	}
}

// Cancel tears down the VM for an execution. Run's own defer already destroys the
// VM when its context is cancelled; this is the explicit belt-and-suspenders path
// R4 calls for, finding the VM by its forge-<execID> name. The list+stop+destroy
// runs in the background so the caller — an HTTP cancel handler — isn't blocked on
// PVE tasks (which can take longer than the server write timeout). Destroying an
// already-gone VM is a harmless no-op. The passed ctx is the request's; it may be
// cancelled as soon as the handler returns, so the goroutine uses its own.
func (r *ProxmoxRuntime) Cancel(_ context.Context, executionID string) error {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		vms, err := r.client.listVMs(ctx)
		if err != nil {
			slog.Warn("forge: proxmox cancel: list vms failed", "execution_id", executionID, "error", err)
			return
		}
		name := proxmoxVMNamePrefix + executionID
		for _, vm := range vms {
			if vm.Name == name {
				r.destroy(vm.VMID)
				return
			}
		}
	}()
	return nil
}

// sweepOrphans destroys every forge-* VM (except the template). Proxmox has no
// TTL equivalent to the k8s job's TTLSecondsAfterFinished, so forge reaps crashed
// jobs' VMs this way. It is destructive and cannot tell a live job's VM from a
// crashed one, so it MUST only run when no worker is processing jobs — i.e. once
// at startup before the worker pool starts (see sweepProxmoxOrphans). Best-effort.
// (In a multi-replica deployment each replica would need its own VM-name
// namespace; v1 assumes a single forge writer per Proxmox node.)
func (r *ProxmoxRuntime) sweepOrphans(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	vms, err := r.client.listVMs(ctx)
	if err != nil {
		slog.WarnContext(ctx, "forge: proxmox orphan sweep: list vms failed", "error", err)
		return
	}
	for _, vm := range vms {
		if strings.HasPrefix(vm.Name, proxmoxVMNamePrefix) && vm.VMID != r.templateVMID {
			slog.InfoContext(ctx, "forge: proxmox reaping orphaned job vm", "vmid", vm.VMID, "name", vm.Name)
			r.destroy(vm.VMID)
		}
	}
}

// sweepProxmoxOrphans reaps crashed-job VMs for every enabled proxmox backend,
// exactly once at startup and synchronously BEFORE the worker pool starts — the
// only window in which the blanket forge-* sweep cannot hit a live job's VM. A
// failure (or a misconfigured backend) is logged and skipped, never fatal.
func sweepProxmoxOrphans(ctx context.Context, reg *runtimeRegistry) {
	rows, err := (RuntimeBackend{}).List(ctx, 0, 0)
	if err != nil {
		slog.WarnContext(ctx, "forge: proxmox startup sweep: list backends failed", "error", err)
		return
	}
	for _, row := range rows {
		b := row.(RuntimeBackend)
		if b.Type != "proxmox" || !b.Enabled {
			continue
		}
		rt, err := reg.Get(ctx, b.Name)
		if err != nil {
			slog.WarnContext(ctx, "forge: proxmox startup sweep: build backend failed", "backend", b.Name, "error", err)
			continue
		}
		if pr, ok := rt.(*ProxmoxRuntime); ok {
			pr.sweepOrphans(ctx)
		}
	}
}

// ensure ProxmoxRuntime satisfies Runtime.
var _ Runtime = (*ProxmoxRuntime)(nil)
