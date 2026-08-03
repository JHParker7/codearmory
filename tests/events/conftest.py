"""Shared fixtures for events integration tests."""
import hashlib
import hmac
import json
import os
import uuid

import pytest
import requests

EVENTS_URL     = os.getenv("EVENTS_URL",     "http://localhost:8093")
WORKFLOWS_URL  = os.getenv("WORKFLOWS_URL",  "http://localhost:8085")
GATEKEEPER_URL = os.getenv("GATEKEEPER_URL", "http://localhost:8080")

# Must match the events service's EVENTS_WEBHOOK_SECRET. The public webhook endpoints reject
# every unsigned payload, so the tests have to sign exactly as a provider would.
WEBHOOK_SECRET = os.getenv("EVENTS_WEBHOOK_SECRET", "events-webhook-local-secret")


def sign_webhook(body: bytes) -> dict:
    """GitHub/Forgejo-style signature headers over the raw body."""
    mac = hmac.new(WEBHOOK_SECRET.encode(), body, hashlib.sha256).hexdigest()
    return {
        "Content-Type": "application/json",
        "X-Hub-Signature-256": f"sha256={mac}",
    }


def post_signed(path: str, payload: dict, extra_headers: dict | None = None):
    """POST a JSON payload to a public webhook endpoint with a valid provider signature.

    The body is serialised once and both signed and sent, because the MAC covers the exact
    bytes on the wire — re-serialising would produce a different byte string and a 401.
    """
    body = json.dumps(payload).encode()
    headers = sign_webhook(body)
    if extra_headers:
        headers.update(extra_headers)
    return requests.post(f"{EVENTS_URL}{path}", data=body, headers=headers)


def _signup_and_login(prefix: str) -> str:
    email    = f"{prefix}_{uuid.uuid4().hex[:8]}@example.com"
    username = f"{prefix}_{uuid.uuid4().hex[:8]}"
    password = f"{prefix}_pass"
    requests.post(f"{GATEKEEPER_URL}/signup",
                  json={"email": email, "username": username, "password": password})
    res = requests.post(f"{GATEKEEPER_URL}/login",
                        json={"email": email, "password": password})
    assert res.status_code == 200, f"login failed: {res.text}"
    return res.json()["token"]


@pytest.fixture(scope="session")
def token():
    return _signup_and_login("events_test")


@pytest.fixture(scope="session")
def other_token():
    return _signup_and_login("events_other")


@pytest.fixture(scope="session")
def bearer(token):
    return {"Authorization": f"Bearer {token}"}


@pytest.fixture(scope="session")
def other_bearer(other_token):
    return {"Authorization": f"Bearer {other_token}"}


@pytest.fixture(scope="session")
def user_id(bearer):
    """The caller's gatekeeper user id — the tenant a webhook's events are attributed to."""
    res = requests.get(f"{GATEKEEPER_URL}/me", headers=bearer)
    assert res.status_code == 200, f"whoami failed: {res.text}"
    body = res.json()
    return body.get("user_id") or body.get("id")


@pytest.fixture(scope="session")
def workflow(bearer):
    """A minimal single-step workflow used by trigger dispatch tests."""
    step_res = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json={
        "name": f"events-healthz-{uuid.uuid4().hex[:6]}",
        "action": "http",
        "with": {
            "service": "gatekeeper",
            "method": "GET",
            "path": "/healthz",
            "expected_status": 200,
        },
        "timeout": 10,
    })
    assert step_res.status_code == 201, f"step creation failed: {step_res.text}"
    step_id = step_res.json()["step_id"]

    res = requests.post(f"{WORKFLOWS_URL}/pipelines", headers=bearer, json={
        "name": f"events-test-wf-{uuid.uuid4().hex[:6]}",
        "steps": [{"step_id": step_id}],
    })
    assert res.status_code == 201, f"workflow creation failed: {res.text}"
    wf = res.json()
    yield wf
    requests.delete(f"{WORKFLOWS_URL}/pipelines/{wf['workflow_id']}", headers=bearer)
    requests.delete(f"{WORKFLOWS_URL}/steps/{step_id}", headers=bearer)
