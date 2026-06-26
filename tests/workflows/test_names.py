"""Integration tests for name-based addressing of workflows, steps, and runs.

Pipelines/steps/runs are addressed by their unique name (scoped to the owner) as
well as by id, and names are enforced unique per owner. These exercise that path
end-to-end through the workflows service + gatekeeper permission check.
"""
import uuid

import requests

from conftest import WORKFLOWS_URL

HTTP_STEP_WITH = {
    "service": "gatekeeper",
    "method": "GET",
    "path": "/healthz",
    "expected_status": 200,
}


def _create_workflow(bearer, name, step_id):
    return requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "name": name, "steps": [{"step_id": step_id}],
    })


# ── Workflow name uniqueness + validation ───────────────────────────────────

def test_duplicate_workflow_name_conflicts(bearer, healthz_step_id):
    name = f"dup-wf-{uuid.uuid4().hex[:8]}"
    r1 = _create_workflow(bearer, name, healthz_step_id)
    assert r1.status_code == 201, r1.text
    try:
        r2 = _create_workflow(bearer, name, healthz_step_id)
        assert r2.status_code == 409, f"expected 409, got {r2.status_code}: {r2.text}"
    finally:
        requests.delete(f"{WORKFLOWS_URL}/pipelines/{r1.json()['workflow_id']}", headers=bearer)


def test_invalid_workflow_name_rejected(bearer, healthz_step_id):
    for bad in ["has space", "has/slash"]:
        r = _create_workflow(bearer, bad, healthz_step_id)
        assert r.status_code == 400, f"name {bad!r}: expected 400, got {r.status_code}: {r.text}"


# ── Workflow name-based addressing ──────────────────────────────────────────

def test_get_workflow_by_name_and_id(bearer, healthz_step_id):
    name = f"by-name-{uuid.uuid4().hex[:8]}"
    r = _create_workflow(bearer, name, healthz_step_id)
    assert r.status_code == 201, r.text
    wf_id = r.json()["workflow_id"]
    try:
        by_name = requests.get(f"{WORKFLOWS_URL}/pipelines/{name}", headers=bearer)
        assert by_name.status_code == 200, by_name.text
        assert by_name.json()["workflow_id"] == wf_id
        by_id = requests.get(f"{WORKFLOWS_URL}/pipelines/{wf_id}", headers=bearer)
        assert by_id.status_code == 200
        assert by_id.json()["name"] == name
    finally:
        requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf_id}", headers=bearer)


def test_workflow_name_is_owner_scoped(bearer, other_bearer, healthz_step_id):
    name = f"private-{uuid.uuid4().hex[:8]}"
    r = _create_workflow(bearer, name, healthz_step_id)
    assert r.status_code == 201, r.text
    wf_id = r.json()["workflow_id"]
    try:
        # A different user must not resolve this workflow by its name.
        res = requests.get(f"{WORKFLOWS_URL}/pipelines/{name}", headers=other_bearer)
        assert res.status_code == 404, f"expected 404, got {res.status_code}: {res.text}"
    finally:
        requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf_id}", headers=bearer)


def test_rename_to_existing_name_conflicts(bearer, healthz_step_id):
    n1 = f"rn-a-{uuid.uuid4().hex[:8]}"
    n2 = f"rn-b-{uuid.uuid4().hex[:8]}"
    a = _create_workflow(bearer, n1, healthz_step_id)
    b = _create_workflow(bearer, n2, healthz_step_id)
    assert a.status_code == 201 and b.status_code == 201, f"{a.text} / {b.text}"
    a_id, b_id = a.json()["workflow_id"], b.json()["workflow_id"]
    try:
        # Renaming B to A's name conflicts.
        clash = requests.put(f"{WORKFLOWS_URL}/pipelines/{b_id}", headers=bearer, json={
            "name": n1, "steps": [{"step_id": healthz_step_id}],
        })
        assert clash.status_code == 409, f"expected 409, got {clash.status_code}: {clash.text}"
        # Keeping B's own name is allowed.
        same = requests.put(f"{WORKFLOWS_URL}/pipelines/{b_id}", headers=bearer, json={
            "name": n2, "steps": [{"step_id": healthz_step_id}],
        })
        assert same.status_code == 200, same.text
    finally:
        requests.delete(f"{WORKFLOWS_URL}/pipelines/{a_id}", headers=bearer)
        requests.delete(f"{WORKFLOWS_URL}/pipelines/{b_id}", headers=bearer)


# ── Step name-based addressing + uniqueness ─────────────────────────────────

def test_duplicate_step_name_conflicts_and_addressable(bearer):
    name = f"dup-step-{uuid.uuid4().hex[:8]}"
    body = {"name": name, "action": "http", "with": HTTP_STEP_WITH, "timeout": 10}
    r1 = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json=body)
    assert r1.status_code == 201, r1.text
    step_id = r1.json()["step_id"]
    try:
        r2 = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json=body)
        assert r2.status_code == 409, f"expected 409, got {r2.status_code}: {r2.text}"
        by_name = requests.get(f"{WORKFLOWS_URL}/steps/{name}", headers=bearer)
        assert by_name.status_code == 200, by_name.text
        assert by_name.json()["step_id"] == step_id
    finally:
        requests.delete(f"{WORKFLOWS_URL}/steps/{step_id}", headers=bearer)


def test_invalid_step_name_rejected(bearer):
    r = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json={
        "name": "bad step name", "action": "http", "with": HTTP_STEP_WITH,
    })
    assert r.status_code == 400, f"expected 400, got {r.status_code}: {r.text}"


# ── Runs namespaced under the workflow name ─────────────────────────────────

def test_run_namespaced_under_workflow_name(bearer, healthz_step_id):
    name = f"runns-{uuid.uuid4().hex[:8]}"
    wf = _create_workflow(bearer, name, healthz_step_id)
    assert wf.status_code == 201, wf.text
    wf_id = wf.json()["workflow_id"]
    try:
        # Trigger the run addressing the workflow by name.
        trig = requests.post(f"{WORKFLOWS_URL}/pipelines/{name}/runs", headers=bearer, json={})
        assert trig.status_code == 202, f"trigger by name got {trig.status_code}: {trig.text}"
        run_id = trig.json()["run_id"]
        # Fetch the run via the workflow-namespaced nested route.
        nested = requests.get(f"{WORKFLOWS_URL}/pipelines/{name}/runs/{run_id}", headers=bearer)
        assert nested.status_code == 200, f"nested get got {nested.status_code}: {nested.text}"
        assert nested.json()["run_id"] == run_id
        # The flat route still works (back-compat).
        flat = requests.get(f"{WORKFLOWS_URL}/runs/{run_id}", headers=bearer)
        assert flat.status_code == 200, flat.text
        # A run fetched under the WRONG workflow ref is not found.
        wrong = requests.get(f"{WORKFLOWS_URL}/pipelines/some-other-name/runs/{run_id}", headers=bearer)
        assert wrong.status_code == 404, f"expected 404 for mismatched workflow ref, got {wrong.status_code}"
    finally:
        requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf_id}", headers=bearer)
