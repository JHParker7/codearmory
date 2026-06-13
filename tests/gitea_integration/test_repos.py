"""Integration tests for /repos endpoints."""
import uuid

import pytest
import requests

from conftest import (
    GITEA_INTEGRATION_URL,
    _create_forgejo_repo,
    _delete_forgejo_user,
    _signup_login,
    _create_forgejo_user,
)


@pytest.fixture(scope="module")
def repo_name():
    return f"itest-{uuid.uuid4().hex[:8]}"


@pytest.fixture(scope="module")
def created_repo(linked, fg_user, repo_name):
    """Creates a repo via the service and deletes it after the module."""
    res = requests.post(
        f"{GITEA_INTEGRATION_URL}/repos",
        headers=linked,
        json={"name": repo_name, "auto_init": True, "default_branch": "main"},
    )
    assert res.status_code == 201, f"repo create failed: {res.text}"
    yield res.json()
    requests.delete(f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/{repo_name}",
                    headers=linked)


# ── auth ───────────────────────────────────────────────────────────────────────

def test_list_repos_no_auth():
    res = requests.get(f"{GITEA_INTEGRATION_URL}/repos")
    assert res.status_code == 401

def test_create_repo_no_auth():
    res = requests.post(f"{GITEA_INTEGRATION_URL}/repos", json={"name": "x"})
    assert res.status_code == 401

def test_get_repo_no_auth(fg_user, repo_name):
    res = requests.get(f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/{repo_name}")
    assert res.status_code == 401

def test_delete_repo_no_auth(fg_user, repo_name):
    res = requests.delete(f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/{repo_name}")
    assert res.status_code == 401


# ── unlinked account ───────────────────────────────────────────────────────────

def test_create_repo_unlinked_returns_422(other_bearer):
    res = requests.post(f"{GITEA_INTEGRATION_URL}/repos",
                        headers=other_bearer,
                        json={"name": "will-fail"})
    assert res.status_code == 422

def test_list_repos_unlinked_returns_422(other_bearer):
    res = requests.get(f"{GITEA_INTEGRATION_URL}/repos", headers=other_bearer)
    assert res.status_code == 422


# ── create ─────────────────────────────────────────────────────────────────────

def test_create_repo_returns_201(linked):
    name = f"itest-{uuid.uuid4().hex[:8]}"
    res = requests.post(f"{GITEA_INTEGRATION_URL}/repos",
                        headers=linked,
                        json={"name": name, "auto_init": True})
    try:
        assert res.status_code == 201
    finally:
        # best-effort cleanup: the delete route is /repos/{owner}/{name}, and the
        # created repo's full_name is already "owner/name", so split it to build
        # the correct path. (The previous .replace('/', '/', 1) was a no-op and
        # leaked the repo.)
        body = res.json()
        full_name = body.get("full_name", "")
        if "/" in full_name:
            owner, repo_name = full_name.split("/", 1)
            requests.delete(
                f"{GITEA_INTEGRATION_URL}/repos/{owner}/{repo_name}",
                headers=linked,
            )

def test_create_repo_missing_name(linked):
    res = requests.post(f"{GITEA_INTEGRATION_URL}/repos", headers=linked, json={})
    assert res.status_code == 400

def test_create_repo_response_shape(created_repo):
    for field in ("id", "name", "full_name", "html_url", "clone_url", "default_branch"):
        assert field in created_repo, f"missing field: {field}"


# ── get ────────────────────────────────────────────────────────────────────────

def test_get_repo_returns_200(linked, fg_user, created_repo, repo_name):
    res = requests.get(f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/{repo_name}",
                       headers=linked)
    assert res.status_code == 200

def test_get_repo_correct_name(linked, fg_user, created_repo, repo_name):
    res = requests.get(f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/{repo_name}",
                       headers=linked)
    assert res.json()["name"] == repo_name

def test_get_repo_not_found(linked, fg_user):
    res = requests.get(f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/no-such-repo",
                       headers=linked)
    assert res.status_code == 404


# ── list ───────────────────────────────────────────────────────────────────────

def test_list_repos_returns_array(linked, created_repo):
    res = requests.get(f"{GITEA_INTEGRATION_URL}/repos", headers=linked)
    assert res.status_code == 200
    assert isinstance(res.json(), list)

def test_list_repos_includes_created(linked, created_repo, repo_name):
    res = requests.get(f"{GITEA_INTEGRATION_URL}/repos", headers=linked)
    names = [r["name"] for r in res.json()]
    assert repo_name in names


# ── branches / tags / commits ──────────────────────────────────────────────────

def test_list_branches_returns_array(linked, fg_user, created_repo, repo_name):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/{repo_name}/branches",
        headers=linked,
    )
    assert res.status_code == 200
    assert isinstance(res.json(), list)

def test_list_branches_includes_default(linked, fg_user, created_repo, repo_name):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/{repo_name}/branches",
        headers=linked,
    )
    branch_names = [b["name"] for b in res.json()]
    assert "main" in branch_names

def test_list_tags_returns_array(linked, fg_user, created_repo, repo_name):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/{repo_name}/tags",
        headers=linked,
    )
    assert res.status_code == 200
    assert isinstance(res.json(), list)

def test_list_commits_returns_array(linked, fg_user, created_repo, repo_name):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/{repo_name}/commits",
        headers=linked,
    )
    assert res.status_code == 200
    assert isinstance(res.json(), list)

def test_list_releases_returns_array(linked, fg_user, created_repo, repo_name):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/{repo_name}/releases",
        headers=linked,
    )
    assert res.status_code == 200
    assert isinstance(res.json(), list)


# ── repo isolation ─────────────────────────────────────────────────────────────

def test_get_other_users_repo_returns_403(linked, other_fg_user):
    """A linked user cannot access a repo owned by a different Forgejo user."""
    other_repo_name = f"other-{uuid.uuid4().hex[:8]}"
    _create_forgejo_repo(other_fg_user, other_repo_name)
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{other_fg_user['username']}/{other_repo_name}",
        headers=linked,
    )
    assert res.status_code in (403, 404)


# ── delete ─────────────────────────────────────────────────────────────────────

def test_delete_repo_returns_204(linked, fg_user):
    name = f"del-{uuid.uuid4().hex[:8]}"
    requests.post(f"{GITEA_INTEGRATION_URL}/repos",
                  headers=linked,
                  json={"name": name, "auto_init": True})
    res = requests.delete(f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/{name}",
                          headers=linked)
    assert res.status_code == 204

def test_delete_nonexistent_repo_returns_404(linked, fg_user):
    res = requests.delete(f"{GITEA_INTEGRATION_URL}/repos/{fg_user['username']}/no-such-repo",
                          headers=linked)
    assert res.status_code == 404
