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


# A minimal valid workflow step that calls gatekeeper's /healthz (no auth
# required on that endpoint, so it completes regardless of user permissions).
HEALTHZ_STEP = {
    "name": "check-gatekeeper",
    "service": "gatekeeper",
    "method": "GET",
    "path": "/healthz",
    "expected_status": 200,
    "timeout_secs": 10,
}
