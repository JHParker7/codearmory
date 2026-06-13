"""Integration tests for API shapes that the forge TUI depends on."""
import time

import pytest
import requests

from conftest import FORGE_URL


def _poll_until_done(bearer, execution_id, timeout=60):
    """Poll GET /executions/{id} until the execution reaches a terminal state."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        res = requests.get(f"{FORGE_URL}/executions/{execution_id}", headers=bearer)
        assert res.status_code == 200, f"poll got {res.status_code}: {res.text}"
        data = res.json()
        if data["status"] in ("completed", "failed", "timed_out", "cancelled"):
            return data
        time.sleep(2)
    pytest.fail(f"execution {execution_id} did not complete within {timeout}s")


# ── List endpoint: JSON shape ─────────────────────────────────────────────────

def test_list_returns_array(bearer):
    res = requests.get(f"{FORGE_URL}/executions", headers=bearer)
    assert res.status_code == 200
    assert isinstance(res.json(), list)


def test_list_item_has_required_fields(bearer):
    """The TUI reads execution_id, status, image, runner_class, created_at from the list."""
    submit = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["echo", "tui-shape-check"],
        "timeout": 30,
    })
    assert submit.status_code == 202, submit.text

    res = requests.get(f"{FORGE_URL}/executions", headers=bearer)
    assert res.status_code == 200
    items = res.json()
    assert len(items) > 0
    item = items[0]
    for field in ("execution_id", "status", "image", "runner_class", "created_at"):
        assert field in item, f"missing field: {field}"


# ── Detail endpoint: JSON shape ───────────────────────────────────────────────

def test_detail_has_output_fields(bearer):
    """The TUI detail view reads stdout and stderr from GET /executions/{id}.

    stdout/stderr are omitempty pointer fields, so they are absent while the
    execution is still pending. Poll to a terminal state first; forge always
    persists both columns when an execution completes, so the keys must appear.
    """
    submit = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["echo", "hello-from-tui"],
        "timeout": 30,
    })
    assert submit.status_code == 202, submit.text
    eid = submit.json()["execution_id"]

    body = _poll_until_done(bearer, eid)
    assert "execution_id" in body
    assert "status" in body
    assert "stdout" in body, body
    assert "stderr" in body, body


def test_detail_not_found_returns_404(bearer):
    res = requests.get(f"{FORGE_URL}/executions/does-not-exist", headers=bearer)
    assert res.status_code == 404


# ── Cancel endpoint ───────────────────────────────────────────────────────────

def test_cancel_nonexistent_returns_error(bearer):
    res = requests.delete(f"{FORGE_URL}/executions/does-not-exist", headers=bearer)
    assert res.status_code in (404, 400)


def test_cancel_own_execution(bearer):
    """The TUI sends DELETE /executions/{id} to cancel pending/running execs."""
    submit = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["sleep", "120"],
        "timeout": 180,
    })
    assert submit.status_code == 202, submit.text
    eid = submit.json()["execution_id"]

    res = requests.delete(f"{FORGE_URL}/executions/{eid}", headers=bearer)
    assert res.status_code in (200, 204, 409)  # 409 if already done


def test_cancel_other_users_execution_forbidden(bearer, other_bearer):
    """TUI cancel must not allow cancelling another user's execution."""
    submit = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["sleep", "120"],
        "timeout": 180,
    })
    assert submit.status_code == 202, submit.text
    eid = submit.json()["execution_id"]

    res = requests.delete(f"{FORGE_URL}/executions/{eid}", headers=other_bearer)
    assert res.status_code in (403, 404)


# ── Status values the TUI uses for conditional logic ─────────────────────────

def test_status_field_is_string(bearer):
    submit = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["true"],
        "timeout": 30,
    })
    assert submit.status_code == 202, submit.text
    eid = submit.json()["execution_id"]

    res = requests.get(f"{FORGE_URL}/executions/{eid}", headers=bearer)
    assert res.status_code == 200
    assert isinstance(res.json()["status"], str)


# ── Isolation: list is user-scoped ────────────────────────────────────────────

def test_list_isolation(bearer, other_bearer):
    """Users should only see their own executions in the list."""
    submit = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["echo", "mine"],
        "timeout": 30,
    })
    assert submit.status_code == 202, submit.text
    eid = submit.json()["execution_id"]

    other_list = requests.get(f"{FORGE_URL}/executions", headers=other_bearer)
    assert other_list.status_code == 200
    other_ids = [e["execution_id"] for e in other_list.json()]
    assert eid not in other_ids
