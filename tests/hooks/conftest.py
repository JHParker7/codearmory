"""Shared fixtures for hooks integration tests."""
import os
import uuid

import pytest
import requests

HOOKS_URL      = os.getenv("HOOKS_URL",      "http://localhost:8087")
WORKFLOWS_URL  = os.getenv("WORKFLOWS_URL",  "http://localhost:8085")
GATEKEEPER_URL = os.getenv("GATEKEEPER_URL", "http://localhost:8080")


@pytest.fixture(scope="session")
def token():
    email    = f"hooks_test_{uuid.uuid4().hex[:8]}@example.com"
    username = f"hooks_tester_{uuid.uuid4().hex[:8]}"
    password = "hooks_test_pass"
    requests.post(f"{GATEKEEPER_URL}/signup",
                  json={"email": email, "username": username, "password": password})
    res = requests.post(f"{GATEKEEPER_URL}/login",
                        json={"email": email, "password": password})
    assert res.status_code == 200, f"login failed: {res.text}"
    return res.json()["token"]


@pytest.fixture(scope="session")
def other_token():
    email    = f"hooks_other_{uuid.uuid4().hex[:8]}@example.com"
    username = f"hooks_other_{uuid.uuid4().hex[:8]}"
    password = "hooks_other_pass"
    requests.post(f"{GATEKEEPER_URL}/signup",
                  json={"email": email, "username": username, "password": password})
    res = requests.post(f"{GATEKEEPER_URL}/login",
                        json={"email": email, "password": password})
    assert res.status_code == 200, f"other login failed: {res.text}"
    return res.json()["token"]


@pytest.fixture(scope="session")
def bearer(token):
    return {"Authorization": f"Bearer {token}"}


@pytest.fixture(scope="session")
def other_bearer(other_token):
    return {"Authorization": f"Bearer {other_token}"}


@pytest.fixture(scope="session")
def workflow(bearer):
    """A minimal single-step workflow used by hook trigger tests."""
    step_res = requests.post(f"{WORKFLOWS_URL}/steps", headers=bearer, json={
        "name": f"hooks-healthz-{uuid.uuid4().hex[:6]}",
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

    res = requests.post(f"{WORKFLOWS_URL}/workflows", headers=bearer, json={
        "name": f"hooks-test-wf-{uuid.uuid4().hex[:6]}",
        "steps": [{"step_id": step_id}],
    })
    assert res.status_code == 201, f"workflow creation failed: {res.text}"
    wf = res.json()
    yield wf
    requests.delete(f"{WORKFLOWS_URL}/workflows/{wf['workflow_id']}", headers=bearer)
    requests.delete(f"{WORKFLOWS_URL}/steps/{step_id}", headers=bearer)
