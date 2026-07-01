"""Integration tests for Forge shared workspace volumes.

Exercises the full lifecycle against a running forge (docker runtime in the compose
stack): create a volume, write into it from one execution, read it back from a
second execution (proving cross-step sharing — the whole point), enforce the
per-workflow size cap, isolate volumes per user, and tear them down.
"""
import time
import uuid

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


def run_with_volume(bearer, workflow_id, name, command, mount_path="/workspace", workdir=False):
    """Submit an execution that attaches the (workflow_id, name) volume and poll it."""
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["sh", "-c", command],
        "timeout": 60,
        "volumes": [{
            "workflow_id": workflow_id,
            "name": name,
            "mount_path": mount_path,
            "workdir": workdir,
        }],
    })
    assert res.status_code == 202, f"submit got {res.status_code}: {res.text}"
    return poll_until_done(bearer, res.json()["execution_id"])


# ── Auth ────────────────────────────────────────────────────────────────────────

def test_create_volume_unauthorized():
    res = requests.post(f"{FORGE_URL}/volumes", json={"workflow_id": "wf", "name": "ws"})
    assert res.status_code == 401


def test_delete_volume_requires_workflow_id(bearer):
    res = requests.delete(f"{FORGE_URL}/volumes", headers=bearer)
    assert res.status_code == 400


# ── Lifecycle ─────────────────────────────────────────────────────────────────

def test_volume_shared_across_executions(bearer):
    """A file written by one execution is visible to a later execution that attaches
    the same volume — the core cross-step sharing guarantee."""
    workflow_id = f"itest-{uuid.uuid4().hex[:8]}"
    try:
        # Create the volume.
        res = requests.post(f"{FORGE_URL}/volumes", headers=bearer, json={
            "workflow_id": workflow_id, "name": "workspace", "size_mb": 128, "medium": "memory",
        })
        assert res.status_code == 201, f"create volume: {res.status_code} {res.text}"
        vol = res.json()
        assert vol["status"] == "active"
        assert vol["workflow_id"] == workflow_id
        assert vol["mount_path"] == "/workspace"

        # Step 1: write a marker into the volume.
        marker = uuid.uuid4().hex
        w = run_with_volume(bearer, workflow_id, "workspace", f"echo {marker} > /workspace/out.txt")
        assert w["status"] == "completed", f"write step: {w}"

        # Step 2: a separate execution reads it back.
        r = run_with_volume(bearer, workflow_id, "workspace", "cat /workspace/out.txt")
        assert r["status"] == "completed", f"read step: {r}"
        assert marker in (r.get("stdout") or ""), f"marker not shared across executions: {r}"
    finally:
        requests.delete(f"{FORGE_URL}/volumes?workflow_id={workflow_id}", headers=bearer)


def test_volume_workdir_sets_working_directory(bearer):
    """A mount with workdir=true runs the command inside the volume."""
    workflow_id = f"itest-{uuid.uuid4().hex[:8]}"
    try:
        res = requests.post(f"{FORGE_URL}/volumes", headers=bearer, json={
            "workflow_id": workflow_id, "name": "workspace", "size_mb": 64,
        })
        assert res.status_code == 201, res.text
        out = run_with_volume(bearer, workflow_id, "workspace", "pwd", workdir=True)
        assert out["status"] == "completed", out
        assert "/workspace" in (out.get("stdout") or ""), out
    finally:
        requests.delete(f"{FORGE_URL}/volumes?workflow_id={workflow_id}", headers=bearer)


def test_create_volume_idempotent(bearer):
    """Creating the same (workflow_id, name) twice is a no-op that returns the row."""
    workflow_id = f"itest-{uuid.uuid4().hex[:8]}"
    try:
        body = {"workflow_id": workflow_id, "name": "workspace", "size_mb": 64}
        first = requests.post(f"{FORGE_URL}/volumes", headers=bearer, json=body)
        assert first.status_code == 201, first.text
        second = requests.post(f"{FORGE_URL}/volumes", headers=bearer, json=body)
        assert second.status_code == 200, f"idempotent create should be 200: {second.status_code} {second.text}"
        assert second.json()["resource_name"] == first.json()["resource_name"]
    finally:
        requests.delete(f"{FORGE_URL}/volumes?workflow_id={workflow_id}", headers=bearer)


def test_attach_nonexistent_volume_rejected(bearer):
    """Submitting an execution that attaches an uncreated volume is a 400."""
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["sh", "-c", "true"],
        "volumes": [{"workflow_id": f"itest-{uuid.uuid4().hex[:8]}", "name": "nope"}],
    })
    assert res.status_code == 400
    assert "not found" in res.text.lower()


def test_workflow_volume_cap_enforced(bearer):
    """The summed size of a workflow's volumes is capped; the over-cap create is 409."""
    workflow_id = f"itest-{uuid.uuid4().hex[:8]}"
    try:
        # The default per-workflow cap is 10 GiB (10240 MB). Two 6 GiB volumes exceed it.
        big = 6 * 1024
        first = requests.post(f"{FORGE_URL}/volumes", headers=bearer, json={
            "workflow_id": workflow_id, "name": "one", "size_mb": big,
        })
        assert first.status_code == 201, f"first volume: {first.status_code} {first.text}"
        second = requests.post(f"{FORGE_URL}/volumes", headers=bearer, json={
            "workflow_id": workflow_id, "name": "two", "size_mb": big,
        })
        assert second.status_code == 409, f"cap should reject the second volume: {second.status_code} {second.text}"
    finally:
        requests.delete(f"{FORGE_URL}/volumes?workflow_id={workflow_id}", headers=bearer)


def test_volume_owner_isolation(bearer, other_bearer):
    """A second user can neither see nor delete the first user's volumes."""
    workflow_id = f"itest-{uuid.uuid4().hex[:8]}"
    try:
        res = requests.post(f"{FORGE_URL}/volumes", headers=bearer, json={
            "workflow_id": workflow_id, "name": "workspace", "size_mb": 64,
        })
        assert res.status_code == 201, res.text

        # Other user lists this workflow → sees nothing.
        listed = requests.get(f"{FORGE_URL}/volumes?workflow_id={workflow_id}", headers=other_bearer)
        assert listed.status_code == 200
        assert listed.json() == []

        # Other user's delete removes nothing.
        deleted = requests.delete(f"{FORGE_URL}/volumes?workflow_id={workflow_id}", headers=other_bearer)
        assert deleted.status_code == 200
        assert deleted.json()["deleted"] == 0

        # Owner still sees it.
        owner_list = requests.get(f"{FORGE_URL}/volumes?workflow_id={workflow_id}", headers=bearer)
        assert owner_list.status_code == 200
        assert len(owner_list.json()) == 1
    finally:
        requests.delete(f"{FORGE_URL}/volumes?workflow_id={workflow_id}", headers=bearer)


def test_teardown_removes_volumes(bearer):
    """DELETE by workflow_id removes every volume for the workflow, idempotently."""
    workflow_id = f"itest-{uuid.uuid4().hex[:8]}"
    for name in ("workspace", "cache"):
        res = requests.post(f"{FORGE_URL}/volumes", headers=bearer, json={
            "workflow_id": workflow_id, "name": name, "size_mb": 64,
        })
        assert res.status_code == 201, res.text

    first = requests.delete(f"{FORGE_URL}/volumes?workflow_id={workflow_id}", headers=bearer)
    assert first.status_code == 200
    assert first.json()["deleted"] == 2

    # Gone now.
    listed = requests.get(f"{FORGE_URL}/volumes?workflow_id={workflow_id}", headers=bearer)
    assert listed.status_code == 200
    assert listed.json() == []

    # Idempotent second teardown.
    second = requests.delete(f"{FORGE_URL}/volumes?workflow_id={workflow_id}", headers=bearer)
    assert second.status_code == 200
    assert second.json()["deleted"] == 0
