"""Enrollment lifecycle + auth for the outpost-gateway user/outpost surfaces."""
import time
import uuid

import requests
from conftest import GATEWAY_URL


def test_create_outpost_returns_enrollment_token(bearer):
    res = requests.post(f"{GATEWAY_URL}/outposts", headers=bearer,
                        json={"name": f"op-{uuid.uuid4().hex[:6]}", "modules": ["chaos"]})
    assert res.status_code == 201, res.text
    body = res.json()
    assert body["enrollment_token"], "enrollment token must be returned on create"
    assert body["status"] == "pending"
    assert body["modules"] == "chaos"


def test_create_outpost_rejects_unknown_module(bearer):
    res = requests.post(f"{GATEWAY_URL}/outposts", headers=bearer,
                        json={"name": "bad", "modules": ["nope"]})
    assert res.status_code == 400


def test_create_outpost_requires_name(bearer):
    res = requests.post(f"{GATEWAY_URL}/outposts", headers=bearer, json={"modules": ["chaos"]})
    assert res.status_code == 400


def test_create_outpost_unauthorized():
    res = requests.post(f"{GATEWAY_URL}/outposts", json={"name": "x", "modules": ["chaos"]})
    assert res.status_code == 401


def test_list_and_get_outpost(bearer):
    name = f"op-{uuid.uuid4().hex[:6]}"
    res = requests.post(f"{GATEWAY_URL}/outposts", headers=bearer,
                        json={"name": name, "modules": ["chaos", "argo"]})
    outpost_id = res.json()["outpost_id"]

    lst = requests.get(f"{GATEWAY_URL}/outposts", headers=bearer)
    assert lst.status_code == 200
    assert any(o["outpost_id"] == outpost_id for o in lst.json())

    got = requests.get(f"{GATEWAY_URL}/outposts/{outpost_id}", headers=bearer)
    assert got.status_code == 200
    assert got.json()["name"] == name


def test_other_user_cannot_see_outpost(bearer, other_bearer):
    res = requests.post(f"{GATEWAY_URL}/outposts", headers=bearer,
                        json={"name": "private", "modules": ["chaos"]})
    outpost_id = res.json()["outpost_id"]
    # Cross-tenant reads return 404 (not 403) to avoid disclosure.
    got = requests.get(f"{GATEWAY_URL}/outposts/{outpost_id}", headers=other_bearer)
    assert got.status_code == 404


def test_enrollment_is_single_use(bearer):
    res = requests.post(f"{GATEWAY_URL}/outposts", headers=bearer,
                        json={"name": f"op-{uuid.uuid4().hex[:6]}", "modules": ["chaos"]})
    token = res.json()["enrollment_token"]
    first = requests.post(f"{GATEWAY_URL}/outpost/register", json={"enrollment_token": token})
    assert first.status_code == 201
    assert first.json()["outpost_key"]
    second = requests.post(f"{GATEWAY_URL}/outpost/register", json={"enrollment_token": token})
    assert second.status_code == 401, "an enrollment token must not be reusable"


def test_register_malformed_token():
    res = requests.post(f"{GATEWAY_URL}/outpost/register", json={"enrollment_token": "garbage"})
    assert res.status_code == 401


def test_outpost_endpoints_reject_bad_key():
    res = requests.get(f"{GATEWAY_URL}/outpost/commands",
                      headers={"X-Outpost-ID": "nope", "Authorization": "Bearer wrong"})
    assert res.status_code == 401


def test_heartbeat_advances_last_seen(bearer, chaos_outpost):
    _, sim = chaos_outpost
    before = requests.get(f"{GATEWAY_URL}/outposts/{sim.outpost_id}", headers=bearer)
    assert before.status_code == 200
    first_seen = before.json().get("last_seen_at")
    assert first_seen, "enrolled outpost should expose last_seen_at"

    # A heartbeat must observably advance the liveness timestamp.
    time.sleep(1.1)
    hb = sim.heartbeat()
    assert hb.status_code == 204
    after = requests.get(f"{GATEWAY_URL}/outposts/{sim.outpost_id}", headers=bearer)
    assert after.status_code == 200
    body = after.json()
    assert body["status"] == "connected"
    assert body["last_seen_at"] > first_seen, (
        f"heartbeat did not advance last_seen_at: {first_seen} -> {body.get('last_seen_at')}")


def test_enrolled_outpost_rejects_wrong_key(bearer, chaos_outpost):
    # An enrolled outpost presenting the wrong key hits the bcrypt-mismatch branch
    # and is rejected 401, even though the outpost id is valid.
    _, sim = chaos_outpost
    res = requests.get(
        f"{GATEWAY_URL}/outpost/commands",
        headers={"X-Outpost-ID": sim.outpost_id, "Authorization": "Bearer bogus-key"},
        timeout=35,
    )
    assert res.status_code == 401, res.text


def test_delete_outpost(bearer):
    res = requests.post(f"{GATEWAY_URL}/outposts", headers=bearer,
                        json={"name": "to-delete", "modules": ["chaos"]})
    outpost_id = res.json()["outpost_id"]
    d = requests.delete(f"{GATEWAY_URL}/outposts/{outpost_id}", headers=bearer)
    assert d.status_code == 204
    got = requests.get(f"{GATEWAY_URL}/outposts/{outpost_id}", headers=bearer)
    assert got.status_code == 404
