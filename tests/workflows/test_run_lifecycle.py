"""Integration tests for run lifecycle edges the happy-path suite skips:
a failing step marks the run failed, a parallel step group runs every step, and
cancelling a running run transitions it to 'cancelled'.
"""
import time
import uuid

import pytest
import requests

from conftest import WORKFLOWS_URL


def make_step(bearer, action, with_, name=None):
    res = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json={
        "name": name or f"step-{uuid.uuid4().hex[:8]}",
        "action": action,
        "with": with_,
        "timeout": 30,
    })
    assert res.status_code == 201, f"create step: {res.status_code} {res.text}"
    return res.json()["step_id"]


def make_pipeline(bearer, steps, routes=None):
    body = {"name": f"wf-{uuid.uuid4().hex[:8]}", "steps": steps}
    if routes:
        body["routes"] = routes
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json=body)
    assert res.status_code == 201, f"create pipeline: {res.status_code} {res.text}"
    wf_id = res.json()["workflow_id"]
    return wf_id


def trigger(bearer, wf_id):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines/{wf_id}/runs", headers=bearer, json={"inputs": {}})
    assert res.status_code in (200, 201, 202), f"trigger: {res.status_code} {res.text}"
    return res.json()["run_id"]


def poll_run(bearer, run_id, timeout=60, until=None):
    deadline = time.time() + timeout
    until = until or (lambda d: d["status"] in ("completed", "failed", "timed_out", "cancelled"))
    last = None
    while time.time() < deadline:
        res = requests.get(f"{WORKFLOWS_URL}/runs/{run_id}", headers=bearer)
        assert res.status_code == 200, res.text
        last = res.json()
        if until(last):
            return last
        time.sleep(1)
    pytest.fail(f"run {run_id} did not reach target state within {timeout}s (last={last and last.get('status')})")


def test_failing_step_marks_run_failed(bearer):
    # HTTP step expects 418 from gatekeeper /healthz (which returns 200) → step fails.
    step = make_step(bearer, "http", {"service": "gatekeeper", "method": "GET", "path": "/healthz", "expected_status": 418})
    wf = make_pipeline(bearer, [{"step_id": step}])
    run_id = trigger(bearer, wf)
    result = poll_run(bearer, run_id)
    assert result["status"] == "failed", f"expected failed, got {result['status']}"
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf}", headers=bearer)


def test_failed_step_surfaces_captured_output(bearer):
    # A forge step that prints to stdout then exits non-zero: the failed run must
    # surface the captured stdout (in `logs`) AND the error summary (in `output`),
    # not just the "exit code: N" summary.
    marker = f"BUILD_LOG_{uuid.uuid4().hex[:8]}"
    step = make_step(bearer, "forge/run", {"run": f"echo {marker}; exit 1", "image": "alpine:3.19"})
    wf = make_pipeline(bearer, [{"step_id": step}])
    run_id = trigger(bearer, wf)
    result = poll_run(bearer, run_id, timeout=90)
    assert result["status"] == "failed", f"expected failed, got {result['status']}"
    step_runs = result.get("step_runs") or []
    assert step_runs, f"no step runs recorded: {result}"
    logs = step_runs[0].get("logs") or ""
    output = step_runs[0].get("output") or ""
    assert marker in logs, f"captured stdout marker missing from step logs: {logs!r}"
    assert "exit code" in output.lower(), f"exit-code summary missing from step output: {output!r}"
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf}", headers=bearer)


def test_successful_step_surfaces_stdout_in_logs(bearer):
    # A forge step that just prints to stdout (no output_env) must surface that
    # stdout in the step run's `logs` for viewing — the consumable `output` stays
    # empty (stdout is never ${steps.NAME.output}). Regression: previously a
    # successful step showed nothing in the run view.
    marker = f"RUN_LOG_{uuid.uuid4().hex[:8]}"
    step = make_step(bearer, "forge/run", {"run": f"echo {marker}", "image": "alpine:3.19"})
    wf = make_pipeline(bearer, [{"step_id": step}])
    run_id = trigger(bearer, wf)
    result = poll_run(bearer, run_id, timeout=90)
    assert result["status"] == "completed", f"expected completed, got {result['status']}: {result}"
    step_runs = result.get("step_runs") or []
    assert step_runs, f"no step runs recorded: {result}"
    logs = step_runs[0].get("logs") or ""
    assert marker in logs, f"stdout marker missing from step logs: {logs!r}"
    # stdout must not leak into the consumable output (no output_env was declared).
    assert not (step_runs[0].get("output") or ""), f"output should be empty, got: {step_runs[0].get('output')!r}"
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf}", headers=bearer)


def test_multiline_output_env_is_captured_whole(bearer):
    # Regression: an output_env value with embedded newlines (e.g. a
    # `find ... -printf '%f\n'` directory list) must be captured whole, not truncated
    # to its first line. The read step sets DIRS to a 3-line value and captures it;
    # all three lines must survive into the step's consumable output.
    tag = uuid.uuid4().hex[:8]
    a, b, c = f"alpha{tag}", f"beta{tag}", f"gamma{tag}"
    step = make_step(bearer, "forge/run", {
        "image": "alpine:3.19",
        "run": f"DIRS=$(printf '%s\\n%s\\n%s\\n' {a} {b} {c})",
        "output_env": ["DIRS"],
    })
    wf = make_pipeline(bearer, [{"step_id": step}])
    run_id = trigger(bearer, wf)
    result = poll_run(bearer, run_id, timeout=90)
    assert result["status"] == "completed", f"expected completed, got {result['status']}: {result}"
    blob = str(result.get("step_runs") or [])
    for val in (a, b, c):
        assert val in blob, f"multi-line output truncated — {val!r} missing from captured output: {blob!r}"
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf}", headers=bearer)
    requests.delete(f"{WORKFLOWS_URL}/steps/{step}", headers=bearer)


def test_failed_step_surfaces_forge_error_message(bearer):
    # exit 127 with no stderr makes forge synthesize a "command not found"
    # diagnostic; the run must surface forge's actual message, not just the generic
    # "forge/run failed (exit code: 127)" summary.
    step = make_step(bearer, "forge/run", {"run": "exit 127", "image": "alpine:3.19"})
    wf = make_pipeline(bearer, [{"step_id": step}])
    run_id = trigger(bearer, wf)
    result = poll_run(bearer, run_id, timeout=90)
    assert result["status"] == "failed", f"expected failed, got {result['status']}"
    output = ((result.get("step_runs") or [{}])[0].get("output") or "").lower()
    assert "not found" in output, f"forge diagnostic missing from step output: {output!r}"
    assert "127" in output, f"exit code missing from step output: {output!r}"
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf}", headers=bearer)


def test_step_timeout_is_enforced_by_forge(bearer):
    # A 5s step timeout must be forwarded to forge: a 20s sleep is killed at ~5s
    # (run fails), not allowed to run to forge's 30s default (which would let the
    # sleep finish and the run complete).
    s = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json={
        "name": f"to-{uuid.uuid4().hex[:8]}", "action": "forge/run",
        "with": {"run": "sleep 20", "image": "alpine:3.19"}, "timeout": 5,
    })
    assert s.status_code == 201, s.text
    sid = s.json()["step_id"]
    wf = make_pipeline(bearer, [{"step_id": sid}])
    run_id = trigger(bearer, wf)
    t0 = time.time()
    result = poll_run(bearer, run_id, timeout=60)
    elapsed = time.time() - t0
    assert result["status"] == "failed", f"expected failed (timed out), got {result['status']} after {elapsed:.0f}s"
    assert elapsed < 25, f"step ran {elapsed:.0f}s — the 5s timeout was not forwarded to forge"
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf}", headers=bearer)
    requests.delete(f"{WORKFLOWS_URL}/steps/{sid}", headers=bearer)


def test_forked_routes_run_all_steps(bearer):
    # Parallelism is expressed by routes: two edges out of one step fork the run.
    # There is no parallel_group — a bare step array is a plain sequence.
    fan = make_step(bearer, "http", {"service": "gatekeeper", "method": "GET", "path": "/healthz", "expected_status": 200})
    s1 = make_step(bearer, "http", {"service": "gatekeeper", "method": "GET", "path": "/healthz", "expected_status": 200})
    s2 = make_step(bearer, "http", {"service": "gatekeeper", "method": "GET", "path": "/healthz", "expected_status": 200})
    wf = make_pipeline(
        bearer,
        [
            {"step_id": fan, "name": "fan"},
            {"step_id": s1, "name": "a"},
            {"step_id": s2, "name": "b"},
        ],
        routes=[
            {"from": "fan", "to": "a"},
            {"from": "fan", "to": "b"},
        ],
    )
    run_id = trigger(bearer, wf)
    result = poll_run(bearer, run_id)
    assert result["status"] == "completed", f"expected completed, got {result['status']}: {result}"
    step_runs = result.get("step_runs") or []
    assert len(step_runs) == 3, f"expected 3 step runs across the fork, got {len(step_runs)}"
    # Both branches must actually have run — a fork that dropped one would still
    # report completed.
    names = {sr["step_name"] for sr in step_runs}
    assert names == {"fan", "a", "b"}, f"expected both branches to run, got {names}"
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf}", headers=bearer)


def test_cancel_running_run(bearer):
    # A slow forge step keeps the run 'running' long enough to cancel it.
    step = make_step(bearer, "forge/run", {"run": "sleep 20", "image": "alpine:3.19"})
    wf = make_pipeline(bearer, [{"step_id": step}])
    run_id = trigger(bearer, wf)

    # Wait until the run is actually running (worker dequeued + forge started).
    poll_run(bearer, run_id, timeout=30, until=lambda d: d["status"] in ("running", "completed", "failed", "cancelled"))

    res = requests.delete(f"{WORKFLOWS_URL}/runs/{run_id}", headers=bearer)
    assert res.status_code in (200, 202, 204), f"cancel: {res.status_code} {res.text}"

    result = poll_run(bearer, run_id, timeout=40)
    assert result["status"] == "cancelled", f"expected cancelled, got {result['status']}"
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf}", headers=bearer)
