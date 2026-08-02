"""Integration tests for the git Smart-HTTP wire protocol (the git plane).

Executable spec for the git surface in ARCHITECTURE.md §2b. These FAIL until the
Smart-HTTP endpoints exist. Two layers of test:

  1. Protocol-level (requests): the advertisement's status, content-type, auth
     challenge, and first pkt-line — the bits `git` is unforgiving about.
  2. End-to-end (real `git` CLI): init -> push -> fresh clone -> file is there,
     plus a second user cannot clone your private repo.

Wire contract (all under /{namespace}/{name}.git):
  GET  info/refs?service=git-upload-pack   -> 200 application/x-git-upload-pack-advertisement
  POST git-upload-pack                      (fetch/clone)   action readRepo
  GET  info/refs?service=git-receive-pack  -> 200 application/x-git-receive-pack-advertisement
  POST git-receive-pack                     (push)          action writeRepo

Auth: HTTP Basic (password = token) OR Bearer. Unauthenticated access to a
private repo -> 401 with `WWW-Authenticate: Basic`. These tests authenticate with
Bearer via `git -c http.extraHeader`, and with a Bearer header via requests.
"""
import shutil
import subprocess

import pytest
import requests

from conftest import SERVICE_URL

pytestmark = pytest.mark.skipif(shutil.which("git") is None, reason="git CLI not installed")


# ── helpers ─────────────────────────────────────────────────────────────────

def _clone_url(repo):
    """Build the clone URL from the service base + returned namespace/name, so the
    test doesn't depend on how the server derives http_url (Host vs env)."""
    return f"{SERVICE_URL}/{repo['namespace']}/{repo['name']}.git"

def _git(*args, cwd=None, token=None, check=False):
    """Run git with prompts disabled and identity/auth injected."""
    cmd = ["git"]
    if token:
        cmd += ["-c", f"http.extraHeader=Authorization: Bearer {token}"]
    cmd += list(args)
    env = {
        "GIT_TERMINAL_PROMPT": "0",
        "GIT_CONFIG_NOSYSTEM": "1",
        "HOME": str(cwd) if cwd else "/tmp",
        "GIT_AUTHOR_NAME": "test", "GIT_AUTHOR_EMAIL": "test@example.com",
        "GIT_COMMITTER_NAME": "test", "GIT_COMMITTER_EMAIL": "test@example.com",
    }
    r = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True, timeout=60)
    if check:
        assert r.returncode == 0, f"git {' '.join(args)} failed:\n{r.stderr}"
    return r


# ── advertisement: status, auth challenge, content-type, pkt-line ───────────

def test_info_refs_requires_auth(make_repo):
    """Unauthenticated access to a private repo is challenged, not served."""
    repo = make_repo()
    res = requests.get(f"{_clone_url(repo)}/info/refs",
                       params={"service": "git-upload-pack"})
    assert res.status_code == 401
    assert "basic" in res.headers.get("WWW-Authenticate", "").lower(), \
        "must send WWW-Authenticate: Basic so git prompts for credentials"

def test_info_refs_upload_pack_advertisement(make_repo, token):
    repo = make_repo()
    res = requests.get(f"{_clone_url(repo)}/info/refs",
                       params={"service": "git-upload-pack"},
                       headers={"Authorization": f"Bearer {token}"})
    assert res.status_code == 200, res.text
    assert res.headers["Content-Type"] == "application/x-git-upload-pack-advertisement"
    # First pkt-line of a Smart-HTTP advertisement: "<len># service=git-upload-pack\n"
    assert b"# service=git-upload-pack" in res.content[:64]

def test_info_refs_receive_pack_advertisement(make_repo, token):
    repo = make_repo()
    res = requests.get(f"{_clone_url(repo)}/info/refs",
                       params={"service": "git-receive-pack"},
                       headers={"Authorization": f"Bearer {token}"})
    assert res.status_code == 200, res.text
    assert res.headers["Content-Type"] == "application/x-git-receive-pack-advertisement"
    assert b"# service=git-receive-pack" in res.content[:64]

def test_info_refs_nonexistent_repo(token):
    res = requests.get(f"{SERVICE_URL}/someuser/does-not-exist.git/info/refs",
                       params={"service": "git-upload-pack"},
                       headers={"Authorization": f"Bearer {token}"})
    assert res.status_code == 404


# ── end-to-end: push then clone with the real git CLI ───────────────────────

def test_push_then_clone_roundtrip(make_repo, token, tmp_path):
    repo = make_repo()
    url = _clone_url(repo)
    branch = repo["default_branch"]

    # build a commit locally and push it to the (empty) remote
    work = tmp_path / "work"
    work.mkdir()
    _git("-c", f"init.defaultBranch={branch}", "init", cwd=work, check=True)
    (work / "README.md").write_text("hello from codearmory\n")
    _git("add", "README.md", cwd=work, check=True)
    _git("commit", "-m", "initial commit", cwd=work, check=True)
    _git("remote", "add", "origin", url, cwd=work, check=True)
    _git("push", "origin", f"HEAD:refs/heads/{branch}", cwd=work, token=token, check=True)

    # a fresh clone must contain the pushed file
    dest = tmp_path / "clone"
    _git("clone", url, str(dest), token=token, check=True)
    assert (dest / "README.md").read_text() == "hello from codearmory\n"

def test_clone_without_auth_fails(make_repo, tmp_path):
    repo = make_repo()
    r = _git("clone", _clone_url(repo), str(tmp_path / "noauth"))
    assert r.returncode != 0, "cloning a private repo without credentials must fail"

def test_other_user_cannot_clone_private_repo(make_repo, other_token, tmp_path):
    repo = make_repo()  # owned by the primary user
    r = _git("clone", _clone_url(repo), str(tmp_path / "other"), token=other_token)
    assert r.returncode != 0, "a different user must not be able to clone a private repo"
