"""Integration tests for workflow run endpoints."""
import time

import pytest
import requests

from conftest import WORKFLOWS_URL, HEALTHZ_STEP


def poll_until_done(bearer, run_id, timeout=30):
    """Poll GET /runs/{id} until the run reaches a terminal state."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        res = requests.get(f"{WORKFLOWS_URL}/runs/{run_id}", headers=bearer)
        assert res.status_code == 200, f"poll got {res.status_code}: {res.text}"
        data = res.json()
        if data["status"] in ("completed", "failed", "cancelled"):
            return data
        time.sleep(1)
    pytest.fail(f"run {run_id} did not complete within {timeout}s")


@pytest.fixture(scope="module")
def workflow(bearer):
    """Create a reusable workflow for run tests."""
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "run-test-pipeline",
        "steps": [HEALTHZ_STEP],
    })
    assert res.status_code == 201, f"setup failed: {res.text}"
    wf = res.json()
    yield wf
    requests.delete(f"{WORKFLOWS_URL}/workflows/{wf['workflow_id']}", headers=bearer)


@pytest.fixture(scope="module")
def multi_step_workflow(bearer):
    """Create a two-step workflow for sequential-execution tests."""
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "multi-step-pipeline",
        "steps": [
            HEALTHZ_STEP,
            {
                "name": "second-step",
                "service": "gatekeeper",
                "method": "GET",
                "path": "/healthz",
                "expected_status": 200,
                "timeout_secs": 10,
            },
        ],
    })
    assert res.status_code == 201, f"setup failed: {res.text}"
    wf = res.json()
    yield wf
    requests.delete(f"{WORKFLOWS_URL}/workflows/{wf['workflow_id']}", headers=bearer)


# ── Authentication ─────────────────────────────────────────────────────────────

def test_trigger_unauthorized(workflow):
    res = requests.post(f"{WORKFLOWS_URL}/workflows/{workflow['workflow_id']}/runs")
    assert res.status_code == 401


def test_get_run_unauthorized():
    res = requests.get(f"{WORKFLOWS_URL}/runs/does-not-matter")
    assert res.status_code == 401


def test_list_runs_unauthorized():
    res = requests.get(f"{WORKFLOWS_URL}/runs")
    assert res.status_code == 401


def test_cancel_run_unauthorized():
    res = requests.delete(f"{WORKFLOWS_URL}/runs/does-not-matter")
    assert res.status_code == 401


# ── Trigger validation ─────────────────────────────────────────────────────────

def test_trigger_nonexistent_workflow(bearer):
    res = requests.post(
        f"{WORKFLOWS_URL}/workflows/00000000-0000-0000-0000-000000000000/runs",
        headers=bearer,
    )
    assert res.status_code == 404


# ── Trigger + response shape ───────────────────────────────────────────────────

def test_trigger_returns_202(bearer, workflow):
    res = requests.post(
        f"{WORKFLOWS_URL}/workflows/{workflow['workflow_id']}/runs",
        headers=bearer,
    )
    assert res.status_code == 202, f"got {res.status_code}: {res.text}"


def test_trigger_response_shape(bearer, workflow):
    res = requests.post(
        f"{WORKFLOWS_URL}/workflows/{workflow['workflow_id']}/runs",
        headers=bearer,
    )
    assert res.status_code == 202
    run = res.json()
    assert run["run_id"] != ""
    assert run["workflow_id"] == workflow["workflow_id"]
    assert run["status"] == "pending"
    assert run["current_step"] == 0


def test_trigger_with_inputs(bearer, workflow):
    res = requests.post(
        f"{WORKFLOWS_URL}/workflows/{workflow['workflow_id']}/runs",
        headers=bearer,
        json={"inputs": {"DEPLOY_ENV": "staging", "VERSION": "v1.2.3"}},
    )
    assert res.status_code == 202
    run = res.json()
    assert run["inputs"]["DEPLOY_ENV"] == "staging"
    assert run["inputs"]["VERSION"] == "v1.2.3"


# ── Get / List ─────────────────────────────────────────────────────────────────

def test_get_run_not_found(bearer):
    res = requests.get(f"{WORKFLOWS_URL}/runs/00000000-0000-0000-0000-000000000000",
                       headers=bearer)
    assert res.status_code == 404


def test_get_run_found(bearer, workflow):
    res = requests.post(
        f"{WORKFLOWS_URL}/workflows/{workflow['workflow_id']}/runs",
        headers=bearer,
    )
    run_id = res.json()["run_id"]

    res = requests.get(f"{WORKFLOWS_URL}/runs/{run_id}", headers=bearer)
    assert res.status_code == 200
    data = res.json()
    assert data["run_id"] == run_id
    assert data["workflow_id"] == workflow["workflow_id"]
    assert isinstance(data["step_runs"], list)


def test_list_runs_returns_array(bearer):
    res = requests.get(f"{WORKFLOWS_URL}/runs", headers=bearer)
    assert res.status_code == 200
    assert isinstance(res.json(), list)


def test_list_runs_filter_by_workflow(bearer, workflow):
    wf_id = workflow["workflow_id"]
    res = requests.post(f"{WORKFLOWS_URL}/workflows/{wf_id}/runs", headers=bearer)
    assert res.status_code == 202

    res = requests.get(f"{WORKFLOWS_URL}/runs?workflow_id={wf_id}", headers=bearer)
    assert res.status_code == 200
    runs = res.json()
    assert len(runs) >= 1
    assert all(r["workflow_id"] == wf_id for r in runs)


# ── Full execution lifecycle ───────────────────────────────────────────────────

def test_run_completes_successfully(bearer, workflow):
    """A single-step workflow calling GET /healthz should complete."""
    res = requests.post(
        f"{WORKFLOWS_URL}/workflows/{workflow['workflow_id']}/runs",
        headers=bearer,
    )
    assert res.status_code == 202
    run_id = res.json()["run_id"]

    result = poll_until_done(bearer, run_id)
    assert result["status"] == "completed"


def test_completed_run_has_step_runs(bearer, workflow):
    res = requests.post(
        f"{WORKFLOWS_URL}/workflows/{workflow['workflow_id']}/runs",
        headers=bearer,
    )
    run_id = res.json()["run_id"]
    result = poll_until_done(bearer, run_id)

    assert len(result["step_runs"]) == 1
    step = result["step_runs"][0]
    assert step["status"] == "completed"
    assert step["response_status"] == 200
    assert step["step_name"] == "check-gatekeeper"
    assert step["step_index"] == 0


def test_multi_step_run_completes(bearer, multi_step_workflow):
    res = requests.post(
        f"{WORKFLOWS_URL}/workflows/{multi_step_workflow['workflow_id']}/runs",
        headers=bearer,
    )
    assert res.status_code == 202
    run_id = res.json()["run_id"]

    result = poll_until_done(bearer, run_id)
    assert result["status"] == "completed"
    assert len(result["step_runs"]) == 2
    for step in result["step_runs"]:
        assert step["status"] == "completed"
        assert step["response_status"] == 200


def test_run_fails_on_bad_expected_status(bearer):
    """A step with expected_status=999 should never match, causing the run to fail."""
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "failing-pipeline",
        "steps": [{
            "name": "will-fail",
            "service": "gatekeeper",
            "method": "GET",
            "path": "/healthz",
            "expected_status": 999,
            "timeout_secs": 10,
        }],
    })
    assert res.status_code == 201
    wf_id = res.json()["workflow_id"]

    res = requests.post(f"{WORKFLOWS_URL}/workflows/{wf_id}/runs", headers=bearer)
    assert res.status_code == 202
    run_id = res.json()["run_id"]

    result = poll_until_done(bearer, run_id)
    assert result["status"] == "failed"
    assert result["step_runs"][0]["status"] == "failed"

    requests.delete(f"{WORKFLOWS_URL}/workflows/{wf_id}", headers=bearer)


def test_input_substitution_in_path(bearer):
    """${ENV} in a step path should be substituted from run inputs."""
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "substitution-pipeline",
        "steps": [{
            "name": "parameterised",
            "service": "gatekeeper",
            "method": "GET",
            "path": "/${ENDPOINT}",
            "expected_status": 200,
            "timeout_secs": 10,
        }],
    })
    assert res.status_code == 201
    wf_id = res.json()["workflow_id"]

    res = requests.post(
        f"{WORKFLOWS_URL}/workflows/{wf_id}/runs",
        headers=bearer,
        json={"inputs": {"ENDPOINT": "healthz"}},
    )
    assert res.status_code == 202
    run_id = res.json()["run_id"]

    result = poll_until_done(bearer, run_id)
    assert result["status"] == "completed", f"step_runs: {result['step_runs']}"

    requests.delete(f"{WORKFLOWS_URL}/workflows/{wf_id}", headers=bearer)


# ── Cancel ─────────────────────────────────────────────────────────────────────

def test_cancel_not_found(bearer):
    res = requests.delete(
        f"{WORKFLOWS_URL}/runs/00000000-0000-0000-0000-000000000000",
        headers=bearer,
    )
    assert res.status_code == 404


def test_cancel_completed_run_returns_conflict(bearer, workflow):
    res = requests.post(
        f"{WORKFLOWS_URL}/workflows/{workflow['workflow_id']}/runs",
        headers=bearer,
    )
    run_id = res.json()["run_id"]
    poll_until_done(bearer, run_id)

    res = requests.delete(f"{WORKFLOWS_URL}/runs/{run_id}", headers=bearer)
    assert res.status_code == 409


def test_cancel_pending_run(bearer, workflow):
    res = requests.post(
        f"{WORKFLOWS_URL}/workflows/{workflow['workflow_id']}/runs",
        headers=bearer,
    )
    assert res.status_code == 202
    run_id = res.json()["run_id"]

    # Cancel immediately — may race with worker pickup, but must end in a
    # terminal state.
    res = requests.delete(f"{WORKFLOWS_URL}/runs/{run_id}", headers=bearer)
    assert res.status_code == 204

    result = poll_until_done(bearer, run_id)
    assert result["status"] in ("cancelled", "completed")
