"""Shared fixtures for workflows integration tests."""
import os
import uuid

import pytest
import requests

WORKFLOWS_URL = os.getenv("WORKFLOWS_URL", "http://localhost:8085")
GATEKEEPER_URL = os.getenv("GATEKEEPER_URL", "http://localhost:8080")


@pytest.fixture(scope="session")
def token():
    email = f"workflows_test_{uuid.uuid4().hex[:8]}@example.com"
    password = "workflows_test_pass"
    username = f"workflows_tester_{uuid.uuid4().hex[:8]}"
    requests.post(f"{GATEKEEPER_URL}/signup", json={
        "email": email, "username": username, "password": password,
    })
    res = requests.post(f"{GATEKEEPER_URL}/login", json={
        "email": email, "password": password,
    })
    assert res.status_code == 200, f"login failed: {res.text}"
    return res.json()["token"]


@pytest.fixture(scope="session")
def other_token():
    email = f"workflows_other_{uuid.uuid4().hex[:8]}@example.com"
    password = "workflows_other_pass"
    username = f"workflows_other_{uuid.uuid4().hex[:8]}"
    requests.post(f"{GATEKEEPER_URL}/signup", json={
        "email": email, "username": username, "password": password,
    })
    res = requests.post(f"{GATEKEEPER_URL}/login", json={
        "email": email, "password": password,
    })
    assert res.status_code == 200, f"other login failed: {res.text}"
    return res.json()["token"]


@pytest.fixture(scope="session")
def bearer(token):
    return {"Authorization": f"Bearer {token}"}


@pytest.fixture(scope="session")
def other_bearer(other_token):
    return {"Authorization": f"Bearer {other_token}"}


# Placeholder step reference used in auth-only tests where the body is
# irrelevant (the request is rejected before step validation).
HEALTHZ_STEP = {"step_id": "00000000-0000-0000-0000-000000000001"}


@pytest.fixture(scope="session")
def healthz_step_id(bearer):
    """Create a reusable healthz step in the library and return its step_id."""
    res = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json={
        "name": f"check-gatekeeper-{uuid.uuid4().hex[:6]}",
        "action": "http",
        "with": {
            "service": "gatekeeper",
            "method": "GET",
            "path": "/healthz",
            "expected_status": 200,
        },
        "timeout": 10,
    })
    assert res.status_code == 201, f"step creation failed: {res.text}"
    step_id = res.json()["step_id"]
    yield step_id
    requests.delete(f"{WORKFLOWS_URL}/steps/{step_id}", headers=bearer)


@pytest.fixture(scope="session")
def second_step_id(bearer):
    """Create a second healthz step for multi-step workflow tests."""
    res = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json={
        "name": f"second-check-{uuid.uuid4().hex[:6]}",
        "action": "http",
        "with": {
            "service": "gatekeeper",
            "method": "GET",
            "path": "/healthz",
            "expected_status": 200,
        },
        "timeout": 10,
    })
    assert res.status_code == 201, f"second step creation failed: {res.text}"
    step_id = res.json()["step_id"]
    yield step_id
    requests.delete(f"{WORKFLOWS_URL}/steps/{step_id}", headers=bearer)
