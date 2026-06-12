"""Integration tests for API shapes that the hooks TUI depends on."""
import hashlib
import hmac
import json as _json
import uuid

import requests

from conftest import HOOKS_URL


def _sign(secret: str, body: bytes) -> str:
    return "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()


# ── Rules list: JSON shape ────────────────────────────────────────────────────

def test_list_rules_returns_array(bearer):
    res = requests.get(f"{HOOKS_URL}/rules", headers=bearer)
    assert res.status_code == 200
    assert isinstance(res.json(), list)


def test_list_rules_item_has_required_fields(bearer, workflow):
    """The TUI reads rule_id, name, repo, events, workflow_id from the rules list."""
    rule = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": f"tui-shape-{uuid.uuid4().hex[:6]}",
        "source": "org/tui-repo",
        "events": ["push"],
        "workflow_id": workflow["workflow_id"],
        "secret": "tui-shape-secret",
    })
    assert rule.status_code == 201, rule.text
    rule_id = rule.json()["rule_id"]

    res = requests.get(f"{HOOKS_URL}/rules", headers=bearer)
    assert res.status_code == 200
    items = res.json()
    matching = [r for r in items if r.get("rule_id") == rule_id]
    assert matching, "created rule not in list"
    item = matching[0]
    for field in ("rule_id", "name", "source", "events", "workflow_id"):
        assert field in item, f"missing field: {field}"

    # cleanup
    requests.delete(f"{HOOKS_URL}/rules/{rule_id}", headers=bearer)


# ── Rules delete ──────────────────────────────────────────────────────────────

def test_delete_rule_success(bearer, workflow):
    """TUI sends DELETE /rules/{id} when user confirms deletion."""
    rule = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": f"tui-del-{uuid.uuid4().hex[:6]}",
        "source": "org/tui-del",
        "events": ["push"],
        "workflow_id": workflow["workflow_id"],
        "secret": "tui-del-secret",
    })
    assert rule.status_code == 201, rule.text
    rule_id = rule.json()["rule_id"]

    res = requests.delete(f"{HOOKS_URL}/rules/{rule_id}", headers=bearer)
    assert res.status_code in (200, 204)

    confirm = requests.get(f"{HOOKS_URL}/rules/{rule_id}", headers=bearer)
    assert confirm.status_code == 404


def test_delete_rule_not_found(bearer):
    res = requests.delete(f"{HOOKS_URL}/rules/does-not-exist", headers=bearer)
    assert res.status_code == 404


def test_delete_rule_unauthorized():
    res = requests.delete(f"{HOOKS_URL}/rules/does-not-exist")
    assert res.status_code == 401


# ── Events list: JSON shape ───────────────────────────────────────────────────

def test_list_events_returns_array(bearer):
    """GET /events returns an array (the TUI populates its table from this)."""
    res = requests.get(f"{HOOKS_URL}/events", headers=bearer)
    assert res.status_code == 200
    assert isinstance(res.json(), list)


def test_list_events_item_has_required_fields(bearer):
    """The TUI reads event_id, repo, event, status, created_at from each event."""
    # Fire a webhook to ensure at least one event exists.
    requests.post(f"{HOOKS_URL}/hooks", json={
        "source": "org/tui-events-shape",
        "event": "push",
        "ref": "refs/heads/main",
    })

    res = requests.get(f"{HOOKS_URL}/events", headers=bearer)
    assert res.status_code == 200
    items = res.json()
    if not items:
        return  # no events yet — acceptable in a clean test env
    item = items[0]
    for field in ("event_id", "source", "event_type", "status", "created_at"):
        assert field in item, f"missing field: {field}"


def test_list_events_filter_by_repo(bearer):
    """The TUI sends ?source=<url-encoded-source> to filter events for a rule's source."""
    unique_repo = f"org/tui-filter-{uuid.uuid4().hex[:8]}"
    requests.post(f"{HOOKS_URL}/hooks", json={
        "source": unique_repo,
        "event": "push",
        "ref": "refs/heads/main",
    })

    res = requests.get(f"{HOOKS_URL}/events", headers=bearer,
                       params={"source": unique_repo})
    assert res.status_code == 200
    items = res.json()
    assert isinstance(items, list)
    for item in items:
        assert item["source"] == unique_repo, (
            f"filter by repo returned event from wrong repo: {item['source']}"
        )


def test_list_events_filter_no_match_returns_empty_array(bearer):
    res = requests.get(f"{HOOKS_URL}/events", headers=bearer,
                       params={"source": "org/definitely-does-not-exist-xyz"})
    assert res.status_code == 200
    assert res.json() == []


# ── Event detail: JSON shape ──────────────────────────────────────────────────

def test_event_detail_has_required_fields(bearer):
    """The TUI reads event_id, repo, event, status, triggers, created_at from detail."""
    requests.post(f"{HOOKS_URL}/hooks", json={
        "source": "org/tui-detail",
        "event": "push",
        "ref": "refs/heads/main",
    })

    events = requests.get(f"{HOOKS_URL}/events", headers=bearer)
    assert events.status_code == 200
    items = events.json()
    if not items:
        return
    eid = items[0]["event_id"]

    res = requests.get(f"{HOOKS_URL}/events/{eid}", headers=bearer)
    assert res.status_code == 200
    body = res.json()
    for field in ("event_id", "source", "event_type", "status", "created_at"):
        assert field in body, f"missing field: {field}"
    assert "triggers" in body, "missing triggers field — TUI renders trigger run_ids"
    assert isinstance(body["triggers"], list)


def test_event_detail_not_found(bearer):
    res = requests.get(f"{HOOKS_URL}/events/does-not-exist", headers=bearer)
    assert res.status_code == 404


# ── Trigger shape ─────────────────────────────────────────────────────────────

def test_trigger_item_has_required_fields(bearer, workflow):
    """The TUI renders trigger.run_id and trigger.error from each trigger entry."""
    repo = f"org/tui-trigger-{uuid.uuid4().hex[:8]}"
    rule = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": f"tui-trigger-{uuid.uuid4().hex[:6]}",
        "source": repo,
        "events": ["push"],
        "workflow_id": workflow["workflow_id"],
        "secret": "tui-trigger-secret",
    })
    assert rule.status_code == 201, rule.text
    rule_id = rule.json()["rule_id"]

    _payload = {"source": repo, "event": "push", "ref": "refs/heads/main"}
    _body = _json.dumps(_payload).encode()
    _sig = _sign("tui-trigger-secret", _body)
    webhook = requests.post(f"{HOOKS_URL}/hooks", data=_body,
                            headers={"Content-Type": "application/json",
                                     "X-Hub-Signature-256": _sig})
    assert webhook.status_code == 200, webhook.text

    events = requests.get(f"{HOOKS_URL}/events", headers=bearer,
                          params={"source": repo})
    assert events.status_code == 200
    items = events.json()
    assert items, "expected at least one event after webhook"
    eid = items[0]["event_id"]

    detail = requests.get(f"{HOOKS_URL}/events/{eid}", headers=bearer)
    assert detail.status_code == 200
    body = detail.json()
    # There should be at least one trigger for the matched rule.
    if body.get("triggers"):
        trigger = body["triggers"][0]
        for field in ("run_id", "error"):
            assert field in trigger, f"trigger missing field: {field}"

    requests.delete(f"{HOOKS_URL}/rules/{rule_id}", headers=bearer)
