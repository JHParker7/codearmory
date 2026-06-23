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
