"""Integration tests for Forge image-build submissions.

An actual build only runs on a kata/gvisor backend (not present in the compose
stack, which is RUNTIME=docker), so these tests exercise the submit-time guards that
protect the feature: builds are rejected on a shared-kernel / non-privileged runner,
and malformed build specs are rejected. That the guard fires is the security-critical
property — a build must never run on the shared-kernel sandbox.
"""
import uuid

import requests

from conftest import FORGE_URL


def test_build_unauthorized():
    res = requests.post(f"{FORGE_URL}/executions", json={"build": {"no_push": True}})
    assert res.status_code == 401


def test_build_missing_destinations(bearer):
    # Pushing (no no_push) requires destinations.
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={"build": {}})
    assert res.status_code == 400
    assert "destinations is required" in res.text.lower()


def test_build_missing_registry_auth(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "build": {"destinations": [f"reg.example.com/acme/app:{uuid.uuid4().hex[:6]}"]},
    })
    assert res.status_code == 400
    assert "registry_auth" in res.text.lower() or "registry" in res.text.lower()


def test_build_bad_dockerfile_rejected(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "build": {"no_push": True, "dockerfile": "../../etc/passwd"},
    })
    assert res.status_code == 400
    assert "dockerfile" in res.text.lower()


def test_build_requires_privileged_runner(bearer):
    """A well-formed build on the default (shared-kernel, non-privileged) runner is
    rejected — building is only allowed on a privileged kata/gvisor class."""
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "build": {
            "destinations": [f"reg.example.com/acme/app:{uuid.uuid4().hex[:6]}"],
            "no_push": True,  # avoid needing real registry creds
        },
        # a plain env (not a secret_ref) satisfies validation without an org secret
        "env": {"REGISTRY_AUTH": "{}"},
    })
    assert res.status_code == 400, f"expected the privileged-runner guard: {res.status_code} {res.text}"
    assert "privileged" in res.text.lower()
