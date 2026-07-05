"""End-to-end integration tests for pipelines-triggering-pipelines.

A parent pipeline runs a child pipeline via a `workflows/trigger` step, passing an
input and capturing the child's declared output; a matrix over the trigger step fans
out one sub-run per value; and the depth guard rejects runaway nesting. Exercises the
whole path: declared inputs/outputs, the workflows/trigger async action, sub-run
output capture, and ${steps.NAME.output.KEY} wiring across pipeline boundaries.

Skips itself if `workflows/trigger` is not in the action catalog (a stack whose
registry manifest predates the feature).
"""
import time
import uuid

import pytest
import requests

from conftest import WORKFLOWS_URL

IMAGE = "alpine:3.19"


def trigger_action_available(bearer):
    res = requests.get(f"{WORKFLOWS_URL}/actions", headers=bearer)
    return res.status_code == 200 and any(a.get("name") == "workflows/trigger" for a in res.json())


def make_step(bearer, action, with_, name):
    res = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json={
        "name": name, "action": action, "with": with_, "timeout": 60,
    })
    assert res.status_code == 201, f"create step ({action}): {res.status_code} {res.text}"
    return res.json()["step_id"]


def make_pipeline(bearer, name, steps, inputs=None, outputs=None):
    body = {"name": name, "steps": steps}
    if inputs is not None:
        body["inputs"] = inputs
    if outputs is not None:
        body["outputs"] = outputs
    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json=body)
    assert res.status_code == 201, f"create pipeline: {res.status_code} {res.text}"
    return res.json()["workflow_id"]


def poll_run(bearer, run_id, timeout=240):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        res = requests.get(f"{WORKFLOWS_URL}/runs/{run_id}", headers=bearer)
        assert res.status_code == 200, res.text
        last = res.json()
        if last["status"] in ("completed", "failed", "timed_out", "cancelled"):
            return last
        time.sleep(2)
    pytest.fail(f"run {run_id} did not finish within {timeout}s (last={last and last.get('status')})")


def _make_child(bearer, tag, cleanup):
    """A child pipeline that greets its `who` input and outputs the greeting."""
    gen_name = f"gen-{tag}"
    gen = make_step(bearer, "forge/run", {
        "image": IMAGE,
        "run": 'export GREETING="hi-${inputs.who}"; echo "greeting=$GREETING"',
        "output_env": ["GREETING"],
    }, name=gen_name)
    cleanup.append(("steps", gen))
    child_name = f"child-{tag}"
    child = make_pipeline(
        bearer, child_name, [{"step_id": gen}],
        inputs=[{"name": "who", "default": "world"}],
        outputs=[{"name": "message", "value": f"${{steps.{gen_name}.output.GREETING}}"}],
    )
    cleanup.append(("pipelines", child))
    return child_name


@pytest.fixture
def cleanup(bearer):
    items = []
    yield items
    for kind, ident in reversed(items):
        requests.delete(f"{WORKFLOWS_URL}/{kind}/{ident}", headers=bearer)


def test_pipeline_triggers_pipeline_and_captures_output(bearer, cleanup):
    if not trigger_action_available(bearer):
        pytest.skip("workflows/trigger not in the action catalog (stale registry manifest)")

    tag = uuid.uuid4().hex[:8]
    child_name = _make_child(bearer, tag, cleanup)

    # Parent: a single workflows/trigger step running the child with who=parent,
    # then re-exports the child's captured `message` as the parent's own output.
    trig_name = f"trig-{tag}"
    trig = make_step(bearer, "workflows/trigger", {
        "pipeline": child_name, "inputs": {"who": "parent"},
    }, name=trig_name)
    cleanup.append(("steps", trig))
    parent = make_pipeline(
        bearer, f"parent-{tag}", [{"step_id": trig}],
        outputs=[{"name": "child_message", "value": f"${{steps.{trig_name}.output.message}}"}],
    )
    cleanup.append(("pipelines", parent))

    res = requests.post(f"{WORKFLOWS_URL}/pipelines/{parent}/runs", headers=bearer, json={})
    assert res.status_code == 202, f"trigger parent: {res.status_code} {res.text}"
    run = poll_run(bearer, res.json()["run_id"])

    assert run["status"] == "completed", f"parent run did not complete: {run}"
    # The trigger step's output IS the child's outputs map.
    blob = str(run.get("step_runs"))
    assert "hi-parent" in blob, f"child output not captured by the trigger step: {blob}"
    # And the parent re-exported it as its own declared output.
    assert run.get("outputs", {}).get("child_message") == "hi-parent", \
        f"parent output not resolved from the child: {run.get('outputs')}"


def test_matrix_over_trigger_fans_out_subpipelines(bearer, cleanup):
    if not trigger_action_available(bearer):
        pytest.skip("workflows/trigger not in the action catalog (stale registry manifest)")

    tag = uuid.uuid4().hex[:8]
    child_name = _make_child(bearer, tag, cleanup)

    # One trigger step, matrixed over two names — each value runs its own sub-run.
    trig = make_step(bearer, "workflows/trigger", {
        "pipeline": child_name, "inputs": {"who": "${matrix.item}"},
    }, name=f"fan-{tag}")
    cleanup.append(("steps", trig))
    parent = make_pipeline(bearer, f"matrix-parent-{tag}", [
        {"step_id": trig, "matrix": {"var": "item", "values": ["alice", "bob"]}},
    ])
    cleanup.append(("pipelines", parent))

    res = requests.post(f"{WORKFLOWS_URL}/pipelines/{parent}/runs", headers=bearer, json={})
    assert res.status_code == 202, res.text
    run = poll_run(bearer, res.json()["run_id"])

    assert run["status"] == "completed", f"matrix parent run did not complete: {run}"
    fan_runs = [sr for sr in run["step_runs"] if str(sr.get("step_name", "")).startswith(f"fan-{tag} [item=")]
    assert len(fan_runs) == 2, f"expected 2 sub-run trigger executions, got {len(fan_runs)}: {run['step_runs']}"
    blob = str(run["step_runs"])
    for who in ("alice", "bob"):
        assert f"hi-{who}" in blob, f"matrix sub-run for {who} not captured: {blob}"


def test_body_trigger_and_depth_cap(bearer, cleanup):
    if not trigger_action_available(bearer):
        pytest.skip("workflows/trigger not in the action catalog (stale registry manifest)")

    tag = uuid.uuid4().hex[:8]
    child_name = _make_child(bearer, tag, cleanup)

    # The body-addressed trigger (POST /runs) creates a normal sub-run.
    ok = requests.post(f"{WORKFLOWS_URL}/runs", headers=bearer,
                       json={"pipeline": child_name, "inputs": {"who": "body"}})
    assert ok.status_code == 202, f"POST /runs should create a run: {ok.status_code} {ok.text}"
    run_id = ok.json()["run_id"]
    cleanup.append(("runs", run_id))
    assert poll_run(bearer, run_id)["status"] == "completed"

    # A create at the nesting cap is rejected before any sub-run is made.
    capped = requests.post(f"{WORKFLOWS_URL}/runs", headers={**bearer, "X-Workflow-Run-Depth": "8"},
                           json={"pipeline": child_name})
    assert capped.status_code == 422, f"depth cap should reject: {capped.status_code} {capped.text}"


def test_missing_required_input_is_rejected(bearer, cleanup):
    tag = uuid.uuid4().hex[:8]
    noop = make_step(bearer, "forge/run", {"image": IMAGE, "run": "echo ${inputs.token}"}, name=f"noop-{tag}")
    cleanup.append(("steps", noop))
    wf = make_pipeline(bearer, f"needs-input-{tag}", [{"step_id": noop}],
                       inputs=[{"name": "token", "required": True}])
    cleanup.append(("pipelines", wf))
    # Triggering with the required input missing (no default) is a 400.
    res = requests.post(f"{WORKFLOWS_URL}/pipelines/{wf}/runs", headers=bearer, json={"inputs": {}})
    assert res.status_code == 400, f"missing required input should be 400: {res.status_code} {res.text}"
    assert "token" in res.text
