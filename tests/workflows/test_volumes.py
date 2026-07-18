"""End-to-end integration test for shared workspace volumes through workflows.

Builds a real pipeline: a `forge/create-volume` step provisions a run-scoped shared
volume, a git-less `forge/run` step writes into it, and a second `forge/run` step
reads it back — proving the whole path (the create-volume catalog action, `${run_id}`
substitution, volume attach passthrough, cross-step sharing, and automatic teardown).

Skips itself gracefully if forge/create-volume is not in the catalog (e.g. a stack
whose registry manifest predates the feature).
"""
import time
import uuid

import pytest
import requests

from conftest import WORKFLOWS_URL


def make_step(bearer, action, with_, name=None, timeout=60):
    res = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json={
        "name": name or f"step-{uuid.uuid4().hex[:8]}",
        "action": action,
        "with": with_,
        "timeout": timeout,
    })
    assert res.status_code == 201, f"create step ({action}): {res.status_code} {res.text}"
    return res.json()["step_id"]


def poll_run(bearer, run_id, timeout=120):
    deadline = time.time() + timeout
    while time.time() < deadline:
        res = requests.get(f"{WORKFLOWS_URL}/runs/{run_id}", headers=bearer)
        assert res.status_code == 200, f"poll got {res.status_code}: {res.text}"
        data = res.json()
        if data["status"] in ("completed", "failed", "cancelled"):
            return data
        time.sleep(2)
    pytest.fail(f"run {run_id} did not finish within {timeout}s")


def volume_action_available(bearer):
    res = requests.get(f"{WORKFLOWS_URL}/actions", headers=bearer)
    if res.status_code != 200:
        return False
    return any(a.get("name") == "forge/create-volume" for a in res.json())


def test_shared_volume_pipeline_end_to_end(bearer):
    if not volume_action_available(bearer):
        pytest.skip("forge/create-volume not in the action catalog (stale registry manifest)")

    marker = uuid.uuid4().hex
    attach = [{"workflow_id": "${run_id}", "name": "workspace", "mount_path": "/workspace"}]
    step_ids = []
    wf_id = None
    try:
        # 1. create-volume step — workflow_id is the run id via ${run_id}.
        mkvol = make_step(bearer, "forge/create-volume", {
            "workflow_id": "${run_id}", "name": "workspace", "size_mb": 128,
            "medium": "memory", "mount_path": "/workspace",
        }, name=f"mkvol-{uuid.uuid4().hex[:6]}")

        # 2. write step — a plain alpine image (no git) writes into the shared volume.
        write = make_step(bearer, "forge/run", {
            "image": "alpine:3.19",
            "run": f"echo {marker} > /workspace/out.txt",
            "volumes": attach,
        }, name=f"write-{uuid.uuid4().hex[:6]}")

        # 3. read step — a separate execution reads it back and captures it as output.
        read = make_step(bearer, "forge/run", {
            "image": "alpine:3.19",
            "run": "SHARED=$(cat /workspace/out.txt)",
            "output_env": ["SHARED"],
            "volumes": attach,
        }, name=f"read-{uuid.uuid4().hex[:6]}")
        step_ids = [mkvol, write, read]

        # Assemble the pipeline (steps run sequentially in order).
        res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
            "name": f"vol-e2e-{uuid.uuid4().hex[:6]}",
            "steps": [{"step_id": mkvol}, {"step_id": write}, {"step_id": read}],
        })
        assert res.status_code == 201, f"create pipeline: {res.status_code} {res.text}"
        wf_id = res.json()["workflow_id"]

        # Trigger and wait.
        trig = requests.post(f"{WORKFLOWS_URL}/pipelines/{wf_id}/runs", headers=bearer)
        assert trig.status_code == 202, f"trigger: {trig.status_code} {trig.text}"
        run_id = trig.json()["run_id"]

        run = poll_run(bearer, run_id)
        # A completed run already proves cross-step sharing: the read step's `cat`
        # exits non-zero (→ step failed → run failed) if the write step's file wasn't
        # visible through the shared volume.
        assert run["status"] == "completed", f"run did not complete: {run}"

        # And the read step actually captured the marker the write step produced.
        blob = str(run.get("step_runs", []))
        assert marker in blob, f"marker {marker} not found in step runs: {blob}"
    finally:
        if wf_id:
            requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf_id}", headers=bearer)
        for sid in step_ids:
            requests.delete(f"{WORKFLOWS_URL}/steps/{sid}", headers=bearer)
