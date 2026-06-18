package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakePVE is an httptest-backed stand-in for the Proxmox VE API. It serves the
// endpoints a full Run drives and records what it was asked to do so tests can
// assert resource mapping and teardown without a real hypervisor.
type fakePVE struct {
	server *httptest.Server

	mu           sync.Mutex
	nextID       int
	configParams url.Values
	resizeParams url.Values
	execCommand  []string
	deleted      []int
	stopped      []int
	listVMs      []pveVM
	execOut      string // raw stdout the guest "produced" (server base64-encodes it)
	execErr      string
	execExit     int
	pingFails    int // number of initial pings that 500 before the agent is "ready"
	pingCount    int
	cloneFailN   int // first N clones return a "VM already exists" conflict
	cloneCalls   int
}

func newFakePVE(t *testing.T) *fakePVE {
	t.Helper()
	f := &fakePVE{nextID: 123, execExit: 0}
	mux := http.NewServeMux()

	writeData := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": v}) //nolint:errcheck
	}

	mux.HandleFunc("GET /api2/json/cluster/nextid", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		id := f.nextID
		f.mu.Unlock()
		writeData(w, strconv.Itoa(id))
	})
	mux.HandleFunc("POST /api2/json/nodes/{node}/qemu/{vmid}/clone", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.cloneCalls++
		conflict := f.cloneCalls <= f.cloneFailN
		f.mu.Unlock()
		if conflict {
			http.Error(w, `{"errors":{"vmid":"VM 123 already exists"}}`, http.StatusInternalServerError)
			return
		}
		writeData(w, "UPID:clone")
	})
	mux.HandleFunc("GET /api2/json/nodes/{node}/tasks/{upid}/status", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
	})
	mux.HandleFunc("POST /api2/json/nodes/{node}/qemu/{vmid}/config", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm() //nolint:errcheck
		f.mu.Lock()
		f.configParams = r.PostForm
		f.mu.Unlock()
		writeData(w, nil)
	})
	mux.HandleFunc("PUT /api2/json/nodes/{node}/qemu/{vmid}/resize", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm() //nolint:errcheck
		f.mu.Lock()
		f.resizeParams = r.PostForm
		f.mu.Unlock()
		writeData(w, nil)
	})
	mux.HandleFunc("POST /api2/json/nodes/{node}/qemu/{vmid}/status/start", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, "UPID:start")
	})
	mux.HandleFunc("POST /api2/json/nodes/{node}/qemu/{vmid}/status/stop", func(w http.ResponseWriter, r *http.Request) {
		vmid, _ := strconv.Atoi(r.PathValue("vmid"))
		f.mu.Lock()
		f.stopped = append(f.stopped, vmid)
		f.mu.Unlock()
		writeData(w, "UPID:stop")
	})
	mux.HandleFunc("DELETE /api2/json/nodes/{node}/qemu/{vmid}", func(w http.ResponseWriter, r *http.Request) {
		vmid, _ := strconv.Atoi(r.PathValue("vmid"))
		f.mu.Lock()
		f.deleted = append(f.deleted, vmid)
		f.mu.Unlock()
		writeData(w, "UPID:delete")
	})
	mux.HandleFunc("POST /api2/json/nodes/{node}/qemu/{vmid}/agent/ping", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.pingCount++
		fail := f.pingCount <= f.pingFails
		f.mu.Unlock()
		if fail {
			http.Error(w, `{"data":null}`, http.StatusInternalServerError)
			return
		}
		writeData(w, nil)
	})
	mux.HandleFunc("POST /api2/json/nodes/{node}/qemu/{vmid}/agent/exec", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm() //nolint:errcheck
		f.mu.Lock()
		f.execCommand = r.Form["command"]
		f.mu.Unlock()
		writeData(w, map[string]any{"pid": 42})
	})
	mux.HandleFunc("GET /api2/json/nodes/{node}/qemu/{vmid}/agent/exec-status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		out := base64.StdEncoding.EncodeToString([]byte(f.execOut))
		errd := base64.StdEncoding.EncodeToString([]byte(f.execErr))
		exit := f.execExit
		f.mu.Unlock()
		writeData(w, map[string]any{"exited": 1, "exitcode": exit, "out-data": out, "err-data": errd})
	})
	mux.HandleFunc("GET /api2/json/nodes/{node}/qemu", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		vms := f.listVMs
		f.mu.Unlock()
		writeData(w, vms)
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakePVE) runtime() *ProxmoxRuntime {
	return &ProxmoxRuntime{
		client: &proxmoxClient{
			baseURL: f.server.URL + "/api2/json",
			node:    "pve",
			token:   "user@pve!t=secret",
			http:    f.server.Client(),
		},
		templateVMID: 9000,
		storage:      "local-lvm",
		bridge:       "vmbr1",
		egressProxy:  "http://egress-proxy:3128",
		bootDisk:     "scsi0",
	}
}

// deletedVMIDs returns a race-safe snapshot of the VMIDs the fake was asked to
// delete — needed when teardown runs in a background goroutine (Cancel).
func (f *fakePVE) deletedVMIDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.deleted...)
}

// waitForDeletes blocks until at least n deletes have been recorded or the
// deadline passes.
func (f *fakePVE) waitForDeletes(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.deletedVMIDs()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestProxmoxRun_FullLifecycle drives clone→size→start→agent→exec→capture→destroy
// against the fake API and asserts the resulting RunResult plus that the VM was
// sized from the runner class and destroyed.
func TestProxmoxRun_FullLifecycle(t *testing.T) {
	requireForgeDB(t)

	className := "pxtest-" + uuid.New().String()
	rc := RunnerClass{Name: className, MemoryMB: 2048, CPUMillicores: 1500, PidsLimit: 64, TmpfsMB: 64, DiskGB: 16, Backend: "default", Enabled: true}
	if err := rc.Add(context.Background()); err != nil {
		t.Fatalf("seed runner class: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM runner_classes WHERE name = ?`, className) }) //nolint:errcheck

	f := newFakePVE(t)
	f.execOut = "hello from the vm\n"
	f.execExit = 0
	rt := f.runtime()

	execID := uuid.New().String()
	exec := Execution{
		ExecutionID: execID, RunnerClass: className, TimeoutSecs: 30,
		Command: []string{"echo", "hi"}, Env: map[string]string{"FOO": "bar"},
	}
	res, err := rt.Run(context.Background(), exec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "hello from the vm\n" {
		t.Errorf("stdout = %q, want decoded guest output", res.Stdout)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}

	// Resource mapping: 1500 millicores → 2 vCPU (ceil), 2048 MiB RAM, 16G disk.
	if got := f.configParams.Get("cores"); got != "2" {
		t.Errorf("cores = %q, want 2 (ceil 1500m)", got)
	}
	if got := f.configParams.Get("memory"); got != "2048" {
		t.Errorf("memory = %q, want 2048", got)
	}
	if got := f.configParams.Get("net0"); got != "virtio,bridge=vmbr1" {
		t.Errorf("net0 = %q, want virtio,bridge=vmbr1", got)
	}
	if got := f.resizeParams.Get("size"); got != "16G" {
		t.Errorf("resize size = %q, want 16G", got)
	}

	// VM was named forge-<execID> and destroyed (stopped + purged).
	if len(f.deleted) != 1 || f.deleted[0] != 123 {
		t.Errorf("deleted = %v, want the cloned vmid 123 destroyed exactly once", f.deleted)
	}
	if len(f.stopped) != 1 {
		t.Errorf("stopped = %v, want the cloned vm stopped once before destroy", f.stopped)
	}
}

// TestProxmoxRun_DestroysOnExecFailure ensures the VM is torn down even when the
// guest command exits non-zero.
func TestProxmoxRun_DestroysOnExecFailure(t *testing.T) {
	requireForgeDB(t)

	// Insert with disk_gb=0 via raw SQL: a struct Add would have GORM omit the zero
	// value and the column default (10) would win, defeating the no-resize assertion.
	className := "pxtest-" + uuid.New().String()
	if err := connect().Exec(
		`INSERT INTO runner_classes (name, memory_mb, cpu_millicores, pids_limit, tmpfs_mb, disk_gb, backend, enabled)
		 VALUES (?, 512, 500, 64, 64, 0, 'default', true)`, className).Error; err != nil {
		t.Fatalf("seed runner class: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM runner_classes WHERE name = ?`, className) }) //nolint:errcheck

	f := newFakePVE(t)
	f.execOut = ""
	f.execErr = "boom"
	f.execExit = 7
	rt := f.runtime()

	exec := Execution{ExecutionID: uuid.New().String(), RunnerClass: className, TimeoutSecs: 30, Command: []string{"false"}}
	res, err := rt.Run(context.Background(), exec)
	if err != nil {
		t.Fatalf("Run returned error for a non-zero exit (should be a normal result): %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 7 {
		t.Errorf("exit code = %v, want 7", res.ExitCode)
	}
	if res.Stderr != "boom" {
		t.Errorf("stderr = %q, want decoded guest stderr", res.Stderr)
	}
	if len(f.deleted) != 1 {
		t.Errorf("deleted = %v, want the vm destroyed despite the failed command", f.deleted)
	}
	// DiskGB=0 → no resize attempt.
	if f.resizeParams != nil {
		t.Errorf("resize was called for DiskGB=0: %v", f.resizeParams)
	}
}

// TestProxmoxRun_RetriesOnVMIDConflict verifies a lost nextID race (clone fails
// with "already exists") is retried with a fresh VMID rather than failing the job.
func TestProxmoxRun_RetriesOnVMIDConflict(t *testing.T) {
	requireForgeDB(t)

	className := "pxtest-" + uuid.New().String()
	rc := RunnerClass{Name: className, MemoryMB: 512, CPUMillicores: 500, PidsLimit: 64, TmpfsMB: 64, DiskGB: 0, Backend: "default", Enabled: true}
	if err := rc.Add(context.Background()); err != nil {
		t.Fatalf("seed runner class: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM runner_classes WHERE name = ?`, className) }) //nolint:errcheck

	f := newFakePVE(t)
	f.cloneFailN = 2 // first two clone attempts collide, third succeeds
	f.execOut = "ok"
	rt := f.runtime()

	exec := Execution{ExecutionID: uuid.New().String(), RunnerClass: className, TimeoutSecs: 30, Command: []string{"true"}}
	res, err := rt.Run(context.Background(), exec)
	if err != nil {
		t.Fatalf("Run should have retried past the VMID conflicts: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0 after a successful retry", res.ExitCode)
	}
	if f.cloneCalls != 3 {
		t.Errorf("cloneCalls = %d, want 3 (two conflicts + one success)", f.cloneCalls)
	}
}

// TestProxmoxRun_VMIDConflictGivesUp verifies a persistent conflict eventually
// fails the job rather than looping forever.
func TestProxmoxRun_VMIDConflictGivesUp(t *testing.T) {
	requireForgeDB(t)

	className := "pxtest-" + uuid.New().String()
	rc := RunnerClass{Name: className, MemoryMB: 512, CPUMillicores: 500, PidsLimit: 64, TmpfsMB: 64, DiskGB: 0, Backend: "default", Enabled: true}
	if err := rc.Add(context.Background()); err != nil {
		t.Fatalf("seed runner class: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM runner_classes WHERE name = ?`, className) }) //nolint:errcheck

	f := newFakePVE(t)
	f.cloneFailN = 1000 // never succeeds
	rt := f.runtime()

	exec := Execution{ExecutionID: uuid.New().String(), RunnerClass: className, TimeoutSecs: 30, Command: []string{"true"}}
	if _, err := rt.Run(context.Background(), exec); err == nil {
		t.Fatal("expected Run to fail after exhausting clone retries")
	}
	if f.cloneCalls != proxmoxCloneRetries+1 {
		t.Errorf("cloneCalls = %d, want %d (initial + retries)", f.cloneCalls, proxmoxCloneRetries+1)
	}
}

// TestProxmoxCancel_DestroysNamedVM verifies Cancel finds the execution's VM by
// its forge-<id> name and destroys it.
func TestProxmoxCancel_DestroysNamedVM(t *testing.T) {
	f := newFakePVE(t)
	execID := "exec-cancel-1"
	f.listVMs = []pveVM{
		{VMID: 555, Name: proxmoxVMNamePrefix + execID, Status: "running"},
		{VMID: 9000, Name: "ubuntu-template", Status: "stopped"},
	}
	rt := f.runtime()

	if err := rt.Cancel(context.Background(), execID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// Cancel tears down in the background so the HTTP handler isn't blocked.
	f.waitForDeletes(t, 1)
	if got := f.deletedVMIDs(); len(got) != 1 || got[0] != 555 {
		t.Errorf("deleted = %v, want only the named VM 555 destroyed", got)
	}
}

// TestProxmoxSweepOrphans destroys leftover forge-* VMs but never the template.
func TestProxmoxSweepOrphans(t *testing.T) {
	f := newFakePVE(t)
	f.listVMs = []pveVM{
		{VMID: 101, Name: proxmoxVMNamePrefix + "old-1", Status: "running"},
		{VMID: 102, Name: proxmoxVMNamePrefix + "old-2", Status: "stopped"},
		{VMID: 9000, Name: proxmoxVMNamePrefix + "template", Status: "stopped"}, // == templateVMID, must be spared
		{VMID: 200, Name: "unrelated-vm", Status: "running"},
	}
	rt := f.runtime() // templateVMID = 9000

	rt.sweepOrphans(context.Background())

	if len(f.deleted) != 2 {
		t.Fatalf("deleted = %v, want exactly the two orphaned forge VMs (not the template, not unrelated)", f.deleted)
	}
	for _, id := range f.deleted {
		if id != 101 && id != 102 {
			t.Errorf("unexpected VM %d destroyed", id)
		}
	}
}
