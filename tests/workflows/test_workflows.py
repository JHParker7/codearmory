"""Integration tests for workflow definition CRUD endpoints."""
import uuid

import pytest
import requests

from conftest import WORKFLOWS_URL, HEALTHZ_STEP


# ── Authentication ────────────────────────────────────────────────────────────

def test_create_unauthorized():
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", json={
        "name": "x", "steps": [HEALTHZ_STEP],
    })
    assert res.status_code == 401


def test_get_unauthorized():
    res = requests.get(f"{WORKFLOWS_URL}/pipelines/does-not-matter")
    assert res.status_code == 401


def test_list_unauthorized():
    res = requests.get(f"{WORKFLOWS_URL}/pipelines")
    assert res.status_code == 401


def test_update_unauthorized():
    res = requests.put(f"{WORKFLOWS_URL}/pipelines/does-not-matter", json={
        "name": "x", "steps": [HEALTHZ_STEP],
    })
    assert res.status_code == 401


def test_delete_unauthorized():
    res = requests.delete(f"{WORKFLOWS_URL}/pipelines/does-not-matter")
    assert res.status_code == 401


# ── Validation ────────────────────────────────────────────────────────────────

def test_create_missing_name(bearer, healthz_step_id):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "steps": [{"step_id": healthz_step_id}],
    })
    assert res.status_code == 400


def test_create_empty_steps(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "name": "empty-steps", "steps": [],
    })
    assert res.status_code == 400


def test_create_step_missing_step_id(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "name": "bad-step",
        "steps": [{}],
    })
    assert res.status_code == 400


def test_create_step_nonexistent_step_id(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "name": "bad-ref",
        "steps": [{"step_id": "00000000-0000-0000-0000-000000000000"}],
    })
    assert res.status_code == 400


def test_create_invalid_json(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, data=b"not json")
    assert res.status_code == 400


# ── CRUD lifecycle ────────────────────────────────────────────────────────────

@pytest.fixture(scope="module")
def created_workflow(bearer, healthz_step_id, second_step_id):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "name": "test-pipeline",
        "description": "integration test workflow",
        "steps": [
            {"step_id": healthz_step_id},
            {"step_id": second_step_id},
        ],
    })
    assert res.status_code == 201, f"create failed: {res.text}"
    wf = res.json()
    yield wf
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf['workflow_id']}", headers=bearer)


def test_create_returns_workflow(created_workflow):
    wf = created_workflow
    assert wf["workflow_id"] != ""
    assert wf["name"] == "test-pipeline"
    assert wf["description"] == "integration test workflow"
    assert len(wf["steps"]) == 2
    assert wf["active"] is True


def test_create_returns_step_details(created_workflow):
    step = created_workflow["steps"][0]
    assert step["step_id"] != ""
    assert step["action"] == "http"


def test_get_workflow(bearer, created_workflow):
    wf_id = created_workflow["workflow_id"]
    res = requests.get(f"{WORKFLOWS_URL}/pipelines/{wf_id}", headers=bearer)
    assert res.status_code == 200
    assert res.json()["workflow_id"] == wf_id


def test_get_not_found(bearer):
    res = requests.get(f"{WORKFLOWS_URL}/pipelines/00000000-0000-0000-0000-000000000000", headers=bearer)
    assert res.status_code == 404


def test_list_workflows_contains_created(bearer, created_workflow):
    wf_id = created_workflow["workflow_id"]
    res = requests.get(f"{WORKFLOWS_URL}/pipelines", headers=bearer)
    assert res.status_code == 200
    ids = [w["workflow_id"] for w in res.json()]
    assert wf_id in ids


def test_list_returns_array(bearer):
    res = requests.get(f"{WORKFLOWS_URL}/pipelines", headers=bearer)
    assert res.status_code == 200
    assert isinstance(res.json(), list)


def test_update_workflow(bearer, created_workflow, healthz_step_id):
    wf_id = created_workflow["workflow_id"]
    res = requests.put(f"{WORKFLOWS_URL}/pipelines/{wf_id}", headers=bearer, json={
        "name": "updated-pipeline",
        "description": "updated description",
        "steps": [{"step_id": healthz_step_id}],
    })
    assert res.status_code == 200
    updated = res.json()
    assert updated["name"] == "updated-pipeline"
    assert updated["description"] == "updated description"
    assert len(updated["steps"]) == 1


def test_update_not_found(bearer, healthz_step_id):
    res = requests.put(f"{WORKFLOWS_URL}/pipelines/00000000-0000-0000-0000-000000000000",
                       headers=bearer, json={
                           "name": "x", "steps": [{"step_id": healthz_step_id}],
                       })
    assert res.status_code == 404


def test_delete_workflow(bearer, healthz_step_id):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "name": f"to-delete-{uuid.uuid4().hex[:6]}",
        "steps": [{"step_id": healthz_step_id}],
    })
    assert res.status_code == 201
    wf_id = res.json()["workflow_id"]

    res = requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf_id}", headers=bearer)
    assert res.status_code == 204

    res = requests.get(f"{WORKFLOWS_URL}/pipelines/{wf_id}", headers=bearer)
    assert res.status_code == 404


def test_delete_not_found(bearer):
    res = requests.delete(f"{WORKFLOWS_URL}/pipelines/00000000-0000-0000-0000-000000000000",
                          headers=bearer)
    assert res.status_code == 404


def test_delete_idempotent_second_call(bearer, healthz_step_id):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "name": f"idempotent-delete-{uuid.uuid4().hex[:6]}",
        "steps": [{"step_id": healthz_step_id}],
    })
    wf_id = res.json()["workflow_id"]
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf_id}", headers=bearer)

    res = requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf_id}", headers=bearer)
    assert res.status_code == 404


# ── Inline steps ──────────────────────────────────────────────────────────────

def _inline_http_step(name):
    """An inline step (definition on the ref, no step_id) targeting gatekeeper/healthz."""
    return {
        "action": "http",
        "name": name,
        "with": {"service": "gatekeeper", "method": "GET", "path": "/healthz", "expected_status": 200},
        "timeout": 10,
    }


@pytest.fixture
def inline_workflow(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "name": f"inline-pipe-{uuid.uuid4().hex[:6]}",
        "steps": [_inline_http_step("inline-check")],
    })
    assert res.status_code == 201, f"inline create failed: {res.text}"
    wf = res.json()
    yield wf
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf['workflow_id']}", headers=bearer)


def test_inline_step_created_and_enriched(inline_workflow):
    steps = inline_workflow["steps"]
    assert len(steps) == 1
    s = steps[0]
    # An inline step is enriched from its own definition and carries no step_id.
    assert s.get("step_id", "") == ""
    assert s["action"] == "http"
    assert s["name"] == "inline-check"


def test_inline_step_not_in_shared_library(bearer, inline_workflow):
    # An inline step must NOT create an entry in the reusable step library.
    res = requests.get(f"{WORKFLOWS_URL}/steps", headers=bearer)
    assert res.status_code == 200
    assert "inline-check" not in [s["name"] for s in res.json()]


def test_inline_step_triggers_a_run(bearer, inline_workflow):
    wf_id = inline_workflow["workflow_id"]
    res = requests.post(f"{WORKFLOWS_URL}/pipelines/{wf_id}/runs", headers=bearer, json={"inputs": {}})
    assert res.status_code in (200, 201, 202), res.text


def test_raw_exposes_inline_step_refs(bearer, inline_workflow):
    wf_id = inline_workflow["workflow_id"]
    res = requests.get(f"{WORKFLOWS_URL}/pipelines/{wf_id}?raw=true", headers=bearer)
    assert res.status_code == 200
    body = res.json()
    assert "step_refs" in body, "?raw=true must expose the stored step refs"
    refs = body["step_refs"]
    assert len(refs) == 1
    assert refs[0]["action"] == "http"
    assert refs[0]["name"] == "inline-check"
    assert refs[0].get("step_id", "") == ""


def test_inline_step_requires_name(bearer):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "name": f"inline-noname-{uuid.uuid4().hex[:6]}",
        "steps": [{"action": "http", "with": {"service": "gatekeeper", "path": "/healthz"}}],
    })
    assert res.status_code == 400


def test_inline_step_rejects_step_id_and_action(bearer, healthz_step_id):
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "name": f"inline-both-{uuid.uuid4().hex[:6]}",
        "steps": [{"step_id": healthz_step_id, "action": "http", "name": "x"}],
    })
    assert res.status_code == 400
