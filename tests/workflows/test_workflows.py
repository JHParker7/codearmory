"""Integration tests for workflow definition CRUD endpoints."""
import pytest
import requests

from conftest import WORKFLOWS_URL, HEALTHZ_STEP


# ── Authentication ────────────────────────────────────────────────────────────

def test_create_unauthorized():
    res = requests.post(f"{WORKFLOWS_URL}/workflows", json={
        "name": "x", "steps": [HEALTHZ_STEP],
    })
    assert res.status_code == 401


def test_get_unauthorized():
    res = requests.get(f"{WORKFLOWS_URL}/workflows/does-not-matter")
    assert res.status_code == 401


def test_list_unauthorized():
    res = requests.get(f"{WORKFLOWS_URL}/workflows")
    assert res.status_code == 401


def test_update_unauthorized():
    res = requests.put(f"{WORKFLOWS_URL}/workflows/does-not-matter", json={
        "name": "x", "steps": [HEALTHZ_STEP],
    })
    assert res.status_code == 401


def test_delete_unauthorized():
    res = requests.delete(f"{WORKFLOWS_URL}/workflows/does-not-matter")
    assert res.status_code == 401


# ── Validation ────────────────────────────────────────────────────────────────

def test_create_missing_name(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "steps": [HEALTHZ_STEP],
    })
    assert res.status_code == 400


def test_create_empty_steps(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "empty-steps", "steps": [],
    })
    assert res.status_code == 400


def test_create_step_missing_service(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "bad-step",
        "steps": [{"path": "/foo", "method": "GET"}],
    })
    assert res.status_code == 400


def test_create_step_missing_path(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "bad-step",
        "steps": [{"service": "forge", "method": "GET"}],
    })
    assert res.status_code == 400


def test_create_step_path_not_slash_prefixed(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "bad-path",
        "steps": [{"service": "forge", "path": "executions", "method": "GET"}],
    })
    assert res.status_code == 400


def test_create_step_invalid_method(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "bad-method",
        "steps": [{"service": "forge", "path": "/executions", "method": "TRACE"}],
    })
    assert res.status_code == 400


def test_create_invalid_json(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, data=b"not json")
    assert res.status_code == 400


# ── CRUD lifecycle ────────────────────────────────────────────────────────────

@pytest.fixture(scope="module")
def created_workflow(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "test-pipeline",
        "description": "integration test workflow",
        "steps": [
            HEALTHZ_STEP,
            {
                "name": "second-check",
                "service": "gatekeeper",
                "method": "GET",
                "path": "/healthz",
                "expected_status": 200,
                "timeout_secs": 10,
            },
        ],
    })
    assert res.status_code == 201, f"create failed: {res.text}"
    wf = res.json()
    yield wf
    # Cleanup
    requests.delete(f"{WORKFLOWS_URL}/workflows/{wf['workflow_id']}", headers=bearer)


def test_create_returns_workflow(created_workflow):
    wf = created_workflow
    assert wf["workflow_id"] != ""
    assert wf["name"] == "test-pipeline"
    assert wf["description"] == "integration test workflow"
    assert len(wf["steps"]) == 2
    assert wf["active"] is True


def test_create_normalises_defaults(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "defaults-wf",
        "steps": [{"service": "gatekeeper", "path": "/healthz"}],
    })
    assert res.status_code == 201
    wf = res.json()
    step = wf["steps"][0]
    assert step["method"] == "POST"         # default method
    assert step["timeout_secs"] == 30       # default timeout
    assert step["name"] == "step-0"         # auto-named
    requests.delete(f"{WORKFLOWS_URL}/workflows/{wf['workflow_id']}", headers=bearer)


def test_create_method_uppercased(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "method-case-wf",
        "steps": [{"service": "gatekeeper", "path": "/healthz", "method": "get"}],
    })
    assert res.status_code == 201
    assert res.json()["steps"][0]["method"] == "GET"
    requests.delete(f"{WORKFLOWS_URL}/workflows/{res.json()['workflow_id']}", headers=bearer)


def test_get_workflow(bearer, created_workflow):
    wf_id = created_workflow["workflow_id"]
    res = requests.get(f"{WORKFLOWS_URL}/workflows/{wf_id}", headers=bearer)
    assert res.status_code == 200
    assert res.json()["workflow_id"] == wf_id


def test_get_not_found(bearer):
    res = requests.get(f"{WORKFLOWS_URL}/workflows/00000000-0000-0000-0000-000000000000", headers=bearer)
    assert res.status_code == 404


def test_list_workflows_contains_created(bearer, created_workflow):
    wf_id = created_workflow["workflow_id"]
    res = requests.get(f"{WORKFLOWS_URL}/workflows", headers=bearer)
    assert res.status_code == 200
    ids = [w["workflow_id"] for w in res.json()]
    assert wf_id in ids


def test_list_returns_array(bearer):
    res = requests.get(f"{WORKFLOWS_URL}/workflows", headers=bearer)
    assert res.status_code == 200
    assert isinstance(res.json(), list)


def test_update_workflow(bearer, created_workflow):
    wf_id = created_workflow["workflow_id"]
    res = requests.put(f"{WORKFLOWS_URL}/workflows/{wf_id}", headers=bearer, json={
        "name": "updated-pipeline",
        "description": "updated description",
        "steps": [HEALTHZ_STEP],
    })
    assert res.status_code == 200
    updated = res.json()
    assert updated["name"] == "updated-pipeline"
    assert updated["description"] == "updated description"
    assert len(updated["steps"]) == 1


def test_update_not_found(bearer):
    res = requests.put(f"{WORKFLOWS_URL}/workflows/00000000-0000-0000-0000-000000000000",
                       headers=bearer, json={
                           "name": "x", "steps": [HEALTHZ_STEP],
                       })
    assert res.status_code == 404


def test_delete_workflow(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "to-delete", "steps": [HEALTHZ_STEP],
    })
    assert res.status_code == 201
    wf_id = res.json()["workflow_id"]

    res = requests.delete(f"{WORKFLOWS_URL}/workflows/{wf_id}", headers=bearer)
    assert res.status_code == 204

    # GET should return 404 after deletion
    res = requests.get(f"{WORKFLOWS_URL}/workflows/{wf_id}", headers=bearer)
    assert res.status_code == 404


def test_delete_not_found(bearer):
    res = requests.delete(f"{WORKFLOWS_URL}/workflows/00000000-0000-0000-0000-000000000000",
                          headers=bearer)
    assert res.status_code == 404


def test_delete_idempotent_second_call(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": "idempotent-delete", "steps": [HEALTHZ_STEP],
    })
    wf_id = res.json()["workflow_id"]
    requests.delete(f"{WORKFLOWS_URL}/workflows/{wf_id}", headers=bearer)

    # Second delete on an already-inactive workflow returns 404
    res = requests.delete(f"{WORKFLOWS_URL}/workflows/{wf_id}", headers=bearer)
    assert res.status_code == 404
