"""Command queue + event ingest plane: enqueue (internal HMAC + tenant authz) →
long-poll claim → ack, and event ingest acceptance + idempotency."""
import json
import uuid

import requests
from conftest import GATEWAY_URL, sign_internal


def _enqueue(sim, integration, type_, payload, user_id=None, org_id=None):
    """Enqueue a command the way a control-plane service does: sign the full body
    and assert the authorizing tenant. Defaults to the outpost's owning tenant so
    the gateway's ownership check passes; override user_id/org_id to test rejection."""
    body = json.dumps({
        "outpost_id": sim.outpost_id,
        "integration": integration,
        "type": type_,
        "org_id": sim.org_id if org_id is None else org_id,
        "user_id": sim.user_id if user_id is None else user_id,
        "payload": payload,
    }).encode()
    token, ts = sign_internal("command", body)
    return requests.post(
        f"{GATEWAY_URL}/internal/commands",
        headers={"X-Internal-Token": token, "X-Internal-Timestamp": ts,
                 "Content-Type": "application/json"},
        data=body,
    )


def test_enqueue_requires_valid_hmac(chaos_outpost):
    outpost_id, _ = chaos_outpost
    res = requests.post(
        f"{GATEWAY_URL}/internal/commands",
        headers={"X-Internal-Token": "wrong", "X-Internal-Timestamp": "0"},
        json={"outpost_id": outpost_id, "integration": "chaos", "type": "run-experiment", "payload": {}},
    )
    assert res.status_code == 401


def test_enqueue_rejects_foreign_tenant(chaos_outpost):
    # A command authorized for a tenant that does NOT own the outpost is rejected
    # with 403 even though the HMAC is valid — the cross-tenant authz fix.
    _, sim = chaos_outpost
    res = _enqueue(sim, "chaos", "run-experiment", {}, user_id=uuid.uuid4().hex, org_id="")
    assert res.status_code == 403, res.text


def test_enqueue_rejects_module_not_enabled(bearer):
    # An outpost without the argo module must not receive argo commands.
    from conftest import register_outpost
    _, sim = register_outpost(bearer, f"chaos-only-{uuid.uuid4().hex[:6]}", ["chaos"])
    res = _enqueue(sim, "argo", "sync", {"app_name": "x"})
    assert res.status_code == 409


def test_command_roundtrip(chaos_outpost):
    outpost_id, sim = chaos_outpost
    payload = {"experiment_id": uuid.uuid4().hex, "engine_name": "exp-x"}
    enq = _enqueue(sim, "chaos", "run-experiment", payload)
    assert enq.status_code == 202, enq.text
    command_id = enq.json()["command_id"]

    # Long-poll returns immediately since a command is pending.
    poll = sim.poll_commands()
    assert poll.status_code == 200
    cmds = poll.json()
    assert any(c["id"] == command_id and c["type"] == "run-experiment"
               and c["payload"]["experiment_id"] == payload["experiment_id"] for c in cmds), cmds

    ack = sim.ack(command_id)
    assert ack.status_code == 204


def test_event_ingest_accepted_and_idempotent(chaos_outpost):
    _, sim = chaos_outpost
    event_id = uuid.uuid4().hex
    first = sim.post_event("chaos", "verdict",
                           {"experiment_id": "unknown", "verdict": "Pass"}, event_id=event_id)
    assert first.status_code == 202
    # Re-posting the same event id (at-least-once outpost retry) is deduped: the
    # gateway returns 200 on the duplicate (vs 202 on a fresh insert), so asserting
    # exactly 200 proves dedup actually happened.
    dup = sim.post_event("chaos", "verdict",
                         {"experiment_id": "unknown", "verdict": "Pass"}, event_id=event_id)
    assert dup.status_code == 200


def test_event_requires_integration_and_type(chaos_outpost):
    _, sim = chaos_outpost
    res = requests.post(f"{GATEWAY_URL}/outpost/events", headers=sim.headers,
                       json={"payload": {}})
    assert res.status_code == 400


def test_event_rejects_module_not_enabled(chaos_outpost):
    # A chaos-only outpost must not be able to forge events for the argo module.
    _, sim = chaos_outpost
    res = sim.post_event("argo", "app-state", {"app_name": "x", "sync_status": "Synced"})
    assert res.status_code == 403, res.text
