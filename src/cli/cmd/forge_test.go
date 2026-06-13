package cmd

import (
	"net/http"
	"strings"
	"testing"
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
