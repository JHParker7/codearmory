"""Shared fixtures for notifications integration tests."""
import os
import uuid

import pytest
import requests

NOTIFICATIONS_URL = os.getenv("NOTIFICATIONS_URL", "http://localhost:8094")
GATEKEEPER_URL = os.getenv("GATEKEEPER_URL", "http://localhost:8080")


def _signup_login(prefix):
    email = f"{prefix}_{uuid.uuid4().hex[:8]}@example.com"
    password = f"{prefix}_pass"
    username = f"{prefix}_{uuid.uuid4().hex[:8]}"
    requests.post(f"{GATEKEEPER_URL}/signup", json={
        "email": email, "username": username, "password": password,
    })
    res = requests.post(f"{GATEKEEPER_URL}/login", json={
        "email": email, "password": password,
    })
    assert res.status_code == 200, f"login failed: {res.text}"
    return res.json()["token"]


@pytest.fixture(scope="session")
def token():
    return _signup_login("notif_test")


@pytest.fixture(scope="session")
def other_token():
    return _signup_login("notif_other")


@pytest.fixture(scope="session")
def bearer(token):
    return {"Authorization": f"Bearer {token}"}


@pytest.fixture(scope="session")
def other_bearer(other_token):
    return {"Authorization": f"Bearer {other_token}"}
