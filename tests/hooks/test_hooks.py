"""Integration tests for the webhook receiver and event log."""
import hashlib
import hmac
import time

import pytest
import requests

from conftest import HOOKS_URL


# ── Webhook validation ─────────────────────────────────────────────────────────

def test_webhook_missing_repo(bearer):
    res = requests.post(f"{HOOKS_URL}/hooks", json={"event": "push"})
    assert res.status_code == 400

def test_webhook_missing_event():
    res = requests.post(f"{HOOKS_URL}/hooks", json={"source": "org/repo"})
    assert res.status_code == 400

def test_webhook_invalid_json():
    res = requests.post(f"{HOOKS_URL}/hooks",
                        data="not-json",
                        headers={"Content-Type": "application/json"})
    assert res.status_code == 400


# ── Webhook with no matching rules ─────────────────────────────────────────────

def test_webhook_no_rules_returns_200():
    """A valid webhook with no matching rules should still return 200."""
    res = requests.post(f"{HOOKS_URL}/hooks", json={
        "source": "unknown/repo",
        "event": "push",
        "ref": "refs/heads/main",
    })
    assert res.status_code == 200
    data = res.json()
    assert data["event_id"] != ""
    assert data["rules_matched"] == 0
    assert data["status"] == "received"
    assert data["triggers"] == []


# ── Webhook with matching rule ─────────────────────────────────────────────────

_PUSH_SECRET = "push-test-secret"


@pytest.fixture(scope="module")
def push_rule(bearer, workflow):
    res = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": "push-to-main",
        "source": "ci/myapp",
        "events": ["push"],
        "workflow_id": workflow["workflow_id"],
        "input_mapping": {"COMMIT_SHA": "commit", "PUSHED_BY": "pusher"},
        "secret": _PUSH_SECRET,
    })
    assert res.status_code == 201, res.text
    r = res.json()
    yield r
    requests.delete(f"{HOOKS_URL}/rules/{r['rule_id']}", headers=bearer)


def test_webhook_matches_rule_and_triggers(push_rule):
    import json as _json
    payload = {"source": "ci/myapp", "event": "push", "ref": "refs/heads/main",
               "commit": "abc123", "pusher": "alice"}
    body = _json.dumps(payload).encode()
    sig = _sign(_PUSH_SECRET, body)
    res = requests.post(f"{HOOKS_URL}/hooks", data=body,
                        headers={"Content-Type": "application/json",
                                 "X-Hub-Signature-256": sig})
    assert res.status_code == 200, res.text
    data = res.json()
    assert data["rules_matched"] >= 1
    assert data["status"] in ("triggered", "partial")
    assert len(data["triggers"]) >= 1
    trig = data["triggers"][0]
    assert trig["status"] == "triggered"
    assert trig["run_id"] is not None


def test_webhook_ignores_ref_filter_on_generic_endpoint(bearer, workflow):
    """The generic /hooks endpoint carries no ref, so a rule with a ref_filter
    can never match here — ref-based filtering is an adapter-level concern
    (see adapter_git / github_app, which pass payload.Ref). A correctly-signed
    push on the filtered branch still matches zero rules via the generic path."""
    import json as _json
    res = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": "ref-filtered",
        "source": "ci/reffed",
        "events": ["push"],
        "ref_filter": "refs/heads/main",
        "workflow_id": workflow["workflow_id"],
        "secret": "ref-filter-secret",
    })
    assert res.status_code == 201, res.text
    rid = res.json()["rule_id"]

    payload = {"source": "ci/reffed", "event": "push", "ref": "refs/heads/main"}
    body = _json.dumps(payload).encode()
    sig = _sign("ref-filter-secret", body)
    res = requests.post(f"{HOOKS_URL}/hooks", data=body,
                        headers={"Content-Type": "application/json",
                                 "X-Hub-Signature-256": sig})
    assert res.status_code == 200
    data = res.json()
    assert data["rules_matched"] == 0
    assert data["status"] == "received"

    requests.delete(f"{HOOKS_URL}/rules/{rid}", headers=bearer)


def test_webhook_event_type_mismatch_no_trigger(push_rule):
    """A merge event should not trigger a push-only rule."""
    res = requests.post(f"{HOOKS_URL}/hooks", json={
        "source": "ci/myapp",
        "event": "merge",
        "ref": "refs/heads/main",
    })
    assert res.status_code == 200
    data = res.json()
    assert data["rules_matched"] == 0


def test_x_hook_event_header_overrides_body(push_rule):
    """X-Hook-Event header should override the event field in the body."""
    import json as _json
    payload = {"source": "ci/myapp", "event": "merge",
               "ref": "refs/heads/main", "commit": "xyz"}
    body = _json.dumps(payload).encode()
    sig = _sign(_PUSH_SECRET, body)
    res = requests.post(f"{HOOKS_URL}/hooks", data=body,
                        headers={"Content-Type": "application/json",
                                 "X-Hook-Event": "push",
                                 "X-Hub-Signature-256": sig})
    assert res.status_code == 200
    data = res.json()
    assert data["event_type"] == "push"
    assert data["rules_matched"] >= 1


# ── HMAC-protected rule ────────────────────────────────────────────────────────

@pytest.fixture(scope="module")
def secret_rule(bearer, workflow):
    res = requests.post(f"{HOOKS_URL}/rules", headers=bearer, json={
        "name": "secret-push-rule",
        "source": "ci/secured",
        "events": ["push"],
        "workflow_id": workflow["workflow_id"],
        "secret": "test-webhook-secret",
    })
    assert res.status_code == 201, res.text
    r = res.json()
    yield r
    requests.delete(f"{HOOKS_URL}/rules/{r['rule_id']}", headers=bearer)


def _sign(secret: str, body: bytes) -> str:
    return "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()


def test_webhook_hmac_valid_signature(secret_rule):
    import json as _json
    payload = {"source": "ci/secured", "event": "push", "ref": "refs/heads/main"}
    body = _json.dumps(payload).encode()
    sig = _sign("test-webhook-secret", body)
    res = requests.post(f"{HOOKS_URL}/hooks",
                        data=body,
                        headers={"Content-Type": "application/json",
                                 "X-Hub-Signature-256": sig})
    assert res.status_code == 200
    data = res.json()
    assert data["rules_matched"] >= 1
    assert data["triggers"][0]["status"] == "triggered"


def test_webhook_hmac_invalid_signature_skips_rule(secret_rule):
    import json as _json
    payload = {"source": "ci/secured", "event": "push", "ref": "refs/heads/main"}
    body = _json.dumps(payload).encode()
    res = requests.post(f"{HOOKS_URL}/hooks",
                        data=body,
                        headers={"Content-Type": "application/json",
                                 "X-Hub-Signature-256": "sha256=badhash"})
    assert res.status_code == 200
    data = res.json()
    # Rule exists for this repo+event but HMAC mismatch should exclude it.
    assert data["rules_matched"] == 0


# ── Event log ─────────────────────────────────────────────────────────────────

def test_list_events_unauthorized():
    res = requests.get(f"{HOOKS_URL}/events")
    assert res.status_code == 401

def test_get_event_unauthorized():
    res = requests.get(f"{HOOKS_URL}/events/does-not-matter")
    assert res.status_code == 401

def test_list_events_returns_array(bearer, push_rule):
    # Fire a webhook to ensure at least one event exists for this user's rules.
    requests.post(f"{HOOKS_URL}/hooks", json={
        "source": "ci/myapp", "event": "push",
        "ref": "refs/heads/main", "commit": "list-test",
    })
    time.sleep(0.5)  # give fire-and-forget DB writes a moment to land

    res = requests.get(f"{HOOKS_URL}/events", headers=bearer)
    assert res.status_code == 200
    assert isinstance(res.json(), list)

def test_list_events_filter_by_repo(bearer, push_rule):
    res = requests.get(f"{HOOKS_URL}/events?source=ci/myapp", headers=bearer)
    assert res.status_code == 200
    for ev in res.json():
        assert ev["source"] == "ci/myapp"

def test_get_event_found(bearer, push_rule):
    import json as _json
    payload = {"source": "ci/myapp", "event": "push",
               "ref": "refs/heads/main", "commit": "get-event-test"}
    body = _json.dumps(payload).encode()
    sig = _sign(_PUSH_SECRET, body)
    res = requests.post(f"{HOOKS_URL}/hooks", data=body,
                        headers={"Content-Type": "application/json",
                                 "X-Hub-Signature-256": sig})
    event_id = res.json()["event_id"]
    time.sleep(0.5)

    res = requests.get(f"{HOOKS_URL}/events/{event_id}", headers=bearer)
    assert res.status_code == 200
    data = res.json()
    assert data["event_id"] == event_id
    assert isinstance(data["triggers"], list)

def test_get_event_not_found(bearer):
    res = requests.get(f"{HOOKS_URL}/events/00000000-0000-0000-0000-000000000000",
                       headers=bearer)
    assert res.status_code == 404
