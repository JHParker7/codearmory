package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestVcpusFor(t *testing.T) {
	cases := map[int64]int64{0: 1, 1: 1, 500: 1, 1000: 1, 1001: 2, 1500: 2, 2000: 2, 2001: 3, 4000: 4}
	for millicores, want := range cases {
		if got := vcpusFor(millicores); got != want {
			t.Errorf("vcpusFor(%d) = %d, want %d", millicores, got, want)
		}
	}
}

func TestNetConfig(t *testing.T) {
	if got := netConfig("vmbr1", ""); got != "virtio,bridge=vmbr1" {
		t.Errorf("netConfig no vlan = %q", got)
	}
	if got := netConfig("vmbr1", "42"); got != "virtio,bridge=vmbr1,tag=42" {
		t.Errorf("netConfig with vlan = %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("bar"); got != "'bar'" {
		t.Errorf("shellQuote(bar) = %q", got)
	}
	// A value containing a single quote must be escaped so it cannot break out.
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("shellQuote(a'b) = %q, want the '\\'' escape", got)
	}
}

func TestBuildCommand_InjectsProxyAndExec(t *testing.T) {
	r := &ProxmoxRuntime{egressProxy: "http://egress-proxy:3128"}
	exec := Execution{Command: []string{"echo", "hi"}, Env: map[string]string{"FOO": "bar"}}

	cmd := r.buildCommand(exec)
	if len(cmd) != 3 || cmd[0] != "/bin/sh" || cmd[1] != "-c" {
		t.Fatalf("command = %v, want [/bin/sh -c <script>]", cmd)
	}
	script := cmd[2]
	if !strings.Contains(script, "export FOO='bar'\n") {
		t.Errorf("script missing user env export:\n%s", script)
	}
	if !strings.Contains(script, "export HTTP_PROXY='http://egress-proxy:3128'\n") {
		t.Errorf("script missing injected proxy export:\n%s", script)
	}
	if !strings.HasSuffix(script, "exec 'echo' 'hi'") {
		t.Errorf("script should end by exec-ing the user command, got:\n%s", script)
	}
}

func TestBuildCommand_UserProxyNotOverridden(t *testing.T) {
	r := &ProxmoxRuntime{egressProxy: "http://egress-proxy:3128"}
	exec := Execution{Command: []string{"true"}, Env: map[string]string{"HTTP_PROXY": "http://user-proxy:1"}}

	script := r.buildCommand(exec)[2]
	if !strings.Contains(script, "export HTTP_PROXY='http://user-proxy:1'\n") {
		t.Errorf("user HTTP_PROXY should be kept:\n%s", script)
	}
	if strings.Contains(script, "egress-proxy:3128") && strings.Contains(script, "export HTTP_PROXY='http://egress-proxy") {
		t.Errorf("injected proxy must not override the user's HTTP_PROXY:\n%s", script)
	}
}

func TestBuildCommand_NoProxyWhenUnset(t *testing.T) {
	r := &ProxmoxRuntime{egressProxy: ""}
	exec := Execution{Command: []string{"ls"}}
	script := r.buildCommand(exec)[2]
	if strings.Contains(script, "HTTP_PROXY") {
		t.Errorf("no proxy should be injected when egressProxy is empty:\n%s", script)
	}
	if script != "exec 'ls'" {
		t.Errorf("script = %q, want just the exec line", script)
	}
}

func TestExecStatusResult_DecodeAndTruncate(t *testing.T) {
	out := base64.StdEncoding.EncodeToString([]byte("hello"))
	st := &execStatus{Exited: true, ExitCode: ptr(3), OutData: out, ErrData: base64.StdEncoding.EncodeToString([]byte("err"))}
	res := execStatusResult(st)
	if res.Stdout != "hello" {
		t.Errorf("stdout = %q, want decoded 'hello'", res.Stdout)
	}
	if res.Stderr != "err" {
		t.Errorf("stderr = %q, want decoded 'err'", res.Stderr)
	}
	if res.ExitCode == nil || *res.ExitCode != 3 {
		t.Errorf("exit = %v, want 3", res.ExitCode)
	}
}

// TestExecStatusResult_SignalIsFailure guards the fix for a signal-killed guest
// command (exited=true, exitcode=null, signal=N): it must be a non-zero failure
// (128+N), not a silent exit 0.
func TestExecStatusResult_SignalIsFailure(t *testing.T) {
	res := execStatusResult(&execStatus{Exited: true, Signal: ptr(9)})
	if res.ExitCode == nil || *res.ExitCode != 137 {
		t.Errorf("exit = %v, want 137 (128+9) for a SIGKILLed command", res.ExitCode)
	}
	if res.Stderr == "" {
		t.Error("expected a stderr note explaining the signal termination")
	}
}

// TestExecStatusResult_NoCodeNoSignalIsFailure: an exited command that reports
// neither an exit code nor a signal is abnormal and must not be a silent success.
func TestExecStatusResult_NoCodeNoSignalIsFailure(t *testing.T) {
	res := execStatusResult(&execStatus{Exited: true})
	if res.ExitCode == nil || *res.ExitCode == 0 {
		t.Errorf("exit = %v, want a non-zero failure when the guest reports no exit code", res.ExitCode)
	}
}

func TestDecodeTruncate_CapsAtMaxOutput(t *testing.T) {
	big := strings.Repeat("a", maxOutputBytes+100)
	enc := base64.StdEncoding.EncodeToString([]byte(big))
	got := decodeTruncate(enc)
	if len(got) != maxOutputBytes {
		t.Errorf("decoded length = %d, want capped at %d", len(got), maxOutputBytes)
	}
}

func TestDecodeTruncate_NonBase64Fallback(t *testing.T) {
	// Not valid base64 → return as-is rather than empty.
	if got := decodeTruncate("not%%%base64"); got != "not%%%base64" {
		t.Errorf("got %q, want the raw string back when not base64", got)
	}
}

func TestPveBool_Unmarshal(t *testing.T) {
	cases := map[string]bool{"true": true, "false": false, "1": true, "0": false, `"1"`: true}
	for in, want := range cases {
		var b pveBool
		if err := b.UnmarshalJSON([]byte(in)); err != nil {
			t.Fatalf("unmarshal %q: %v", in, err)
		}
		if bool(b) != want {
			t.Errorf("pveBool(%q) = %v, want %v", in, bool(b), want)
		}
	}
}

func TestNewProxmoxRuntime_ValidationErrors(t *testing.T) {
	validCfg := func() map[string]string {
		return map[string]string{
			pmKeyURL: "https://pve:8006/api2/json", pmKeyNode: "pve",
			pmKeyTemplate: "9000", pmKeyStorage: "local-lvm", pmKeyBridge: "vmbr1",
		}
	}

	t.Run("missing config key", func(t *testing.T) {
		cfg := validCfg()
		delete(cfg, pmKeyBridge)
		b := RuntimeBackend{Name: "px", Type: "proxmox", Config: cfg, SecretRefs: map[string]string{pmSecretToken: "PX_TOKEN"}}
		if _, err := newProxmoxRuntime(b); err == nil {
			t.Fatal("expected error for missing bridge")
		}
	})

	t.Run("non-integer template", func(t *testing.T) {
		cfg := validCfg()
		cfg[pmKeyTemplate] = "notanint"
		b := RuntimeBackend{Name: "px", Type: "proxmox", Config: cfg, SecretRefs: map[string]string{pmSecretToken: "PX_TOKEN"}}
		if _, err := newProxmoxRuntime(b); err == nil {
			t.Fatal("expected error for non-integer template_vmid")
		}
	})

	t.Run("missing token ref", func(t *testing.T) {
		b := RuntimeBackend{Name: "px", Type: "proxmox", Config: validCfg(), SecretRefs: map[string]string{}}
		if _, err := newProxmoxRuntime(b); err == nil {
			t.Fatal("expected error for missing token secret_ref")
		}
	})

	t.Run("empty token value", func(t *testing.T) {
		t.Setenv("PX_TOKEN_EMPTY", "")
		b := RuntimeBackend{Name: "px", Type: "proxmox", Config: validCfg(), SecretRefs: map[string]string{pmSecretToken: "PX_TOKEN_EMPTY"}}
		if _, err := newProxmoxRuntime(b); err == nil {
			t.Fatal("expected error when the token secret resolves empty")
		}
	})
}
