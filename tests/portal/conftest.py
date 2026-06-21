"""
Fixtures for portal integration tests.

Expects a running stack. Defaults:
  PORTAL_URL  http://localhost:3001   portal BFF serving the SPA + proxying /api → conductor
  API_URL     http://localhost:8080   conductor (used directly to bootstrap auth)
"""

import os
import uuid

import pytest
import requests


@pytest.fixture(scope="session")
def portal_url() -> str:
    return os.getenv("PORTAL_URL", "http://localhost:3001")


@pytest.fixture(scope="session")
def api_url() -> str:
    """Conductor base URL — used directly for auth bootstrap and assertions."""
    return os.getenv("API_URL", "http://localhost:8080")


def _unique_user(api_url: str) -> dict:
    uid = uuid.uuid4().hex[:8]
    payload = {
        "email": f"portal_test_{uid}@example.com",
        "username": f"portal_{uid}",
        "password": "TestPassword1!",
    }
    resp = requests.post(f"{api_url}/gatekeeper/signup", json=payload, timeout=10)
    assert resp.status_code == 201, f"fixture signup failed: {resp.text}"
    payload["user_id"] = resp.json()["user_id"]
    return payload


@pytest.fixture(scope="session")
def user(api_url: str) -> dict:
    """A real user created via conductor — reused across the session."""
    return _unique_user(api_url)


@pytest.fixture(scope="session")
def token(api_url: str, user: dict) -> str:
    resp = requests.post(
        f"{api_url}/gatekeeper/login",
        json={"email": user["email"], "password": user["password"]},
        timeout=10,
    )
    assert resp.status_code == 200, f"fixture login failed: {resp.text}"
    return resp.json()["token"]


@pytest.fixture(scope="session")
def auth_header(token: str) -> dict:
    return {"Authorization": f"Bearer {token}"}
