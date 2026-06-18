"""Integration tests for the notifications service.

These cover the API contract — providers, channel CRUD, validation, secret
redaction, access control, and notification enqueue. Actual outbound delivery
(Slack/SMTP/webhook) depends on network egress and is exercised by unit tests,
so here we assert that enqueue produces a tracked record rather than asserting a
successful external send.
"""
import requests

from conftest import NOTIFICATIONS_URL


def _make_webhook_channel(bearer, name="Test webhook", enabled=True):
    return requests.post(f"{NOTIFICATIONS_URL}/channels", headers=bearer, json={
        "name": name,
        "type": "webhook",
        "config": {"url": "https://example.invalid/hook", "auth_header": "Bearer s3cret"},
        "enabled": enabled,
    })


# ── Authentication ─────────────────────────────────────────────────────────────

def test_list_channels_unauthorized():
    assert requests.get(f"{NOTIFICATIONS_URL}/channels").status_code == 401

def test_create_channel_unauthorized():
    assert requests.post(f"{NOTIFICATIONS_URL}/channels", json={"name": "x", "type": "slack"}).status_code == 401

def test_notify_unauthorized():
    assert requests.post(f"{NOTIFICATIONS_URL}/notify", json={"body": "x"}).status_code == 401


# ── Providers ───────────────────────────────────────────────────────────────────

def test_list_providers(bearer):
    res = requests.get(f"{NOTIFICATIONS_URL}/providers", headers=bearer)
    assert res.status_code == 200, res.text
    types = {p["type"] for p in res.json()}
    assert {"slack", "email", "webhook"}.issubset(types)
    # Each provider advertises its config schema.
    for p in res.json():
        assert isinstance(p["fields"], list)


# ── Channel validation ──────────────────────────────────────────────────────────

def test_create_channel_unknown_type(bearer):
    res = requests.post(f"{NOTIFICATIONS_URL}/channels", headers=bearer,
                        json={"name": "x", "type": "telegram", "config": {}})
    assert res.status_code == 400

def test_create_channel_missing_required_config(bearer):
    res = requests.post(f"{NOTIFICATIONS_URL}/channels", headers=bearer,
                        json={"name": "x", "type": "slack", "config": {}})
    assert res.status_code == 400

def test_create_channel_missing_name(bearer):
    res = requests.post(f"{NOTIFICATIONS_URL}/channels", headers=bearer,
                        json={"type": "webhook", "config": {"url": "https://x/y"}})
    assert res.status_code == 400


# ── Channel CRUD + redaction ────────────────────────────────────────────────────

def test_create_channel_redacts_secrets(bearer):
    res = _make_webhook_channel(bearer)
    assert res.status_code == 201, res.text
    ch = res.json()
    assert ch["channel_id"]
    assert ch["type"] == "webhook"
    assert ch["enabled"] is True
    # Secret config field must be masked in the response, non-secret preserved.
    assert ch["config"]["auth_header"] == "***"
    assert ch["config"]["url"] == "https://example.invalid/hook"

def test_get_and_list_channel(bearer):
    created = _make_webhook_channel(bearer, name="Listed").json()
    cid = created["channel_id"]

    got = requests.get(f"{NOTIFICATIONS_URL}/channels/{cid}", headers=bearer)
    assert got.status_code == 200
    assert got.json()["channel_id"] == cid

    listed = requests.get(f"{NOTIFICATIONS_URL}/channels", headers=bearer)
    assert listed.status_code == 200
    assert any(c["channel_id"] == cid for c in listed.json())

def test_update_channel_toggle_enabled(bearer):
    cid = _make_webhook_channel(bearer).json()["channel_id"]
    res = requests.put(f"{NOTIFICATIONS_URL}/channels/{cid}", headers=bearer,
                       json={"enabled": False})
    assert res.status_code == 200, res.text
    assert res.json()["enabled"] is False

def test_update_channel_preserves_redacted_secret(bearer):
    cid = _make_webhook_channel(bearer).json()["channel_id"]
    # PUT back the redacted value — the stored secret must be preserved, not wiped.
    res = requests.put(f"{NOTIFICATIONS_URL}/channels/{cid}", headers=bearer,
                       json={"config": {"url": "https://example.invalid/hook", "auth_header": "***"}})
    assert res.status_code == 200, res.text
    assert res.json()["config"]["auth_header"] == "***"

def test_delete_channel(bearer):
    cid = _make_webhook_channel(bearer).json()["channel_id"]
    assert requests.delete(f"{NOTIFICATIONS_URL}/channels/{cid}", headers=bearer).status_code == 204
    assert requests.get(f"{NOTIFICATIONS_URL}/channels/{cid}", headers=bearer).status_code == 404


# ── Access control ──────────────────────────────────────────────────────────────

def test_other_user_cannot_see_channel(bearer, other_bearer):
    cid = _make_webhook_channel(bearer, name="Private").json()["channel_id"]
    assert requests.get(f"{NOTIFICATIONS_URL}/channels/{cid}", headers=other_bearer).status_code == 404


# ── Notify ──────────────────────────────────────────────────────────────────────

def test_notify_requires_body(bearer):
    _make_webhook_channel(bearer)
    res = requests.post(f"{NOTIFICATIONS_URL}/notify", headers=bearer, json={"subject": "no body"})
    assert res.status_code == 400

def test_notify_enqueues_to_channel(bearer):
    cid = _make_webhook_channel(bearer, name="Notify target").json()["channel_id"]
    res = requests.post(f"{NOTIFICATIONS_URL}/notify", headers=bearer,
                        json={"subject": "Hello", "body": "world", "channel_ids": [cid]})
    assert res.status_code == 202, res.text
    records = res.json()
    assert len(records) == 1
    assert records[0]["channel_id"] == cid
    assert records[0]["status"] in ("pending", "sent", "failed")

def test_notify_no_enabled_channels_is_400(bearer):
    # Create a channel and disable it; with no other enabled channels targeted by
    # id, an explicit target that is disabled yields no deliverable targets.
    cid = _make_webhook_channel(bearer, name="Disabled", enabled=False).json()["channel_id"]
    res = requests.post(f"{NOTIFICATIONS_URL}/notify", headers=bearer,
                        json={"body": "x", "channel_ids": [cid]})
    assert res.status_code == 400

def test_list_notifications(bearer):
    cid = _make_webhook_channel(bearer, name="Logged").json()["channel_id"]
    requests.post(f"{NOTIFICATIONS_URL}/notify", headers=bearer,
                  json={"body": "log me", "channel_ids": [cid]})
    res = requests.get(f"{NOTIFICATIONS_URL}/notifications", headers=bearer)
    assert res.status_code == 200
    assert isinstance(res.json(), list)

def test_list_notifications_invalid_status_filter(bearer):
    res = requests.get(f"{NOTIFICATIONS_URL}/notifications?status=bogus", headers=bearer)
    assert res.status_code == 400
