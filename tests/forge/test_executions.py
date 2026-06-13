"""Integration tests for Forge execution endpoints."""
import time

import pytest
import requests

from conftest import FORGE_URL


def poll_until_done(bearer, execution_id, timeout=60):
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


# ── Authentication ────────────────────────────────────────────────────────────

def test_submit_unauthorized():
    res = requests.post(f"{FORGE_URL}/executions", json={
        "image": "alpine:3.19",
        "command": ["echo", "hi"],
    })
    assert res.status_code == 401


def test_get_unauthorized():
    res = requests.get(f"{FORGE_URL}/executions/does-not-matter")
    assert res.status_code == 401


def test_list_unauthorized():
    res = requests.get(f"{FORGE_URL}/executions")
    assert res.status_code == 401


def test_cancel_unauthorized():
    res = requests.delete(f"{FORGE_URL}/executions/does-not-matter")
    assert res.status_code == 401


# ── Submit validation ─────────────────────────────────────────────────────────

def test_submit_invalid_json(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, data=b"not json")
    assert res.status_code == 400


def test_submit_missing_image(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "command": ["echo", "hi"],
    })
    assert res.status_code == 400


def test_submit_missing_command(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
    })
    assert res.status_code == 400


# ── Full execution lifecycle ──────────────────────────────────────────────────

def test_submit_and_complete(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["sh", "-c", "echo hello"],
        "timeout": 30,
    })
    assert res.status_code == 202, f"submit got {res.status_code}: {res.text}"
    body = res.json()
    assert "execution_id" in body
    execution_id = body["execution_id"]

    result = poll_until_done(bearer, execution_id)
    assert result["status"] == "completed"
    assert result["exit_code"] == 0
    assert "hello" in (result.get("stdout") or "")


def test_direct_command_runs_and_returns_output(bearer):
    """Run a command directly (no shell wrapper) and verify its exact stdout —
    the scenario that surfaced the K8s non-root admission bug, where the container
    never started so every command 'failed' with exit 1 and empty output."""
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["echo", "hello world"],
        "timeout": 30,
    })
    assert res.status_code == 202
    execution_id = res.json()["execution_id"]

    result = poll_until_done(bearer, execution_id)
    assert result["status"] == "completed", result
    assert result["exit_code"] == 0, result
    assert (result.get("stdout") or "").strip() == "hello world", result


def test_failed_command(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["sh", "-c", "exit 1"],
        "timeout": 30,
    })
    assert res.status_code == 202
    execution_id = res.json()["execution_id"]

    result = poll_until_done(bearer, execution_id)
    assert result["status"] == "failed"
    assert result["exit_code"] != 0


def test_failed_command_captures_output(bearer):
    """A non-zero exit must still capture stdout and record the real exit code,
    so failures are debuggable rather than an empty 'failed' record."""
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["sh", "-c", "echo before-fail; exit 3"],
        "timeout": 30,
    })
    assert res.status_code == 202
    execution_id = res.json()["execution_id"]

    result = poll_until_done(bearer, execution_id)
    assert result["status"] == "failed"
    assert result["exit_code"] == 3
    assert "before-fail" in (result.get("stdout") or ""), result


def test_stderr_is_captured(bearer):
    """Output written to stderr must be captured so the failure reason is visible.
    Asserted against the combined output to stay runtime-agnostic — the Kubernetes
    runtime merges stdout and stderr into a single stream."""
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["sh", "-c", "echo oops 1>&2; exit 1"],
        "timeout": 30,
    })
    assert res.status_code == 202
    execution_id = res.json()["execution_id"]

    result = poll_until_done(bearer, execution_id)
    assert result["status"] == "failed"
    combined = (result.get("stdout") or "") + (result.get("stderr") or "")
    assert "oops" in combined, f"expected 'oops' in captured output, got {result}"


def test_timeout_enforced(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["sleep", "300"],
        "timeout": 5,
    })
    assert res.status_code == 202
    execution_id = res.json()["execution_id"]

    result = poll_until_done(bearer, execution_id, timeout=90)
    assert result["status"] == "timed_out"


# ── Get / List ────────────────────────────────────────────────────────────────

def test_get_execution(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["echo", "get-test"],
        "timeout": 30,
    })
    assert res.status_code == 202
    execution_id = res.json()["execution_id"]

    res = requests.get(f"{FORGE_URL}/executions/{execution_id}", headers=bearer)
    assert res.status_code == 200
    data = res.json()
    assert data["execution_id"] == execution_id
    assert data["image"] == "alpine:3.19"


def test_get_not_found(bearer):
    res = requests.get(f"{FORGE_URL}/executions/00000000-0000-0000-0000-000000000000", headers=bearer)
    assert res.status_code == 404


def test_list_executions(bearer):
    res = requests.get(f"{FORGE_URL}/executions", headers=bearer)
    assert res.status_code == 200
    assert isinstance(res.json(), list)


# ── Ownership isolation ───────────────────────────────────────────────────────

def test_other_user_cannot_see_execution(bearer, other_bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["echo", "private"],
        "timeout": 30,
    })
    assert res.status_code == 202
    execution_id = res.json()["execution_id"]

    res = requests.get(f"{FORGE_URL}/executions/{execution_id}", headers=other_bearer)
    assert res.status_code == 404


# ── Cancel ────────────────────────────────────────────────────────────────────

def test_cancel_pending_execution(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["sleep", "300"],
        "timeout": 300,
    })
    assert res.status_code == 202
    execution_id = res.json()["execution_id"]

    # Cancel before it is picked up. This races the worker: a still-cancelable
    # execution returns 204, but if the worker already finished it the cancel of a
    # no-longer-cancelable execution returns 409.
    res = requests.delete(f"{FORGE_URL}/executions/{execution_id}", headers=bearer)
    assert res.status_code in (204, 409)

    # Either way, the execution must end in a terminal state.
    result = poll_until_done(bearer, execution_id, timeout=30)
    assert result["status"] in ("cancelled", "completed", "timed_out", "failed")


def test_cancel_not_found(bearer):
    res = requests.delete(f"{FORGE_URL}/executions/00000000-0000-0000-0000-000000000000", headers=bearer)
    assert res.status_code == 404


def test_cancel_already_finished(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["echo", "done"],
        "timeout": 30,
    })
    assert res.status_code == 202
    execution_id = res.json()["execution_id"]

    poll_until_done(bearer, execution_id)

    res = requests.delete(f"{FORGE_URL}/executions/{execution_id}", headers=bearer)
    assert res.status_code == 409
