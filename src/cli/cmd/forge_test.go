package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// printExecution must surface stdout/stderr, which the generic record view hides.
func TestPrintExecution_ShowsOutput(t *testing.T) {
	body := []byte(`{"execution_id":"e1","status":"failed","exit_code":1,"stdout":"out-here","stderr":"err-here"}`)
	out := captureStdoutDuring(func() { printExecution(body) })
	for _, want := range []string{"stdout:", "out-here", "stderr:", "err-here"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestPrintExecution_NoOutput(t *testing.T) {
	body := []byte(`{"execution_id":"e1","status":"failed","exit_code":1}`)
	out := captureStdoutDuring(func() { printExecution(body) })
	if !strings.Contains(out, "no output was captured") {
		t.Errorf("want a 'no output' note, got:\n%s", out)
	}
}

func TestWaitForExecution_Completed(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{"status":"completed","stdout":"hello world\n"}`)
	setupCLI(t, srv)

	var err error
	out := captureStdoutDuring(func() { err = waitForExecution("exec-1", 30) })
	if err != nil {
		t.Fatalf("waitForExecution returned error: %v", err)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("stdout = %q, want the job output", out)
	}
}

func TestWaitForExecution_Failed(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK,
		`{"status":"failed","exit_code":1,"image":"alpine:3.19","stderr":"boom"}`)
	setupCLI(t, srv)

	var err error
	captureStdoutDuring(func() { err = waitForExecution("exec-1", 30) }) // swallow printed output
	if err == nil {
		t.Fatal("expected an error for a failed job")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exited with code 1") {
		t.Errorf("error = %q, want it to mention the exit code", msg)
	}
	if !strings.Contains(msg, "see output above") {
		t.Errorf("error = %q, want it to point at the printed output", msg)
	}
}

func TestWaitForExecution_TimedOut(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{"status":"timed_out","image":"alpine:3.19"}`)
	setupCLI(t, srv)

	var err error
	captureStdoutDuring(func() { err = waitForExecution("exec-1", 30) })
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want a 'timed out' message", err)
	}
}

// rerun fetches the original execution and resubmits a faithful copy of its
// image, command, env, timeout, and runner class. The single canned body stands
// in for both the GET (source record) and the POST (new execution_id); rec holds
// the final request, which is the POST.
func TestRerunForgeExecution_ResubmitsCopiedParams(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK,
		`{"execution_id":"orig-1","image":"ubuntu:22.04","command":["sh","-c","echo hi"],"env":{"FOO":"bar"},"timeout":120,"runner_class":"large"}`)
	setupCLI(t, srv)

	var err error
	captureStdoutDuring(func() { err = rerunForgeExecution(&cobra.Command{}, "orig-1", false) })
	if err != nil {
		t.Fatalf("rerunForgeExecution returned error: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/forge/executions" {
		t.Fatalf("final request = %s %s, want POST /forge/executions", rec.Method, rec.Path)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("resubmit body not JSON: %v", err)
	}
	if got["image"] != "ubuntu:22.04" {
		t.Errorf("image = %v, want ubuntu:22.04", got["image"])
	}
	cmd, _ := got["command"].([]any)
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[2] != "echo hi" {
		t.Errorf("command = %v, want [sh -c echo hi]", got["command"])
	}
	env, _ := got["env"].(map[string]any)
	if env["FOO"] != "bar" {
		t.Errorf("env = %v, want FOO=bar", got["env"])
	}
	if got["timeout"].(float64) != 120 {
		t.Errorf("timeout = %v, want 120", got["timeout"])
	}
	if got["runner_class"] != "large" {
		t.Errorf("runner_class = %v, want large", got["runner_class"])
	}
}

// An execution with no recorded image/command can't be reconstructed, so rerun
// must fail loudly rather than POST an empty job.
func TestRerunForgeExecution_MissingParams_Errors(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{"execution_id":"orig-1","status":"completed"}`)
	setupCLI(t, srv)

	err := rerunForgeExecution(&cobra.Command{}, "orig-1", false)
	if err == nil || !strings.Contains(err.Error(), "cannot be rerun") {
		t.Fatalf("error = %v, want a 'cannot be rerun' message", err)
	}
}

// A fetch failure for the source execution surfaces as an error, not a silent
// no-op or a bogus resubmit.
func TestRerunForgeExecution_FetchError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusNotFound, `{"error":"not found"}`)
	setupCLI(t, srv)

	if err := rerunForgeExecution(&cobra.Command{}, "nope", false); err == nil {
		t.Fatal("expected an error when the source execution can't be fetched")
	}
}

func TestForgeExecCmd_HasRerunSubcommand(t *testing.T) {
	execCmd := findSubcmd(t, forgeCmd, "exec")
	if execCmd == nil {
		t.Fatal("exec subcommand not registered under forge")
	}
	if findSubcmd(t, execCmd, "rerun") == nil {
		t.Error("rerun subcommand not registered under forge exec")
	}
}
