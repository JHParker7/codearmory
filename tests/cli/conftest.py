"""Shared fixtures for armory CLI integration tests.

The tests run the actual armory binary against a live conductor service.
Set CONDUCTOR_URL to target a non-default endpoint (default: http://localhost:8082).
Set ARMORY_BIN to use a pre-built binary; otherwise the fixture builds one from
src/cli/ relative to this file.
"""

import os
import subprocess
import tempfile
import uuid

import pytest
import requests

# ---------------------------------------------------------------------------
# Binary
# ---------------------------------------------------------------------------

_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
_CLI_SRC = os.path.join(_REPO_ROOT, "src", "cli")


@pytest.fixture(scope="session")
def armory_bin(tmp_path_factory):
    """Path to the compiled armory binary. Builds once per test session."""
    if path := os.getenv("ARMORY_BIN"):
        return path
    tmp = tmp_path_factory.mktemp("armory-cli-test")
    bin_path = tmp / "armory"
    result = subprocess.run(
        ["go", "build", "-o", str(bin_path), "."],
        cwd=_CLI_SRC,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        pytest.fail(f"Failed to build armory binary:\n{result.stderr}")
    return str(bin_path)


# ---------------------------------------------------------------------------
# Service URL
# ---------------------------------------------------------------------------


@pytest.fixture(scope="session")
def conductor_url():
    return os.getenv("CONDUCTOR_URL", "http://localhost:8082")


# ---------------------------------------------------------------------------
# Users & tokens
# ---------------------------------------------------------------------------


@pytest.fixture
def new_user(conductor_url):
    uid = uuid.uuid4().hex[:8]
    payload = {
        "email": f"cli_{uid}@example.com",
        "username": f"cli_{uid}",
        "password": "password123",
    }
    resp = requests.post(f"{conductor_url}/signup", json=payload)
    assert resp.status_code == 201, f"signup failed: {resp.text}"
    payload["user_id"] = resp.json()["user_id"]
    return payload


@pytest.fixture
def token(conductor_url, new_user):
    resp = requests.post(
        f"{conductor_url}/login",
        json={"email": new_user["email"], "password": new_user["password"]},
    )
    assert resp.status_code == 200, f"login failed: {resp.text}"
    return resp.json()["token"]


# ---------------------------------------------------------------------------
# CLI runner
# ---------------------------------------------------------------------------


@pytest.fixture
def run_cli(armory_bin, conductor_url):
    """Return a callable that runs the armory binary and returns (stdout, stderr, returncode)."""

    def _run(*args, token=None, extra_env=None, stdin=None):
        with tempfile.TemporaryDirectory(prefix="armory-home-") as home_dir:
            env = {
                "HOME": home_dir,
                "PATH": os.environ.get("PATH", ""),
                "CODEARMORY_URL": conductor_url,
            }
            if token:
                env["CODEARMORY_TOKEN"] = token
            if extra_env:
                env.update(extra_env)

            result = subprocess.run(
                [armory_bin, *args],
                env=env,
                capture_output=True,
                text=True,
                input=stdin,
            )
        return result.stdout, result.stderr, result.returncode

    return _run
