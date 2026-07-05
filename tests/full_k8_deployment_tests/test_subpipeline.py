"""End-to-end portal test for pipelines-triggering-pipelines, driven through the UI.

Builds two pipelines entirely in the portal's visual builder — a CHILD that declares
an input and an output, and a PARENT with a workflows/trigger block that runs the
child and re-publishes its output — then triggers the parent and asserts (on the run
page and via the portal's own /api) that the parent captured the child's output.

This exercises the new builder surfaces: the declared inputs/outputs editors, the
workflows/trigger palette action with its pipeline picker + inputs map, and the
run page's captured-outputs view. Skips if workflows/trigger isn't in the catalog.
"""
import uuid

import pytest

from portal_ui import PortalUI
from test_full_pipeline import fetch_run, _diag

pytestmark = [pytest.mark.selenium, pytest.mark.e2e]


def test_subpipeline_trigger_via_portal(driver, portal_url, creds, forge_image):
    ui = PortalUI(driver, portal_url)
    ui.bootstrap_user(**creds)
    ui.open_workflows()

    tag = uuid.uuid4().hex[:8]
    child = f"child-{tag}"
    parent = f"parent-{tag}"

    # ── CHILD: greets its `who` input and publishes the greeting as an output ──
    ui.open_new_pipeline(child)
    if not ui.actions_available("forge/run", "workflows/trigger"):
        pytest.skip("forge/run or workflows/trigger is not in this deployment's action catalog")
    ui.add_forge_run_step(
        "gen", forge_image,
        'export GREETING="hi-${inputs.who}"; echo "greeting=$GREETING"',
        output_env="GREETING",
    )
    ui.add_declared_input("who", default="world")
    ui.add_declared_output("message", "${steps.gen.output.GREETING}")
    ui.save_pipeline()

    # ── PARENT: a workflows/trigger block runs the child with who=parent ──
    ui.open_new_pipeline(parent)
    ui.add_trigger_step("trig", child, inputs_kv="who=parent")
    ui.add_declared_output("child_message", "${steps.trig.output.message}")
    ui.save_pipeline()

    run_id = ui.trigger()
    ui.wait_for_status("COMPLETED", timeout=360)

    # The parent must have captured the child's output end to end.
    run = fetch_run(portal_url, ui.token(), run_id)
    assert run["status"] == "completed", _diag(run)
    assert run.get("outputs", {}).get("child_message") == "hi-parent", (
        f"parent did not capture the child's output: {run.get('outputs')}\n{_diag(run)}"
    )
    # And the run page surfaces the captured value.
    assert ui.page_has_text("hi-parent"), "run page did not show the captured sub-pipeline output"
