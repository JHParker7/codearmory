"""Integration tests for the scatter/gather forge primitives against a running forge.

Exercises the full chain the workflows `scatter` step is built on — resolve-paths,
volume-copy (clone), a per-leg run, and volume-copy (gather) — end to end on the
compose stack (docker runtime), proving they compose the way the orchestrator drives
them: partition a workspace by regex, clone it per leg, build the parts in parallel on
independent volumes, then union the owned outputs back into the base.
"""
import time
import uuid

import pytest
import requests

from conftest import FORGE_URL


def poll_until_done(bearer, execution_id, timeout=120):
    deadline = time.time() + timeout
    while time.time() < deadline:
        res = requests.get(f"{FORGE_URL}/executions/{execution_id}", headers=bearer)
        assert res.status_code == 200, f"poll got {res.status_code}: {res.text}"
        data = res.json()
        if data["status"] in ("completed", "failed", "timed_out", "cancelled"):
            return data
        time.sleep(2)
    pytest.fail(f"execution {execution_id} did not complete within {timeout}s")


def submit(bearer, body):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json=body)
    assert res.status_code == 202, f"submit got {res.status_code}: {res.text}"
    return poll_until_done(bearer, res.json()["execution_id"])


def create_volume(bearer, workflow_id, name, size_mb=128):
    res = requests.post(f"{FORGE_URL}/volumes", headers=bearer, json={
        "workflow_id": workflow_id, "name": name, "size_mb": size_mb, "medium": "memory",
    })
    assert res.status_code == 201, f"create volume {name}: {res.status_code} {res.text}"


def run(bearer, workflow_id, name, command, mount_path="/workspace"):
    return submit(bearer, {
        "image": "alpine:3.19",
        "command": ["sh", "-c", command],
        "timeout": 60,
        "volumes": [{"workflow_id": workflow_id, "name": name, "mount_path": mount_path, "workdir": True}],
    })


def test_scatter_gather_end_to_end(bearer):
    """resolve → clone per leg → per-leg run → disjoint gather, all against real forge."""
    wf = f"itest-{uuid.uuid4().hex[:8]}"
    try:
        # Base workspace seeded with two "service" directories (the partitions).
        create_volume(bearer, wf, "workspace")
        seed = run(bearer, wf, "workspace",
                   "mkdir -p svc/a svc/b lib/x && echo A > svc/a/src && echo B > svc/b/src")
        assert seed["status"] == "completed", seed

        # 1) resolve-paths: the two svc/* dirs are the fan-out set (lib/x is excluded).
        r = submit(bearer, {
            "timeout": 60,
            "volumes": [{"workflow_id": wf, "name": "workspace", "read_only": True}],
            "resolve": {"volume": "workspace", "regex": "^svc/[^/]+$", "mode": "dir"},
        })
        assert r["status"] == "completed", r
        paths = sorted((r.get("outputs") or {}).get("paths", "").split())
        assert paths == ["svc/a", "svc/b"], f"resolve matched {paths}"

        # 2+3) For each leg: clone the base into a shard volume, then build into its
        # own owned output dir on that shard (parallel legs never share a PVC).
        shards = {"svc/a": "workspace-s0", "svc/b": "workspace-s1"}
        for path, shard in shards.items():
            create_volume(bearer, wf, shard)
            clone = submit(bearer, {
                "timeout": 60,
                "volumes": [
                    {"workflow_id": wf, "name": shard, "mount_path": "/workspace", "workdir": True},
                    {"workflow_id": wf, "name": "workspace", "mount_path": "/scatter-src", "read_only": True},
                ],
                "copy": {"sources": [{"volume": "workspace"}]},
            })
            assert clone["status"] == "completed", f"clone {shard}: {clone}"
            # The leg sees the whole cloned workspace and writes its own dist output.
            leg = run(bearer, wf, shard,
                      f"test -f {path}/src && mkdir -p {path}/dist && echo built-{path} > {path}/dist/out")
            assert leg["status"] == "completed", f"leg {path}: {leg}"

        # 4) gather: disjoint-union each leg's owned dist dir back into the base.
        gather = submit(bearer, {
            "timeout": 60,
            "volumes": [
                {"workflow_id": wf, "name": "workspace", "mount_path": "/workspace", "workdir": True},
                {"workflow_id": wf, "name": "workspace-s0", "mount_path": "/g0", "read_only": True},
                {"workflow_id": wf, "name": "workspace-s1", "mount_path": "/g1", "read_only": True},
            ],
            "copy": {"disjoint": True, "sources": [
                {"volume": "workspace-s0", "paths": ["svc/a/dist"]},
                {"volume": "workspace-s1", "paths": ["svc/b/dist"]},
            ]},
        })
        assert gather["status"] == "completed", f"gather: {gather}"

        # Verify: the base workspace now holds both legs' outputs.
        check = run(bearer, wf, "workspace", "cat svc/a/dist/out svc/b/dist/out")
        assert check["status"] == "completed", check
        out = check.get("stdout") or ""
        assert "built-svc/a" in out and "built-svc/b" in out, f"gathered outputs missing: {out!r}"
    finally:
        requests.delete(f"{FORGE_URL}/volumes?workflow_id={wf}", headers=bearer)


def test_gather_conflict_fails(bearer):
    """Two legs claiming the same path fail the disjoint gather rather than clobbering."""
    wf = f"itest-{uuid.uuid4().hex[:8]}"
    try:
        create_volume(bearer, wf, "workspace")
        for shard in ("workspace-a", "workspace-b"):
            create_volume(bearer, wf, shard)
            # Both legs write the SAME relative path — a conflict at gather time.
            leg = run(bearer, wf, shard, "mkdir -p shared && echo x > shared/f")
            assert leg["status"] == "completed", leg

        gather = submit(bearer, {
            "timeout": 60,
            "volumes": [
                {"workflow_id": wf, "name": "workspace", "mount_path": "/workspace", "workdir": True},
                {"workflow_id": wf, "name": "workspace-a", "mount_path": "/g0", "read_only": True},
                {"workflow_id": wf, "name": "workspace-b", "mount_path": "/g1", "read_only": True},
            ],
            "copy": {"disjoint": True, "sources": [
                {"volume": "workspace-a", "paths": ["shared"]},
                {"volume": "workspace-b", "paths": ["shared"]},
            ]},
        })
        assert gather["status"] == "failed", f"overlapping gather should fail, got {gather['status']}"
        assert "conflict" in (gather.get("stderr") or "").lower(), gather
    finally:
        requests.delete(f"{FORGE_URL}/volumes?workflow_id={wf}", headers=bearer)
