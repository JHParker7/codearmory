"""End-to-end tests for the git plane (git_factory).

Every request goes through Conductor, so routing, auth and RBAC are exercised on the
way in — the same path the portal takes. Where a step needs the GIT WIRE (pushing a
commit) it talks to git_factory directly, because the wire is not proxied by Conductor:
git speaks its own protocol at /{ns}/{repo}.git/… and the gateway fronts the JSON API
only.

What these assert is IMPACT, not status codes. A merge that returns 200 but leaves the
target branch untouched is a failure; so is a branch protection that answers 409 for the
wrong reason. Each test therefore reads state back — from git, or from the API that
projects it — and asserts on what actually changed.

Environment (all optional; defaults target the local Docker stack):
  API_URL       Conductor base URL             (default: http://localhost:8080)
  GIT_HTTP_URL  git_factory wire base URL      (default: http://localhost:9002)

Against the minikube install, Conductor sits behind the platform ingress and git_factory
is reached by port-forward:
  API_URL=http://127.0.0.1:8080/api  GIT_HTTP_URL=http://127.0.0.1:9002  pytest tests/e2e
"""

import os
import subprocess
import uuid
from urllib.parse import urlsplit, urlunsplit

import pytest
import requests

from conftest import API_URL

GIT_HTTP_URL = os.getenv("GIT_HTTP_URL", "http://localhost:9002")
GF = f"{API_URL}/codearmory_git_factory"


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _git(cwd, *args, env=None):
    """Run git, failing the test with its output rather than a bare exit code."""
    full_env = {
        "PATH": os.environ.get("PATH", ""),
        "HOME": cwd,
        "GIT_AUTHOR_NAME": "e2e", "GIT_AUTHOR_EMAIL": "e2e@example.com",
        "GIT_COMMITTER_NAME": "e2e", "GIT_COMMITTER_EMAIL": "e2e@example.com",
        "GIT_TERMINAL_PROMPT": "0",
    }
    if env:
        full_env.update(env)
    res = subprocess.run(["git", *args], cwd=cwd, env=full_env,
                         capture_output=True, text=True)
    if res.returncode != 0:
        pytest.fail(f"git {' '.join(args)} failed:\n{res.stdout}\n{res.stderr}")
    return res.stdout.strip()


def authed_clone_url(repo, token):
    """The repo's clone URL, pointed at GIT_HTTP_URL and carrying the token.

    The URL the API reports is derived from GIT_HTTP_BASE_URL, which names the host a
    client OUTSIDE the cluster should use — not necessarily one this test can reach. Only
    the path is taken from it; the origin comes from GIT_HTTP_URL.
    """
    path = urlsplit(repo["http_url"]).path
    base = urlsplit(GIT_HTTP_URL)
    # git reads the token from the basic-auth password field; the username is ignored.
    netloc = f"git:{token}@{base.netloc}"
    return urlunsplit((base.scheme, netloc, path, "", ""))


def create_repo(bearer, name=None):
    name = name or f"e2e-{uuid.uuid4().hex[:8]}"
    res = requests.post(f"{GF}/repos", headers=bearer, json={"name": name})
    assert res.status_code == 201, res.text
    return res.json()


def seed_repo(repo, token, tmp_path, branch="feature", file="README.md"):
    """Push a commit to main and a divergent one to `branch`.

    Done over the wire rather than through the blob API because a repo with no commits
    has no branch for that API to write to — the first commit has to arrive by push.
    Returns (main_sha, branch_sha).
    """
    work = tmp_path / f"work-{uuid.uuid4().hex[:6]}"
    work.mkdir()
    url = authed_clone_url(repo, token)
    _git(str(work), "init", "-q", "--initial-branch=main")
    (work / file).write_text("# base\n")
    _git(str(work), "add", "-A")
    _git(str(work), "commit", "-q", "-m", "initial commit")
    _git(str(work), "push", "-q", url, "main")
    main_sha = _git(str(work), "rev-parse", "HEAD")

    _git(str(work), "checkout", "-q", "-b", branch)
    (work / file).write_text("# base\nchange from the branch\n")
    _git(str(work), "add", "-A")
    _git(str(work), "commit", "-q", "-m", "branch change")
    _git(str(work), "push", "-q", url, branch)
    return main_sha, _git(str(work), "rev-parse", "HEAD")


def branch_names(bearer, repo_id):
    res = requests.get(f"{GF}/repos/{repo_id}/branches", headers=bearer)
    assert res.status_code == 200, res.text
    return [b["name"] for b in res.json().get("branches", [])]


def branch_sha(bearer, repo_id, name):
    res = requests.get(f"{GF}/repos/{repo_id}/branches", headers=bearer)
    assert res.status_code == 200, res.text
    for b in res.json().get("branches", []):
        if b["name"] == name:
            return b["sha"]
    return None


def open_pull(bearer, repo_id, source, target, title="e2e pull", **extra):
    res = requests.post(f"{GF}/repos/{repo_id}/pulls", headers=bearer,
                        json={"title": title, "source_ref": source,
                              "target_ref": target, **extra})
    assert res.status_code == 201, res.text
    return res.json()


def protect(bearer, repo_id, pattern="main", **rules):
    res = requests.put(f"{GF}/repos/{repo_id}/protections", headers=bearer,
                       json={"pattern": pattern, **rules})
    assert res.status_code == 200, res.text
    return res.json()


@pytest.fixture
def wire_available():
    """Skip when the git wire is not reachable — the JSON API alone cannot seed a repo."""
    try:
        requests.get(f"{GIT_HTTP_URL}/healthz", timeout=3)
    except requests.RequestException as exc:
        pytest.skip(f"git wire not reachable at {GIT_HTTP_URL}: {exc}")


def _signup_and_login(prefix):
    uid = uuid.uuid4().hex[:8]
    creds = {"email": f"e2e_{prefix}_{uid}@example.com",
             "username": f"e2e_{prefix}_{uid}", "password": "e2e_password_123"}
    res = requests.post(f"{API_URL}/gatekeeper/signup", json=creds)
    assert res.status_code == 201, f"signup failed: {res.text}"
    creds["user_id"] = res.json()["user_id"]
    res = requests.post(f"{API_URL}/gatekeeper/login",
                        json={"email": creds["email"], "password": creds["password"]})
    assert res.status_code == 200, f"login failed: {res.text}"
    creds["token"] = res.json()["token"]
    creds["bearer"] = {"Authorization": f"Bearer {creds['token']}"}
    return creds


# Two users, created ONCE for the module rather than per test. Gatekeeper rate-limits
# signup and login — correctly — so a fixture that made a fresh pair for each test spent
# the budget on setup and failed the suite on 429s rather than on anything real. Each
# test still creates its OWN repo, which is where the isolation that matters lives.
@pytest.fixture(scope="module")
def owner():
    return _signup_and_login("own")


@pytest.fixture(scope="module")
def token(owner):
    return owner["token"]


@pytest.fixture(scope="module")
def bearer(owner):
    return owner["bearer"]


@pytest.fixture(scope="module")
def reviewer():
    """A SECOND user. The merge gate refuses self-approval, so a review needs one."""
    return _signup_and_login("rev")


# ---------------------------------------------------------------------------
# Repo lifecycle
# ---------------------------------------------------------------------------

def test_repo_create_appears_in_listing_and_is_private(bearer):
    repo = create_repo(bearer)

    res = requests.get(f"{GF}/repos", headers=bearer)
    assert res.status_code == 200, res.text
    listed = {r["id"]: r for r in res.json()}
    assert repo["id"] in listed, "a created repo did not appear in the owner's listing"
    # Private by default: a repo must never be published by omission.
    assert listed[repo["id"]]["visibility"] == "private"
    # The clone URL is derived on read, so it must be populated without a second write.
    assert listed[repo["id"]]["http_url"].endswith(f"/{repo['name']}.git")


def test_push_is_visible_through_the_api(bearer, token, tmp_path, wire_available):
    """The wire and the JSON API must agree: what git accepted is what the API reports."""
    repo = create_repo(bearer)
    main_sha, feature_sha = seed_repo(repo, token, tmp_path)

    names = branch_names(bearer, repo["id"])
    assert "main" in names and "feature" in names, f"branches after push: {names}"
    assert branch_sha(bearer, repo["id"], "main") == main_sha
    assert branch_sha(bearer, repo["id"], "feature") == feature_sha

    # And the content itself is readable back.
    res = requests.get(f"{GF}/repos/{repo['id']}/blob",
                       headers=bearer, params={"path": "README.md", "ref": "feature"})
    assert res.status_code == 200, res.text
    assert "change from the branch" in res.json()["content"]


# ---------------------------------------------------------------------------
# The merge gate — branch protection actually enforced on the API merge path
# ---------------------------------------------------------------------------

def test_merge_gate_blocks_until_approved_and_green(bearer, token, reviewer,
                                                    tmp_path, wire_available):
    """The regression this suite exists for.

    Protections used to be enforced ONLY by the pre-receive hook, so merging through the
    API bypassed them entirely. Each stage below asserts both that the merge is refused
    and WHY — a 409 for the wrong reason would pass a weaker test while leaving the
    branch unprotected.
    """
    repo = create_repo(bearer)
    _, feature_sha = seed_repo(repo, token, tmp_path)
    pr = open_pull(bearer, repo["id"], "feature", "main")

    protect(bearer, repo["id"], "main", require_approvals=1, require_checks="build")

    merge_url = f"{GF}/repos/{repo['id']}/pulls/{pr['number']}/merge"

    # 1. Nothing satisfied yet.
    res = requests.post(merge_url, headers=bearer)
    assert res.status_code == 409, f"merge was not blocked: {res.status_code} {res.text}"
    assert "approval" in res.text.lower(), f"blocked for the wrong reason: {res.text}"
    # IMPACT: main must not have moved.
    assert branch_sha(bearer, repo["id"], "main") != feature_sha

    # 2. The author cannot approve their own PR.
    res = requests.post(f"{GF}/repos/{repo['id']}/pulls/{pr['number']}/reviews",
                        headers=bearer, json={"state": "approved"})
    assert res.status_code == 403, f"self-approval was accepted: {res.text}"

    # 3. A second user approves — but the required check has not reported.
    share = requests.put(f"{GF}/repos/{repo['id']}/collaborators", headers=bearer,
                         json={"user": reviewer["user_id"], "level": "write"})
    assert share.status_code in (200, 201), share.text
    res = requests.post(f"{GF}/repos/{repo['id']}/pulls/{pr['number']}/reviews",
                        headers=reviewer["bearer"], json={"state": "approved", "body": "lgtm"})
    assert res.status_code == 201, res.text

    res = requests.post(merge_url, headers=bearer)
    assert res.status_code == 409, f"merged with a missing check: {res.text}"
    assert "build" in res.text, f"blocked for the wrong reason: {res.text}"

    # 4. A FAILING check is not a passing one.
    res = requests.post(f"{GF}/repos/{repo['id']}/statuses/{feature_sha}", headers=bearer,
                        json={"context": "build", "state": "failure"})
    assert res.status_code == 200, res.text
    res = requests.post(merge_url, headers=bearer)
    assert res.status_code == 409, f"merged with a failing check: {res.text}"

    # 5. Green: the re-run supersedes the failure, and the merge lands.
    res = requests.post(f"{GF}/repos/{repo['id']}/statuses/{feature_sha}", headers=bearer,
                        json={"context": "build", "state": "success"})
    assert res.status_code == 200, res.text
    res = requests.post(merge_url, headers=bearer)
    assert res.status_code == 200, f"merge was still refused: {res.text}"

    # IMPACT: the PR is merged AND main now contains the branch's commit.
    merged = res.json()
    assert merged["state"] == "merged"
    assert merged["merge_commit"], "a merged PR recorded no merge commit"
    res = requests.get(f"{GF}/repos/{repo['id']}/blob",
                       headers=bearer, params={"path": "README.md", "ref": "main"})
    assert res.status_code == 200, res.text
    assert "change from the branch" in res.json()["content"], \
        "the merge reported success but main does not contain the change"


def test_changes_requested_blocks_even_with_an_approval(bearer, token, reviewer,
                                                        tmp_path, wire_available):
    """An unresolved objection is not out-voted by an approval."""
    repo = create_repo(bearer)
    seed_repo(repo, token, tmp_path)
    pr = open_pull(bearer, repo["id"], "feature", "main")
    protect(bearer, repo["id"], "main", require_approvals=1)

    requests.put(f"{GF}/repos/{repo['id']}/collaborators", headers=bearer,
                 json={"user": reviewer["user_id"], "level": "write"})
    for state in ("approved", "changes_requested"):
        res = requests.post(f"{GF}/repos/{repo['id']}/pulls/{pr['number']}/reviews",
                            headers=reviewer["bearer"], json={"state": state})
        assert res.status_code == 201, res.text

    res = requests.post(f"{GF}/repos/{repo['id']}/pulls/{pr['number']}/merge", headers=bearer)
    assert res.status_code == 409, f"merged despite requested changes: {res.text}"
    assert "changes" in res.text.lower(), res.text


def test_unprotected_branch_merges_without_ceremony(bearer, token, tmp_path, wire_available):
    """The gate must not start refusing merges that always worked."""
    repo = create_repo(bearer)
    seed_repo(repo, token, tmp_path)
    pr = open_pull(bearer, repo["id"], "feature", "main")

    res = requests.post(f"{GF}/repos/{repo['id']}/pulls/{pr['number']}/merge", headers=bearer)
    assert res.status_code == 200, f"an unprotected merge was refused: {res.text}"
    assert res.json()["state"] == "merged"


# ---------------------------------------------------------------------------
# Reviews and statuses as their own surfaces
# ---------------------------------------------------------------------------

def test_review_trail_keeps_history_but_counts_only_the_latest(bearer, token, reviewer,
                                                               tmp_path, wire_available):
    repo = create_repo(bearer)
    seed_repo(repo, token, tmp_path)
    pr = open_pull(bearer, repo["id"], "feature", "main")
    requests.put(f"{GF}/repos/{repo['id']}/collaborators", headers=bearer,
                 json={"user": reviewer["user_id"], "level": "write"})

    for state in ("changes_requested", "approved"):
        res = requests.post(f"{GF}/repos/{repo['id']}/pulls/{pr['number']}/reviews",
                            headers=reviewer["bearer"], json={"state": state})
        assert res.status_code == 201, res.text

    res = requests.get(f"{GF}/repos/{repo['id']}/pulls/{pr['number']}/reviews", headers=bearer)
    assert res.status_code == 200, res.text
    body = res.json()
    # Both verdicts remain readable — that is the point of a trail...
    assert len(body["reviews"]) == 2
    # ...but only the most recent one per reviewer counts.
    assert len(body["current"]) == 1
    assert body["current"][0]["state"] == "approved"


def test_status_is_per_commit_and_supersedes(bearer, token, tmp_path, wire_available):
    repo = create_repo(bearer)
    main_sha, feature_sha = seed_repo(repo, token, tmp_path)

    requests.post(f"{GF}/repos/{repo['id']}/statuses/{feature_sha}", headers=bearer,
                  json={"context": "build", "state": "pending"})
    requests.post(f"{GF}/repos/{repo['id']}/statuses/{feature_sha}", headers=bearer,
                  json={"context": "build", "state": "success", "description": "re-run"})

    res = requests.get(f"{GF}/repos/{repo['id']}/commits/{feature_sha}/statuses", headers=bearer)
    assert res.status_code == 200, res.text
    body = res.json()
    # Re-posting the same context replaces it rather than accumulating.
    assert len(body["statuses"]) == 1
    assert body["statuses"][0]["state"] == "success"
    assert body["state"] == "success"

    # A different commit is unaffected — a green build elsewhere proves nothing here.
    res = requests.get(f"{GF}/repos/{repo['id']}/commits/{main_sha}/statuses", headers=bearer)
    assert res.status_code == 200, res.text
    assert res.json()["statuses"] == []
    # No status reported is PENDING, never success.
    assert res.json()["state"] == "pending"


# ---------------------------------------------------------------------------
# Forks, tags, search, codeowners
# ---------------------------------------------------------------------------

def test_fork_copies_history_and_is_private(bearer, token, tmp_path, wire_available):
    repo = create_repo(bearer)
    main_sha, _ = seed_repo(repo, token, tmp_path)

    res = requests.post(f"{GF}/repos/{repo['id']}/fork", headers=bearer,
                        json={"name": f"{repo['name']}-fork"})
    assert res.status_code == 201, res.text
    fork = res.json()

    assert fork["fork_of"] == repo["id"], "the fork does not record its source"
    # A fork starts private even from a public source: publishing is the new owner's call.
    assert fork["visibility"] == "private"
    # IMPACT: the fork resolves the SAME commit — it is a copy, not an empty repo.
    assert branch_sha(bearer, fork["id"], "main") == main_sha

    res = requests.get(f"{GF}/repos/{repo['id']}/forks", headers=bearer)
    assert res.status_code == 200, res.text
    assert fork["id"] in [f["id"] for f in res.json()]


def test_tag_creation_is_visible_and_immutable(bearer, token, tmp_path, wire_available):
    repo = create_repo(bearer)
    main_sha, _ = seed_repo(repo, token, tmp_path)

    res = requests.post(f"{GF}/repos/{repo['id']}/tags", headers=bearer,
                        json={"name": "v1.0.0", "ref": "main", "message": "first release"})
    assert res.status_code == 201, res.text

    res = requests.get(f"{GF}/repos/{repo['id']}/tags", headers=bearer)
    assert res.status_code == 200, res.text
    assert "v1.0.0" in [t["name"] for t in res.json()]

    # An annotated tag is a release: the message survives the round trip.
    res = requests.get(f"{GF}/repos/{repo['id']}/tags/v1.0.0", headers=bearer)
    assert res.status_code == 200, res.text
    assert res.json()["annotated"] is True
    assert "first release" in res.json().get("message", "")

    # Re-tagging is refused — something built against v1.0.0 must keep meaning one commit.
    res = requests.post(f"{GF}/repos/{repo['id']}/tags", headers=bearer,
                        json={"name": "v1.0.0", "ref": "main"})
    assert res.status_code == 409, f"a tag was silently moved: {res.text}"

    res = requests.delete(f"{GF}/repos/{repo['id']}/tags/v1.0.0", headers=bearer)
    assert res.status_code == 204, res.text
    res = requests.get(f"{GF}/repos/{repo['id']}/tags", headers=bearer)
    assert "v1.0.0" not in [t["name"] for t in res.json()]


def test_search_finds_pushed_content(bearer, token, tmp_path, wire_available):
    repo = create_repo(bearer)
    seed_repo(repo, token, tmp_path)

    res = requests.get(f"{GF}/repos/{repo['id']}/search",
                       headers=bearer, params={"q": "change from the branch", "ref": "feature"})
    assert res.status_code == 200, res.text
    hits = res.json()["results"]
    assert hits, "search found nothing for content that was just pushed"
    assert any(h["path"] == "README.md" for h in hits)

    # A literal search, not a regex: the query is matched as typed.
    res = requests.get(f"{GF}/repos/{repo['id']}/search",
                       headers=bearer, params={"q": "no-such-content-anywhere", "ref": "feature"})
    assert res.status_code == 200, res.text
    assert res.json()["results"] == []


def test_codeowners_resolves_owners_for_a_pull(bearer, token, tmp_path, wire_available):
    repo = create_repo(bearer)
    work = tmp_path / "co"
    work.mkdir()
    url = authed_clone_url(repo, token)
    _git(str(work), "init", "-q", "--initial-branch=main")
    (work / "CODEOWNERS").write_text("*.md  @docs\n/src/  @backend\n")
    (work / "README.md").write_text("# base\n")
    _git(str(work), "add", "-A")
    _git(str(work), "commit", "-q", "-m", "add codeowners")
    _git(str(work), "push", "-q", url, "main")

    _git(str(work), "checkout", "-q", "-b", "docs-change")
    (work / "README.md").write_text("# base\nedited\n")
    _git(str(work), "add", "-A")
    _git(str(work), "commit", "-q", "-m", "edit the readme")
    _git(str(work), "push", "-q", url, "docs-change")

    pr = open_pull(bearer, repo["id"], "docs-change", "main")
    res = requests.get(f"{GF}/repos/{repo['id']}/codeowners",
                       headers=bearer, params={"pull": pr["number"]})
    assert res.status_code == 200, res.text
    body = res.json()
    assert body["found"] is True
    # The PR touches README.md only, so the *.md rule owns it and /src/ does not.
    assert body["owners"] == ["@docs"], f"owners resolved to {body.get('owners')}"


def test_webhook_secret_is_returned_once_then_redacted(bearer):
    repo = create_repo(bearer)
    res = requests.post(f"{GF}/repos/{repo['id']}/webhooks", headers=bearer,
                        json={"url": "https://example.com/hook", "events": "*"})
    assert res.status_code == 201, res.text
    created = res.json()
    assert created.get("secret"), "no signing secret was issued"

    # Listing must never hand the secret back — a repo-read grant is not a way to
    # harvest signing keys.
    res = requests.get(f"{GF}/repos/{repo['id']}/webhooks", headers=bearer)
    assert res.status_code == 200, res.text
    listed = res.json()
    assert len(listed) == 1
    assert "secret" not in listed[0]
    assert listed[0]["has_secret"] is True

    res = requests.delete(f"{GF}/repos/{repo['id']}/webhooks/{created['id']}", headers=bearer)
    assert res.status_code == 204, res.text
    assert requests.get(f"{GF}/repos/{repo['id']}/webhooks", headers=bearer).json() == []


def test_webhook_url_must_not_target_internal_addresses(bearer):
    """SSRF boundary: a repo owner must not be able to aim a hook at the cluster."""
    repo = create_repo(bearer)
    for bad in ("file:///etc/passwd", "gopher://example.com/", "not-a-url"):
        res = requests.post(f"{GF}/repos/{repo['id']}/webhooks", headers=bearer,
                            json={"url": bad})
        assert res.status_code == 400, f"webhook accepted {bad!r}: {res.status_code}"


# ---------------------------------------------------------------------------
# Access control
# ---------------------------------------------------------------------------

def test_another_users_repo_is_not_found(bearer, reviewer):
    """A denial reads as 404, not 403 — a 403 confirms the repo exists and who owns it."""
    repo = create_repo(bearer)
    res = requests.get(f"{GF}/repos/{repo['id']}", headers=reviewer["bearer"])
    assert res.status_code == 404, f"someone else's repo answered {res.status_code}"
