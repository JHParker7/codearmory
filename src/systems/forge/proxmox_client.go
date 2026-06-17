package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// proxmoxClient is a thin Proxmox VE REST client. It speaks the /api2/json API
// with PVE API-token auth and is modelled on the containers/gitea service
// clients: a base URL, a 30s-timeout http.Client, a 512-byte error-body cap, and
// an optional custom CA. Async PVE operations return a UPID (task id) which the
// caller resolves with waitTask.
type proxmoxClient struct {
	baseURL string // e.g. https://pve:8006/api2/json
	node    string
	token   string // full token: USER@REALM!TOKENID=SECRET
	http    *http.Client
}

type proxmoxError struct {
	Status int
	Body   string
}

func (e *proxmoxError) Error() string {
	return fmt.Sprintf("proxmox %d: %s", e.Status, e.Body)
}

// newRequest builds a request with the PVE API-token auth header. body, when
// non-nil, is form-encoded (the encoding the PVE API expects for parameters).
func (c *proxmoxClient) newRequest(ctx context.Context, method, path string, body url.Values) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(body.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return req, nil
}

// do executes the request and decodes the PVE response envelope ({"data": …})
// into out, which may be nil to discard the body.
func (c *proxmoxClient) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &proxmoxError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

func (c *proxmoxClient) qemuPath(vmid int, suffix string) string {
	return fmt.Sprintf("/nodes/%s/qemu/%d%s", c.node, vmid, suffix)
}

// nextID asks the cluster for a free VM ID. PVE returns it as a quoted string.
func (c *proxmoxClient) nextID(ctx context.Context) (int, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/cluster/nextid", nil)
	if err != nil {
		return 0, err
	}
	var raw json.RawMessage
	if err := c.do(req, &raw); err != nil {
		return 0, err
	}
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse nextid %q: %w", s, err)
	}
	return id, nil
}

// cloneVM clones the template VM into newid. full=false makes a fast linked
// clone; full=true a standalone copy. Returns the clone task's UPID.
func (c *proxmoxClient) cloneVM(ctx context.Context, template, newid int, name string, full bool, storage string) (string, error) {
	form := url.Values{}
	form.Set("newid", strconv.Itoa(newid))
	form.Set("name", name)
	if full {
		form.Set("full", "1")
	} else {
		form.Set("full", "0")
	}
	if storage != "" {
		form.Set("storage", storage)
	}
	req, err := c.newRequest(ctx, http.MethodPost, c.qemuPath(template, "/clone"), form)
	if err != nil {
		return "", err
	}
	var upid string
	if err := c.do(req, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// setVMConfig applies config keys (cores, memory, net0, …) to a stopped VM. This
// is synchronous in PVE (no task).
func (c *proxmoxClient) setVMConfig(ctx context.Context, vmid int, cfg map[string]string) error {
	form := url.Values{}
	for k, v := range cfg {
		form.Set(k, v)
	}
	req, err := c.newRequest(ctx, http.MethodPost, c.qemuPath(vmid, "/config"), form)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// resizeDisk grows a VM disk (e.g. disk="scsi0", size="10G"). Synchronous.
func (c *proxmoxClient) resizeDisk(ctx context.Context, vmid int, disk, size string) error {
	form := url.Values{}
	form.Set("disk", disk)
	form.Set("size", size)
	req, err := c.newRequest(ctx, http.MethodPut, c.qemuPath(vmid, "/resize"), form)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

func (c *proxmoxClient) startVM(ctx context.Context, vmid int) (string, error) {
	return c.statusAction(ctx, vmid, "start", nil)
}

// stopVM force-stops the VM (the equivalent of pulling the plug — fine for a
// throwaway job VM about to be destroyed).
func (c *proxmoxClient) stopVM(ctx context.Context, vmid int) (string, error) {
	return c.statusAction(ctx, vmid, "stop", nil)
}

func (c *proxmoxClient) statusAction(ctx context.Context, vmid int, action string, form url.Values) (string, error) {
	req, err := c.newRequest(ctx, http.MethodPost, c.qemuPath(vmid, "/status/"+action), form)
	if err != nil {
		return "", err
	}
	var upid string
	if err := c.do(req, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// deleteVM destroys the VM, purging it from any backup/replication jobs.
func (c *proxmoxClient) deleteVM(ctx context.Context, vmid int) (string, error) {
	req, err := c.newRequest(ctx, http.MethodDelete, c.qemuPath(vmid, "?purge=1"), nil)
	if err != nil {
		return "", err
	}
	var upid string
	if err := c.do(req, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// agentPing returns nil once the qemu-guest-agent inside the VM is responsive.
func (c *proxmoxClient) agentPing(ctx context.Context, vmid int) error {
	req, err := c.newRequest(ctx, http.MethodPost, c.qemuPath(vmid, "/agent/ping"), url.Values{})
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// agentExec starts a command in the guest via the guest agent and returns its
// guest PID. command is the program plus arguments.
func (c *proxmoxClient) agentExec(ctx context.Context, vmid int, command []string) (int, error) {
	form := url.Values{}
	for _, part := range command {
		form.Add("command", part)
	}
	req, err := c.newRequest(ctx, http.MethodPost, c.qemuPath(vmid, "/agent/exec"), form)
	if err != nil {
		return 0, err
	}
	var out struct {
		PID int `json:"pid"`
	}
	if err := c.do(req, &out); err != nil {
		return 0, err
	}
	return out.PID, nil
}

// pveBool decodes the guest agent's "exited" flag, which different PVE versions
// render as either a JSON boolean or 0/1.
type pveBool bool

func (b *pveBool) UnmarshalJSON(data []byte) error {
	s := strings.Trim(strings.TrimSpace(string(data)), `"`)
	*b = pveBool(s == "true" || s == "1")
	return nil
}

// execStatus is the guest agent's view of a previously-started command. out-data
// and err-data are base64-encoded by the guest agent. A signal-terminated command
// reports `signal` in place of `exitcode`.
type execStatus struct {
	Exited   pveBool `json:"exited"`
	ExitCode *int    `json:"exitcode"`
	Signal   *int    `json:"signal"`
	OutData  string  `json:"out-data"`
	ErrData  string  `json:"err-data"`
	OutTrunc bool    `json:"out-truncated"`
	ErrTrunc bool    `json:"err-truncated"`
}

func (c *proxmoxClient) agentExecStatus(ctx context.Context, vmid, pid int) (*execStatus, error) {
	req, err := c.newRequest(ctx, http.MethodGet, c.qemuPath(vmid, "/agent/exec-status?pid="+strconv.Itoa(pid)), nil)
	if err != nil {
		return nil, err
	}
	var st execStatus
	if err := c.do(req, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// pveVM is one entry from the node's VM list, used by the orphan sweep.
type pveVM struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (c *proxmoxClient) listVMs(ctx context.Context) ([]pveVM, error) {
	req, err := c.newRequest(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu", c.node), nil)
	if err != nil {
		return nil, err
	}
	var vms []pveVM
	if err := c.do(req, &vms); err != nil {
		return nil, err
	}
	return vms, nil
}

// waitTask polls a UPID's task status until it stops, returning an error if the
// task ended with a non-OK exit status. It respects ctx for cancellation/timeout.
func (c *proxmoxClient) waitTask(ctx context.Context, upid string) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	path := fmt.Sprintf("/nodes/%s/tasks/%s/status", c.node, url.PathEscape(upid))
	for {
		req, err := c.newRequest(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := c.do(req, &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			// PVE sets exitstatus to "OK" on success and an error string otherwise.
			// Treat anything that is not exactly "OK" (including an empty/absent
			// status on an abnormally-stopped task) as a failure rather than
			// proceeding on, say, a half-completed clone.
			if st.ExitStatus != "OK" {
				return fmt.Errorf("task %s failed: %s", upid, st.ExitStatus)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
