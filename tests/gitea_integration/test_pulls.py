"""Integration tests for /repos/{owner}/{name}/pulls endpoints.

A Forgejo repo is bootstrapped via the admin API with two branches so that
PR creation, retrieval, and merge can be tested end-to-end without needing
a local git clone.
"""
import uuid

import pytest
import requests

from conftest import (
    GITEA_INTEGRATION_URL,
    GITEA_URL,
    GITEA_ADMIN_TOKEN,
    _create_forgejo_repo,
    _create_forgejo_branch,
    _create_forgejo_file,
)


@pytest.fixture(scope="module")
def pr_repo(linked, fg_user):
    """Auto-initialised Forgejo repo with a feature branch ready for a PR."""
    name = f"pr-itest-{uuid.uuid4().hex[:8]}"

    # Create via the gitea_integration service so the service owns the resource.
    res = requests.post(f"{GITEA_INTEGRATION_URL}/repos",
                        headers=linked,
                        json={"name": name, "auto_init": True, "default_branch": "main"})
    assert res.status_code == 201, f"repo create failed: {res.text}"

    owner = fg_user["username"]
    # Add a commit on a feature branch so the PR has something to merge.
    _create_forgejo_branch(fg_user, owner, name, "feature")
    _create_forgejo_file(fg_user, owner, name, "feature.txt",
                         "hello from feature branch", branch="feature")

    yield {"owner": owner, "name": name}

    requests.delete(f"{GITEA_INTEGRATION_URL}/repos/{owner}/{name}", headers=linked)


@pytest.fixture(scope="module")
def open_pr(linked, pr_repo):
    """Creates one open PR and returns its response body."""
    res = requests.post(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls",
        headers=linked,
        json={"title": "Integration test PR", "head": "feature", "base": "main"},
    )
    assert res.status_code == 201, f"PR create failed: {res.text}"
    return res.json()


# ── auth ───────────────────────────────────────────────────────────────────────

def test_list_pulls_no_auth(pr_repo):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls")
    assert res.status_code == 401

def test_create_pull_no_auth(pr_repo):
    res = requests.post(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls",
        json={"title": "x", "head": "feature", "base": "main"})
    assert res.status_code == 401

def test_get_pull_no_auth(pr_repo):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls/1")
    assert res.status_code == 401


# ── create validation ──────────────────────────────────────────────────────────

def test_create_pull_missing_title(linked, pr_repo):
    res = requests.post(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls",
        headers=linked,
        json={"head": "feature", "base": "main"},
    )
    assert res.status_code == 400

def test_create_pull_missing_head(linked, pr_repo):
    res = requests.post(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls",
        headers=linked,
        json={"title": "x", "base": "main"},
    )
    assert res.status_code == 400

def test_create_pull_missing_base(linked, pr_repo):
    res = requests.post(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls",
        headers=linked,
        json={"title": "x", "head": "feature"},
    )
    assert res.status_code == 400


# ── create ─────────────────────────────────────────────────────────────────────


def test_open_pr_response_shape(open_pr):
    for field in ("id", "number", "title", "state", "html_url", "head", "base"):
        assert field in open_pr, f"missing field: {field}"

def test_open_pr_state_is_open(open_pr):
    assert open_pr["state"] == "open"

def test_open_pr_title(open_pr):
    assert open_pr["title"] == "Integration test PR"

def test_open_pr_branches(open_pr):
    assert open_pr["head"]["ref"] == "feature"
    assert open_pr["base"]["ref"] == "main"


# ── list ───────────────────────────────────────────────────────────────────────

def test_list_pulls_returns_array(linked, pr_repo, open_pr):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls",
        headers=linked,
    )
    assert res.status_code == 200
    assert isinstance(res.json(), list)

def test_list_pulls_includes_open_pr(linked, pr_repo, open_pr):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls",
        headers=linked,
    )
    numbers = [p["number"] for p in res.json()]
    assert open_pr["number"] in numbers

def test_list_pulls_invalid_state(linked, pr_repo):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls",
        headers=linked,
        params={"state": "bogus"},
    )
    assert res.status_code == 400

def test_list_closed_pulls_returns_array(linked, pr_repo, open_pr):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls",
        headers=linked,
        params={"state": "closed"},
    )
    assert res.status_code == 200
    assert isinstance(res.json(), list)


# ── get ────────────────────────────────────────────────────────────────────────

def test_get_pull_returns_200(linked, pr_repo, open_pr):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls/{open_pr['number']}",
        headers=linked,
    )
    assert res.status_code == 200

def test_get_pull_correct_number(linked, pr_repo, open_pr):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls/{open_pr['number']}",
        headers=linked,
    )
    assert res.json()["number"] == open_pr["number"]

def test_get_pull_not_found(linked, pr_repo):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls/99999",
        headers=linked,
    )
    assert res.status_code == 404


# ── merge ──────────────────────────────────────────────────────────────────────

def test_merge_pull_returns_204(linked, pr_repo, open_pr):
    res = requests.post(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls/{open_pr['number']}/merge",
        headers=linked,
        json={"Do": "merge", "merge_message_field": "Integration test merge"},
    )
    assert res.status_code == 204

def test_merged_pull_state_is_closed(linked, pr_repo, open_pr):
    res = requests.get(
        f"{GITEA_INTEGRATION_URL}/repos/{pr_repo['owner']}/{pr_repo['name']}/pulls/{open_pr['number']}",
        headers=linked,
    )
    body = res.json()
    assert body["merged"] is True or body["state"] == "closed"
