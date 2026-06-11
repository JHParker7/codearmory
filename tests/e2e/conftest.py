"""Shared fixtures for end-to-end integration tests.

These tests exercise the full request path: CLI or HTTP client → Conductor
(port 8080) → Gatekeeper auth check → backend service. All three surfaces
(API, CLI, TUI-backing APIs) are covered in this test directory.

Environment variables (all optional — defaults target the local Docker stack):
  API_URL        Conductor base URL for HTTP tests   (default: http://localhost:8080)
  CONDUCTOR_URL  Conductor URL passed to the CLI     (default: value of API_URL)
  ARMORY_BIN     Path to a pre-built armory binary   (otherwise built from source)
"""

import os
import subprocess
import tempfile
import uuid

import pytest
import requests

API_URL = os.getenv("API_URL", "http://localhost:8080")
CONDUCTOR_URL = os.getenv("CONDUCTOR_URL", API_URL)

_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
_CLI_SRC = os.path.join(_REPO_ROOT, "src", "cli")


@pytest.fixture(scope="session")
def api_url():
    return API_URL


@pytest.fixture(scope="session")
def conductor_url():
    return CONDUCTOR_URL


@pytest.fixture(scope="session")
def armory_bin(tmp_path_factory):
    """Compiled armory binary, built once per test session."""
    if path := os.getenv("ARMORY_BIN"):
        return path
    tmp = tmp_path_factory.mktemp("armory-e2e")
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


@pytest.fixture
def new_user():
    """Sign up a unique test user and return their credentials + user_id."""
    uid = uuid.uuid4().hex[:8]
    payload = {
        "email": f"e2e_{uid}@example.com",
        "username": f"e2e_{uid}",
        "password": "e2e_password_123",
    }
    resp = requests.post(f"{API_URL}/signup", json=payload)
    assert resp.status_code == 201, f"signup failed: {resp.text}"
    payload["user_id"] = resp.json()["user_id"]
    return payload


@pytest.fixture
def token(new_user):
    """JWT for the test user, obtained from POST /login."""
    resp = requests.post(
        f"{API_URL}/login",
        json={"email": new_user["email"], "password": new_user["password"]},
    )
    assert resp.status_code == 200, f"login failed: {resp.text}"
    return resp.json()["token"]


@pytest.fixture
def bearer(token):
    return {"Authorization": f"Bearer {token}"}


@pytest.fixture
def run_cli(armory_bin):
    """Run the armory binary and return (stdout, stderr, returncode).

    Passes CODEARMORY_URL so the binary targets the local stack rather than
    its hardcoded default of localhost:8082.
    """
    def _run(*args, token=None, extra_env=None, stdin=None):
        with tempfile.TemporaryDirectory(prefix="armory-e2e-home-") as home_dir:
            env = {
                "HOME": home_dir,
                "PATH": os.environ.get("PATH", ""),
                "CODEARMORY_URL": CONDUCTOR_URL,
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
