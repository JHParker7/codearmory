"""
End-to-end portal test against a full Kubernetes (kata) codearmory deployment.

Everything is done **through the portal in a real browser** (Selenium): the
account is created on the signup page, the whole workflow is assembled by
clicking the visual pipeline builder's palette/forms/toggles, and the run is
triggered, approved and watched on the live run page. No step or pipeline is
created via the API and nothing is stubbed — if a builder control is broken, the
test breaks.

The workflow it builds, exactly as requested:

  1. a shared **volume**                                    (forge/create-volume)
  2. a forge/run step that writes **3 random strings** to a file in the volume
  3. a manual **approval gate**
  4. a forge/run step that **reads the file** and outputs the list of strings
     (captured as the `STRINGS` output variable)
  5. a **matrix** fanned out over that list — one run per string, echoing it
  6. a simple **parallel block** of two concurrent steps

The build/trigger/approve/observe flow is all UI. For the two facts the run
page cannot show — that the matrix really fanned out into 3 distinct runs, and
that the parallel steps truly overlapped in time — the assertions read the run
back through the portal's own BFF with the portal's own token, purely as a
verification oracle.
"""

import json
import re
import uuid
from datetime import datetime

import pytest
import requests

from portal_ui import PortalUI

pytestmark = [pytest.mark.selenium, pytest.mark.e2e]

# ── forge/run shell scripts (POSIX sh; work on busybox/alpine and coreutils) ────

# Write three random 12-hex-char strings, one per line, into the shared volume.
WRITE_SCRIPT = (
    "set -e\n"
    ": > /workspace/strings.txt\n"
    "n=0\n"
    "while [ $n -lt 3 ]; do\n"
    "  head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \\n' >> /workspace/strings.txt\n"
    "  echo >> /workspace/strings.txt\n"
    "  n=$((n + 1))\n"
    "done\n"
    'echo "wrote strings:"\n'
    "cat /workspace/strings.txt\n"
)

# Read the file back, join into a comma-separated list, and export it so forge
# captures it as the STRINGS output variable; also echo it for the logs.
READ_SCRIPT = (
    "set -e\n"
    "STRINGS=$(tr '\\n' ',' < /workspace/strings.txt | sed 's/,*$//')\n"
    'echo "read STRINGS=$STRINGS"\n'
    "export STRINGS\n"
)

# Each matrix execution echoes its bound value (${matrix.item} is substituted by
# the workflows worker per execution before the command reaches forge).
ECHO_SCRIPT = 'echo "matrix item: ${matrix.item}"'


# ── verification oracle (reads run truth via the portal's own BFF + token) ──────

def fetch_run(portal_url: str, token: str, run_id: str) -> dict:
    resp = requests.get(
        f"{portal_url.rstrip('/')}/api/workflows/runs/{run_id}",
        headers={"Authorization": f"Bearer {token}"},
        timeout=20,
    )
    resp.raise_for_status()
    return resp.json()


def _parse_ts(s: str) -> datetime:
    s = s.strip()
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    # Trim sub-microsecond precision Go emits so fromisoformat can parse it.
    m = re.match(r"(.*\.\d{6})\d*(.*)", s)
    if m:
        s = m.group(1) + m.group(2)
    return datetime.fromisoformat(s)


def _step_run(run: dict, name: str) -> dict:
    for sr in run.get("step_runs", []):
        if sr.get("step_name") == name:
            return sr
    raise AssertionError(f"no step run named {name!r} in {[s.get('step_name') for s in run.get('step_runs', [])]}")


def _logs(sr: dict) -> str:
    return sr.get("logs") or ""


def _diag(run: dict) -> str:
    lines = [f"run status={run.get('status')}"]
    for sr in run.get("step_runs", []):
        lines.append(
            f"  [{sr.get('step_index')}] {sr.get('step_name')} -> {sr.get('status')} "
            f"out={sr.get('output')!r} logs={(sr.get('logs') or '')[:200]!r}"
        )
    return "\n".join(lines)


def assert_all_steps_succeeded(run: dict) -> None:
    """Every recorded step run must have completed — no step failed, was cancelled,
    or is still stuck. A failed step in a matrix/parallel group can otherwise be
    masked if the group as a whole is tolerated, so check each one explicitly."""
    srs = run.get("step_runs", [])
    assert srs, f"run recorded no step runs\n{_diag(run)}"
    bad = [(sr.get("step_name"), sr.get("status")) for sr in srs if sr.get("status") != "completed"]
    assert not bad, f"these step runs did not complete: {bad}\n{_diag(run)}"


def read_output_strings(run: dict, expected: int = 3) -> list[str]:
    """The list the read step captured as its STRINGS output variable — the real
    output content the run produced, not just its status."""
    read = _step_run(run, "read-strings")
    assert read["status"] == "completed", f"read step did not complete: {read}\n{_diag(run)}"
    published = [v for v in str(json.loads(read.get("output") or "{}").get("STRINGS", "")).split(",") if v]
    assert len(published) == expected, (
        f"read step's STRINGS output was {published}, expected {expected} values\n{_diag(run)}"
    )
    return published


def assert_volume_roundtrip(run: dict, published: list[str]) -> None:
    """The write step must actually have produced those exact strings into the
    shared volume — proving the file it wrote is what the read step read back
    (i.e. the volume genuinely carried data between the two pods)."""
    write = _step_run(run, "make-strings")
    assert write["status"] == "completed", f"write step did not complete: {write}\n{_diag(run)}"
    wlogs = _logs(write)
    assert wlogs, f"write step captured no logs to verify its output\n{_diag(run)}"
    for s in published:
        assert s in wlogs, (
            f"string {s!r} read from the volume was not in the write step's output "
            f"{wlogs!r} — the volume did not carry what was written\n{_diag(run)}"
        )


def assert_matrix_fanned_out(run: dict, published: list[str], base: str = "echo-string") -> None:
    """Exactly one completed run per published string, and each run really echoed
    its bound value (not just carried it in the step name)."""
    rows = [sr for sr in run["step_runs"] if str(sr.get("step_name", "")).startswith(f"{base} [item=")]
    assert len(rows) == len(published), (
        f"expected {len(published)} matrix runs, got {len(rows)}\n{_diag(run)}"
    )
    by_value = {}
    for sr in rows:
        assert sr["status"] == "completed", f"matrix run not completed: {sr}\n{_diag(run)}"
        value = re.match(rf"^{re.escape(base)} \[item=(.+)\]$", sr["step_name"]).group(1)
        by_value[value] = sr
    assert sorted(by_value) == sorted(published), (
        f"matrix ran over {sorted(by_value)} but the list was {sorted(published)}\n{_diag(run)}"
    )
    # Each execution's stdout must contain the string it echoed.
    for value, sr in by_value.items():
        assert f"matrix item: {value}" in _logs(sr), (
            f"matrix run for {value!r} did not echo it; logs={_logs(sr)!r}\n{_diag(run)}"
        )


def assert_parallel(run: dict, a: str, b: str, a_log: str | None = None, b_log: str | None = None) -> None:
    """Both parallel steps completed, overlapped in time, and (optionally) produced
    the expected stdout."""
    sa, sb = _step_run(run, a), _step_run(run, b)
    assert sa["status"] == "completed" and sb["status"] == "completed", _diag(run)
    if a_log:
        assert a_log in _logs(sa), f"{a} logs missing {a_log!r}: {_logs(sa)!r}\n{_diag(run)}"
    if b_log:
        assert b_log in _logs(sb), f"{b} logs missing {b_log!r}: {_logs(sb)!r}\n{_diag(run)}"
    a0, a1 = _parse_ts(sa["started_at"]), _parse_ts(sa["ended_at"])
    b0, b1 = _parse_ts(sb["started_at"]), _parse_ts(sb["ended_at"])
    assert a0 < b1 and b0 < a1, (
        f"parallel steps did not overlap: {a}=[{a0}..{a1}] {b}=[{b0}..{b1}]\n{_diag(run)}"
    )


# ── the full pipeline ──────────────────────────────────────────────────────────

def test_full_pipeline_via_portal(driver, portal_url, creds, forge_image, volume_size_mb, volume_medium):
    ui = PortalUI(driver, portal_url)

    # 0. create the user through the portal (signup, or first-run setup)
    ui.bootstrap_user(**creds)
    ui.open_workflows()

    name = f"k8s-e2e-{uuid.uuid4().hex[:8]}"
    ui.open_new_pipeline(name)
    # The palette (and its action buttons) only exists inside the open builder.
    if not ui.actions_available("forge/create-volume", "forge/run"):
        pytest.skip("forge/create-volume or forge/run is not in this deployment's action catalog")

    # 1. shared volume
    ui.add_volume_step("mkvol", volume="workspace", size_mb=volume_size_mb, medium=volume_medium)

    # 2. write 3 random strings into a file in the volume
    ui.add_forge_run_step("make-strings", forge_image, WRITE_SCRIPT)
    ui.attach_volume("workspace")

    # 3. manual approval gate
    gate_msg = "Approve reading the generated strings?"
    ui.add_gate(gate_msg)

    # 4. read the file back and output the list of strings
    ui.add_forge_run_step("read-strings", forge_image, READ_SCRIPT, output_env="STRINGS")
    ui.attach_volume("workspace")

    # 5. matrix over that list — one run per string, echoing it
    ui.start_matrix_mode()
    ui.add_forge_run_step("echo-string", forge_image, ECHO_SCRIPT)
    ui.set_matrix("item", "${steps.read-strings.output.STRINGS}")

    # 6. a simple parallel block
    ui.start_parallel_mode()
    ui.add_forge_run_step("par-a", forge_image, 'echo "parallel A"; sleep 3')
    ui.add_forge_run_step("par-b", forge_image, 'echo "parallel B"; sleep 3')
    ui.finish_parallel_mode()

    ui.save_pipeline()
    run_id = ui.trigger()

    # ── drive + observe the run, all through the run page ──
    ui.wait_for_approval_gate()
    assert ui.page_has_text(gate_msg), "gate message was not surfaced to the approver"
    ui.approve()

    ui.wait_for_status("COMPLETED")

    # ── authoritative deep assertions via the oracle: success AND output content ──
    run = fetch_run(portal_url, ui.token(), run_id)
    assert run["status"] == "completed", _diag(run)
    assert_all_steps_succeeded(run)                     # every step, not just the run, succeeded
    published = read_output_strings(run, expected=3)    # the 3 strings the read step output
    assert_volume_roundtrip(run, published)             # the write step produced those exact strings
    assert_matrix_fanned_out(run, published, base="echo-string")  # 3 runs, each echoed its string
    assert_parallel(run, "par-a", "par-b", a_log="parallel A", b_log="parallel B")

    # UI evidence of the matrix + parallel structure on the live run page. The
    # matrix fan-out is collapsed into one block with a dropdown; each combination
    # must be individually selectable there and surface its OWN output — guarding
    # against the run page collapsing the fan-out (which all share one step_index)
    # into a single shared output.
    assert ui.page_has_text("∥ parallel"), "run page did not render the parallel stage"
    assert ui.page_has_text("⊞ matrix"), "run page did not render the matrix stage"
    for value in published:
        ui.select_matrix_combo(f"echo-string [item={value}]")  # pick this combo in the dropdown
        ui.wait_logs_contains(f"matrix item: {value}")         # its own echoed stdout, not another run's


# ── a focused, standalone parallel-block test ──────────────────────────────────

def test_parallel_block_runs_concurrently(driver, portal_url, creds, forge_image):
    """A minimal pipeline of two forge/run steps in one parallel block, built and
    run entirely through the portal, asserting the two steps really overlapped."""
    ui = PortalUI(driver, portal_url)
    ui.bootstrap_user(**creds)
    ui.open_workflows()

    name = f"k8s-parallel-{uuid.uuid4().hex[:8]}"
    ui.open_new_pipeline(name)
    if not ui.actions_available("forge/run"):
        pytest.skip("forge/run is not in this deployment's action catalog")

    ui.start_parallel_mode()
    ui.add_forge_run_step("p-one", forge_image, 'echo "one"; sleep 3')
    ui.add_forge_run_step("p-two", forge_image, 'echo "two"; sleep 3')
    ui.finish_parallel_mode()

    ui.save_pipeline()
    run_id = ui.trigger()

    ui.wait_for_status("COMPLETED")
    assert ui.page_has_text("∥ parallel"), "run page did not render the parallel stage"

    run = fetch_run(portal_url, ui.token(), run_id)
    assert run["status"] == "completed", _diag(run)
    assert_all_steps_succeeded(run)
    assert_parallel(run, "p-one", "p-two", a_log="one", b_log="two")
