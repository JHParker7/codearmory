"""Shared fixtures for tickets integration tests."""
import os
import uuid

import pytest
import requests

TICKETS_URL = os.getenv("TICKETS_URL", "http://localhost:8086")
GATEKEEPER_URL = os.getenv("GATEKEEPER_URL", "http://localhost:8080")


@pytest.fixture(scope="session")
def token():
    email = f"tickets_test_{uuid.uuid4().hex[:8]}@example.com"
    password = "tickets_test_pass"
    username = f"tickets_tester_{uuid.uuid4().hex[:8]}"
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
    email = f"tickets_other_{uuid.uuid4().hex[:8]}@example.com"
    password = "tickets_other_pass"
    username = f"tickets_other_{uuid.uuid4().hex[:8]}"
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
