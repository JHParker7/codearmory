"""Shared fixtures for gitea_integration integration tests.

Requires a live Forgejo instance and the gitea_integration service to be
reachable.  Set the following env vars before running:

    GITEA_INTEGRATION_URL  — gitea_integration service base URL
    GATEKEEPER_URL         — gatekeeper service base URL
    GITEA_URL              — Forgejo/Gitea base URL (used for admin setup)
    GITEA_ADMIN_TOKEN      — Forgejo admin token with user-management scope
"""
import base64
import os
import uuid

import pytest
import requests

GITEA_INTEGRATION_URL = os.getenv("GITEA_INTEGRATION_URL", "http://localhost:8088")
GATEKEEPER_URL        = os.getenv("GATEKEEPER_URL",        "http://localhost:8081")
GITEA_URL             = os.getenv("GITEA_URL",             "http://localhost:3000")
GITEA_ADMIN_TOKEN     = os.getenv("GITEA_ADMIN_TOKEN",     "")


# ── helpers ────────────────────────────────────────────────────────────────────

def _signup_login(tag=""):
    uid      = uuid.uuid4().hex[:8]
    email    = f"gi_{tag}{uid}@example.com"
    username = f"gi_{tag}{uid}"
    password = "TestPass_123!"
    requests.post(f"{GATEKEEPER_URL}/signup",
                  json={"email": email, "username": username, "password": password})
    res = requests.post(f"{GATEKEEPER_URL}/login",
                        json={"email": email, "password": password})
    assert res.status_code == 200, f"CA login failed: {res.text}"
    return res.json()["token"]


def _create_forgejo_user():
    username = f"fg{uuid.uuid4().hex[:8]}"
    res = requests.post(
        f"{GITEA_URL}/api/v1/admin/users",
        headers={"Authorization": f"token {GITEA_ADMIN_TOKEN}"},
        json={
            "username":             username,
            "email":                f"{username}@example.com",
            "password":             "FgTest_123!",
            "must_change_password": False,
            "login_name":           username,
            "source_id":            0,
        },
    )
    assert res.status_code == 201, f"Forgejo user create failed: {res.text}"
    pat = requests.post(
        f"{GITEA_URL}/api/v1/users/{username}/tokens",
        headers={"Authorization": f"token {GITEA_ADMIN_TOKEN}", "Sudo": username},
        json={"name": "itest-token"},
    )
    assert pat.status_code == 201, f"Forgejo PAT create failed: {pat.text}"
    return {"username": username, "token": pat.json()["sha1"]}


def _delete_forgejo_user(username):
    requests.delete(
        f"{GITEA_URL}/api/v1/admin/users/{username}",
        headers={"Authorization": f"token {GITEA_ADMIN_TOKEN}"},
        params={"purge": True},
    )


def _fg_headers(token):
    return {"Authorization": f"token {token}", "Content-Type": "application/json"}


def _create_forgejo_repo(fg_user, name, auto_init=True):
    res = requests.post(
        f"{GITEA_URL}/api/v1/user/repos",
        headers={"Authorization": f"token {fg_user['token']}", "Sudo": fg_user["username"]},
        json={"name": name, "auto_init": auto_init, "default_branch": "main", "private": False},
    )
    assert res.status_code == 201, f"Forgejo repo create failed: {res.text}"
    return res.json()


def _create_forgejo_branch(fg_user, owner, repo, new_branch, from_branch="main"):
    res = requests.post(
        f"{GITEA_URL}/api/v1/repos/{owner}/{repo}/branches",
        headers=_fg_headers(fg_user["token"]),
        json={"new_branch_name": new_branch, "old_branch_name": from_branch},
    )
    assert res.status_code == 201, f"Forgejo branch create failed: {res.text}"


def _create_forgejo_file(fg_user, owner, repo, path, content, branch="main"):
    encoded = base64.b64encode(content.encode()).decode()
    res = requests.post(
        f"{GITEA_URL}/api/v1/repos/{owner}/{repo}/contents/{path}",
        headers=_fg_headers(fg_user["token"]),
        json={"message": f"add {path}", "content": encoded, "branch": branch},
    )
    assert res.status_code in (200, 201), f"Forgejo file create failed: {res.text}"


# ── session fixtures ───────────────────────────────────────────────────────────

@pytest.fixture(scope="session")
def bearer():
    return {"Authorization": f"Bearer {_signup_login('a')}"}


@pytest.fixture(scope="session")
def other_bearer():
    return {"Authorization": f"Bearer {_signup_login('b')}"}


@pytest.fixture(scope="session")
def fg_user():
    user = _create_forgejo_user()
    yield user
    _delete_forgejo_user(user["username"])


@pytest.fixture(scope="session")
def other_fg_user():
    user = _create_forgejo_user()
    yield user
    _delete_forgejo_user(user["username"])


@pytest.fixture(scope="session")
def linked(bearer, fg_user):
    """Links the primary CA user to their Forgejo account for the whole session."""
    res = requests.put(
        f"{GITEA_INTEGRATION_URL}/account",
        headers=bearer,
        json={"gitea_username": fg_user["username"], "gitea_token": fg_user["token"]},
    )
    assert res.status_code in (200, 201), f"account link failed: {res.text}"
    yield bearer
    requests.delete(f"{GITEA_INTEGRATION_URL}/account", headers=bearer)
