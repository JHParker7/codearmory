"""Chaos experiments: validation, RBAC isolation, and the full command/verdict
round-trip with the test standing in for the outpost."""
import requests
from conftest import CHAOS_URL, poll_experiment


def _create(bearer, outpost_id, **overrides):
    body = {
        "outpost_id": outpost_id,
        "experiment_type": "pod-delete",
        "target_app_ns": "demo",
        "target_app_label": "app.kubernetes.io/component=conductor",
        "target_app_kind": "deployment",
    }
    body.update(overrides)
    return requests.post(f"{CHAOS_URL}/experiments", headers=bearer, json=body)


def test_experiment_types(bearer):
    res = requests.get(f"{CHAOS_URL}/experiment-types", headers=bearer)
    assert res.status_code == 200
    names = [t["name"] for t in res.json()]
    assert "pod-delete" in names and "pod-network-latency" in names


def test_create_requires_fields(bearer, outpost):
    # Missing outpost_id.
    res = requests.post(f"{CHAOS_URL}/experiments", headers=bearer,
                       json={"experiment_type": "pod-delete", "target_app_ns": "demo",
                             "target_app_label": "app=x"})
    assert res.status_code == 400
    # Unknown experiment type.
    res = _create(bearer, outpost.outpost_id, experiment_type="nope")
    assert res.status_code == 400
    # Missing target label.
    res = _create(bearer, outpost.outpost_id, target_app_label="")
    assert res.status_code == 400


def test_create_unauthorized():
    res = requests.post(f"{CHAOS_URL}/experiments", json={})
    assert res.status_code == 401


def test_create_returns_202_and_dispatches_command(bearer, outpost):
    res = _create(bearer, outpost.outpost_id)
    assert res.status_code == 202, res.text
    exp = res.json()
    assert exp["status"] == "pending"
    experiment_id = exp["experiment_id"]

    # The outpost should receive a run-experiment command carrying our id.
    cmd = outpost.wait_for_command("run-experiment")
    assert cmd is not None, "no run-experiment command was delivered"
    assert cmd["payload"]["experiment_id"] == experiment_id
    assert cmd["payload"]["experiment_type"] == "pod-delete"


def test_full_pass_verdict_flow(bearer, outpost):
    res = _create(bearer, outpost.outpost_id)
    experiment_id = res.json()["experiment_id"]

    cmd = outpost.wait_for_command("run-experiment")
    assert cmd is not None

    # Outpost reports the run started, then a passing verdict.
    outpost.post_event("chaos", "run-started", {"experiment_id": experiment_id})
    outpost.post_event("chaos", "verdict", {
        "experiment_id": experiment_id, "verdict": "Pass", "phase": "Completed",
        "fail_step": "N/A", "probe_success_percentage": "100",
    })

    final = poll_experiment(bearer, experiment_id, until={"Pass", "Fail", "Error"})
    assert final is not None and final["status"] == "Pass", final
    assert final["verdict"] == "Pass"


def test_full_fail_verdict_flow(bearer, outpost):
    res = _create(bearer, outpost.outpost_id)
    experiment_id = res.json()["experiment_id"]
    outpost.wait_for_command("run-experiment")
    outpost.post_event("chaos", "verdict", {
        "experiment_id": experiment_id, "verdict": "Fail", "phase": "Completed",
        "fail_step": "probe-failure",
    })
    final = poll_experiment(bearer, experiment_id, until={"Pass", "Fail", "Error"})
    assert final is not None and final["status"] == "Fail", final
    assert final["verdict"] == "Fail"


def test_list_and_get(bearer, outpost):
    res = _create(bearer, outpost.outpost_id)
    experiment_id = res.json()["experiment_id"]

    lst = requests.get(f"{CHAOS_URL}/experiments", headers=bearer)
    assert lst.status_code == 200
    assert any(e["experiment_id"] == experiment_id for e in lst.json())

    got = requests.get(f"{CHAOS_URL}/experiments/{experiment_id}", headers=bearer)
    assert got.status_code == 200


def test_cross_tenant_isolation(bearer, other_bearer, outpost):
    res = _create(bearer, outpost.outpost_id)
    experiment_id = res.json()["experiment_id"]
    got = requests.get(f"{CHAOS_URL}/experiments/{experiment_id}", headers=other_bearer)
    assert got.status_code == 404, "another user must not see the experiment"


def test_delete_experiment(bearer, outpost):
    res = _create(bearer, outpost.outpost_id)
    experiment_id = res.json()["experiment_id"]
    d = requests.delete(f"{CHAOS_URL}/experiments/{experiment_id}", headers=bearer)
    assert d.status_code == 204
    got = requests.get(f"{CHAOS_URL}/experiments/{experiment_id}", headers=bearer)
    assert got.status_code == 404
