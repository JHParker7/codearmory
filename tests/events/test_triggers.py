"""Integration tests for trigger CRUD and its tenant isolation."""
import uuid

import pytest
import requests

from conftest import EVENTS_URL


def _trigger_body(name, event_type, workflow_id, subject=None):
    leaves = [{"field": "type", "op": "eq", "value": event_type}]
    if subject:
        leaves.append({"field": "subject", "op": "eq", "value": subject})
    return {
        "name": name,
        "match": {"all": leaves},
        "actions": [{"kind": "run_pipeline", "config": {"pipeline_id": workflow_id}}],
    }


# ── Auth ───────────────────────────────────────────────────────────────────────

def test_create_requires_auth():
    res = requests.post(f"{EVENTS_URL}/triggers", json={"name": "x"})
    assert res.status_code == 401

def test_list_requires_auth():
    assert requests.get(f"{EVENTS_URL}/triggers").status_code == 401

def test_get_requires_auth():
    assert requests.get(f"{EVENTS_URL}/triggers/does-not-matter").status_code == 401

def test_update_requires_auth():
    assert requests.put(f"{EVENTS_URL}/triggers/does-not-matter", json={"name": "x"}).status_code == 401

def test_delete_requires_auth():
    assert requests.delete(f"{EVENTS_URL}/triggers/does-not-matter").status_code == 401


# ── Validation ─────────────────────────────────────────────────────────────────

def test_create_rejects_missing_name(bearer, workflow):
    body = _trigger_body("", "repo.push", workflow["workflow_id"])
    res = requests.post(f"{EVENTS_URL}/triggers", headers=bearer, json=body)
    assert res.status_code == 400

def test_create_rejects_no_actions(bearer):
    res = requests.post(f"{EVENTS_URL}/triggers", headers=bearer, json={
        "name": "no-actions",
        "match": {"all": [{"field": "type", "op": "eq", "value": "repo.push"}]},
        "actions": [],
    })
    assert res.status_code == 400

def test_create_rejects_invalid_json(bearer):
    res = requests.post(f"{EVENTS_URL}/triggers",
                        headers={**bearer, "Content-Type": "application/json"},
                        data="not-json")
    assert res.status_code == 400


# ── CRUD round trip ────────────────────────────────────────────────────────────

@pytest.fixture(scope="module")
def trigger(bearer, workflow):
    name = f"ci-push-{uuid.uuid4().hex[:6]}"
    res = requests.post(f"{EVENTS_URL}/triggers", headers=bearer,
                        json=_trigger_body(name, "repo.push", workflow["workflow_id"], "ci/myapp"))
    assert res.status_code == 201, f"create failed: {res.text}"
    t = res.json()
    yield t
    requests.delete(f"{EVENTS_URL}/triggers/{t['id']}", headers=bearer)


def test_create_returns_the_trigger(trigger, workflow):
    assert trigger["id"]
    assert trigger["enabled"] is True
    assert trigger["actions"][0]["kind"] == "run_pipeline"
    assert trigger["actions"][0]["config"]["pipeline_id"] == workflow["workflow_id"]

def test_list_includes_it(bearer, trigger):
    res = requests.get(f"{EVENTS_URL}/triggers", headers=bearer)
    assert res.status_code == 200
    assert any(t["id"] == trigger["id"] for t in res.json())

def test_get_returns_it(bearer, trigger):
    res = requests.get(f"{EVENTS_URL}/triggers/{trigger['id']}", headers=bearer)
    assert res.status_code == 200
    assert res.json()["name"] == trigger["name"]

def test_update_replaces_mutable_fields(bearer, trigger, workflow):
    body = _trigger_body(trigger["name"] + "-renamed", "repo.pull_request.opened", workflow["workflow_id"])
    body["enabled"] = False
    res = requests.put(f"{EVENTS_URL}/triggers/{trigger['id']}", headers=bearer, json=body)
    assert res.status_code == 200, res.text
    got = res.json()
    assert got["name"].endswith("-renamed")
    assert got["enabled"] is False
    # Identity and tenant are fixed at creation.
    assert got["id"] == trigger["id"]
    # Restore so later tests see an enabled trigger.
    body["enabled"] = True
    requests.put(f"{EVENTS_URL}/triggers/{trigger['id']}", headers=bearer, json=body)


# ── Tenant isolation ───────────────────────────────────────────────────────────
#
# Another tenant's trigger must be reported as missing, not forbidden: a 403 would confirm
# the id exists for someone else.

def test_other_tenant_cannot_list_it(other_bearer, trigger):
    res = requests.get(f"{EVENTS_URL}/triggers", headers=other_bearer)
    assert res.status_code == 200
    assert not any(t["id"] == trigger["id"] for t in res.json())

def test_other_tenant_gets_404(other_bearer, trigger):
    res = requests.get(f"{EVENTS_URL}/triggers/{trigger['id']}", headers=other_bearer)
    assert res.status_code == 404

def test_other_tenant_cannot_delete(other_bearer, bearer, trigger):
    requests.delete(f"{EVENTS_URL}/triggers/{trigger['id']}", headers=other_bearer)
    # Whatever the status, the trigger must survive.
    res = requests.get(f"{EVENTS_URL}/triggers/{trigger['id']}", headers=bearer)
    assert res.status_code == 200, "another tenant's delete removed the trigger"


def test_delete_removes_it(bearer, workflow):
    res = requests.post(f"{EVENTS_URL}/triggers", headers=bearer,
                        json=_trigger_body(f"tmp-{uuid.uuid4().hex[:6]}", "repo.push", workflow["workflow_id"]))
    assert res.status_code == 201
    tid = res.json()["id"]
    assert requests.delete(f"{EVENTS_URL}/triggers/{tid}", headers=bearer).status_code == 204
    assert requests.get(f"{EVENTS_URL}/triggers/{tid}", headers=bearer).status_code == 404


# ── test-match ─────────────────────────────────────────────────────────────────

def test_match_dry_run_hits(bearer):
    res = requests.post(f"{EVENTS_URL}/triggers/test-match", headers=bearer, json={
        "match": {"all": [
            {"field": "type", "op": "eq", "value": "repo.push"},
            {"field": "data.ref", "op": "eq", "value": "main"},
        ]},
        "event": {
            "type": "repo.push", "source": "git", "subject": "ci/myapp",
            "data": {"ref": "main"},
        },
    })
    assert res.status_code == 200, res.text
    assert res.json()["matched"] is True

def test_match_dry_run_misses(bearer):
    res = requests.post(f"{EVENTS_URL}/triggers/test-match", headers=bearer, json={
        "match": {"all": [{"field": "data.ref", "op": "eq", "value": "main"}]},
        "event": {
            "type": "repo.push", "source": "git", "subject": "ci/myapp",
            "data": {"ref": "dev"},
        },
    })
    assert res.status_code == 200
    assert res.json()["matched"] is False

def test_match_dry_run_requires_auth():
    res = requests.post(f"{EVENTS_URL}/triggers/test-match", json={"match": {}, "event": {}})
    assert res.status_code == 401
