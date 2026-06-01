"""Integration tests for pipeline rule CRUD."""
import pytest
import requests

from conftest import HOOKS_URL


# ── Authentication ─────────────────────────────────────────────────────────────

def test_create_rule_unauthorized():
    res = requests.post(f"{HOOKS_URL}/rules", json={"name": "x"})
    assert res.status_code == 401

def test_list_rules_unauthorized():
    res = requests.get(f"{HOOKS_URL}/rules")
    assert res.status_code == 401

def test_get_rule_unauthorized():
    res = requests.get(f"{HOOKS_URL}/rules/does-not-matter")
    assert res.status_code == 401

def test_update_rule_unauthorized():
    res = requests.put(f"{HOOKS_URL}/rules/does-not-matter", json={"name": "x"})
    assert res.status_code == 401

def test_delete_rule_unauthorized():
    res = requests.delete(f"{HOOKS_URL}/rules/does-not-matter")
    assert res.status_code == 401


# ── Create validation ──────────────────────────────────────────────────────────

def test_create_rule_missing_name(bearer, workflow):
    res = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "repo": "org/repo", "events": ["push"],
        "workflow_id": workflow["workflow_id"],
    })
    assert res.status_code == 400

def test_create_rule_missing_repo(bearer, workflow):
    res = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": "my-rule", "events": ["push"],
        "workflow_id": workflow["workflow_id"],
    })
    assert res.status_code == 400

def test_create_rule_empty_events(bearer, workflow):
    res = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": "my-rule", "repo": "org/repo", "events": [],
        "workflow_id": workflow["workflow_id"],
    })
    assert res.status_code == 400

def test_create_rule_missing_workflow_id(bearer):
    res = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": "my-rule", "repo": "org/repo", "events": ["push"],
    })
    assert res.status_code == 400


# ── CRUD happy path ────────────────────────────────────────────────────────────

@pytest.fixture
def rule(bearer, workflow):
    res = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": "deploy-on-push",
        "repo": "myorg/myrepo",
        "events": ["push"],
        "ref_filter": "refs/heads/main",
        "workflow_id": workflow["workflow_id"],
        "input_mapping": {"COMMIT": "commit", "BRANCH": "ref"},
        "secret": "rule-test-secret",
    })
    assert res.status_code == 201, res.text
    r = res.json()
    yield r
    requests.delete(f"{HOOKS_URL}/rules/{r['rule_id']}", headers=bearer)


def test_create_rule_returns_201(rule):
    assert rule["rule_id"] != ""
    assert rule["name"] == "deploy-on-push"
    assert rule["repo"] == "myorg/myrepo"
    assert "push" in rule["events"]
    assert rule["ref_filter"] == "refs/heads/main"
    assert rule["active"] is True

def test_create_rule_secret_not_in_response(bearer, workflow):
    res = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": "secret-rule",
        "repo": "org/secretrepo",
        "events": ["push"],
        "workflow_id": workflow["workflow_id"],
        "secret": "my-webhook-secret",
    })
    assert res.status_code == 201
    data = res.json()
    rid = data["rule_id"]
    assert "secret" not in data or data.get("secret") in (None, "")
    requests.delete(f"{HOOKS_URL}/rules/{rid}", headers=bearer)

def test_list_includes_own_rule(bearer, rule):
    res = requests.get(f"{HOOKS_URL}/rules", headers=bearer)
    assert res.status_code == 200
    ids = [r["rule_id"] for r in res.json()]
    assert rule["rule_id"] in ids

def test_list_excludes_other_users_rules(other_bearer, rule):
    res = requests.get(f"{HOOKS_URL}/rules", headers=other_bearer)
    assert res.status_code == 200
    ids = [r["rule_id"] for r in res.json()]
    assert rule["rule_id"] not in ids

def test_get_own_rule(bearer, rule):
    res = requests.get(f"{HOOKS_URL}/rules/{rule['rule_id']}", headers=bearer)
    assert res.status_code == 200
    assert res.json()["rule_id"] == rule["rule_id"]

def test_get_other_user_rule_not_found(other_bearer, rule):
    res = requests.get(f"{HOOKS_URL}/rules/{rule['rule_id']}", headers=other_bearer)
    assert res.status_code == 404

def test_get_nonexistent_rule(bearer):
    res = requests.get(f"{HOOKS_URL}/rules/00000000-0000-0000-0000-000000000000",
                       headers=bearer)
    assert res.status_code == 404

def test_update_rule(bearer, rule, workflow):
    res = requests.put(f"{HOOKS_URL}/rules/{rule['rule_id']}", headers=bearer, json={
        "name": "updated-rule",
        "repo": "myorg/myrepo",
        "events": ["push", "merge"],
        "workflow_id": workflow["workflow_id"],
    })
    assert res.status_code == 200
    data = res.json()
    assert data["name"] == "updated-rule"
    assert "merge" in data["events"]
    assert data["ref_filter"] == ""

def test_update_other_user_rule_not_found(other_bearer, rule, workflow):
    res = requests.put(f"{HOOKS_URL}/rules/{rule['rule_id']}", headers=other_bearer, json={
        "name": "hijack", "repo": "x", "events": ["push"],
        "workflow_id": workflow["workflow_id"],
    })
    assert res.status_code == 404

def test_delete_rule(bearer, workflow):
    res = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": "to-delete", "repo": "x/y",
        "events": ["push"], "workflow_id": workflow["workflow_id"],
        "secret": "delete-test-secret",
    })
    rid = res.json()["rule_id"]

    res = requests.delete(f"{HOOKS_URL}/rules/{rid}", headers=bearer)
    assert res.status_code == 204

    res = requests.get(f"{HOOKS_URL}/rules/{rid}", headers=bearer)
    assert res.status_code == 404

def test_delete_other_user_rule_not_found(bearer, other_bearer, workflow):
    res = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": "protected-rule", "repo": "x/y",
        "events": ["push"], "workflow_id": workflow["workflow_id"],
        "secret": "protected-test-secret",
    })
    rid = res.json()["rule_id"]

    res = requests.delete(f"{HOOKS_URL}/rules/{rid}", headers=other_bearer)
    assert res.status_code == 404

    requests.delete(f"{HOOKS_URL}/rules/{rid}", headers=bearer)
