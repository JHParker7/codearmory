"""Integration tests for the repo management API (the control plane).

These are the executable spec for Step 1 of ARCHITECTURE.md — they FAIL until the
`/repos` resource is implemented. They mirror test_widgets.py, plus the two fields
the git wire needs (namespace, http_url).

Contract:
  POST   /repos        createRepo  codearmory_git_factory/repos
  GET    /repos        listRepo    codearmory_git_factory/repos
  GET    /repos/{id}   getRepo     codearmory_git_factory/repos/{id}
  PATCH  /repos/{id}   updateRepo  codearmory_git_factory/repos/{id}
  DELETE /repos/{id}   deleteRepo  codearmory_git_factory/repos/{id}

createRepo response (JSON) MUST include:
  id             stable uuid (used in the RBAC resource + on-disk path)
  namespace      the URL segment for clone paths: /{namespace}/{name}.git
  name           repo name
  description
  default_branch e.g. "main"
  http_url       clone URL ending in /{namespace}/{name}.git
  created_at, updated_at
"""
import uuid
from urllib.parse import urlparse

import pytest
import requests

from conftest import GATEKEEPER_URL, SERVICE_URL, _new_user


# ── Authentication ──────────────────────────────────────────────────────────

def test_create_unauthorized():
    assert requests.post(f"{SERVICE_URL}/repos", json={"name": "x"}).status_code == 401

def test_list_unauthorized():
    assert requests.get(f"{SERVICE_URL}/repos").status_code == 401

def test_get_unauthorized():
    assert requests.get(f"{SERVICE_URL}/repos/whatever").status_code == 401


# ── Create ──────────────────────────────────────────────────────────────────

def test_create_missing_name(bearer):
    res = requests.post(f"{SERVICE_URL}/repos", headers=bearer, json={"description": "no name"})
    assert res.status_code == 400

def test_create_returns_201(bearer):
    res = requests.post(f"{SERVICE_URL}/repos", headers=bearer,
                        json={"name": "alpha", "description": "first"})
    assert res.status_code == 201, res.text
    repo = res.json()
    assert repo["id"] != ""
    assert repo["name"] == "alpha"
    assert repo["description"] == "first"
    requests.delete(f"{SERVICE_URL}/repos/{repo['id']}", headers=bearer)

def test_create_response_has_git_fields(make_repo):
    """The fields the Smart-HTTP layer and the UI depend on."""
    repo = make_repo(name="withfields")
    assert repo["namespace"], "namespace (clone-URL segment) is required"
    assert repo["default_branch"], "default_branch is required (e.g. main)"
    assert repo["http_url"].endswith(f"/{repo['namespace']}/{repo['name']}.git"), repo["http_url"]

def test_create_duplicate_name_conflicts(bearer, make_repo):
    repo = make_repo()
    res = requests.post(f"{SERVICE_URL}/repos", headers=bearer, json={"name": repo["name"]})
    assert res.status_code == 409

def test_create_rejects_bad_name(bearer):
    """Names become filesystem/URL path segments — reject traversal & separators."""
    for bad in ["../escape", "a/b", "with space", ".git", ""]:
        res = requests.post(f"{SERVICE_URL}/repos", headers=bearer, json={"name": bad})
        assert res.status_code == 400, f"expected 400 for name {bad!r}, got {res.status_code}"


# ── Name allowlist (ARCHITECTURE §6) ────────────────────────────────────────
# The rule is a positive allowlist, [A-Za-z0-9._-]+, not a blocklist of separators.
# That means dots are legal and only the specific unsafe cases are refused.

def test_create_accepts_dotted_name(make_repo):
    """A dot is a permitted character; only a .git suffix is special."""
    repo = make_repo(name=f"my.repo.{uuid.uuid4().hex[:6]}")
    assert repo["name"].startswith("my.repo."), repo["name"]


def test_create_accepts_dashes_and_underscores(make_repo):
    repo = make_repo(name=f"my-repo_v2.{uuid.uuid4().hex[:6]}")
    assert repo["id"]


def test_create_rejects_names_the_pattern_would_admit(bearer):
    """These match [A-Za-z0-9._-]+ but are still unsafe and must be refused:
    the traversal directory names, and any .git suffix (which would produce a
    doubled /name.git.git clone URL)."""
    for bad in [".", "..", "foo.git"]:
        res = requests.post(f"{SERVICE_URL}/repos", headers=bearer, json={"name": bad})
        assert res.status_code == 400, f"expected 400 for name {bad!r}, got {res.status_code}"


# ── Namespace / ownership model ─────────────────────────────────────────────
# Two identities, deliberately not conflated:
#   owner     = the gatekeeper user_id (a uuid) — the authorization filter, never
#               serialized and never in a URL
#   namespace = the human handle — the clone-URL path segment
# In this stack the two genuinely differ, which is what makes these meaningful.

def test_namespace_is_the_callers_username(make_repo, username):
    """The clone URL carries the username, not the opaque user_id."""
    repo = make_repo()
    assert repo["namespace"] == username, (
        f"namespace {repo['namespace']!r} should be the caller's username {username!r} — "
        "a uuid here means the user_id leaked into the clone URL"
    )


def test_owner_is_never_serialized(make_repo):
    """owner is the authorization filter, not client data."""
    repo = make_repo()
    assert "owner" not in repo, f"createRepo leaked the owner field: {repo}"


def test_repo_is_scoped_by_user_id_not_namespace(bearer, make_repo):
    """Ownership filters on the user_id. If create stored the *username* as the
    owner, this read-back would 404 — the user could create repos they can never
    see, list, or delete."""
    repo = make_repo()
    res = requests.get(f"{SERVICE_URL}/repos/{repo['id']}", headers=bearer)
    assert res.status_code == 200, (
        f"owner-scoped read of a just-created repo returned {res.status_code}; "
        "create likely stored the namespace instead of the user_id as owner"
    )


def test_same_name_allowed_in_different_namespaces(other_bearer, make_repo):
    """Uniqueness is (namespace, name), so two users may each own the same name —
    they resolve to different clone URLs."""
    shared = f"shared-{uuid.uuid4().hex[:8]}"
    make_repo(name=shared)  # primary user
    res = requests.post(f"{SERVICE_URL}/repos", headers=other_bearer, json={"name": shared})
    assert res.status_code == 201, (
        f"a second namespace must be able to reuse the name {shared!r}, got "
        f"{res.status_code}: {res.text}"
    )
    requests.delete(f"{SERVICE_URL}/repos/{res.json()['id']}", headers=other_bearer)


# ── Clone URL ───────────────────────────────────────────────────────────────

def test_http_url_is_absolute_with_a_real_host(make_repo):
    """http_url must be built from the configured base, not a placeholder."""
    url = make_repo()["http_url"]
    assert "<host>" not in url, f"http_url still contains the placeholder host: {url}"
    parsed = urlparse(url)
    assert parsed.scheme in ("http", "https"), f"http_url is not absolute: {url}"
    assert parsed.netloc, f"http_url has no host: {url}"


def test_http_url_path_is_namespace_and_name(make_repo):
    """The path is exactly /{namespace}/{name}.git — what the git wire routes
    will parse back into a repo lookup."""
    repo = make_repo()
    assert urlparse(repo["http_url"]).path == f"/{repo['namespace']}/{repo['name']}.git"


# ── Org repos ───────────────────────────────────────────────────────────────
# The namespace is polymorphic: an org repo is published under the org's name,
# a personal repo under the username. Ownership stays the user_id either way.

def test_org_repo_namespace_is_the_org_name():
    """org_repo=true resolves the namespace from the org, not the username.

    Uses a dedicated user, because creating an org sets that user's active org and
    would otherwise leak into other tests. Skips if the stack does not grant a
    fresh user createOrg.
    """
    u = _new_user()
    hdrs = {"Authorization": f"Bearer {u['token']}"}
    org_name = f"acme{uuid.uuid4().hex[:8]}"

    res = requests.post(f"{GATEKEEPER_URL}/orgs", headers=hdrs, json={"org_name": org_name})
    if res.status_code not in (200, 201):
        pytest.skip(f"fresh user cannot create an org here: {res.status_code} {res.text}")

    name = f"repo-{uuid.uuid4().hex[:8]}"
    res = requests.post(f"{SERVICE_URL}/repos", headers=hdrs,
                        json={"name": name, "org_repo": True})
    assert res.status_code == 201, res.text
    repo = res.json()

    try:
        assert repo["namespace"] == org_name, (
            f"namespace {repo['namespace']!r} should be the org name {org_name!r}, "
            f"not the creator's username {u['username']!r}"
        )
        assert repo["http_url"].endswith(f"/{org_name}/{name}.git"), repo["http_url"]

        # Ownership is still the creating user_id, so the creator can read it back.
        got = requests.get(f"{SERVICE_URL}/repos/{repo['id']}", headers=hdrs)
        assert got.status_code == 200, (
            f"creator cannot read back their own org repo ({got.status_code}) — "
            "an org repo must still be owner-scoped to the user_id"
        )
    finally:
        requests.delete(f"{SERVICE_URL}/repos/{repo['id']}", headers=hdrs)


# ── List ────────────────────────────────────────────────────────────────────

def test_list_returns_array(bearer):
    res = requests.get(f"{SERVICE_URL}/repos", headers=bearer)
    assert res.status_code == 200
    assert isinstance(res.json(), list)

def test_list_includes_own_repo(bearer, make_repo):
    repo = make_repo()
    names = [x["name"] for x in requests.get(f"{SERVICE_URL}/repos", headers=bearer).json()]
    assert repo["name"] in names

def test_list_excludes_other_users_repo(other_bearer, make_repo):
    repo = make_repo()  # owned by primary user
    names = [x["name"] for x in requests.get(f"{SERVICE_URL}/repos", headers=other_bearer).json()]
    assert repo["name"] not in names


# ── Get / Update / Delete ───────────────────────────────────────────────────

def test_get_not_found(bearer):
    res = requests.get(f"{SERVICE_URL}/repos/00000000-0000-0000-0000-000000000000", headers=bearer)
    assert res.status_code == 404

def test_get_own_repo(bearer, make_repo):
    repo = make_repo(description="fetch me")
    res = requests.get(f"{SERVICE_URL}/repos/{repo['id']}", headers=bearer)
    assert res.status_code == 200
    assert res.json()["description"] == "fetch me"

def test_update_own_repo(bearer, make_repo):
    repo = make_repo()
    res = requests.patch(f"{SERVICE_URL}/repos/{repo['id']}", headers=bearer,
                         json={"description": "updated"})
    assert res.status_code == 200
    assert res.json()["description"] == "updated"

def test_other_user_cannot_get_repo(other_bearer, make_repo):
    repo = make_repo()  # owned by primary user
    res = requests.get(f"{SERVICE_URL}/repos/{repo['id']}", headers=other_bearer)
    assert res.status_code in (403, 404)

def test_delete_own_repo(bearer, make_repo):
    repo = make_repo()
    assert requests.delete(f"{SERVICE_URL}/repos/{repo['id']}", headers=bearer).status_code == 204
    assert requests.get(f"{SERVICE_URL}/repos/{repo['id']}", headers=bearer).status_code == 404

def test_delete_other_users_repo(other_bearer, make_repo):
    repo = make_repo()  # owned by primary user
    res = requests.delete(f"{SERVICE_URL}/repos/{repo['id']}", headers=other_bearer)
    assert res.status_code in (403, 404)
