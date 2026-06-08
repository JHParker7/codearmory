"""Integration tests for workflow run endpoints."""
import time
import uuid

import pytest
import requests

from conftest import WORKFLOWS_URL, GATEKEEPER_URL, HEALTHZ_STEP


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
def workflow(bearer, healthz_step_id):
    """Create a reusable single-step workflow for run tests."""
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": f"run-test-pipeline-{uuid.uuid4().hex[:6]}",
        "steps": [{"step_id": healthz_step_id}],
    })
    assert res.status_code == 201, f"setup failed: {res.text}"
    wf = res.json()
    yield wf
    requests.delete(f"{WORKFLOWS_URL}/workflows/{wf['workflow_id']}", headers=bearer)


@pytest.fixture(scope="module")
def multi_step_workflow(bearer, healthz_step_id, second_step_id):
    """Create a two-step workflow for sequential-execution tests."""
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": f"multi-step-pipeline-{uuid.uuid4().hex[:6]}",
        "steps": [
            {"step_id": healthz_step_id},
            {"step_id": second_step_id},
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


def test_run_fails_on_bad_expected_status(bearer, healthz_step_id):
    """A step expecting status 999 should never match, causing the run to fail."""
    # Create a step that will always fail.
    bad_step_res = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json={
        "name": f"will-fail-{uuid.uuid4().hex[:6]}",
        "action": "http",
        "with": {
            "service": "gatekeeper",
            "method": "GET",
            "path": "/healthz",
            "expected_status": 999,
        },
        "timeout": 10,
    })
    assert bad_step_res.status_code == 201
    bad_step_id = bad_step_res.json()["step_id"]

    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": f"failing-pipeline-{uuid.uuid4().hex[:6]}",
        "steps": [{"step_id": bad_step_id}],
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
    requests.delete(f"{WORKFLOWS_URL}/steps/{bad_step_id}", headers=bearer)


def test_input_substitution_in_path(bearer):
    """${ENDPOINT} in a step path should be substituted from run inputs."""
    param_step_res = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json={
        "name": f"parameterised-{uuid.uuid4().hex[:6]}",
        "action": "http",
        "with": {
            "service": "gatekeeper",
            "method": "GET",
            "path": "/${ENDPOINT}",
            "expected_status": 200,
        },
        "timeout": 10,
    })
    assert param_step_res.status_code == 201
    param_step_id = param_step_res.json()["step_id"]

    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": f"substitution-pipeline-{uuid.uuid4().hex[:6]}",
        "steps": [{"step_id": param_step_id}],
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
    requests.delete(f"{WORKFLOWS_URL}/steps/{param_step_id}", headers=bearer)


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

    res = requests.delete(f"{WORKFLOWS_URL}/runs/{run_id}", headers=bearer)
    assert res.status_code == 204

    result = poll_until_done(bearer, run_id)
    assert result["status"] in ("cancelled", "completed")


# ── Internal catalog refresh ───────────────────────────────────────────────────


def test_catalog_refresh_no_key_returns_401():
    res = requests.post(f"{WORKFLOWS_URL}/internal/catalog/refresh")
    assert res.status_code == 401


def test_catalog_refresh_wrong_key_returns_401():
    res = requests.post(
        f"{WORKFLOWS_URL}/internal/catalog/refresh",
        headers={"X-Service-Key": "registry:definitely-wrong-key"},
    )
    assert res.status_code == 401


def test_catalog_refresh_bearer_token_returns_401(bearer):
    """A user bearer token cannot call the internal catalog refresh endpoint."""
    res = requests.post(
        f"{WORKFLOWS_URL}/internal/catalog/refresh",
        headers=bearer,
    )
    assert res.status_code == 401
