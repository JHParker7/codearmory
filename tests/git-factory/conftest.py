"""Shared fixtures for codearmory_git_factory integration tests.

Requires a running stack: gatekeeper (for JWTs) + this service. `./run.sh up`
brings up a minimal one via docker-compose.yml. If either service is unreachable
the whole session skips (not fails), so you can run it early.

Env:
  CODEARMORY_GIT_FACTORY_URL  default http://localhost:9002
  GATEKEEPER_URL   default http://localhost:8081
"""
import os
import time
import uuid

import pytest
import requests

SERVICE_URL = os.getenv("CODEARMORY_GIT_FACTORY_URL", "http://localhost:9002").rstrip("/")
GATEKEEPER_URL = os.getenv("GATEKEEPER_URL", "http://localhost:8081").rstrip("/")


def _reachable(url: str) -> bool:
    try:
        requests.get(url, timeout=2)
        return True
    except requests.RequestException:
        return False


@pytest.fixture(scope="session", autouse=True)
def require_stack():
    if not _reachable(f"{GATEKEEPER_URL}/healthz") and not _reachable(GATEKEEPER_URL):
        pytest.skip(f"gatekeeper not reachable at {GATEKEEPER_URL}")
    if not _reachable(f"{SERVICE_URL}/healthz") and not _reachable(SERVICE_URL):
        pytest.skip(f"codearmory_git_factory not reachable at {SERVICE_URL}")


def _new_user():
    """Sign up a fresh user in gatekeeper; return {token, username, email}.

    The username is returned because it is *not* the user_id: gatekeeper issues an
    opaque uuid for authorization and a human handle for display. The repo model
    depends on that distinction (owner = user_id, namespace = username), so tests
    need both to tell them apart.

    Retries signup through the 503 window while gatekeeper's poller warms up its
    default-grants cache (a fresh non-admin user is refused until grants load).
    """
    suffix = uuid.uuid4().hex[:8]
    email = f"codearmory_git_factory_test_{suffix}@example.com"
    username = f"codearmory_git_factory_{suffix}"
    password = "codearmory_git_factory_test_pass"

    deadline = time.monotonic() + 30
    while True:
        r = requests.post(f"{GATEKEEPER_URL}/signup", json={
            "email": email, "username": username, "password": password,
        })
        if r.status_code == 503 and time.monotonic() < deadline:
            time.sleep(2)
            continue
        assert r.status_code in (200, 201), f"signup failed: {r.status_code} {r.text}"
        break

    r = requests.post(f"{GATEKEEPER_URL}/login", json={"email": email, "password": password})
    assert r.status_code == 200, f"login failed: {r.text}"
    return {"token": r.json()["token"], "username": username, "email": email}


@pytest.fixture(scope="session")
def user():
    """The primary test user: {token, username, email}."""
    return _new_user()


@pytest.fixture(scope="session")
def other_user():
    return _new_user()


@pytest.fixture(scope="session")
def token(user):
    return user["token"]


@pytest.fixture(scope="session")
def other_token(other_user):
    return other_user["token"]


@pytest.fixture(scope="session")
def username(user):
    """The caller's gatekeeper username — what a repo's namespace must equal."""
    return user["username"]


@pytest.fixture(scope="session")
def other_username(other_user):
    return other_user["username"]


@pytest.fixture(scope="session")
def bearer(token):
    return {"Authorization": f"Bearer {token}"}


@pytest.fixture(scope="session")
def other_bearer(other_token):
    return {"Authorization": f"Bearer {other_token}"}


@pytest.fixture
def make_widget(bearer):
    """Factory: create a widget owned by the primary user; auto-cleanup."""
    created = []

    def _make(name=None, description=""):
        name = name or f"widget-{uuid.uuid4().hex[:8]}"
        res = requests.post(f"{SERVICE_URL}/widgets", headers=bearer,
                            json={"name": name, "description": description})
        assert res.status_code == 201, res.text
        wg = res.json()
        created.append(wg["id"])
        return wg

    yield _make

    for wid in created:
        requests.delete(f"{SERVICE_URL}/widgets/{wid}", headers=bearer)


@pytest.fixture
def make_repo(bearer):
    """Factory: create a repo owned by the primary user; auto-cleanup.

    Returns the createRepo JSON, which the git-wire tests rely on for:
      id, namespace, name, http_url, default_branch
    """
    created = []

    def _make(name=None, description="", headers=None, org_repo=False):
        name = name or f"repo-{uuid.uuid4().hex[:8]}"
        body = {"name": name, "description": description}
        if org_repo:
            body["org_repo"] = True
        res = requests.post(f"{SERVICE_URL}/repos", headers=headers or bearer, json=body)
        assert res.status_code == 201, res.text
        repo = res.json()
        created.append((repo["id"], headers or bearer))
        return repo

    yield _make

    for rid, hdrs in created:
        requests.delete(f"{SERVICE_URL}/repos/{rid}", headers=hdrs)
