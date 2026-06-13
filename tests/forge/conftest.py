"""Shared fixtures for Forge integration tests."""
import os
import uuid

import pytest
import requests

FORGE_URL = os.getenv("FORGE_URL", "http://localhost:8083")
# The fixtures below hit gatekeeper's unprefixed /signup and /login, which only
# exist on gatekeeper directly (not through conductor). Gatekeeper is NOT
# published to localhost by infra/local/compose.yml, so GATEKEEPER_URL must be
# set explicitly for local runs. CI sets it to http://gatekeeper:8081.
GATEKEEPER_URL = os.getenv("GATEKEEPER_URL", "http://localhost:8081")


@pytest.fixture(scope="module")
def token():
    email = f"forge_test_{uuid.uuid4().hex[:8]}@example.com"
    password = "forge_test_pass"
    username = f"forge_tester_{uuid.uuid4().hex[:8]}"
    requests.post(f"{GATEKEEPER_URL}/signup", json={
        "email": email, "username": username, "password": password
    })
    res = requests.post(f"{GATEKEEPER_URL}/login", json={
        "email": email, "password": password
    })
    return res.json()["token"]


@pytest.fixture(scope="module")
def other_token():
    email = f"forge_other_{uuid.uuid4().hex[:8]}@example.com"
    password = "forge_other_pass"
    username = f"forge_other_{uuid.uuid4().hex[:8]}"
    requests.post(f"{GATEKEEPER_URL}/signup", json={
        "email": email, "username": username, "password": password
    })
    res = requests.post(f"{GATEKEEPER_URL}/login", json={
        "email": email, "password": password
    })
    return res.json()["token"]


@pytest.fixture(scope="module")
def bearer(token):
    return {"Authorization": f"Bearer {token}"}


@pytest.fixture(scope="module")
def other_bearer(other_token):
    return {"Authorization": f"Bearer {other_token}"}
