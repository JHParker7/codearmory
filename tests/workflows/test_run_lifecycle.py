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


def make_pipeline(bearer, steps):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "name": f"wf-{uuid.uuid4().hex[:8]}", "steps": steps,
    })
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
    # surface the captured output (not just the "exit code: N" summary).
    marker = f"BUILD_LOG_{uuid.uuid4().hex[:8]}"
    step = make_step(bearer, "forge/run", {"run": f"echo {marker}; exit 1", "image": "alpine:3.19"})
    wf = make_pipeline(bearer, [{"step_id": step}])
    run_id = trigger(bearer, wf)
    result = poll_run(bearer, run_id, timeout=90)
    assert result["status"] == "failed", f"expected failed, got {result['status']}"
    step_runs = result.get("step_runs") or []
    assert step_runs, f"no step runs recorded: {result}"
    output = step_runs[0].get("output") or ""
    assert marker in output, f"captured stdout marker missing from step output: {output!r}"
    assert "exit code" in output.lower(), f"exit-code summary missing from step output: {output!r}"
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf}", headers=bearer)


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


def test_parallel_group_runs_all_steps(bearer):
    s1 = make_step(bearer, "http", {"service": "gatekeeper", "method": "GET", "path": "/healthz", "expected_status": 200})
    s2 = make_step(bearer, "http", {"service": "gatekeeper", "method": "GET", "path": "/healthz", "expected_status": 200})
    # Both steps in the same parallel group execute concurrently.
    wf = make_pipeline(bearer, [
        {"step_id": s1, "parallel_group": 0},
        {"step_id": s2, "parallel_group": 0},
    ])
    run_id = trigger(bearer, wf)
    result = poll_run(bearer, run_id)
    assert result["status"] == "completed", f"expected completed, got {result['status']}: {result}"
    step_runs = result.get("step_runs") or []
    assert len(step_runs) == 2, f"expected 2 step runs for the parallel group, got {len(step_runs)}"
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
